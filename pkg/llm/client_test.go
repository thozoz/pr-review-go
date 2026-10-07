package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*fakeWaiter
}

type fakeWaiter struct {
	target time.Time
	ch     chan time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (fc *fakeClock) Now() time.Time {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.now
}

func (fc *fakeClock) After(d time.Duration) <-chan time.Time {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- fc.now
		return ch
	}
	target := fc.now.Add(d)
	fc.waiters = append(fc.waiters, &fakeWaiter{target: target, ch: ch})
	return ch
}

func (fc *fakeClock) Advance(d time.Duration) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.now = fc.now.Add(d)
	remaining := fc.waiters[:0]
	for _, w := range fc.waiters {
		if !w.target.After(fc.now) {
			w.ch <- fc.now
		} else {
			remaining = append(remaining, w)
		}
	}
	fc.waiters = remaining
}

func (fc *fakeClock) WaiterCount() int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return len(fc.waiters)
}

func TestLLMAdmission(t *testing.T) {
	// 1. Validation tests: negative/zero/overflow intervals and concurrency reject
	invalidCases := []struct {
		name        string
		concurrency int
		interval    time.Duration
		maxBytes    int64
	}{
		{"zero concurrency", 0, 1 * time.Second, 1048576},
		{"negative concurrency", -1, 1 * time.Second, 1048576},
		{"overflow concurrency", 17, 1 * time.Second, 1048576},
		{"zero interval", 2, 0, 1048576},
		{"negative interval", 2, -1 * time.Second, 1048576},
		{"too small interval", 2, 100 * time.Microsecond, 1048576},
		{"overflow interval", 2, 2 * time.Minute, 1048576},
		{"zero max bytes", 2, 1 * time.Second, 0},
		{"underflow max bytes", 2, 1 * time.Second, 65535},
		{"overflow max bytes", 2, 1 * time.Second, 16777217},
	}
	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewRequestGate(tc.concurrency, tc.interval, tc.maxBytes, nil)
			if err == nil {
				t.Fatalf("expected error for case %s, got nil", tc.name)
			}
		})
	}

	// 2. Exact limit and limit-plus-one waiting test
	startTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := newFakeClock(startTime)
	gate, err := NewRequestGate(2, 1*time.Second, 1048576, clk)
	if err != nil {
		t.Fatalf("failed to create gate: %v", err)
	}

	var inFlight int64
	var totalReceived int64
	unblockCh := make(chan struct{})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&totalReceived, 1)
		atomic.AddInt64(&inFlight, 1)
		defer atomic.AddInt64(&inFlight, -1)

		<-unblockCh

		resp := ChatResponse{
			Choices: []ChatChoice{
				{Message: ChatMessage{Role: "assistant", Content: "OK"}},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "test-key", "gpt-4o")

	// Helper to run ChatCompletion with gate
	runReq := func(ctx context.Context) (string, error) {
		ctx = WithAdmission(ctx, gate)
		return client.ChatCompletion(ctx, "system", "user")
	}

	// Start Request 1: should wait for startup interval
	errCh1 := make(chan error, 1)
	go func() {
		_, err := runReq(context.Background())
		errCh1 <- err
	}()

	// Wait until Request 1 is waiting on the clock
	for i := 0; i < 50; i++ {
		if clk.WaiterCount() > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if atomic.LoadInt64(&totalReceived) != 0 {
		t.Fatalf("request 1 started before startup interval elapsed")
	}

	// Advance clock by 1s (startup interval satisfied)
	clk.Advance(1 * time.Second)

	// Wait for Request 1 to hit HTTP server
	for i := 0; i < 50; i++ {
		if atomic.LoadInt64(&inFlight) == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if atomic.LoadInt64(&inFlight) != 1 {
		t.Fatalf("expected 1 in-flight request, got %d", atomic.LoadInt64(&inFlight))
	}

	// Start Request 2: should wait for minInterval (1s) from Request 1 start
	errCh2 := make(chan error, 1)
	go func() {
		_, err := runReq(context.Background())
		errCh2 <- err
	}()

	for i := 0; i < 50; i++ {
		if clk.WaiterCount() > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if atomic.LoadInt64(&inFlight) != 1 {
		t.Fatalf("request 2 should wait for min interval before HTTP start")
	}

	// Advance clock by 1s: Request 2 hits HTTP server (now 2 in flight = concurrency limit)
	clk.Advance(1 * time.Second)
	for i := 0; i < 50; i++ {
		if atomic.LoadInt64(&inFlight) == 2 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if atomic.LoadInt64(&inFlight) != 2 {
		t.Fatalf("expected 2 in-flight requests, got %d", atomic.LoadInt64(&inFlight))
	}

	// Start Request 3 (limit-plus-one): concurrency limit 2 is full, so Request 3 blocks on permit
	ctx3, cancel3 := context.WithCancel(context.Background())
	errCh3 := make(chan error, 1)
	go func() {
		_, err := runReq(ctx3)
		errCh3 <- err
	}()

	time.Sleep(10 * time.Millisecond)
	if atomic.LoadInt64(&totalReceived) != 2 {
		t.Fatalf("request 3 must not start HTTP while concurrency limit is occupied")
	}

	// Test cancellation while waiting for permit: cancels without starting HTTP and leaks no permits
	cancel3()
	select {
	case err := <-errCh3:
		if err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("expected context canceled error, got: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("timed out waiting for request 3 cancellation")
	}

	if atomic.LoadInt64(&totalReceived) != 2 {
		t.Fatalf("canceled request 3 must not have sent HTTP request")
	}

	// Unblock in-flight requests 1 & 2
	close(unblockCh)

	select {
	case err := <-errCh1:
		if err != nil {
			t.Fatalf("request 1 failed: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("request 1 timed out")
	}

	select {
	case err := <-errCh2:
		if err != nil {
			t.Fatalf("request 2 failed: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("request 2 timed out")
	}

	// After release, advance clock so minInterval has elapsed, then request 4 acquires permit
	clk.Advance(1 * time.Second)
	resp4, err4 := runReq(context.Background())
	if err4 != nil {
		t.Fatalf("request 4 failed after permits released: %v", err4)
	}
	if resp4 != "OK" {
		t.Fatalf("expected 'OK', got %q", resp4)
	}
}

func TestLLMResponseLimit(t *testing.T) {
	// 1. Success within response size limit
	smallResp := ChatResponse{
		Choices: []ChatChoice{
			{Message: ChatMessage{Role: "assistant", Content: "Hello within limit"}},
		},
	}
	smallBytes, _ := json.Marshal(smallResp)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(smallBytes)
	}))
	defer ts.Close()

	gate, err := NewRequestGate(2, 1*time.Millisecond, 100000, nil)
	if err != nil {
		t.Fatalf("gate creation failed: %v", err)
	}

	client := NewClient(ts.URL, "key", "model")
	ctx := WithAdmission(context.Background(), gate)
	res, err := client.ChatCompletion(ctx, "sys", "usr")
	if err != nil {
		t.Fatalf("expected success within limit, got: %v", err)
	}
	if res != "Hello within limit" {
		t.Fatalf("unexpected content: %s", res)
	}

	// 2. Oversize successful response triggers explicit error and never parses truncated data
	oversizeResp := ChatResponse{
		Choices: []ChatChoice{
			{Message: ChatMessage{Role: "assistant", Content: strings.Repeat("A", 70000)}},
		},
	}
	oversizeBytes, _ := json.Marshal(oversizeResp)

	limitGate, _ := NewRequestGate(1, 1*time.Millisecond, 65536, nil) // min allowed is 65536

	tsOversize := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(oversizeBytes)
	}))
	defer tsOversize.Close()

	clientOversize := NewClient(tsOversize.URL, "key", "model")
	ctxOversize := WithAdmission(context.Background(), limitGate)
	_, err = clientOversize.ChatCompletion(ctxOversize, "sys", "usr")
	if err == nil {
		t.Fatalf("expected oversize error, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("expected 'exceeds limit' error, got: %v", err)
	}

	// Verify permit was released on oversize error: next request advances
	tsNext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(smallResp)
	}))
	defer tsNext.Close()

	clientNext := NewClient(tsNext.URL, "key", "model")
	resNext, errNext := clientNext.ChatCompletion(ctxOversize, "sys", "usr")
	if errNext != nil {
		t.Fatalf("expected next request to advance after oversize error, got: %v", errNext)
	}
	if resNext != "Hello within limit" {
		t.Fatalf("unexpected next response: %s", resNext)
	}

	// 3. Oversize error body (status 500) triggers explicit oversize error
	tsOversizeErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(strings.Repeat("E", 70000)))
	}))
	defer tsOversizeErr.Close()

	clientOversizeErr := NewClient(tsOversizeErr.URL, "key", "model")
	_, err = clientOversizeErr.ChatCompletion(ctxOversize, "sys", "usr")
	if err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("expected oversize error on error response body, got: %v", err)
	}
}
