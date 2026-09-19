package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/thozoz/pr-review-go/pkg/config"
	"github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/reviewer"
)

func main() {
	prURL := flag.String("pr", "", "Full GitHub PR URL (e.g. https://github.com/owner/repo/pull/12)")
	repoFlag := flag.String("repo", "", "Repository in owner/repo format")
	numFlag := flag.Int("num", 0, "PR Number")
	postComment := flag.Bool("post", false, "Post the review markdown as a comment on GitHub")
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

	fmt.Printf("🚀 Starting review for %s/%s #%d...\n", owner, repo, num)
	fmt.Printf("📦 Sandbox Verification: %v | Model: %s\n", cfg.EnableSandbox, cfg.LLMModel)

	engine := reviewer.NewEngine(cfg)
	ctx := context.Background()

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
