package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-github/v68/github"
	"github.com/thozoz/pr-review-go/pkg/assistant"
	"github.com/thozoz/pr-review-go/pkg/config"
	"github.com/thozoz/pr-review-go/pkg/describer"
	"github.com/thozoz/pr-review-go/pkg/docgen"
	ghclient "github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/labeler"
	"github.com/thozoz/pr-review-go/pkg/reviewer"
	"github.com/thozoz/pr-review-go/pkg/sandbox"
	"github.com/thozoz/pr-review-go/pkg/summarizer"
)

type Server struct {
	cfg        *config.Config
	engine     *reviewer.Engine
	labeler    *labeler.Labeler
	summarizer *summarizer.Summarizer
	assistant  *assistant.Assistant
	describer  *describer.Describer
	gh         *ghclient.Client
}

func NewServer(cfg *config.Config) *Server {
	if err := cfg.Validate(); err != nil {
		panic(fmt.Sprintf("invalid config: %v", err))
	}
	return &Server{
		cfg:        cfg,
		engine:     reviewer.NewEngine(cfg),
		labeler:    labeler.NewLabeler(cfg),
		summarizer: summarizer.NewSummarizer(cfg),
		assistant:  assistant.NewAssistant(cfg),
		describer:  describer.NewDescriber(cfg),
		gh:         ghclient.NewClient(cfg.GitHubToken),
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// Health check
	mux.HandleFunc("GET /", s.handleHealth)
	mux.HandleFunc("GET /healthz", s.handleHealth)

	// Webhook endpoint
	mux.HandleFunc("POST /api/v1/github_webhooks", s.handleWebhook)

	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"service": "pr-review-go",
		"version": "1.0.0",
	})
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	var payload []byte
	var err error

	if s.cfg.WebhookSecret != "" {
		payload, err = github.ValidatePayload(r, []byte(s.cfg.WebhookSecret))
		if err != nil {
			log.Printf("[webhook] HMAC validation failed: %v", err)
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
	} else {
		payload, err = github.ValidatePayload(r, nil)
		if err != nil {
			log.Printf("[webhook] Failed to read payload: %v", err)
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
	}

	event, err := github.ParseWebHook(github.WebHookType(r), payload)
	if err != nil {
		log.Printf("[webhook] Failed to parse webhook event: %v", err)
		http.Error(w, "cannot parse webhook", http.StatusBadRequest)
		return
	}

	switch e := event.(type) {
	case *github.PullRequestEvent:
		s.handlePullRequestEvent(e)
	case *github.IssueCommentEvent:
		s.handleIssueCommentEvent(e)
	default:
		// Other events ignored silently
		log.Printf("[webhook] Ignoring event type: %s", github.WebHookType(r))
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"accepted"}`))
}

func (s *Server) handlePullRequestEvent(e *github.PullRequestEvent) {
	action := e.GetAction()
	// Trigger review on opened or when new commits are pushed (synchronize)
	if action != "opened" && action != "synchronize" && action != "reopened" {
		return
	}

	owner := e.GetRepo().GetOwner().GetLogin()
	repo := e.GetRepo().GetName()
	prNum := e.GetPullRequest().GetNumber()

	if owner == "" || repo == "" {
		log.Printf("[webhook] Invalid owner/repo in PR event")
		return
	}

	log.Printf("[webhook] PR %s/%s #%d triggered by action: %s", owner, repo, prNum, action)

	// Configured actions run only when a PR first opens. Later pushes run review
	// only, preventing description/label churn.
	if action == "opened" {
		if s.cfg.AutoActionEnabled("labels") {
			go s.dispatchLabels(owner, repo, prNum)
		}
		if s.cfg.AutoActionEnabled("describe") && strings.TrimSpace(e.GetPullRequest().GetBody()) == "" {
			go s.dispatchDescribe(owner, repo, prNum)
		}
		if s.cfg.AutoActionEnabled("improve") {
			go s.dispatchImprove(owner, repo, prNum)
		}
		if s.cfg.AutoActionEnabled("review") {
			go s.dispatchReview(owner, repo, prNum)
		}
		return
	}

	if s.cfg.AutoActionEnabled("review") {
		go s.dispatchReview(owner, repo, prNum)
	}
}

func (s *Server) handleIssueCommentEvent(e *github.IssueCommentEvent) {
	if e.GetAction() != "created" {
		return
	}

	// Must be a comment on a PR (not a plain issue)
	if !e.GetIssue().IsPullRequest() {
		return
	}

	body := strings.TrimSpace(e.GetComment().GetBody())
	owner := e.GetRepo().GetOwner().GetLogin()
	repo := e.GetRepo().GetName()
	prNum := e.GetIssue().GetNumber()

	if owner == "" || repo == "" {
		return
	}

	if strings.HasPrefix(body, "/review") {
		log.Printf("[webhook] PR %s/%s #%d triggered review by comment: %s", owner, repo, prNum, body)
		go s.dispatchReview(owner, repo, prNum)
	} else if strings.HasPrefix(body, "/improve") {
		log.Printf("[webhook] PR %s/%s #%d triggered improvements", owner, repo, prNum)
		go s.dispatchImprove(owner, repo, prNum)
	} else if strings.HasPrefix(body, "/describe") {
		log.Printf("[webhook] PR %s/%s #%d triggered description generation", owner, repo, prNum)
		go s.dispatchDescribe(owner, repo, prNum)
	} else if strings.HasPrefix(body, "/generate_labels") || strings.HasPrefix(body, "/labels") {
		log.Printf("[webhook] PR %s/%s #%d triggered label generation by comment: %s", owner, repo, prNum, body)
		go s.dispatchLabels(owner, repo, prNum)
	} else if strings.HasPrefix(body, "/summarize") || strings.HasPrefix(body, "/summary") {
		log.Printf("[webhook] PR %s/%s #%d triggered discussion summary by comment: %s", owner, repo, prNum, body)
		go s.dispatchSummary(owner, repo, prNum)
	} else if strings.HasPrefix(body, "/add_docs") || strings.HasPrefix(body, "/docs") {
		log.Printf("[webhook] PR %s/%s #%d triggered add_docs by comment: %s", owner, repo, prNum, body)
		go s.dispatchAddDocs(owner, repo, prNum)
	} else if strings.HasPrefix(body, "@bot") || strings.HasPrefix(body, "@pr-review") || strings.HasPrefix(body, "/ask") {
		question := strings.TrimSpace(strings.TrimPrefix(body, "@bot"))
		question = strings.TrimSpace(strings.TrimPrefix(question, "@pr-review"))
		question = strings.TrimSpace(strings.TrimPrefix(question, "/ask"))
		log.Printf("[webhook] PR %s/%s #%d triggered interactive assistant: %s", owner, repo, prNum, question)
		go s.dispatchAssistant(owner, repo, prNum, question)
	}
}

func (s *Server) dispatchDescribe(owner, repo string, prNum int) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	updated, err := s.describer.RunAndUpdate(ctx, owner, repo, prNum)
	if err != nil {
		log.Printf("[describer] Failed for %s/%s #%d: %v", owner, repo, prNum, err)
		return
	}
	if updated {
		log.Printf("[describer] Updated PR body for %s/%s #%d", owner, repo, prNum)
	}
}

func (s *Server) dispatchImprove(owner, repo string, prNum int) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	report, err := s.engine.ReviewPR(ctx, owner, repo, prNum)
	if err != nil {
		log.Printf("[improve] Review failed for %s/%s #%d: %v", owner, repo, prNum, err)
		return
	}
	if len(report.Suggestions) == 0 {
		log.Printf("[improve] No safe one-click suggestions for %s/%s #%d", owner, repo, prNum)
		return
	}
	if err := s.gh.PostSuggestions(ctx, owner, repo, prNum, report.HeadSHA, report.Suggestions); err != nil {
		log.Printf("[improve] Failed posting suggestions for %s/%s #%d: %v", owner, repo, prNum, err)
		return
	}
	log.Printf("[improve] Posted %d suggestions to %s/%s #%d", len(report.Suggestions), owner, repo, prNum)
}

func (s *Server) dispatchSummary(owner, repo string, prNum int) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	log.Printf("[summarizer] Generating discussion summary for %s/%s #%d...", owner, repo, prNum)
	_, err := s.summarizer.RunAndPost(ctx, owner, repo, prNum)
	if err != nil {
		log.Printf("[summarizer] Failed to generate/post summary for %s/%s #%d: %v", owner, repo, prNum, err)
		return
	}
	log.Printf("[summarizer] Successfully posted discussion summary to %s/%s #%d", owner, repo, prNum)
}

func (s *Server) dispatchAddDocs(owner, repo string, prNum int) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	log.Printf("[docgen] Scanning for undocumented items on %s/%s #%d...", owner, repo, prNum)
	pr, err := s.gh.GetPR(ctx, owner, repo, prNum)
	if err != nil {
		log.Printf("[docgen] Failed to fetch PR %s/%s #%d: %v", owner, repo, prNum, err)
		return
	}

	runner := sandbox.NewRunner(0)
	workDir, cleanup, err := runner.PrepareWorkspace(ctx, pr.CloneURL, pr.HeadRef, pr.HeadSHA)
	if err != nil {
		log.Printf("[docgen] Failed to checkout workspace for %s/%s #%d: %v", owner, repo, prNum, err)
		return
	}
	defer cleanup()

	items, err := docgen.FindUndocumentedGoItems(workDir)
	if err != nil {
		log.Printf("[docgen] Failed scanning %s/%s #%d: %v", owner, repo, prNum, err)
		return
	}

	var reportText string
	if len(items) == 0 {
		reportText = "## 📝 Documentation Report\n\nAll exported Go functions and types have doc comments! Everything looks great."
	} else {
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("## 📝 Documentation Report: Found %d Undocumented Declarations\n\n", len(items)))
		for _, it := range items {
			relPath, _ := filepath.Rel(workDir, it.File)
			sb.WriteString(fmt.Sprintf("- `%s` (`%s` in `%s`)\n", it.Name, it.Kind, relPath))
		}
		reportText = sb.String()
	}

	if err := s.gh.PostComment(ctx, owner, repo, prNum, reportText); err != nil {
		log.Printf("[docgen] Failed to post comment on %s/%s #%d: %v", owner, repo, prNum, err)
		return
	}
	log.Printf("[docgen] Successfully posted documentation report to %s/%s #%d", owner, repo, prNum)
}

func (s *Server) dispatchAssistant(owner, repo string, prNum int, question string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	log.Printf("[assistant] Running interactive task for %s/%s #%d: %s", owner, repo, prNum, question)
	_, err := s.assistant.RunAndReply(ctx, owner, repo, prNum, question)
	if err != nil {
		log.Printf("[assistant] Failed to run assistant task for %s/%s #%d: %v", owner, repo, prNum, err)
		return
	}
	log.Printf("[assistant] Successfully posted assistant reply to %s/%s #%d", owner, repo, prNum)
}

func (s *Server) dispatchLabels(owner, repo string, prNum int) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	log.Printf("[labeler] Generating labels for %s/%s #%d...", owner, repo, prNum)
	applied, err := s.labeler.RunAndApply(ctx, owner, repo, prNum)
	if err != nil {
		log.Printf("[labeler] Failed to generate/apply labels for %s/%s #%d: %v", owner, repo, prNum, err)
		return
	}
	log.Printf("[labeler] Successfully applied labels to %s/%s #%d: %v", owner, repo, prNum, applied)
}

func (s *Server) dispatchReview(owner, repo string, prNum int) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	log.Printf("[runner] Starting review for %s/%s #%d...", owner, repo, prNum)
	report, err := s.engine.ReviewPR(ctx, owner, repo, prNum)
	if err != nil {
		log.Printf("[runner] Review failed for %s/%s #%d: %v", owner, repo, prNum, err)
		return
	}

	log.Printf("[runner] Review finished for %s/%s #%d (Score: %d). Posting comment...", owner, repo, prNum, report.Score)
	if err := s.gh.PostComment(ctx, owner, repo, prNum, report.RawMarkdown); err != nil {
		log.Printf("[runner] Failed to post comment on %s/%s #%d: %v", owner, repo, prNum, err)
		return
	}

	log.Printf("[runner] Review comment successfully posted to %s/%s #%d", owner, repo, prNum)
}

func (s *Server) ListenAndServe() error {
	addr := fmt.Sprintf("0.0.0.0:%d", s.cfg.Port)
	log.Printf("🚀 PR Review Server listening on http://%s", addr)
	return http.ListenAndServe(addr, s.Routes())
}
