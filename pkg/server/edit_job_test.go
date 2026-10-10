// Executor safety tests for the gated edit pipeline (plan 05-04, Task 3).
//
// Every scenario runs against fakes only: a counting fake GitHub client, a
// temp-dir snapshot preparer, a scripted edit loop and verifier, and a
// scripted intent classifier. No live credentials, no Podman. Push-call
// counts are asserted as exactly 0 or 1 in every case (D-22/D-34).
package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thozoz/pr-review-go/pkg/assistant"
	"github.com/thozoz/pr-review-go/pkg/config"
	ghclient "github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/sandbox"
)

const (
	editTestBase  = "1111111111111111111111111111111111111111"
	editTestHead  = "2222222222222222222222222222222222222222"
	editTestHeadB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	editTestNew   = "cccccccccccccccccccccccccccccccccccccccc"
	editTestOld   = "dddddddddddddddddddddddddddddddddddddddd"
)

type stubLLM struct{}

func (stubLLM) ChatCompletion(_ context.Context, _, _ string) (string, error) {
	return "", errors.New("stub LLM must not be called")
}

// fakeEditGH implements editGitHub with scripted responses and exact
// mutation counting for push-call accounting.
type fakeEditGH struct {
	prs      []*ghclient.PRDetails
	prErr    error
	getCalls int

	writeVerdicts []bool
	writeErr      error
	writeCalls    int

	diff      string
	diffErr   error
	diffCalls int

	comments []string

	commitCalls      int
	commitSHA        string
	commitErr        error
	lastHeadline     string
	lastFiles        map[string]string
	lastBranch       string
	lastExpectedHead string

	minted     int
	revoked    int
	revokeErr  error
	pushCredErr error
	nextCred   *ghclient.PushCredential
}

func (f *fakeEditGH) GetPR(_ context.Context, _, _ string, _ int) (*ghclient.PRDetails, error) {
	f.getCalls++
	if f.prErr != nil {
		return nil, f.prErr
	}
	if len(f.prs) == 0 {
		return nil, errors.New("no scripted PR")
	}
	i := f.getCalls - 1
	if i >= len(f.prs) {
		i = len(f.prs) - 1
	}
	return f.prs[i], nil
}

func (f *fakeEditGH) CanWriteRepository(_ context.Context, _, _, _ string) (bool, error) {
	f.writeCalls++
	if f.writeErr != nil {
		return false, f.writeErr
	}
	if len(f.writeVerdicts) == 0 {
		return true, nil
	}
	i := f.writeCalls - 1
	if i >= len(f.writeVerdicts) {
		i = len(f.writeVerdicts) - 1
	}
	return f.writeVerdicts[i], nil
}

func (f *fakeEditGH) GetDiffAtCommits(_ context.Context, _, _, _, _ string) (string, error) {
	f.diffCalls++
	if f.diffErr != nil {
		return "", f.diffErr
	}
	return f.diff, nil
}

func (f *fakeEditGH) PostComment(_ context.Context, _, _ string, _ int, body string) error {
	f.comments = append(f.comments, body)
	return nil
}

func (f *fakeEditGH) CreatePushCredential(_ context.Context, _, _ string, repoID int64) (*ghclient.PushCredential, error) {
	f.minted++
	if f.pushCredErr != nil {
		return nil, f.pushCredErr
	}
	if f.nextCred != nil {
		return f.nextCred, nil
	}
	return &ghclient.PushCredential{Token: "fake-push-token", RepositoryID: repoID}, nil
}

func (f *fakeEditGH) RevokePushCredential(_ context.Context, cred *ghclient.PushCredential) error {
	f.revoked++
	cred.Zeroize()
	return f.revokeErr
}

func (f *fakeEditGH) CommitPush(_ context.Context, _ *ghclient.PushCredential, _, _, branchName, expectedHeadOID, headline string, files map[string]string, _ []string) (string, error) {
	f.commitCalls++
	f.lastHeadline = headline
	f.lastFiles = files
	f.lastBranch = branchName
	f.lastExpectedHead = expectedHeadOID
	if f.commitErr != nil {
		return "", f.commitErr
	}
	if f.commitSHA != "" {
		return f.commitSHA, nil
	}
	return editTestNew, nil
}

func editTestPR(headSHA string) *ghclient.PRDetails {
	return &ghclient.PRDetails{
		Owner: "o", Repo: "r", Number: 7,
		HeadRef: "feat-x", HeadSHA: headSHA, BaseSHA: editTestBase,
		HeadRepoOwner: "o", HeadRepoName: "r", HeadRepoID: 99,
		CloneURL: "https://github.com/o/r.git",
	}
}

func editTestForkPR(headSHA string) *ghclient.PRDetails {
	pr := editTestPR(headSHA)
	pr.HeadRepoOwner = "fork-user"
	pr.HeadRepoName = "r"
	return pr
}

type editHarness struct {
	store *BoltJobStore
	exec  *ServerJobExecutor
	gh    *fakeEditGH

	published   []string
	verdict     IntentVerdict
	verdictErr  error
	stickySeen  []string
	instrSeen   []string
	editResult  assistant.EditResult
	editErr     error
	reportState sandbox.VerificationStatus
	reportStates []sandbox.VerificationStatus
	reportErr   error
	reportNote  string

	prepareCalls int
	prepareFiles map[string]string
	prepareErr   error
	verifyCalls  int
}

func newEditHarness(t *testing.T) *editHarness {
	t.Helper()
	store, err := OpenJobStore(filepath.Join(t.TempDir(), "jobs.db"), StoreOptions{
		BacklogLimit:  20,
		DeliveryLimit: 100,
		StateMaxBytes: 16777216,
		DeliveryTTL:   24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	h := &editHarness{
		store: store,
		gh: &fakeEditGH{
			prs:       []*ghclient.PRDetails{editTestPR(editTestHead), editTestPR(editTestHead)},
			commitSHA: editTestNew,
		},
		verdict:      IntentVerdict{Intent: "commit", Confidence: 0.95, Summary: "Fix nil panic"},
		reportState:  sandbox.StatusPassed,
		prepareFiles: map[string]string{"a.go": "package a\n", "b.go": "package b\n"},
		editResult:   assistant.EditResult{FilesChanged: []string{"a.go", "b.go"}, TotalLines: 4, TotalBytes: 32},
	}
	srv := &Server{cfg: &config.Config{}}
	h.exec = NewServerJobExecutor(srv, store)
	h.exec.editGH = h.gh
	h.exec.editLLM = stubLLM{}
	h.exec.editClassify = func(_ context.Context, _ LLMCaller, _ string) (IntentVerdict, error) {
		return h.verdict, h.verdictErr
	}
	h.exec.editPublish = func(_ context.Context, job *Job, text string) error {
		h.published = append(h.published, text)
		if job.StatusCommentID == 0 {
			job.StatusCommentID = 9001
		}
		return nil
	}
	h.exec.editPrepare = func(_ context.Context, _, _, headSHA string) (*sandbox.Snapshot, func(), error) {
		h.prepareCalls++
		if h.prepareErr != nil {
			return nil, nil, h.prepareErr
		}
		dir := t.TempDir()
		for p, c := range h.prepareFiles {
			full := filepath.Join(dir, filepath.FromSlash(p))
			if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(full, []byte(c), 0644); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
		return &sandbox.Snapshot{CommitSHA: headSHA, SourceDir: dir}, func() {}, nil
	}
	h.exec.editLoop = func(_ context.Context, _, instruction, sticky string) (assistant.EditResult, error) {
		h.instrSeen = append(h.instrSeen, instruction)
		h.stickySeen = append(h.stickySeen, sticky)
		return h.editResult, h.editErr
	}
	h.exec.editVerify = func(_ context.Context, snap *sandbox.Snapshot) (*sandbox.VerificationReport, error) {
		h.verifyCalls++
		if h.reportErr != nil {
			return nil, h.reportErr
		}
		state := h.reportState
		if len(h.reportStates) > 0 {
			i := h.verifyCalls - 1
			if i >= len(h.reportStates) {
				i = len(h.reportStates) - 1
			}
			state = h.reportStates[i]
		}
		rep := &sandbox.VerificationReport{Status: state}
		if snap != nil {
			rep.WorkspaceDir = snap.SourceDir
		}
		if h.reportNote != "" {
			rep.Reason = h.reportNote
		}
		return rep, nil
	}
	return h
}

func (h *editHarness) admitEdit(t *testing.T, prNum int, head, base, author, payload string, commentID int64) *Job {
	t.Helper()
	ctx := context.Background()
	prKey, err := MakePRKey("github.com", 4242, prNum)
	if err != nil {
		t.Fatalf("pr key: %v", err)
	}
	res, err := h.store.Admit(ctx, Delivery{
		Host: "github.com", RepoID: 4242,
		DeliveryID:  fmt.Sprintf("deliv-%d", commentID),
		PayloadHash: fmt.Sprintf("h-%d", commentID),
		ReceivedAt:  time.Now(),
	}, []Job{{
		Kind: "edit", Trigger: "explicit", Author: author, CommentID: commentID,
		PRKey: prKey, Owner: "o", Repo: "r", PRNumber: prNum,
		BaseSHA: base, HeadSHA: head, Payload: payload,
	}})
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if res.Status != AdmitAccepted {
		t.Fatalf("admit status = %v (%s), want accepted", res.Status, res.Reason)
	}
	queued, err := h.store.ListQueuedJobs(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, j := range queued {
		if j.Payload == payload && j.PRNumber == prNum {
			return j
		}
	}
	t.Fatalf("admitted job not found in queue")
	return nil
}

func (h *editHarness) run(t *testing.T, job *Job) (*Job, error) {
	t.Helper()
	err := h.exec.executeEditJob(context.Background(), job)
	after, gerr := h.store.GetJob(context.Background(), job.ID)
	if gerr != nil {
		t.Fatalf("get job: %v", gerr)
	}
	return after, err
}

func (h *editHarness) publishedText() string {
	return strings.Join(h.published, "\n")
}

func TestEditJobConfirmPaths(t *testing.T) {
	cases := []struct {
		name    string
		verdict IntentVerdict
		verr    error
	}{
		{"unclear", IntentVerdict{Intent: "unclear", Confidence: 0.5}, nil},
		{"question", IntentVerdict{Intent: "question", Confidence: 0.9, Summary: "What does this do"}, nil},
		{"low-confidence-commit", IntentVerdict{Intent: "commit", Confidence: 0.69, Summary: "Fix it"}, nil},
		{"classifier-error", IntentVerdict{Intent: "unclear"}, errors.New("llm boom")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newEditHarness(t)
			h.verdict, h.verdictErr = tc.verdict, tc.verr
			job := h.admitEdit(t, 7, editTestHead, editTestBase, "alice", "please fix it", 101)
			after, err := h.run(t, job)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if after.Status != "completed" {
				t.Fatalf("status = %q, want completed", after.Status)
			}
			if !strings.Contains(h.publishedText(), "anlayamad") {
				t.Fatalf("missing multilingual confirm comment, got: %q", h.publishedText())
			}
			if h.prepareCalls != 0 {
				t.Fatalf("prepareCalls = %d, want 0 (zero snapshot work)", h.prepareCalls)
			}
			if h.gh.commitCalls != 0 {
				t.Fatalf("commitCalls = %d, want 0 pushes", h.gh.commitCalls)
			}
		})
	}
}

func TestEditJobForkRefusal(t *testing.T) {
	t.Run("fork", func(t *testing.T) {
		h := newEditHarness(t)
		h.gh.prs = []*ghclient.PRDetails{editTestForkPR(editTestHead)}
		job := h.admitEdit(t, 7, editTestHead, editTestBase, "alice", "fix it", 102)
		after, err := h.run(t, job)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if after.Status != "completed" {
			t.Fatalf("status = %q, want completed", after.Status)
		}
		if !strings.Contains(h.publishedText(), "Fork") {
			t.Fatalf("missing fork-refusal comment, got: %q", h.publishedText())
		}
		if h.prepareCalls != 0 || h.gh.commitCalls != 0 {
			t.Fatalf("prepare=%d commits=%d, want 0/0", h.prepareCalls, h.gh.commitCalls)
		}
	})
	t.Run("nil-metadata-fail-closed", func(t *testing.T) {
		h := newEditHarness(t)
		bare := editTestPR(editTestHead)
		bare.HeadRepoOwner, bare.HeadRepoName = "", ""
		h.gh.prs = []*ghclient.PRDetails{bare}
		job := h.admitEdit(t, 7, editTestHead, editTestBase, "alice", "fix it", 103)
		after, err := h.run(t, job)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if after.Status != "completed" {
			t.Fatalf("status = %q, want completed", after.Status)
		}
		if h.prepareCalls != 0 || h.gh.commitCalls != 0 {
			t.Fatalf("prepare=%d commits=%d, want 0/0", h.prepareCalls, h.gh.commitCalls)
		}
	})
}

func TestEditJobSnapshotUnavailable(t *testing.T) {
	h := newEditHarness(t)
	h.prepareErr = sandbox.ErrSourceProviderUnavailable
	job := h.admitEdit(t, 7, editTestHead, editTestBase, "alice", "fix it", 104)
	after, err := h.run(t, job)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if after.Status != "completed" {
		t.Fatalf("status = %q, want completed (honest degradation)", after.Status)
	}
	if !strings.Contains(h.publishedText(), "zole") {
		t.Fatalf("missing unavailable notice, got: %q", h.publishedText())
	}
	if h.gh.commitCalls != 0 {
		t.Fatalf("commitCalls = %d, want 0 pushes", h.gh.commitCalls)
	}
	if len(h.instrSeen) != 0 {
		t.Fatalf("edit loop ran despite unavailable snapshot")
	}
}

func TestEditJobPushTimeReauthDenial(t *testing.T) {
	t.Run("denied", func(t *testing.T) {
		h := newEditHarness(t)
		h.gh.writeVerdicts = []bool{false}
		job := h.admitEdit(t, 7, editTestHead, editTestBase, "alice", "fix it", 105)
		after, err := h.run(t, job)
		if err == nil {
			t.Fatalf("expected denial error")
		}
		if after.Status != "failed" {
			t.Fatalf("status = %q, want failed", after.Status)
		}
		if !strings.Contains(h.publishedText(), "yetki") {
			t.Fatalf("missing reauthorization-denied comment, got: %q", h.publishedText())
		}
		if h.gh.commitCalls != 0 || h.gh.minted != 0 {
			t.Fatalf("commits=%d minted=%d, want 0/0", h.gh.commitCalls, h.gh.minted)
		}
	})
	t.Run("revoked-mid-job", func(t *testing.T) {
		h := newEditHarness(t)
		// Admission-time check passes...
		allowed, err := h.gh.CanWriteRepository(context.Background(), "o", "r", "alice")
		if err != nil || !allowed {
			t.Fatalf("admission check: allowed=%v err=%v", allowed, err)
		}
		// ...then the permission is revoked before push time.
		h.gh.writeVerdicts = []bool{false}
		job := h.admitEdit(t, 7, editTestHead, editTestBase, "alice", "fix it", 106)
		after, rerr := h.run(t, job)
		if rerr == nil {
			t.Fatalf("expected denial error")
		}
		if after.Status != "failed" {
			t.Fatalf("status = %q, want failed", after.Status)
		}
		if h.gh.commitCalls != 0 {
			t.Fatalf("commitCalls = %d, want 0 pushes", h.gh.commitCalls)
		}
	})
}

func TestEditJobNonGreenVerification(t *testing.T) {
	cases := []struct {
		name       string
		status     sandbox.VerificationStatus
		wantStatus string
		noticeFrag string
	}{
		{"build-failed", sandbox.StatusBuildFailed, "failed", "Doğrulama"},
		{"test-failed", sandbox.StatusTestFailed, "failed", "Doğrulama"},
		{"unsupported-language", sandbox.StatusUnsupportedLanguage, "failed", "Doğrulama"},
		{"unavailable", sandbox.StatusUnavailable, "completed", "zole"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newEditHarness(t)
			h.reportState = tc.status
			h.reportNote = "no container backend in test"
			job := h.admitEdit(t, 7, editTestHead, editTestBase, "alice", "fix it", 200+int64(len(tc.name)))
			after, _ := h.run(t, job)
			if after.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", after.Status, tc.wantStatus)
			}
			if !strings.Contains(h.publishedText(), tc.noticeFrag) {
				t.Fatalf("missing %q notice, got: %q", tc.noticeFrag, h.publishedText())
			}
			if h.gh.commitCalls != 0 || h.gh.minted != 0 {
				t.Fatalf("commits=%d minted=%d, want 0/0", h.gh.commitCalls, h.gh.minted)
			}
		})
	}
}

func TestEditJobVerifyRetryThenGreen(t *testing.T) {
	h := newEditHarness(t)
	h.reportStates = []sandbox.VerificationStatus{sandbox.StatusTestFailed, sandbox.StatusPassed}
	h.reportNote = "assertion failed: want 99"
	job := h.admitEdit(t, 7, editTestHead, editTestBase, "alice", "fix it", 701)
	after, _ := h.run(t, job)
	if after.Status != "completed" {
		t.Fatalf("status = %q, want completed", after.Status)
	}
	if h.verifyCalls != 2 {
		t.Fatalf("verifyCalls = %d, want 2", h.verifyCalls)
	}
	if len(h.stickySeen) != 2 {
		t.Fatalf("edit loop calls = %d, want 2", len(h.stickySeen))
	}
	if !strings.Contains(h.stickySeen[1], "FAILED") || !strings.Contains(h.stickySeen[1], "assertion failed") {
		t.Fatalf("retry sticky missing failure feedback, got: %q", h.stickySeen[1])
	}
	if h.gh.commitCalls != 1 {
		t.Fatalf("commits=%d, want 1", h.gh.commitCalls)
	}
}

func TestEditJobVerifyRetryExhausted(t *testing.T) {
	h := newEditHarness(t)
	h.reportStates = []sandbox.VerificationStatus{
		sandbox.StatusTestFailed, sandbox.StatusTestFailed,
		sandbox.StatusTestFailed, sandbox.StatusTestFailed,
	}
	job := h.admitEdit(t, 7, editTestHead, editTestBase, "alice", "fix it", 702)
	after, _ := h.run(t, job)
	if after.Status != "failed" {
		t.Fatalf("status = %q, want failed", after.Status)
	}
	if h.verifyCalls != editVerifyMaxAttempts {
		t.Fatalf("verifyCalls = %d, want %d", h.verifyCalls, editVerifyMaxAttempts)
	}
	if h.gh.commitCalls != 0 || h.gh.minted != 0 {
		t.Fatalf("commits=%d minted=%d, want 0/0", h.gh.commitCalls, h.gh.minted)
	}
}

func TestEditJobPrePushStale(t *testing.T) {
	h := newEditHarness(t)
	h.gh.prs = []*ghclient.PRDetails{editTestPR(editTestHead), editTestPR(editTestHeadB)}
	job := h.admitEdit(t, 7, editTestHead, editTestBase, "alice", "fix it", 107)
	after, err := h.run(t, job)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if after.Status != "superseded" {
		t.Fatalf("status = %q, want superseded", after.Status)
	}
	if !strings.Contains(h.publishedText(), short8(editTestHead)) {
		t.Fatalf("stale comment must name old short SHA, got: %q", h.publishedText())
	}
	if h.gh.commitCalls != 0 || h.gh.minted != 0 {
		t.Fatalf("commits=%d minted=%d, want 0/0 (stale aborts before mint)", h.gh.commitCalls, h.gh.minted)
	}
}

func TestEditJobHeadMovedOnPush(t *testing.T) {
	h := newEditHarness(t)
	h.gh.commitErr = &ghclient.HeadMovedError{
		ExpectedHeadOID: editTestHead,
		ActualHeadOID:   editTestHeadB,
		Message:         "expected branch to point to old head",
	}
	job := h.admitEdit(t, 7, editTestHead, editTestBase, "alice", "fix it", 108)
	after, err := h.run(t, job)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if after.Status != "superseded" {
		t.Fatalf("status = %q, want superseded", after.Status)
	}
	if h.gh.commitCalls != 1 {
		t.Fatalf("commitCalls = %d, want exactly 1 (zero retries)", h.gh.commitCalls)
	}
	if h.gh.revoked != 1 {
		t.Fatalf("revoked = %d, want 1 (mint-and-burn even on CAS conflict)", h.gh.revoked)
	}
	if !strings.Contains(h.publishedText(), short8(editTestHead)) {
		t.Fatalf("stale comment must name old SHA, got: %q", h.publishedText())
	}
}

func TestEditJobUncertainPush(t *testing.T) {
	h := newEditHarness(t)
	h.gh.commitErr = errors.New("push transport timeout: graphql unexpected http status 503")
	if !ghclient.IsUncertainWriteError(h.gh.commitErr) {
		t.Fatalf("fixture must classify as uncertain write")
	}
	job := h.admitEdit(t, 7, editTestHead, editTestBase, "alice", "fix it", 109)
	after, err := h.run(t, job)
	if err == nil {
		t.Fatalf("expected uncertain error return")
	}
	if after.Status != "needs_attention" {
		t.Fatalf("status = %q, want needs_attention", after.Status)
	}
	if after.FinishedAt != nil {
		t.Fatalf("needs_attention must stay nonterminal (no FinishedAt)")
	}
	if h.gh.commitCalls != 1 {
		t.Fatalf("commitCalls = %d, want exactly 1 remote create", h.gh.commitCalls)
	}
	// Resolvable via --queue-resolve: operator rerun requeues the job.
	if rerr := h.store.ResolveJob(context.Background(), job.ID, "rerun", true); rerr != nil {
		t.Fatalf("resolve rerun: %v", rerr)
	}
	resolved, _ := h.store.GetJob(context.Background(), job.ID)
	if resolved.Status != "queued" {
		t.Fatalf("resolved status = %q, want queued", resolved.Status)
	}
}

func TestEditJobRefusedPush403(t *testing.T) {
	h := newEditHarness(t)
	h.gh.commitErr = errors.New("graphql auth error (status 403): resource not accessible by integration")
	job := h.admitEdit(t, 7, editTestHead, editTestBase, "alice", "fix it", 110)
	after, err := h.run(t, job)
	if err == nil {
		t.Fatalf("expected refusal error")
	}
	if after.Status != "failed" {
		t.Fatalf("status = %q, want failed", after.Status)
	}
	if h.gh.commitCalls != 1 {
		t.Fatalf("commitCalls = %d, want exactly 1 attempt, no bypass retry", h.gh.commitCalls)
	}
	if !strings.Contains(h.publishedText(), "reddedildi") {
		t.Fatalf("missing refuse-with-reason comment, got: %q", h.publishedText())
	}
}

func TestEditJobHappyPathTwoFiles(t *testing.T) {
	h := newEditHarness(t)
	payload := "lutfen nil kontrolu ekle"
	job := h.admitEdit(t, 7, editTestHead, editTestBase, "alice", payload, 111)
	after, err := h.run(t, job)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if after.Status != "completed" {
		t.Fatalf("status = %q, want completed (err=%q)", after.Status, after.Error)
	}
	if h.gh.commitCalls != 1 {
		t.Fatalf("commitCalls = %d, want exactly 1", h.gh.commitCalls)
	}
	if h.gh.minted != 1 || h.gh.revoked != 1 {
		t.Fatalf("minted=%d revoked=%d, want 1/1 single-use credential", h.gh.minted, h.gh.revoked)
	}
	if h.gh.lastBranch != "feat-x" {
		t.Fatalf("branch = %q, want PR head ref feat-x", h.gh.lastBranch)
	}
	if h.gh.lastExpectedHead != editTestHead {
		t.Fatalf("expectedHead = %q, want job head", h.gh.lastExpectedHead)
	}
	if len(h.gh.lastFiles) != 2 || h.gh.lastFiles["a.go"] != "package a\n" || h.gh.lastFiles["b.go"] != "package b\n" {
		t.Fatalf("pushed files mismatch: %v", h.gh.lastFiles)
	}
	// D-35: headline carries BOTH the English summary AND the quoted original.
	if !strings.Contains(h.gh.lastHeadline, "Fix nil panic") {
		t.Fatalf("headline missing English summary: %q", h.gh.lastHeadline)
	}
	if !strings.Contains(h.gh.lastHeadline, `"`+payload+`"`) {
		t.Fatalf("headline missing quoted original: %q", h.gh.lastHeadline)
	}
	text := h.publishedText()
	if !strings.Contains(text, "`a.go`") || !strings.Contains(text, "`b.go`") {
		t.Fatalf("summary must list files, got: %q", text)
	}
	if !strings.Contains(text, "passed") || !strings.Contains(text, editTestNew) {
		t.Fatalf("summary must carry verification result plus commit SHA, got: %q", text)
	}
	st, serr := h.store.GetPRState(context.Background(), job.PRKey)
	if serr != nil {
		t.Fatalf("pr state: %v", serr)
	}
	if st.LastBotCommitSHA != editTestNew || st.LastBotBaseSHA != editTestHead {
		t.Fatalf("sticky SHAs = %q/%q, want %q/%q", st.LastBotCommitSHA, st.LastBotBaseSHA, editTestNew, editTestHead)
	}
	if len(st.EditHistory) != 1 || st.EditHistory[0] != payload {
		t.Fatalf("history = %v", st.EditHistory)
	}
}

func TestEditJobStickyChaining(t *testing.T) {
	h := newEditHarness(t)
	ctx := context.Background()
	job := h.admitEdit(t, 7, editTestHead, editTestBase, "alice", "second fix", 112)
	prKey := job.PRKey
	// Seed prior bot state: one earlier landed edit plus its base.
	if err := h.store.UpdatePRState(ctx, &PRState{
		PRKey: prKey, Owner: "o", Repo: "r", Number: 7,
		LastBotCommitSHA: editTestOld, LastBotBaseSHA: editTestBase,
		EditHistory: []string{"first fix"},
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	h.gh.diff = "diff --git a/a.go b/a.go\n+old bot line\n"
	after, err := h.run(t, job)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if after.Status != "completed" {
		t.Fatalf("status = %q, want completed", after.Status)
	}
	if len(h.stickySeen) != 1 {
		t.Fatalf("loop calls = %d, want 1", len(h.stickySeen))
	}
	if !strings.Contains(h.stickySeen[0], "first fix") || !strings.Contains(h.stickySeen[0], "old bot line") {
		t.Fatalf("second job must receive history plus prior diff, got: %q", h.stickySeen[0])
	}
	if h.instrSeen[0] != "second fix" {
		t.Fatalf("instruction = %q", h.instrSeen[0])
	}
	st, _ := h.store.GetPRState(ctx, prKey)
	if st.LastBotCommitSHA != editTestNew {
		t.Fatalf("LastBotCommitSHA = %q, want %q", st.LastBotCommitSHA, editTestNew)
	}
	if len(st.EditHistory) != 2 || st.EditHistory[0] != "first fix" || st.EditHistory[1] != "second fix" {
		t.Fatalf("history = %v", st.EditHistory)
	}
}

func TestEditJobSerialization(t *testing.T) {
	h := newEditHarness(t)
	ctx := context.Background()
	job1 := h.admitEdit(t, 7, editTestHead, editTestBase, "alice", "first instruction", 120)
	job2 := h.admitEdit(t, 7, editTestHead, editTestBase, "alice", "second instruction", 121)
	if job1.ID == job2.ID {
		t.Fatalf("distinct edit commands must stay distinct jobs")
	}
	// A different PR stays claimable while PR 7 is busy (fairness).
	job3 := h.admitEdit(t, 8, editTestHead, editTestBase, "alice", "other pr", 122)

	claimed1, err := h.store.ClaimNextJob(ctx, map[string]bool{})
	if err != nil || claimed1 == nil || claimed1.ID != job1.ID {
		t.Fatalf("first claim = %+v err=%v, want job1", claimed1, err)
	}
	active := map[string]bool{claimed1.PRKey.String(): true}
	// A different PR stays claimable while PR 7 is busy (fairness).
	other, err := h.store.ClaimNextJob(ctx, active)
	if err != nil || other == nil || other.ID != job3.ID {
		t.Fatalf("other PR must proceed while PR 7 busy, got %+v err=%v", other, err)
	}
	// With both PRs held, nothing is claimable: the second PR-7 edit waits
	// while the first holds the per-PR slot.
	active[other.PRKey.String()] = true
	if second, err := h.store.ClaimNextJob(ctx, active); err != nil || second != nil {
		t.Fatalf("same-PR second claim must wait, got %+v err=%v", second, err)
	}
	// State gate alone (no active map) also blocks the waiting edit: the
	// only queued job left is PR 7's second edit, held by ActiveJobID.
	if second, err := h.store.ClaimNextJob(ctx, map[string]bool{}); err != nil || second != nil {
		t.Fatalf("waiting edit must hold on state gate, got %+v err=%v", second, err)
	}
	// Release the slot: the waiting edit becomes claimable in FIFO order.
	_ = h.store.ReleasePR(ctx, claimed1.PRKey)
	delete(active, claimed1.PRKey.String())
	active[other.PRKey.String()] = true
	next, err := h.store.ClaimNextJob(ctx, active)
	if err != nil || next == nil || next.ID != job2.ID {
		t.Fatalf("waiting edit must claim after release, got %+v err=%v", next, err)
	}
	_ = job3
}

func TestEditJobStoreGuards(t *testing.T) {
	h := newEditHarness(t)
	ctx := context.Background()
	prKey, _ := MakePRKey("github.com", 4242, 7)

	admit := func(deliveryID, head, base string) AdmitResult {
		res, err := h.store.Admit(ctx, Delivery{
			Host: "github.com", RepoID: 4242, DeliveryID: deliveryID,
			PayloadHash: deliveryID, ReceivedAt: time.Now(),
		}, []Job{{
			Kind: "edit", Trigger: "explicit", Author: "alice", CommentID: int64(len(deliveryID) * 1000),
			PRKey: prKey, Owner: "o", Repo: "r", PRNumber: 7,
			BaseSHA: base, HeadSHA: head, Payload: "x",
		}})
		if err != nil {
			t.Fatalf("admit: %v", err)
		}
		return res
	}
	if res := admit("edit-bad-head", "zzz", editTestBase); res.Status != AdmitCapacityFull {
		t.Fatalf("malformed head SHA must fail admission closed, got %v", res.Status)
	}
	if res := admit("edit-empty-base", editTestHead, ""); res.Status != AdmitCapacityFull {
		t.Fatalf("empty base SHA must fail admission closed, got %v", res.Status)
	}

	// EditHistory caps at 5, oldest dropped.
	var hist []string
	for i := 1; i <= 7; i++ {
		hist = appendEditHistory(hist, fmt.Sprintf("fix %d", i))
	}
	if len(hist) != MaxEditHistoryEntries {
		t.Fatalf("history len = %d, want %d", len(hist), MaxEditHistoryEntries)
	}
	if hist[0] != "fix 3" || hist[4] != "fix 7" {
		t.Fatalf("history = %v, want fixes 3..7", hist)
	}

	// Interrupted edit jobs park as needs_attention (unproven remote writes).
	job := h.admitEdit(t, 7, editTestHead, editTestBase, "alice", "crash me", 130)
	claimed, err := h.store.ClaimNextJob(ctx, map[string]bool{})
	if err != nil || claimed == nil {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	recovered, err := h.store.RecoverJobs(ctx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	found := false
	for _, j := range recovered {
		if j.ID == job.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("interrupted edit missing from recovered set")
	}
	after, _ := h.store.GetJob(ctx, job.ID)
	if after.Status != "needs_attention" {
		t.Fatalf("interrupted edit status = %q, want needs_attention", after.Status)
	}
}

func TestEditJobDispatchViaExecuteJob(t *testing.T) {
	h := newEditHarness(t)
	job := h.admitEdit(t, 7, editTestHead, editTestBase, "alice", "dispatch me", 140)
	// e.server.gh is nil in the harness, so the inherited job-start reauth is
	// skipped and the edit dispatch path runs with injected seams.
	if err := h.exec.ExecuteJob(context.Background(), job); err != nil {
		t.Fatalf("ExecuteJob: %v", err)
	}
	after, _ := h.store.GetJob(context.Background(), job.ID)
	if after.Status != "completed" {
		t.Fatalf("status = %q, want completed", after.Status)
	}
	if h.gh.commitCalls != 1 {
		t.Fatalf("commitCalls = %d, want 1 via dispatch", h.gh.commitCalls)
	}
}
