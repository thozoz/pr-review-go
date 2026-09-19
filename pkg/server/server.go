package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-github/v68/github"
	"github.com/thozoz/pr-review-go/pkg/config"
	ghclient "github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/reviewer"
)

type Server struct {
	cfg    *config.Config
	engine *reviewer.Engine
	gh     *ghclient.Client
}

func NewServer(cfg *config.Config) *Server {
	return &Server{
		cfg:    cfg,
		engine: reviewer.NewEngine(cfg),
		gh:     ghclient.NewClient(cfg.GitHubToken),
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

	log.Printf("[webhook] PR %s/%s #%d triggered by action: %s", owner, repo, prNum, action)

	// Run review asynchronously in background goroutine
	go s.dispatchReview(owner, repo, prNum)
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
	if !strings.HasPrefix(body, "/review") {
		return
	}

	owner := e.GetRepo().GetOwner().GetLogin()
	repo := e.GetRepo().GetName()
	prNum := e.GetIssue().GetNumber()

	log.Printf("[webhook] PR %s/%s #%d triggered by comment: %s", owner, repo, prNum, body)

	// Run review asynchronously in background goroutine
	go s.dispatchReview(owner, repo, prNum)
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
