package sandbox

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type mockSourceProvider struct {
	mu         sync.Mutex
	inFlight   int64
	maxActive  int64
	prepCalls  int64
	cleanCalls int64
	unblockCh  chan struct{}
	shouldFail bool
}

func (m *mockSourceProvider) PrepareSource(ctx context.Context, cloneURL, headRef, headSHA string) (*Snapshot, func(), error) {
	atomic.AddInt64(&m.prepCalls, 1)
	if m.shouldFail {
		return nil, nil, errors.New("simulated source preparation failure")
	}

	atomic.AddInt64(&m.inFlight, 1)
	m.mu.Lock()
	if m.inFlight > m.maxActive {
		m.maxActive = m.inFlight
	}
	m.mu.Unlock()

	if m.unblockCh != nil {
		select {
		case <-m.unblockCh:
		case <-ctx.Done():
			atomic.AddInt64(&m.inFlight, -1)
			return nil, nil, ctx.Err()
		}
	}

	snap := &Snapshot{
		CommitSHA: headSHA,
		SourceDir: "/tmp/mock-source",
	}

	cleanup := func() {
		atomic.AddInt64(&m.cleanCalls, 1)
		atomic.AddInt64(&m.inFlight, -1)
	}

	return snap, cleanup, nil
}

func TestSnapshotAdmission(t *testing.T) {
	// 1. Validation bounds
	invalidCases := []int{0, -1, 17, 100}
	for _, c := range invalidCases {
		_, err := NewAdmissionGate(c)
		if err == nil {
			t.Fatalf("expected error for concurrency %d, got nil", c)
		}
	}

	gate, err := NewAdmissionGate(1)
	if err != nil {
		t.Fatalf("failed creating admission gate: %v", err)
	}
	if gate.Concurrency() != 1 {
		t.Fatalf("expected concurrency 1, got %d", gate.Concurrency())
	}

	// 2. Limit and limit-plus-one waiting
	mockSP := &mockSourceProvider{}
	runner := NewRunner(1 * time.Minute)
	runner.SetSourceProvider(mockSP)

	ctx := WithAdmission(context.Background(), gate)
	headSHA := "1111111111111111111111111111111111111111"

	// Job 1 acquires snapshot permit
	snap1, cleanup1, err := runner.PrepareSnapshot(ctx, "https://github.com/org/repo", "main", headSHA)
	if err != nil {
		t.Fatalf("job 1 PrepareSnapshot failed: %v", err)
	}
	if snap1 == nil || !snap1.hasPermit {
		t.Fatalf("job 1 snapshot expected to have permit flag set")
	}

	// Job 2 attempts PrepareSnapshot with same gate (concurrency 1 is occupied) -> waits
	ctx2, cancel2 := context.WithCancel(ctx)
	job2Started := make(chan struct{})
	job2Err := make(chan error, 1)

	go func() {
		close(job2Started)
		_, _, err2 := runner.PrepareSnapshot(ctx2, "https://github.com/org/repo", "main", headSHA)
		job2Err <- err2
	}()

	<-job2Started
	time.Sleep(20 * time.Millisecond)

	// Verify Job 2 has not started source preparation because gate permit is occupied
	if atomic.LoadInt64(&mockSP.prepCalls) != 1 {
		t.Fatalf("expected exactly 1 PrepareSource call so far, got %d", atomic.LoadInt64(&mockSP.prepCalls))
	}

	// Cancel Job 2 while waiting: must unblock without backend work or permit leak
	cancel2()
	select {
	case err2 := <-job2Err:
		if err2 == nil || !errors.Is(err2, context.Canceled) {
			t.Fatalf("expected context.Canceled error for job 2, got: %v", err2)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("timed out waiting for job 2 cancellation")
	}

	if atomic.LoadInt64(&mockSP.prepCalls) != 1 {
		t.Fatalf("cancelled job 2 must not have called PrepareSource")
	}

	// 3. Cleanup releases permit exactly once, and is idempotent
	cleanup1()
	// Call cleanup1 again to verify idempotency (must not double release)
	cleanup1()

	if atomic.LoadInt64(&mockSP.cleanCalls) != 1 {
		t.Fatalf("underlying cleanup expected to be called once, got %d", atomic.LoadInt64(&mockSP.cleanCalls))
	}

	// Job 3 can now acquire the released permit
	_, cleanup3, err := runner.PrepareSnapshot(ctx, "https://github.com/org/repo", "main", headSHA)
	if err != nil {
		t.Fatalf("job 3 failed to acquire released permit: %v", err)
	}
	cleanup3()

	if atomic.LoadInt64(&mockSP.prepCalls) != 2 {
		t.Fatalf("expected 2 total successful PrepareSource calls, got %d", atomic.LoadInt64(&mockSP.prepCalls))
	}

	// 4. Source error releases permit immediately
	mockSP.shouldFail = true
	_, _, errFail := runner.PrepareSnapshot(ctx, "https://github.com/org/repo", "main", headSHA)
	if errFail == nil {
		t.Fatalf("expected failure, got nil")
	}
	mockSP.shouldFail = false

	// Next caller can acquire permit immediately because error path released it
	_, cleanup4, err4 := runner.PrepareSnapshot(ctx, "https://github.com/org/repo", "main", headSHA)
	if err4 != nil {
		t.Fatalf("expected permit to be available after source error: %v", err4)
	}
	cleanup4()

	// 5. RunSnapshot idempotent check: does not reacquire already held permit
	snap5, cleanup5, _ := runner.PrepareSnapshot(ctx, "https://github.com/org/repo", "main", headSHA)
	defer cleanup5()

	// If RunSnapshot tried to acquire again, it would deadlock since gate concurrency is 1.
	// Because snap5.hasPermit is true, it does not reacquire.
	report, runErr := runner.RunSnapshot(ctx, snap5)
	if runErr != nil {
		t.Fatalf("RunSnapshot failed: %v", runErr)
	}
	if report == nil {
		t.Fatal("expected non-nil report")
	}
}
