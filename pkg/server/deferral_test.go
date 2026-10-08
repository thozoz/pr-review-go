package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thozoz/pr-review-go/pkg/config"
	ghclient "github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/retry"
)

func openDeferTestStore(t *testing.T, backlog int) *BoltJobStore {
	t.Helper()
	store, err := OpenJobStore(filepath.Join(t.TempDir(), "jobs.db"), StoreOptions{BacklogLimit: backlog})
	if err != nil {
		t.Fatalf("OpenJobStore failed: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func seedDeferJob(t *testing.T, store JobStore, host string, repoID int64, number int, kind string) *Job {
	t.Helper()
	prKey, err := MakePRKey(host, repoID, number)
	if err != nil {
		t.Fatal(err)
	}
	delivery := Delivery{
		Host:        prKey.Host,
		RepoID:      prKey.RepoID,
		DeliveryID:  fmt.Sprintf("defer-%d-%d-%d", repoID, number, time.Now().UnixNano()),
		EventKind:   "pull_request",
		PayloadHash: fmt.Sprintf("hash-%d-%d-%d", repoID, number, time.Now().UnixNano()),
		ReceivedAt:  time.Now().UTC(),
	}
	res, err := store.Admit(context.Background(), delivery, []Job{
		{
			Kind:     kind,
			Trigger:  "automatic",
			PRKey:    prKey,
			Owner:    "owner",
			Repo:     "repo",
			PRNumber: number,
			BaseSHA:  "1111111111111111111111111111111111111111",
			HeadSHA:  "2222222222222222222222222222222222222222",
		},
	})
	if err != nil || res.Status != AdmitAccepted {
		t.Fatalf("Admit failed: %v, status: %v", err, res.Status)
	}
	queued, err := store.ListQueuedJobs(context.Background())
	if err != nil || len(queued) == 0 {
		t.Fatalf("ListQueuedJobs failed: %v", err)
	}
	return queued[len(queued)-1]
}

// TestDeferLongWaitParksDurablyAndFreesWorker: a claimed (running) job whose
// retry wait exceeds the worker budget returns to queued with a future due
// time; the released worker pool no longer sees it until due.
func TestDeferLongWaitParksDurablyAndFreesWorker(t *testing.T) {
	ctx := context.Background()
	store := openDeferTestStore(t, 10)
	job := seedDeferJob(t, store, "github.com", 9001, 1, "review")

	claimed, err := store.ClaimNextJob(ctx, map[string]bool{})
	if err != nil || claimed == nil || claimed.ID != job.ID {
		t.Fatalf("ClaimNextJob failed: %v, claimed: %+v", claimed, err)
	}

	due := time.Now().UTC().Add(10 * time.Minute)
	deferred, err := store.DeferJob(ctx, job.ID, due, "retry wait exceeds worker budget")
	if err != nil {
		t.Fatalf("DeferJob failed: %v", err)
	}
	if deferred.Status != "queued" {
		t.Errorf("deferred status = %q, want queued", deferred.Status)
	}
	if deferred.StartedAt != nil {
		t.Errorf("deferred StartedAt should be cleared")
	}
	if deferred.RecoveryPhase != "deferred_retry" {
		t.Errorf("RecoveryPhase = %q, want deferred_retry", deferred.RecoveryPhase)
	}
	if deferred.NotBeforeAt.IsZero() || deferred.NotBeforeAt.Before(time.Now().UTC()) {
		t.Errorf("NotBeforeAt not parked in the future: %v", deferred.NotBeforeAt)
	}

	// Worker release: PR slot freed, but the parked job is skipped.
	if err := store.ReleasePR(ctx, job.PRKey); err != nil {
		t.Fatalf("ReleasePR failed: %v", err)
	}
	if next, err := store.ClaimNextJob(ctx, map[string]bool{}); err != nil || next != nil {
		t.Fatalf("not-due deferred job must be skipped, got %+v, err %v", next, err)
	}

	// Queue depth evidence: still exactly one queued job, counted deferred.
	queued, err := store.ListQueuedJobs(ctx)
	if err != nil || len(queued) != 1 {
		t.Fatalf("ListQueuedJobs = %d, err %v; want 1 parked job", len(queued), err)
	}
	counts, err := store.GetHealthCounts(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Queued != 1 || counts.Deferred != 1 {
		t.Errorf("counts = %+v, want Queued=1 Deferred=1", counts)
	}
	if dueOut, ok, err := store.EarliestDeferredDue(ctx); err != nil || !ok || !dueOut.Equal(due) {
		t.Errorf("EarliestDeferredDue = %v, %v, %v; want %v true nil", dueOut, ok, err, due)
	}
}

// TestDeferredDueJobResumesClaim: a deferred attempt whose due time passed is
// immediately eligible again.
func TestDeferredDueJobResumesClaim(t *testing.T) {
	ctx := context.Background()
	store := openDeferTestStore(t, 10)
	job := seedDeferJob(t, store, "github.com", 9002, 1, "review")

	if _, err := store.DeferJob(ctx, job.ID, time.Now().UTC().Add(-time.Minute), "wait elapsed"); err != nil {
		t.Fatalf("DeferJob failed: %v", err)
	}
	parked, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := DescribeQueueState(parked, time.Now().UTC()); got != "due (deferred)" {
		t.Errorf("queue state = %q, want %q", got, "due (deferred)")
	}
	claimed, err := store.ClaimNextJob(ctx, map[string]bool{})
	if err != nil || claimed == nil || claimed.ID != job.ID {
		t.Fatalf("due deferred job must be claimable, got %+v, err %v", claimed, err)
	}
}

// TestFairnessDeferredSkipsNotDue: a not-due deferred job for PR-A never
// blocks a ready job for PR-B; FIFO resumes once A is due.
func TestFairnessDeferredSkipsNotDue(t *testing.T) {
	ctx := context.Background()
	store := openDeferTestStore(t, 10)
	jobA := seedDeferJob(t, store, "github.com", 9011, 1, "review")
	jobB := seedDeferJob(t, store, "github.com", 9012, 2, "review")

	if _, err := store.DeferJob(ctx, jobA.ID, time.Now().UTC().Add(time.Hour), "long wait"); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextJob(ctx, map[string]bool{})
	if err != nil || claimed == nil || claimed.ID != jobB.ID {
		t.Fatalf("PR-B ready job must win over parked PR-A, got %+v, err %v", claimed, err)
	}
	if err := store.ReleasePR(ctx, jobB.PRKey); err != nil {
		t.Fatal(err)
	}

	// A becomes due: FIFO order resumes with the older job.
	if _, err := store.DeferJob(ctx, jobA.ID, time.Now().UTC().Add(-time.Second), "due now"); err != nil {
		t.Fatal(err)
	}
	again, err := store.ClaimNextJob(ctx, map[string]bool{})
	if err != nil || again == nil || again.ID != jobA.ID {
		t.Fatalf("due PR-A job must be claimed, got %+v, err %v", again, err)
	}
}

// TestDeferredSurvivesRestartWithActiveSlot: crash between the defer commit
// and worker release recovers the deferred job without losing it.
func TestDeferredSurvivesRestartWithActiveSlot(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "jobs.db")
	store, err := OpenJobStore(dbPath, StoreOptions{BacklogLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	job := seedDeferJob(t, store, "github.com", 9021, 1, "review")
	if _, err := store.ClaimNextJob(ctx, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	due := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Second)
	if _, err := store.DeferJob(ctx, job.ID, due, "long wait"); err != nil {
		t.Fatal(err)
	}
	// Crash: close without ReleasePR, reopen, recover.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenJobStore(dbPath, StoreOptions{BacklogLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	recovered, err := reopened.RecoverJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = recovered

	got, err := reopened.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "queued" || !got.NotBeforeAt.Equal(due) {
		t.Errorf("deferred job lost across restart: status=%q due=%v want queued %v", got.Status, got.NotBeforeAt, due)
	}
	st, err := reopened.GetPRState(ctx, job.PRKey)
	if err != nil {
		t.Fatal(err)
	}
	if st.ActiveJobID != "" {
		t.Errorf("ActiveJobID = %q after recovery, want cleared", st.ActiveJobID)
	}
	if st.HasBlockedAction {
		t.Errorf("parked deferral must not block its PR")
	}
}

// TestDeferDoubleDeliveryCommitsOnce: repeating the same deferral is
// idempotent and never duplicates queue rows.
func TestDeferDoubleDeliveryCommitsOnce(t *testing.T) {
	ctx := context.Background()
	store := openDeferTestStore(t, 10)
	job := seedDeferJob(t, store, "github.com", 9031, 1, "review")

	due := time.Now().UTC().Add(3 * time.Minute)
	first, err := store.DeferJob(ctx, job.ID, due, "wait")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.DeferJob(ctx, job.ID, due, "wait")
	if err != nil {
		t.Fatal(err)
	}
	if !first.NotBeforeAt.Equal(second.NotBeforeAt) || second.Status != "queued" {
		t.Errorf("repeat deferral not idempotent: %+v vs %+v", first, second)
	}
	all, err := store.InspectJobs(ctx)
	if err != nil || len(all) != 1 {
		t.Errorf("InspectJobs = %d rows, want exactly 1", len(all))
	}
}

// TestDeferredBacklogAdmissionRejectsAtomically: parked deferrals still hold
// backlog slots, and over-capacity admission rejects without partial writes.
func TestDeferredBacklogAdmissionRejectsAtomically(t *testing.T) {
	ctx := context.Background()
	store := openDeferTestStore(t, 1)
	job := seedDeferJob(t, store, "github.com", 9041, 1, "review")
	if _, err := store.DeferJob(ctx, job.ID, time.Now().UTC().Add(time.Hour), "wait"); err != nil {
		t.Fatal(err)
	}

	prKey, _ := MakePRKey("github.com", 9042, 2)
	res, err := store.Admit(ctx, Delivery{
		Host: prKey.Host, RepoID: prKey.RepoID, DeliveryID: "over-cap",
		EventKind: "pull_request", PayloadHash: "h2", ReceivedAt: time.Now().UTC(),
	}, []Job{{Kind: "review", Trigger: "automatic", PRKey: prKey, Owner: "owner", Repo: "repo", PRNumber: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != AdmitCapacityFull {
		t.Errorf("admit status = %v, want capacity-full while deferral waits", res.Status)
	}
	queued, err := store.ListQueuedJobs(ctx)
	if err != nil || len(queued) != 1 {
		t.Errorf("queue mutated by rejected admit: %d jobs", len(queued))
	}
}

// TestDeferTerminalJobRejected: completed/failed jobs can never be
// resurrected into the queue by a stray deferral.
func TestDeferTerminalJobRejected(t *testing.T) {
	ctx := context.Background()
	store := openDeferTestStore(t, 10)
	job := seedDeferJob(t, store, "github.com", 9111, 1, "review")
	done := *job
	done.Status = "completed"
	now := time.Now().UTC()
	done.FinishedAt = &now
	if err := store.UpdateJob(ctx, &done); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeferJob(ctx, job.ID, time.Now().UTC().Add(time.Minute), "stray"); err == nil {
		t.Errorf("deferring a completed job must fail")
	}
}

// TestDeferredEarliestDue: the scheduler sees the minimum future due time,
// and false when nothing is parked.
func TestDeferredEarliestDue(t *testing.T) {
	ctx := context.Background()
	store := openDeferTestStore(t, 10)
	if _, ok, err := store.EarliestDeferredDue(ctx); err != nil || ok {
		t.Fatalf("empty queue earliest = %v, %v; want false", ok, err)
	}
	a := seedDeferJob(t, store, "github.com", 9051, 1, "review")
	b := seedDeferJob(t, store, "github.com", 9052, 2, "review")
	dueA := time.Now().UTC().Add(10 * time.Minute)
	dueB := time.Now().UTC().Add(5 * time.Minute)
	if _, err := store.DeferJob(ctx, a.ID, dueA, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeferJob(ctx, b.ID, dueB, "b"); err != nil {
		t.Fatal(err)
	}
	due, ok, err := store.EarliestDeferredDue(ctx)
	if err != nil || !ok || !due.Equal(dueB) {
		t.Errorf("earliest = %v, %v, %v; want %v true", due, ok, err, dueB)
	}
}

// TestDeferredQueueStateLabels: inspection labels distinguish waiting,
// due, ready, and terminal work.
func TestDeferredQueueStateLabels(t *testing.T) {
	now := time.Now().UTC()
	if got := DescribeQueueState(nil, now); got != "" {
		t.Errorf("nil job = %q, want empty", got)
	}
	ready := &Job{Status: "queued"}
	if got := DescribeQueueState(ready, now); got != "queued" {
		t.Errorf("ready = %q, want queued", got)
	}
	waiting := &Job{Status: "queued", NotBeforeAt: now.Add(time.Hour)}
	if got := DescribeQueueState(waiting, now); got != "deferred until "+waiting.NotBeforeAt.Format("2006-01-02T15:04:05Z07:00") {
		t.Errorf("waiting = %q", got)
	}
	due := &Job{Status: "queued", NotBeforeAt: now.Add(-time.Minute)}
	if got := DescribeQueueState(due, now); got != "due (deferred)" {
		t.Errorf("due = %q", got)
	}
	running := &Job{Status: "running"}
	if got := DescribeQueueState(running, now); got != "running" {
		t.Errorf("running = %q", got)
	}
}

// TestDeferredResolveRerunRequeuesImmediately: operator rerun clears the
// parked due time so the job runs now instead of staying deferred.
func TestDeferredResolveRerunRequeuesImmediately(t *testing.T) {
	ctx := context.Background()
	store := openDeferTestStore(t, 10)
	job := seedDeferJob(t, store, "github.com", 9061, 1, "review")
	if _, err := store.DeferJob(ctx, job.ID, time.Now().UTC().Add(time.Hour), "wait"); err != nil {
		t.Fatal(err)
	}
	failed := *job
	failed.Status = "needs_attention"
	failed.Error = "uncertain write"
	if err := store.UpdateJob(ctx, &failed); err != nil {
		t.Fatal(err)
	}
	if err := store.ResolveJob(ctx, job.ID, "rerun", true); err != nil {
		t.Fatalf("ResolveJob rerun failed: %v", err)
	}
	got, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "queued" || !got.NotBeforeAt.IsZero() {
		t.Errorf("rerun left status=%q due=%v, want queued with no due time", got.Status, got.NotBeforeAt)
	}
	if claimed, err := store.ClaimNextJob(ctx, map[string]bool{}); err != nil || claimed == nil || claimed.ID != job.ID {
		t.Errorf("rerun job must be immediately claimable, got %+v, err %v", claimed, err)
	}
}

// TestDeferredNeedsAttentionBlocksOwnPR: an unprovable outcome blocks only
// its own PR; other PRs keep progressing in FIFO order.
func TestDeferredNeedsAttentionBlocksOwnPR(t *testing.T) {
	ctx := context.Background()
	store := openDeferTestStore(t, 10)
	jobA := seedDeferJob(t, store, "github.com", 9071, 1, "review")
	jobB := seedDeferJob(t, store, "github.com", 9072, 2, "review")

	exec := NewServerJobExecutor(&Server{cfg: &config.Config{}}, store)
	exec.markNeedsAttention(ctx, jobA, "unprovable write outcome")

	st, err := store.GetPRState(ctx, jobA.PRKey)
	if err != nil {
		t.Fatal(err)
	}
	if !st.HasBlockedAction {
		t.Errorf("own PR must be blocked by needs_attention")
	}
	claimed, err := store.ClaimNextJob(ctx, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || claimed.ID != jobB.ID {
		t.Errorf("other PR must progress, claimed %+v", claimed)
	}
}

// ambiguousPostFake fails comment creation with an ambiguous 500 after
// counting the single attempt (no live credentials).
type ambiguousPostFake struct {
	posts atomic.Int32
	srv   *httptest.Server
}

func newAmbiguousPostFake(t *testing.T) *ambiguousPostFake {
	f := &ambiguousPostFake{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			f.posts.Add(1)
			http.Error(w, "upstream failure", http.StatusInternalServerError)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// TestUncertainWriteNeedsAttentionWithoutDuplicate: an ambiguous POST
// produces at most one remote create and lands in needs_attention with the
// operator resolve path, never a blind retry.
func TestUncertainWriteNeedsAttentionWithoutDuplicate(t *testing.T) {
	ctx := context.Background()
	store := openDeferTestStore(t, 10)
	fake := newAmbiguousPostFake(t)
	gh, err := ghclient.NewTestClient(fake.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	job := seedDeferJob(t, store, "github.com", 9081, 1, "improve")

	exec := NewServerJobExecutor(&Server{gh: gh, cfg: &config.Config{}}, store)
	if err := exec.executeImproveJob(ctx, job); err == nil {
		t.Fatalf("ambiguous POST must surface an error")
	}
	if n := fake.posts.Load(); n != 1 {
		t.Errorf("remote creates = %d, want exactly 1 (no blind retry)", n)
	}
	got, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "needs_attention" {
		t.Errorf("status = %q, want needs_attention", got.Status)
	}
	if got.FinishedAt != nil {
		t.Errorf("needs_attention must stay nonterminal")
	}
}

// TestDeferExecutorBudgetMapping: ExceedsWorkerBudget required waits are
// honored up to the defer cap, and unknown waits clamp to the cap.
func TestDeferExecutorBudgetMapping(t *testing.T) {
	ctx := context.Background()
	store := openDeferTestStore(t, 10)
	fake := newPubFake(t)
	gh, err := ghclient.NewTestClient(fake.srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("HonorsRequiredWait", func(t *testing.T) {
		job := seedDeferJob(t, store, "github.com", 9091, 1, "review")
		exec := NewServerJobExecutor(&Server{gh: gh, cfg: &config.Config{}}, store)
		before := time.Now().UTC()
		budgetErr := &retry.ExceedsWorkerBudgetError{RequiredWait: 90 * time.Second, MaxWait: time.Minute}
		if wait, ok := budgetWait(budgetErr); !ok || wait != 90*time.Second {
			t.Fatalf("budgetWait = %v, %v", wait, ok)
		}
		if err := exec.deferForExhaustedBudget(ctx, job, 90*time.Second, "rate limited"); err != nil {
			t.Fatalf("defer failed: %v", err)
		}
		got, err := store.GetJob(ctx, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "queued" || got.NotBeforeAt.Before(before.Add(89*time.Second)) || got.NotBeforeAt.After(before.Add(91*time.Second)) {
			t.Errorf("due = %v, want ~90s out", got.NotBeforeAt)
		}
		if got.StatusCommentID == 0 {
			t.Errorf("waiting notice must reuse the owned status comment")
		}
	})

	t.Run("ClampsToCap", func(t *testing.T) {
		job := seedDeferJob(t, store, "github.com", 9092, 2, "review")
		exec := NewServerJobExecutor(&Server{gh: gh, cfg: &config.Config{}}, store)
		before := time.Now().UTC()
		if err := exec.deferForExhaustedBudget(ctx, job, 5*time.Hour, "rate limited"); err != nil {
			t.Fatalf("defer failed: %v", err)
		}
		got, err := store.GetJob(ctx, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		cap := config.DefaultDeferMaxWait
		if got.NotBeforeAt.Before(before.Add(cap-time.Minute)) || got.NotBeforeAt.After(before.Add(cap+time.Minute)) {
			t.Errorf("due = %v, want clamped to cap %v", got.NotBeforeAt, cap)
		}
	})
}

// TestDeferredSchedulerIdleWaitBounds: idle workers poll fast with nothing
// parked, sleep to the due time when near, and cap at the poll interval.
func TestDeferredSchedulerIdleWaitBounds(t *testing.T) {
	ctx := context.Background()
	store := openDeferTestStore(t, 10)
	sched := NewScheduler(store, nil, 1)

	if got := sched.idleWait(ctx); got != 100*time.Millisecond {
		t.Errorf("empty idle = %v, want 100ms", got)
	}

	job := seedDeferJob(t, store, "github.com", 9101, 1, "review")
	if _, err := store.DeferJob(ctx, job.ID, time.Now().UTC().Add(time.Hour), "wait"); err != nil {
		t.Fatal(err)
	}
	if got := sched.idleWait(ctx); got != config.DefaultDeferPollInterval {
		t.Errorf("far idle = %v, want poll cap %v", got, config.DefaultDeferPollInterval)
	}

	near := seedDeferJob(t, store, "github.com", 9102, 2, "review")
	if _, err := store.DeferJob(ctx, near.ID, time.Now().UTC().Add(50*time.Millisecond), "soon"); err != nil {
		t.Fatal(err)
	}
	if got := sched.idleWait(ctx); got <= 0 || got > time.Second {
		t.Errorf("near idle = %v, want small positive wait", got)
	}
}
