package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v68/github"
	"github.com/thozoz/pr-review-go/pkg/assistant"
	"github.com/thozoz/pr-review-go/pkg/changelog"
	"github.com/thozoz/pr-review-go/pkg/config"
	"github.com/thozoz/pr-review-go/pkg/describer"
	"github.com/thozoz/pr-review-go/pkg/docgen"
	ghclient "github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/labeler"
	"github.com/thozoz/pr-review-go/pkg/reviewer"
	"github.com/thozoz/pr-review-go/pkg/sandbox"
	"github.com/thozoz/pr-review-go/pkg/summarizer"
	"github.com/thozoz/pr-review-go/pkg/version"
)

type Server struct {
	cfg          *config.Config
	engine       *reviewer.Engine
	labeler      *labeler.Labeler
	summarizer   *summarizer.Summarizer
	assistant    *assistant.Assistant
	describer    *describer.Describer
	changelog    *changelog.Updater
	gh           *ghclient.Client
	dispatchHook func(action, owner, repo string, prNum int)

	store        JobStore
	scheduler    *Scheduler
	runtimeReady bool
	runtimeErr   error
	mu           sync.RWMutex
}

func NewServer(cfg *config.Config) *Server {
	if err := cfg.Validate(); err != nil {
		panic(fmt.Sprintf("invalid config: %v", err))
	}
	if cfg.IsGitHubAppSetupMode() {
		return &Server{
			cfg: cfg,
		}
	}
	return &Server{
		cfg:        cfg,
		engine:     reviewer.NewEngine(cfg),
		labeler:    labeler.NewLabeler(cfg),
		summarizer: summarizer.NewSummarizer(cfg),
		assistant:  assistant.NewAssistant(cfg),
		describer:  describer.NewDescriber(cfg),
		changelog:  changelog.NewUpdater(cfg),
		gh:         ghclient.NewClientFromConfig(cfg),
	}
}

// Start opens durable state and starts workers. It creates no filesystem writes
// in setup mode. In normal mode, it opens the bbolt store and initiates the scheduler.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cfg.IsGitHubAppSetupMode() {
		s.runtimeReady = true
		return nil
	}

	if s.store == nil {
		dbPath := filepath.Join(s.cfg.WebhookStateDir, "jobs.db")
		storeOpts := StoreOptions{
			BacklogLimit:  s.cfg.WebhookBacklog,
			DeliveryLimit: s.cfg.WebhookDeliveryLimit,
			StateMaxBytes: s.cfg.WebhookStateMaxBytes,
			DeliveryTTL:   s.cfg.WebhookDeliveryTTL,
			OpenTimeout:   1 * time.Second,
		}
		store, err := OpenJobStore(dbPath, storeOpts)
		if err != nil {
			s.runtimeErr = err
			return err
		}
		s.store = store
	}

	if s.scheduler == nil {
		executor := NewServerJobExecutor(s, s.store)
		s.scheduler = NewScheduler(s.store, executor, s.cfg.WebhookWorkers)
	}

	if err := s.scheduler.Start(ctx); err != nil {
		s.runtimeErr = err
		return err
	}

	s.runtimeReady = true
	s.runtimeErr = nil
	return nil
}

// Stop stops workers and closes the durable job store.
func (s *Server) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scheduler != nil {
		s.scheduler.Stop()
	}
	if s.store != nil {
		_ = s.store.Close()
	}
	s.runtimeReady = false
}

func (s *Server) SetStore(store JobStore) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store = store
}

func (s *Server) SetScheduler(scheduler *Scheduler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scheduler = scheduler
}

func (s *Server) SetEngine(engine *reviewer.Engine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.engine = engine
}

func (s *Server) SetRuntimeError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runtimeErr = err
}

func (s *Server) Store() JobStore {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.store
}

func (s *Server) Scheduler() *Scheduler {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.scheduler
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// Health check (always available, including in setup mode)
	mux.HandleFunc("GET /{$}", s.handleHealth)
	mux.HandleFunc("GET /healthz", s.handleHealth)

	if s.cfg.IsGitHubAppSetupMode() {
		mux.HandleFunc("GET /setup/github-app", s.handleGitHubAppSetup)
		mux.HandleFunc("GET /setup/github-app/callback", s.handleGitHubAppCallback)
		return mux
	}

	// Webhook endpoint (only in normal mode)
	mux.HandleFunc("POST /api/v1/github_webhooks", s.handleWebhook)

	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	runtimeErr := s.runtimeErr
	ready := s.runtimeReady
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	status := "ok"
	runtimeStatus := "ready"
	if s.cfg.IsGitHubAppSetupMode() {
		runtimeStatus = "setup_mode"
	} else if runtimeErr != nil {
		status = "degraded"
		runtimeStatus = "unavailable"
	} else if !ready && s.store == nil {
		runtimeStatus = "uninitialized"
	}

	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  status,
		"runtime": runtimeStatus,
		"service": "pr-review-go",
		"version": version.Version,
	})
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if s.cfg.WebhookSecret == "" {
		log.Printf("[webhook] Webhook secret not configured")
		http.Error(w, "webhook secret not configured", http.StatusUnauthorized)
		return
	}

	// Apply MaxBytesReader before ValidatePayload
	if s.cfg.WebhookBodyMaxBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, s.cfg.WebhookBodyMaxBytes)
	}

	payload, err := github.ValidatePayload(r, []byte(s.cfg.WebhookSecret))
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) || strings.Contains(err.Error(), "too large") {
			http.Error(w, "request entity too large", http.StatusRequestEntityTooLarge)
			return
		}
		log.Printf("[webhook] HMAC validation failed: %v", err)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	eventType := github.WebHookType(r)
	if eventType == "ping" {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
		return
	}

	s.mu.RLock()
	store := s.store
	runtimeErr := s.runtimeErr
	s.mu.RUnlock()

	if runtimeErr != nil {
		w.Header().Set("Retry-After", "30")
		http.Error(w, "runtime unavailable", http.StatusServiceUnavailable)
		return
	}

	deliveryID := r.Header.Get("X-GitHub-Delivery")

	if store == nil {
		if s.dispatchHook != nil && deliveryID == "" {
			s.handleLegacyWebhook(w, r, payload)
			return
		}
		w.Header().Set("Retry-After", "30")
		http.Error(w, "runtime unavailable", http.StatusServiceUnavailable)
		return
	}

	if !isValidDeliveryID(deliveryID) {
		http.Error(w, "invalid delivery identifier", http.StatusBadRequest)
		return
	}

	event, err := github.ParseWebHook(eventType, payload)
	if err != nil {
		log.Printf("[webhook] Failed to parse webhook event: %v", err)
		http.Error(w, "cannot parse webhook", http.StatusBadRequest)
		return
	}

	switch e := event.(type) {
	case *github.PullRequestEvent:
		s.handlePullRequestWebhook(r.Context(), w, e, deliveryID, payload)
	case *github.IssueCommentEvent:
		s.handleIssueCommentWebhook(r.Context(), w, e, deliveryID, payload)
	default:
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ignored"}`))
	}
}

func (s *Server) handlePullRequestWebhook(ctx context.Context, w http.ResponseWriter, e *github.PullRequestEvent, deliveryID string, payload []byte) {
	action := e.GetAction()
	if action != "opened" && action != "synchronize" && action != "reopened" {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ignored"}`))
		return
	}

	repoID := e.GetRepo().GetID()
	prNum := e.GetPullRequest().GetNumber()
	owner := e.GetRepo().GetOwner().GetLogin()
	repo := e.GetRepo().GetName()

	if repoID <= 0 || prNum <= 0 || owner == "" || repo == "" {
		http.Error(w, "invalid repository or pull request identifier", http.StatusBadRequest)
		return
	}

	prKey, err := MakePRKey("github.com", repoID, prNum)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	baseSHA := e.GetPullRequest().GetBase().GetSHA()
	headSHA := e.GetPullRequest().GetHead().GetSHA()

	var jobs []Job
	if action == "opened" {
		if s.cfg.AutoActionEnabled("labels") {
			jobs = append(jobs, Job{
				Kind:     "labels",
				Trigger:  "automatic",
				PRKey:    prKey,
				Owner:    owner,
				Repo:     repo,
				PRNumber: prNum,
				BaseSHA:  baseSHA,
				HeadSHA:  headSHA,
			})
		}
		if s.cfg.AutoActionEnabled("describe") && strings.TrimSpace(e.GetPullRequest().GetBody()) == "" {
			jobs = append(jobs, Job{
				Kind:     "describe",
				Trigger:  "automatic",
				PRKey:    prKey,
				Owner:    owner,
				Repo:     repo,
				PRNumber: prNum,
				BaseSHA:  baseSHA,
				HeadSHA:  headSHA,
			})
		}
		if s.cfg.AutoActionEnabled("improve") {
			jobs = append(jobs, Job{
				Kind:     "improve",
				Trigger:  "automatic",
				PRKey:    prKey,
				Owner:    owner,
				Repo:     repo,
				PRNumber: prNum,
				BaseSHA:  baseSHA,
				HeadSHA:  headSHA,
			})
		}
		if s.cfg.AutoActionEnabled("review") {
			jobs = append(jobs, Job{
				Kind:     "review",
				Trigger:  "automatic",
				PRKey:    prKey,
				Owner:    owner,
				Repo:     repo,
				PRNumber: prNum,
				BaseSHA:  baseSHA,
				HeadSHA:  headSHA,
			})
		}
	} else {
		if s.cfg.AutoActionEnabled("review") {
			jobs = append(jobs, Job{
				Kind:     "review",
				Trigger:  "automatic",
				PRKey:    prKey,
				Owner:    owner,
				Repo:     repo,
				PRNumber: prNum,
				BaseSHA:  baseSHA,
				HeadSHA:  headSHA,
			})
		}
	}

	if len(jobs) == 0 {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ignored"}`))
		return
	}

	h := sha256.Sum256(payload)
	payloadHash := hex.EncodeToString(h[:])
	delivery := Delivery{
		Host:        "github.com",
		RepoID:      repoID,
		DeliveryID:  deliveryID,
		EventKind:   "pull_request",
		PayloadHash: payloadHash,
		ReceivedAt:  time.Now().UTC(),
	}

	res, err := s.store.Admit(ctx, delivery, jobs)
	if err != nil {
		log.Printf("[webhook] Store admission error: %v", err)
		w.Header().Set("Retry-After", "30")
		http.Error(w, "storage failure", http.StatusServiceUnavailable)
		return
	}

	switch res.Status {
	case AdmitAccepted:
		if s.dispatchHook != nil {
			for _, j := range jobs {
				s.dispatchHook(j.Kind, owner, repo, prNum)
			}
		}
		if s.scheduler != nil {
			s.scheduler.Wake()
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"accepted"}`))
	case AdmitDuplicate:
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"duplicate"}`))
	case AdmitCollision:
		http.Error(w, "delivery collision", http.StatusConflict)
	case AdmitCapacityFull:
		w.Header().Set("Retry-After", "30")
		http.Error(w, res.Reason, http.StatusServiceUnavailable)
	}
}

func (s *Server) handleIssueCommentWebhook(ctx context.Context, w http.ResponseWriter, e *github.IssueCommentEvent, deliveryID string, payload []byte) {
	if e.GetAction() != "created" || !e.GetIssue().IsPullRequest() {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ignored"}`))
		return
	}

	repoID := e.GetRepo().GetID()
	prNum := e.GetIssue().GetNumber()
	owner := e.GetRepo().GetOwner().GetLogin()
	repo := e.GetRepo().GetName()

	if repoID <= 0 || prNum <= 0 || owner == "" || repo == "" {
		http.Error(w, "invalid repository or pull request identifier", http.StatusBadRequest)
		return
	}

	body := strings.TrimSpace(e.GetComment().GetBody())
	if !isCommentCommand(body) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ignored"}`))
		return
	}

	username := e.GetComment().GetUser().GetLogin()
	permCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	allowed, err := s.gh.CanWriteRepository(permCtx, owner, repo, username)
	if err != nil || !allowed {
		log.Printf("[webhook] Denied comment command from %q on %s/%s #%d: permission denied or unavailable: %v", username, owner, repo, prNum, err)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ignored"}`))
		return
	}

	kind, payloadText := parseCommentCommand(body)
	if kind == "" {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ignored"}`))
		return
	}

	prKey, err := MakePRKey("github.com", repoID, prNum)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	job := Job{
		Kind:     kind,
		Trigger:  "explicit",
		Author:   username,
		PRKey:    prKey,
		Owner:    owner,
		Repo:     repo,
		PRNumber: prNum,
		Payload:  payloadText,
	}

	if kind == "review" {
		pr, prErr := s.gh.GetPR(ctx, owner, repo, prNum)
		if prErr == nil && pr != nil {
			job.BaseSHA = pr.BaseSHA
			job.HeadSHA = pr.HeadSHA
		}
	}

	h := sha256.Sum256(payload)
	payloadHash := hex.EncodeToString(h[:])
	delivery := Delivery{
		Host:        "github.com",
		RepoID:      repoID,
		DeliveryID:  deliveryID,
		EventKind:   "issue_comment",
		PayloadHash: payloadHash,
		ReceivedAt:  time.Now().UTC(),
	}

	res, err := s.store.Admit(ctx, delivery, []Job{job})
	if err != nil {
		log.Printf("[webhook] Store admission error: %v", err)
		w.Header().Set("Retry-After", "30")
		http.Error(w, "storage failure", http.StatusServiceUnavailable)
		return
	}

	switch res.Status {
	case AdmitAccepted:
		if s.dispatchHook != nil {
			s.dispatchHook(kind, owner, repo, prNum)
		}
		if s.scheduler != nil {
			s.scheduler.Wake()
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"accepted"}`))
	case AdmitDuplicate:
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"duplicate"}`))
	case AdmitCollision:
		http.Error(w, "delivery collision", http.StatusConflict)
	case AdmitCapacityFull:
		w.Header().Set("Retry-After", "30")
		http.Error(w, res.Reason, http.StatusServiceUnavailable)
	}
}

func (s *Server) handleLegacyWebhook(w http.ResponseWriter, r *http.Request, payload []byte) {
	event, err := github.ParseWebHook(github.WebHookType(r), payload)
	if err != nil {
		http.Error(w, "cannot parse webhook", http.StatusBadRequest)
		return
	}
	switch e := event.(type) {
	case *github.PullRequestEvent:
		s.handlePullRequestEvent(e)
	case *github.IssueCommentEvent:
		s.handleIssueCommentEvent(r.Context(), e)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"accepted"}`))
}

func isValidDeliveryID(s string) bool {
	if len(s) < 1 || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] <= 32 || s[i] > 126 {
			return false
		}
	}
	return true
}

func parseCommentCommand(body string) (kind string, payload string) {
	switch {
	case strings.HasPrefix(body, "/review"):
		return "review", ""
	case strings.HasPrefix(body, "/improve"):
		return "improve", ""
	case strings.HasPrefix(body, "/describe"):
		return "describe", ""
	case strings.HasPrefix(body, "/update_changelog"):
		return "changelog", ""
	case strings.HasPrefix(body, "/generate_labels") || strings.HasPrefix(body, "/labels"):
		return "labels", ""
	case strings.HasPrefix(body, "/summarize") || strings.HasPrefix(body, "/summary"):
		return "summary", ""
	case strings.HasPrefix(body, "/add_docs") || strings.HasPrefix(body, "/docs"):
		return "docs", ""
	case strings.HasPrefix(body, "@bot") || strings.HasPrefix(body, "@pr-review") || strings.HasPrefix(body, "/ask"):
		question := strings.TrimSpace(strings.TrimPrefix(body, "@bot"))
		question = strings.TrimSpace(strings.TrimPrefix(question, "@pr-review"))
		question = strings.TrimSpace(strings.TrimPrefix(question, "/ask"))
		return "assistant", question
	default:
		return "", ""
	}
}

func (s *Server) handlePullRequestEvent(e *github.PullRequestEvent) {
	action := e.GetAction()
	if action != "opened" && action != "synchronize" && action != "reopened" {
		return
	}

	owner := e.GetRepo().GetOwner().GetLogin()
	repo := e.GetRepo().GetName()
	prNum := e.GetPullRequest().GetNumber()

	if owner == "" || repo == "" {
		return
	}

	if action == "opened" {
		if s.cfg.AutoActionEnabled("labels") {
			go s.dispatchLabels(owner, repo, prNum)
		}
		if s.cfg.AutoActionEnabled("describe") && strings.TrimSpace(e.GetPullRequest().GetBody()) == "" {
			go s.dispatchDescribe(owner, repo, prNum)
		}
		if s.cfg.AutoActionEnabled("improve") {
			log.Printf("[improve] Skipped for %s/%s #%d: container verification unavailable", owner, repo, prNum)
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

func (s *Server) handleIssueCommentEvent(ctx context.Context, e *github.IssueCommentEvent) {
	if e.GetAction() != "created" || !e.GetIssue().IsPullRequest() {
		return
	}

	body := strings.TrimSpace(e.GetComment().GetBody())
	owner := e.GetRepo().GetOwner().GetLogin()
	repo := e.GetRepo().GetName()
	prNum := e.GetIssue().GetNumber()

	if owner == "" || repo == "" || !isCommentCommand(body) {
		return
	}

	username := e.GetComment().GetUser().GetLogin()
	permissionCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	allowed, err := s.gh.CanWriteRepository(permissionCtx, owner, repo, username)
	if err != nil || !allowed {
		log.Printf("[webhook] Ignoring comment command from %q on %s/%s #%d: permission denied or unavailable: %v", username, owner, repo, prNum, err)
		return
	}

	dispatch := func(action string, runner func()) {
		if s.dispatchHook != nil {
			s.dispatchHook(action, owner, repo, prNum)
		}
		go runner()
	}

	if strings.HasPrefix(body, "/review") {
		dispatch("review", func() { s.dispatchReview(owner, repo, prNum) })
	} else if strings.HasPrefix(body, "/improve") {
		dispatch("improve", func() { s.dispatchImprove(owner, repo, prNum) })
	} else if strings.HasPrefix(body, "/describe") {
		dispatch("describe", func() { s.dispatchDescribe(owner, repo, prNum) })
	} else if strings.HasPrefix(body, "/update_changelog") {
		dispatch("changelog", func() { s.dispatchChangelog(owner, repo, prNum) })
	} else if strings.HasPrefix(body, "/generate_labels") || strings.HasPrefix(body, "/labels") {
		dispatch("labels", func() { s.dispatchLabels(owner, repo, prNum) })
	} else if strings.HasPrefix(body, "/summarize") || strings.HasPrefix(body, "/summary") {
		dispatch("summary", func() { s.dispatchSummary(owner, repo, prNum) })
	} else if strings.HasPrefix(body, "/add_docs") || strings.HasPrefix(body, "/docs") {
		dispatch("add_docs", func() { s.dispatchAddDocs(owner, repo, prNum) })
	} else if strings.HasPrefix(body, "@bot") || strings.HasPrefix(body, "@pr-review") || strings.HasPrefix(body, "/ask") {
		question := strings.TrimSpace(strings.TrimPrefix(body, "@bot"))
		question = strings.TrimSpace(strings.TrimPrefix(question, "@pr-review"))
		question = strings.TrimSpace(strings.TrimPrefix(question, "/ask"))
		dispatch("assistant", func() { s.dispatchAssistant(owner, repo, prNum, question) })
	}
}

func isCommentCommand(body string) bool {
	for _, prefix := range []string{
		"/review", "/improve", "/describe", "/update_changelog", "/generate_labels", "/labels",
		"/summarize", "/summary", "/add_docs", "/docs", "@bot", "@pr-review", "/ask",
	} {
		if strings.HasPrefix(body, prefix) {
			return true
		}
	}
	return false
}

func (s *Server) dispatchChangelog(owner, repo string, prNum int) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := s.changelog.RunAndPost(ctx, owner, repo, prNum); err != nil {
		log.Printf("[changelog] Failed for %s/%s #%d: %v", owner, repo, prNum, err)
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const message = "## PR Improvement Suggestions\n\nOne-click suggestions are unavailable until isolated project verification is configured. No build or tests were run."
	if err := s.gh.PostComment(ctx, owner, repo, prNum, message); err != nil {
		log.Printf("[improve] Failed posting unavailable notice for %s/%s #%d: %v", owner, repo, prNum, err)
	}
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

	runner := sandbox.NewPlatformRunner(s.cfg, s.gh, nil, nil)
	snap, cleanup, err := runner.PrepareSnapshot(ctx, pr.CloneURL, pr.HeadRef, pr.HeadSHA)
	if err != nil {
		log.Printf("[docgen] Failed to prepare snapshot for %s/%s #%d: %v", owner, repo, prNum, err)
		return
	}
	defer cleanup()

	items, err := docgen.FindUndocumentedGoItems(snap.SourceDir)
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
			relPath, _ := filepath.Rel(snap.SourceDir, it.File)
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
	if err := s.Start(context.Background()); err != nil {
		log.Printf("[server] Failed to start durable runtime: %v", err)
		return fmt.Errorf("failed to start durable runtime: %w", err)
	}
	addr := fmt.Sprintf("0.0.0.0:%d", s.cfg.Port)
	log.Printf("🚀 PR Review Server listening on http://%s", addr)
	return http.ListenAndServe(addr, s.Routes())
}
