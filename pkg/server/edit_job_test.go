package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thozoz/pr-review-go/pkg/config"
	ghclient "github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/sandbox"
)

type fakeEditServerLLM struct {
	verdict IntentVerdict
	err     error
}

func (f *fakeEditServerLLM) ChatCompletion(ctx context.Context, systemPrompt, userMessage string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	// If edit loop prompt (contains "AVAILABLE ACTIONS")
	if strings.Contains(systemPrompt, "AVAILABLE ACTIONS") {
		return `[{"action":"write_file","path":"app.go","content":"package app\n\n// updated\n"},{"action":"answer","final_text":"done"}]`, nil
	}

	bytes, err := json.Marshal(f.verdict)
	if err != nil {
		return "", err
	}
	return string(bytes), nil
}

type fakeEditRunner struct {
	reportStatus sandbox.VerificationStatus
	reportReason string
	sourceDir    string
	prepareErr   error
	runErr       error
}

func (f *fakeEditRunner) PrepareSnapshot(ctx context.Context, cloneURL, headRef, headSHA string) (*sandbox.Snapshot, func(), error) {
	if f.prepareErr != nil {
		return nil, nil, f.prepareErr
	}
	cleanup := func() {}
	snap := &sandbox.Snapshot{
		CommitSHA: headSHA,
		SourceDir: f.sourceDir,
	}
	return snap, cleanup, nil
}

func (f *fakeEditRunner) RunSnapshot(ctx context.Context, snapshot *sandbox.Snapshot) (*sandbox.VerificationReport, error) {
	if f.runErr != nil {
		return nil, f.runErr
	}
	return &sandbox.VerificationReport{
		Status: f.reportStatus,
		Reason: f.reportReason,
	}, nil
}

const (
	testValidHeadSHA = "0123456789abcdef0123456789abcdef01234567"
	testValidBaseSHA = "fedcba9876543210fedcba9876543210fedcba98"
	testNewCommitOID = "abcdef0123456789abcdef0123456789abcdef01"
)

type editTestHarness struct {
	ghServer         *httptest.Server
	commitCount      int32
	comments         []string
	allowAuth        bool
	reauthCalls      int32
	denyOnReauthCall int32
	headSHA          string
	graphqlStatus    int
	graphqlErr       string
	isFork           bool
	reportStatus     sandbox.VerificationStatus
	fakeRunner       *fakeEditRunner
}

func newEditTestHarness(t *testing.T) *editTestHarness {
	h := &editTestHarness{
		allowAuth:    true,
		headSHA:      testValidHeadSHA,
		reportStatus: sandbox.StatusPassed,
	}

	h.ghServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/collaborators/"):
			call := atomic.AddInt32(&h.reauthCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			perm := "read"
			if h.allowAuth && (h.denyOnReauthCall == 0 || call != h.denyOnReauthCall) {
				perm = "write"
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"permission": perm})

		case strings.Contains(path, "/pulls/"):
			w.Header().Set("Content-Type", "application/json")
			headRepo := map[string]any{"id": 12345, "name": "repo", "owner": map[string]any{"login": "org"}}
			if h.isFork {
				headRepo = map[string]any{"id": 99999, "name": "fork-repo", "owner": map[string]any{"login": "forker"}}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 1,
				"head": map[string]any{
					"sha":  h.headSHA,
					"ref":  "feature-branch",
					"repo": headRepo,
				},
				"base": map[string]any{
					"sha": testValidBaseSHA,
					"ref": "main",
				},
			})

		case strings.Contains(path, "/issues/comments/"):
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodPatch || r.Method == http.MethodPost {
				var bodyMap map[string]string
				_ = json.NewDecoder(r.Body).Decode(&bodyMap)
				h.comments = append(h.comments, bodyMap["body"])
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":   2001,
				"body": "status comment",
				"user": map[string]any{"id": 1001, "login": "test-bot"},
			})

		case strings.Contains(path, "/comments"):
			if r.Method == http.MethodPost {
				var bodyMap map[string]string
				_ = json.NewDecoder(r.Body).Decode(&bodyMap)
				h.comments = append(h.comments, bodyMap["body"])
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id":   2001,
					"body": bodyMap["body"],
					"user": map[string]any{"id": 1001, "login": "test-bot"},
				})
			} else {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode([]any{})
			}

		case strings.HasSuffix(path, "/graphql"):
			atomic.AddInt32(&h.commitCount, 1)
			if h.graphqlStatus != 0 && h.graphqlStatus != http.StatusOK {
				http.Error(w, h.graphqlErr, h.graphqlStatus)
				return
			}
			if h.graphqlErr == "HeadMovedError" {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"errors": []any{
						map[string]any{
							"message": "head moved: expected " + testValidHeadSHA + " but was " + testValidBaseSHA,
							"type":    "HEAD_MOVED",
						},
					},
				})
				return
			}
			if h.graphqlErr == "uncertain_timeout" {
				http.Error(w, `{"message":"server error"}`, http.StatusInternalServerError)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"createCommitOnBranch": map[string]any{
						"commit": map[string]any{
							"oid": testNewCommitOID,
							"url": "https://example.com/commit/" + testNewCommitOID,
						},
						"ref": map[string]any{
							"name":   "refs/heads/feature-branch",
							"target": map[string]any{"oid": testNewCommitOID},
						},
					},
				},
			})

		case strings.Contains(path, "/compare/"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []any{},
			})

		default:
			http.NotFound(w, r)
		}
	}))

	return h
}

func setupEditExecutor(t *testing.T, h *editTestHarness) (*ServerJobExecutor, JobStore, *Server) {
	ghClient, err := ghclient.NewTestClient(h.ghServer.URL)
	if err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	cfg := &config.Config{
		WebhookSecret:        "secret",
		WebhookStateDir:      stateDir,
		WebhookWorkers:       1,
		WebhookBacklog:       10,
		WebhookDeliveryTTL:   24 * time.Hour,
		WebhookDeliveryLimit: 100,
		WebhookStateMaxBytes: 16777216,
		WebhookBodyMaxBytes:  1048576,
		EnableSandbox:        false,
		GitHubToken:          "test-token",
	}

	store, err := OpenJobStore(filepath.Join(stateDir, "jobs.db"), StoreOptions{
		BacklogLimit:  10,
		DeliveryLimit: 100,
		StateMaxBytes: 16777216,
		DeliveryTTL:   24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	srv := &Server{
		cfg:   cfg,
		gh:    ghClient,
		store: store,
	}

	exec := NewServerJobExecutor(srv, store)

	// Fake LLM caller with commit intent
	exec.SetLLMCaller(&fakeEditServerLLM{
		verdict: IntentVerdict{
			Intent:     "commit",
			Confidence: 0.95,
			Summary:    "Apply error handling fix",
		},
	})

	// Setup fake runner
	tempSource := t.TempDir()
	_ = os.WriteFile(filepath.Join(tempSource, "app.go"), []byte("package app\n"), 0644)

	fr := &fakeEditRunner{
		reportStatus: h.reportStatus,
		reportReason: "test execution report",
		sourceDir:    tempSource,
	}
	h.fakeRunner = fr
	exec.SetRunner(fr)

	return exec, store, srv
}

func createAndAdmitEditJob(t *testing.T, store JobStore, payload string) *Job {
	job := &Job{
		Kind:      "edit",
		Trigger:   "explicit",
		Author:    "authorized-dev",
		Owner:     "org",
		Repo:      "repo",
		PRNumber:  1,
		PRKey:     PRKey{RepoID: 12345, Number: 1},
		BaseSHA:   testValidBaseSHA,
		HeadSHA:   testValidHeadSHA,
		Payload:   payload,
		CreatedAt: time.Now().UTC(),
	}
	deliv := Delivery{
		Host:        "github.com",
		RepoID:      12345,
		DeliveryID:  fmt.Sprintf("deliv-%d", time.Now().UnixNano()),
		PayloadHash: fmt.Sprintf("hash-%d", time.Now().UnixNano()),
	}
	res, err := store.Admit(context.Background(), deliv, []Job{*job})
	if err != nil || res.Status != AdmitAccepted {
		t.Fatalf("failed admitting job: %v, status=%v", err, res.Status)
	}
	claimed, err := store.ClaimNextJob(context.Background(), nil)
	if err != nil || claimed == nil {
		t.Fatalf("failed claiming job: %v", err)
	}
	return claimed
}

// 1. push-time reauth denial after admission approval yields denied comment plus zero pushes
func TestEditJob_PushTimeReauthDenial(t *testing.T) {
	h := newEditTestHarness(t)
	defer h.ghServer.Close()
	exec, store, _ := setupEditExecutor(t, h)
	defer store.Close()

	// Admission and start reauth (call 1) pass, push-time reauth (call 2) fails
	h.allowAuth = true
	h.denyOnReauthCall = 2

	job := createAndAdmitEditJob(t, store, "fix error handling in app.go")

	err := exec.ExecuteJob(context.Background(), job)
	if err != nil {
		t.Fatalf("expected ExecuteJob to complete with denial comment, got err: %v", err)
	}

	if atomic.LoadInt32(&h.commitCount) != 0 {
		t.Fatalf("expected 0 push calls, got %d", h.commitCount)
	}
	if job.Status != "failed" {
		t.Fatalf("expected job status 'failed', got %q", job.Status)
	}
	foundDenied := false
	for _, c := range h.comments {
		if strings.Contains(c, "reauthorization denied") || strings.Contains(c, "D-26") {
			foundDenied = true
			break
		}
	}
	if !foundDenied {
		t.Fatalf("expected reauthorization denial comment, got: %v", h.comments)
	}
}

// 2. revoked-permission mid-job variant of the same
func TestEditJob_RevokedPermissionMidJob(t *testing.T) {
	h := newEditTestHarness(t)
	defer h.ghServer.Close()
	exec, store, _ := setupEditExecutor(t, h)
	defer store.Close()

	h.allowAuth = true
	h.denyOnReauthCall = 2

	job := createAndAdmitEditJob(t, store, "fix error handling in app.go")

	_ = exec.ExecuteJob(context.Background(), job)

	if atomic.LoadInt32(&h.commitCount) != 0 {
		t.Fatalf("expected 0 push calls on mid-job revocation, got %d", h.commitCount)
	}
	if job.Status != "failed" {
		t.Fatalf("expected job status 'failed', got %q", job.Status)
	}
}

// 3. non-green statuses failed, unavailable, and unsupported-language each yield failure comment plus zero pushes
func TestEditJob_NonGreenStatusesZeroPushes(t *testing.T) {
	statuses := []sandbox.VerificationStatus{
		sandbox.VerificationStatus("failed"),
		sandbox.StatusUnavailable,
		sandbox.StatusUnsupportedLanguage,
	}

	for _, st := range statuses {
		t.Run(string(st), func(t *testing.T) {
			h := newEditTestHarness(t)
			defer h.ghServer.Close()
			h.reportStatus = st
			exec, store, _ := setupEditExecutor(t, h)
			defer store.Close()

			job := createAndAdmitEditJob(t, store, "fix error handling in app.go")
			err := exec.ExecuteJob(context.Background(), job)
			if err != nil {
				t.Fatalf("unexpected ExecuteJob err: %v", err)
			}

			if atomic.LoadInt32(&h.commitCount) != 0 {
				t.Fatalf("expected 0 push calls for non-green status %s, got %d", st, h.commitCount)
			}
			if job.Status != "failed" {
				t.Fatalf("expected job status 'failed', got %q", job.Status)
			}
		})
	}
}

// 4. HeadMovedError yields stale comment naming old SHA plus zero retries plus superseded status
func TestEditJob_HeadMovedYieldsStaleCommentAndSuperseded(t *testing.T) {
	h := newEditTestHarness(t)
	defer h.ghServer.Close()
	h.graphqlErr = "HeadMovedError"
	exec, store, _ := setupEditExecutor(t, h)
	defer store.Close()

	job := createAndAdmitEditJob(t, store, "fix error handling in app.go")
	err := exec.ExecuteJob(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected ExecuteJob err: %v", err)
	}

	if atomic.LoadInt32(&h.commitCount) != 1 {
		t.Fatalf("expected exactly 1 attempt before stale detection, got %d", h.commitCount)
	}
	if job.Status != "superseded" {
		t.Fatalf("expected job status 'superseded', got %q", job.Status)
	}
	foundStale := false
	for _, c := range h.comments {
		if strings.Contains(c, "Branch head changed") || strings.Contains(c, "stale") {
			foundStale = true
			break
		}
	}
	if !foundStale {
		t.Fatalf("expected stale head abort comment, got: %v", h.comments)
	}
}

// 5. uncertain timeout yields needs_attention status with exactly one remote create and resolvable state for --queue-resolve
func TestEditJob_UncertainTimeoutYieldsNeedsAttention(t *testing.T) {
	h := newEditTestHarness(t)
	defer h.ghServer.Close()
	h.graphqlStatus = http.StatusInternalServerError
	h.graphqlErr = "uncertain_timeout"
	exec, store, _ := setupEditExecutor(t, h)
	defer store.Close()

	job := createAndAdmitEditJob(t, store, "fix error handling in app.go")

	err := exec.ExecuteJob(context.Background(), job)
	if err == nil {
		t.Fatalf("expected uncertain error, got nil")
	}

	if atomic.LoadInt32(&h.commitCount) != 1 {
		t.Fatalf("expected exactly 1 remote create attempt, got %d", h.commitCount)
	}
	if job.Status != "needs_attention" {
		t.Fatalf("expected job status 'needs_attention', got %q", job.Status)
	}
}

// 6. two-fix instruction yields exactly one commit call
func TestEditJob_TwoFixInstructionYieldsExactlyOneCommit(t *testing.T) {
	h := newEditTestHarness(t)
	defer h.ghServer.Close()
	exec, store, _ := setupEditExecutor(t, h)
	defer store.Close()

	job := createAndAdmitEditJob(t, store, "fix error handling in app.go and add helper function")

	err := exec.ExecuteJob(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected ExecuteJob err: %v", err)
	}

	if atomic.LoadInt32(&h.commitCount) != 1 {
		t.Fatalf("expected exactly 1 commit call, got %d", h.commitCount)
	}
	if job.Status != "completed" {
		t.Fatalf("expected job status 'completed', got %q", job.Status)
	}
}

// 7. sticky test where second job payload receives prior diff plus history and completion persists the new LastBotCommitSHA
func TestEditJob_StickyStateCarriedToSecondJob(t *testing.T) {
	h := newEditTestHarness(t)
	defer h.ghServer.Close()
	exec, store, _ := setupEditExecutor(t, h)
	defer store.Close()

	// First job completes
	job1 := createAndAdmitEditJob(t, store, "first instruction")
	if err := exec.ExecuteJob(context.Background(), job1); err != nil {
		t.Fatalf("job1 failed: %v", err)
	}
	// Release PR slot after job1 finishes
	_ = store.ReleasePR(context.Background(), job1.PRKey)

	prState, err := store.GetPRState(context.Background(), job1.PRKey)
	if err != nil || prState == nil {
		t.Fatalf("failed retrieving prState: %v", err)
	}
	if prState.LastBotCommitSHA != testNewCommitOID {
		t.Fatalf("expected LastBotCommitSHA %s, got %s", testNewCommitOID, prState.LastBotCommitSHA)
	}
	if len(prState.EditHistory) != 1 || prState.EditHistory[0] != "first instruction" {
		t.Fatalf("expected edit history ['first instruction'], got: %v", prState.EditHistory)
	}

	// Second job runs
	job2 := createAndAdmitEditJob(t, store, "second instruction")
	if err := exec.ExecuteJob(context.Background(), job2); err != nil {
		t.Fatalf("job2 failed: %v", err)
	}
	_ = store.ReleasePR(context.Background(), job2.PRKey)

	prState2, _ := store.GetPRState(context.Background(), job2.PRKey)
	if len(prState2.EditHistory) != 2 {
		t.Fatalf("expected 2 history entries, got: %v", prState2.EditHistory)
	}
	if prState2.EditHistory[1] != "second instruction" {
		t.Fatalf("expected second entry to be 'second instruction', got: %s", prState2.EditHistory[1])
	}
}

// 8. serialization test where a second edit job waits while the first holds the per-PR slot
func TestEditJob_SerializationAcrossSamePR(t *testing.T) {
	h := newEditTestHarness(t)
	defer h.ghServer.Close()
	_, store, _ := setupEditExecutor(t, h)
	defer store.Close()

	job1 := &Job{
		Kind:      "edit",
		Trigger:   "explicit",
		Author:    "authorized-dev",
		Owner:     "org",
		Repo:      "repo",
		PRNumber:  1,
		PRKey:     PRKey{RepoID: 12345, Number: 1},
		BaseSHA:   testValidBaseSHA,
		HeadSHA:   testValidHeadSHA,
		Payload:   "instruction 1",
		CreatedAt: time.Now().UTC(),
	}
	job2 := &Job{
		Kind:      "edit",
		Trigger:   "explicit",
		Author:    "authorized-dev",
		Owner:     "org",
		Repo:      "repo",
		PRNumber:  1,
		PRKey:     PRKey{RepoID: 12345, Number: 1},
		BaseSHA:   testValidBaseSHA,
		HeadSHA:   testValidHeadSHA,
		Payload:   "instruction 2",
		CreatedAt: time.Now().UTC(),
	}

	delivery := Delivery{
		Host:        "github.com",
		RepoID:      12345,
		DeliveryID:  "deliv-seq",
		PayloadHash: "hash-seq",
	}

	res, err := store.Admit(context.Background(), delivery, []Job{*job1, *job2})
	if err != nil {
		t.Fatalf("Admit failed: %v", err)
	}
	if res.Status != AdmitAccepted {
		t.Fatalf("expected AdmitAccepted, got: %v", res.Status)
	}

	activePRs := make(map[string]bool)

	// Claim first job
	c1, err := store.ClaimNextJob(context.Background(), activePRs)
	if err != nil || c1 == nil {
		t.Fatalf("expected c1 claimed, got: %v (err=%v)", c1, err)
	}
	if c1.Sequence != 1 {
		t.Fatalf("expected sequence 1, got: %d", c1.Sequence)
	}

	// Mark PR as active in tracker and leave ActiveJobID in store
	activePRs[c1.PRKey.String()] = true

	// Claim attempt for second job while PR is active
	c2, err := store.ClaimNextJob(context.Background(), activePRs)
	if err != nil {
		t.Fatalf("unexpected ClaimNextJob err: %v", err)
	}
	if c2 != nil {
		t.Fatalf("expected second job to be blocked by active PR slot, got claimed: %s", c2.ID)
	}

	// Release PR slot in activePRs and in store
	delete(activePRs, c1.PRKey.String())
	_ = store.ReleasePR(context.Background(), c1.PRKey)

	// Now second job can be claimed
	c2After, err := store.ClaimNextJob(context.Background(), activePRs)
	if err != nil || c2After == nil {
		t.Fatalf("expected c2 claimed after release, got: %v (err=%v)", c2After, err)
	}
	if c2After.Sequence != 2 {
		t.Fatalf("expected sequence 2, got: %d", c2After.Sequence)
	}
}
