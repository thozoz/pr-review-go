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
	"os"
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
	"github.com/thozoz/pr-review-go/pkg/llm"
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

	store          JobStore
	scheduler      *Scheduler
	statusOutbox   *StatusOutbox
	decisionLedger *DecisionLedger
	runtimeReady   bool
	shuttingDown bool
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
	gh := ghclient.NewClientFromConfig(cfg)
	llmClient := llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)
	return NewServerWithClients(cfg, gh, llmClient)
}

// NewServerWithClients creates a Server with injected GitHub and LLM clients for testing.
func NewServerWithClients(cfg *config.Config, gh *ghclient.Client, llmClient *llm.Client) *Server {
	if err := cfg.Validate(); err != nil {
		panic(fmt.Sprintf("invalid config: %v", err))
	}
	if cfg.IsGitHubAppSetupMode() {
		return &Server{
			cfg: cfg,
			gh:  gh,
		}
	}
	return &Server{
		cfg:        cfg,
		engine:     reviewer.NewEngineWithClients(cfg, gh, llmClient, nil),
		labeler:    labeler.NewLabeler(cfg),
		summarizer: summarizer.NewSummarizerWithClients(cfg, gh, llmClient),
		assistant:  assistant.NewAssistant(cfg),
		describer:  describer.NewDescriber(cfg),
		changelog:  changelog.NewUpdater(cfg),
		gh:         gh,
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
	if s.cfg != nil && s.cfg.DeferPollInterval > 0 {
		s.scheduler.SetDeferPollInterval(s.cfg.DeferPollInterval)
	}

	if s.scheduler.LLMGate() == nil && s.cfg != nil {
		llmGate, err := llm.NewRequestGate(s.cfg.LLMConcurrency, s.cfg.LLMMinInterval, s.cfg.LLMResponseMaxBytes, nil)
		if err != nil {
			s.runtimeErr = err
			return err
		}
		s.scheduler.SetLLMGate(llmGate)
	}

	if s.scheduler.SandboxGate() == nil && s.cfg != nil {
		sGate, err := sandbox.NewAdmissionGate(s.cfg.SandboxConcurrency)
		if err != nil {
			s.runtimeErr = err
			return err
		}
		s.scheduler.SetSandboxGate(sGate)
	}

	if err := s.scheduler.Start(ctx); err != nil {
		s.runtimeErr = err
		return err
	}

	if s.statusOutbox == nil && s.store != nil && s.gh != nil {
		s.statusOutbox = NewStatusOutbox(s.store, s.gh, NewPublication(s.store, s.gh))
	}
	if s.statusOutbox != nil {
		s.statusOutbox.Start(ctx)
	}

	s.runtimeReady = true
	s.runtimeErr = nil
	return nil
}

func (s *Server) ensureStore() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store != nil {
		return nil
	}
	stateDir := s.cfg.WebhookStateDir
	if stateDir == "" {
		stateDir = filepath.Join(os.TempDir(), fmt.Sprintf("pr-review-ephemeral-%d", time.Now().UnixNano()))
	}
	dbPath := filepath.Join(stateDir, "jobs.db")
	storeOpts := StoreOptions{
		BacklogLimit:  s.cfg.WebhookBacklog,
		DeliveryLimit: s.cfg.WebhookDeliveryLimit,
		StateMaxBytes: s.cfg.WebhookStateMaxBytes,
		DeliveryTTL:   s.cfg.WebhookDeliveryTTL,
		OpenTimeout:   1 * time.Second,
	}
	store, err := OpenJobStore(dbPath, storeOpts)
	if err != nil {
		if errors.Is(err, ErrDatabaseLocked) {
			ephemeralDir := filepath.Join(os.TempDir(), fmt.Sprintf("pr-review-ephemeral-%d-%d", os.Getpid(), time.Now().UnixNano()))
			dbPath = filepath.Join(ephemeralDir, "jobs.db")
			store, err = OpenJobStore(dbPath, storeOpts)
		}
		if err != nil {
			return err
		}
	}
	s.store = store
	if s.scheduler == nil {
		executor := NewServerJobExecutor(s, store)
		workers := s.cfg.WebhookWorkers
		if workers <= 0 {
			workers = 2
		}
		s.scheduler = NewScheduler(store, executor, workers)
	}
	if s.statusOutbox == nil && store != nil && s.gh != nil {
		s.statusOutbox = NewStatusOutbox(store, s.gh, NewPublication(store, s.gh))
		s.statusOutbox.Start(context.Background())
	}
	return s.scheduler.Start(context.Background())
}

// Shutdown stops admission of new webhook events, drains workers within the provided
// context deadline, and closes the persistent database only after workers have stopped.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.shuttingDown = true
	s.runtimeReady = false
	s.mu.Unlock()

	if s.cfg.IsGitHubAppSetupMode() {
		return nil
	}

	var shutdownErr error
	if s.statusOutbox != nil {
		s.statusOutbox.Stop()
	}
	if s.scheduler != nil {
		if err := s.scheduler.Shutdown(ctx); err != nil {
			shutdownErr = err
		}
	}

	// Close database ONLY after workers have stopped
	if s.store != nil {
		if err := s.store.Close(); err != nil && shutdownErr == nil {
			shutdownErr = err
		}
	}

	return shutdownErr
}

// Stop stops workers and closes the durable job store.
func (s *Server) Stop() {
	_ = s.Shutdown(context.Background())
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

// DescribeQueueState renders the operator-visible queue state of a job for
// inspection surfaces (--queue-inspect, health). Deferred attempts report
// their park state so operators can distinguish waiting work from stuck work;
// all other jobs report their ledger status unchanged.
func DescribeQueueState(j *Job, now time.Time) string {
	if j == nil {
		return ""
	}
	if j.Status == "queued" && !j.NotBeforeAt.IsZero() {
		if now.Before(j.NotBeforeAt) {
			return "deferred until " + j.NotBeforeAt.Format(time.RFC3339)
		}
		return "due (deferred)"
	}
	return j.Status
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	runtimeErr := s.runtimeErr
	ready := s.runtimeReady
	shuttingDown := s.shuttingDown
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	status := "ok"
	runtimeStatus := "ready"
	if s.cfg.IsGitHubAppSetupMode() {
		runtimeStatus = "setup_mode"
	} else if shuttingDown {
		status = "degraded"
		runtimeStatus = "shutting_down"
	} else if runtimeErr != nil {
		status = "degraded"
		runtimeStatus = "unavailable"
	} else if !ready && s.store == nil {
		runtimeStatus = "uninitialized"
	}

	resp := map[string]any{
		"status":  status,
		"runtime": runtimeStatus,
		"service": "pr-review-go",
		"version": version.Version,
	}

	if s.store != nil && !s.cfg.IsGitHubAppSetupMode() {
		warningTTL := s.cfg.WebhookOldQueueWarning
		if warningTTL <= 0 {
			warningTTL = 24 * time.Hour
		}
		counts, err := s.store.GetHealthCounts(r.Context(), warningTTL)
		if err == nil {
			resp["counts"] = map[string]int{
				"queued":          counts.Queued,
				"running":         counts.Running,
				"uncertain":       counts.Uncertain,
				"needs_attention": counts.NeedsAttention,
				"deferred":        counts.Deferred,
			}
			resp["old_wait_warning"] = counts.OldWaitWarning
			if counts.Uncertain > 0 || counts.NeedsAttention > 0 || counts.OldWaitWarning {
				if status == "ok" {
					resp["status"] = "degraded"
				}
			}
		}
	}

	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	shuttingDown := s.shuttingDown
	s.mu.RUnlock()
	if shuttingDown {
		w.Header().Set("Retry-After", "30")
		http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
		return
	}

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
		if err := s.ensureStore(); err != nil {
			log.Printf("[webhook] Failed to initialize store: %v", err)
			w.Header().Set("Retry-After", "30")
			http.Error(w, "runtime unavailable", http.StatusServiceUnavailable)
			return
		}
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
		// Auto improve remains skipped on opened
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
		if s.scheduler != nil {
			s.scheduler.Wake()
		}
		if s.statusOutbox != nil {
			s.statusOutbox.Wake()
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

	if len(payloadText) > 4096 {
		payloadText = payloadText[:4096]
	}

	commentID := e.GetComment().GetID()

	prKey, err := MakePRKey("github.com", repoID, prNum)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	job := Job{
		Kind:      kind,
		Trigger:   "explicit",
		Author:    username,
		CommentID: commentID,
		PRKey:     prKey,
		Owner:     owner,
		Repo:      repo,
		PRNumber:  prNum,
		Payload:   payloadText,
	}

	if kind == "review" || kind == "approve" || kind == "request_changes" {
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
		if s.scheduler != nil {
			s.scheduler.Wake()
		}
		if s.statusOutbox != nil {
			s.statusOutbox.Wake()
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

func matchWordPrefix(body, prefix string) bool {
	if body == prefix {
		return true
	}
	if strings.HasPrefix(body, prefix) {
		rest := body[len(prefix):]
		if len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t' || rest[0] == '\n' || rest[0] == '\r') {
			return true
		}
	}
	return false
}

func parseCommentCommand(body string) (kind string, payload string) {
	switch {
	case matchWordPrefix(body, "/request_changes"):
		return "request_changes", ""
	case matchWordPrefix(body, "/approve"):
		return "approve", ""
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

func isCommentCommand(body string) bool {
	if matchWordPrefix(body, "/request_changes") || matchWordPrefix(body, "/approve") {
		return true
	}
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

func (s *Server) dispatchAddDocs(ctx context.Context, owner, repo string, prNum int) {
	if ctx == nil {
		ctx = context.Background()
	}

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

func (s *Server) getDecisionLedger() *DecisionLedger {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	ledger := s.decisionLedger
	s.mu.RUnlock()
	if ledger != nil {
		return ledger
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.decisionLedger != nil {
		return s.decisionLedger
	}
	if bStore, ok := s.store.(*BoltJobStore); ok && bStore != nil {
		s.decisionLedger = NewDecisionLedger(bStore.DB())
	}
	return s.decisionLedger
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
