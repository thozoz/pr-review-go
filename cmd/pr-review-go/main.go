package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/thozoz/pr-review-go/pkg/assistant"
	"github.com/thozoz/pr-review-go/pkg/config"
	"github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/labeler"
	"github.com/thozoz/pr-review-go/pkg/reviewer"
	"github.com/thozoz/pr-review-go/pkg/server"
	"github.com/thozoz/pr-review-go/pkg/summarizer"
)

func main() {
	serverMode := flag.Bool("server", false, "Run in background webhook server daemon mode")
	prURL := flag.String("pr", "", "Full GitHub PR URL (e.g. https://github.com/owner/repo/pull/12)")
	repoFlag := flag.String("repo", "", "Repository in owner/repo format")
	numFlag := flag.Int("num", 0, "PR Number")
	postComment := flag.Bool("post", false, "Post the review markdown as a comment on GitHub")
	onlyLabels := flag.Bool("labels", false, "Generate and apply labels to PR instead of full code review")
	onlySummary := flag.Bool("summary", false, "Summarize PR discussions and reviews instead of code review")
	askQuestion := flag.String("ask", "", "Ask the interactive PR assistant a question or give an execution command")
	noSandbox := flag.Bool("no-sandbox", false, "Disable local runner sandbox verification")
	modelFlag := flag.String("model", "", "LLM model to use")
	flag.Parse()

	cfg := config.Load()
	if *modelFlag != "" {
		cfg.LLMModel = *modelFlag
	}
	if *noSandbox {
		cfg.EnableSandbox = false
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Configuration error: %v\n", err)
		fmt.Fprintf(os.Stderr, "Please set the required environment variables.\n")
		os.Exit(1)
	}

	if *serverMode || (len(flag.Args()) > 0 && flag.Args()[0] == "server") {
		srv := server.NewServer(cfg)
		if err := srv.ListenAndServe(); err != nil {
			fmt.Fprintf(os.Stderr, "Server failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	var owner, repo string
	var num int

	if *prURL != "" {
		var err error
		owner, repo, num, err = github.ParsePRURL(*prURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error parsing PR URL: %v\n", err)
			os.Exit(1)
		}
	} else if *repoFlag != "" && *numFlag > 0 {
		var err error
		parts := github.ParseRepoOwner(*repoFlag)
		if len(parts) != 2 {
			fmt.Fprintf(os.Stderr, "Invalid repo flag format, expected owner/repo: %s\n", *repoFlag)
			os.Exit(1)
		}
		owner, repo = parts[0], parts[1]
		num = *numFlag
		_ = err
	} else {
		fmt.Fprintf(os.Stderr, "Usage: pr-review-go -pr <PR_URL> [-post] [-no-sandbox] [-model <model>]\n")
		os.Exit(1)
	}

	ctx := context.Background()

	if *onlyLabels {
		fmt.Printf("🏷️ Generating labels for %s/%s #%d...\n", owner, repo, num)
		lbl := labeler.NewLabeler(cfg)
		labels, err := lbl.RunAndApply(ctx, owner, repo, num)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Labeling failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✅ Applied labels: %v\n", labels)
		return
	}

	if *onlySummary {
		fmt.Printf("📝 Summarizing PR discussions for %s/%s #%d...\n", owner, repo, num)
		summ := summarizer.NewSummarizer(cfg)
		var text string
		var summErr error
		if *postComment {
			text, summErr = summ.RunAndPost(ctx, owner, repo, num)
		} else {
			text, summErr = summ.SummarizeDiscussions(ctx, owner, repo, num)
		}
		if summErr != nil {
			fmt.Fprintf(os.Stderr, "❌ Summary failed: %v\n", summErr)
			os.Exit(1)
		}
		fmt.Printf("\n%s\n", text)
		return
	}

	if *askQuestion != "" {
		fmt.Printf("🤖 Asking interactive assistant for %s/%s #%d: %s\n", owner, repo, num, *askQuestion)
		asst := assistant.NewAssistant(cfg)
		var text string
		var asstErr error
		if *postComment {
			text, asstErr = asst.RunAndReply(ctx, owner, repo, num, *askQuestion)
		} else {
			text, asstErr = asst.HandleMention(ctx, owner, repo, num, *askQuestion)
		}
		if asstErr != nil {
			fmt.Fprintf(os.Stderr, "❌ Assistant failed: %v\n", asstErr)
			os.Exit(1)
		}
		fmt.Printf("\n%s\n", text)
		return
	}

	fmt.Printf("🚀 Starting review for %s/%s #%d...\n", owner, repo, num)
	fmt.Printf("📦 Sandbox Verification: %v | Model: %s\n", cfg.EnableSandbox, cfg.LLMModel)

	engine := reviewer.NewEngine(cfg)

	report, err := engine.ReviewPR(ctx, owner, repo, num)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Review failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("\n" + report.RawMarkdown)

	if *postComment {
		ghClient := github.NewClient(cfg.GitHubToken)
		fmt.Printf("💬 Posting review comment to GitHub...\n")
		if err := ghClient.PostComment(ctx, owner, repo, num, report.RawMarkdown); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Failed to post comment to GitHub: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("✅ Review comment posted successfully.")
	}
}
