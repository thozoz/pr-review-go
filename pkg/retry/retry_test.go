package retry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-github/v68/github"
)

// FakeClock implements Clock for deterministic testing without sleeping.
type FakeClock struct {
	mu          sync.Mutex
	now         time.Time
	sleeps      []time.Duration
	onSleepHook func(d time.Duration)
}

func NewFakeClock(t0 time.Time) *FakeClock {
	return &FakeClock{now: t0}
}

func (fc *FakeClock) Now() time.Time {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.now
}

func (fc *FakeClock) Sleep(ctx context.Context, d time.Duration) error {
	fc.mu.Lock()
	fc.sleeps = append(fc.sleeps, d)
	fc.now = fc.now.Add(d)
	hook := fc.onSleepHook
	fc.mu.Unlock()

	if hook != nil {
		hook(d)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func (fc *FakeClock) Sleeps() []time.Duration {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	cp := make([]time.Duration, len(fc.sleeps))
	copy(cp, fc.sleeps)
	return cp
}

func TestClassify_HTTPStatuses(t *testing.T) {
	tests := []struct {
		name     string
		input    ClassifyInput
		expected Classification
	}{
		{
			name:     "401 Unauthorized is permanent",
			input:    ClassifyInput{StatusCode: 401, Kind: OpKindRead},
			expected: ClassPermanent,
		},
		{
			name:     "422 Unprocessable is permanent",
			input:    ClassifyInput{StatusCode: 422, Kind: OpKindRead},
			expected: ClassPermanent,
		},
		{
			name:     "404 Not Found is permanent",
			input:    ClassifyInput{StatusCode: 404, Kind: OpKindRead},
			expected: ClassPermanent,
		},
		{
			name:     "501 Not Implemented is permanent",
			input:    ClassifyInput{StatusCode: 501, Kind: OpKindRead},
			expected: ClassPermanent,
		},
		{
			name:     "503 Service Unavailable on read is transient",
			input:    ClassifyInput{StatusCode: 503, Kind: OpKindRead},
			expected: ClassTransient,
		},
		{
			name:     "500 Internal Error on read is transient",
			input:    ClassifyInput{StatusCode: 500, Kind: OpKindRead},
			expected: ClassTransient,
		},
		{
			name:     "500 Internal Error on write is uncertain-write",
			input:    ClassifyInput{StatusCode: 500, Kind: OpKindWrite},
			expected: ClassUncertainWrite,
		},
		{
			name:     "502 Bad Gateway on write is uncertain-write",
			input:    ClassifyInput{StatusCode: 502, Kind: OpKindWrite},
			expected: ClassUncertainWrite,
		},
		{
			name:     "429 Too Many Requests is transient",
			input:    ClassifyInput{StatusCode: 429, Kind: OpKindRead},
			expected: ClassTransient,
		},
		{
			name: "429 with insufficient_quota is permanent",
			input: ClassifyInput{
				StatusCode: 429,
				Body:       `{"error": {"message": "You exceeded your current quota, please check your plan and billing details.", "type": "insufficient_quota"}}`,
				Kind:       OpKindRead,
			},
			expected: ClassPermanent,
		},
		{
			name: "403 with secondary rate limit is transient",
			input: ClassifyInput{
				StatusCode: 403,
				Body:       "You have exceeded a secondary rate limit. Please wait a few minutes before you try again.",
				Kind:       OpKindRead,
			},
			expected: ClassTransient,
		},
		{
			name: "403 with Retry-After header is transient",
			input: ClassifyInput{
				StatusCode: 403,
				Headers:    http.Header{"Retry-After": []string{"60"}},
				Kind:       OpKindRead,
			},
			expected: ClassTransient,
		},
		{
			name: "403 standard permission denied is permanent",
			input: ClassifyInput{
				StatusCode: 403,
				Body:       "Resource not accessible by integration",
				Kind:       OpKindRead,
			},
			expected: ClassPermanent,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.input)
			if got != tc.expected {
				t.Fatalf("expected classification %q, got %q", tc.expected, got)
			}
		})
	}
}

func TestClassify_NetworkErrorsAndUncertainWrites(t *testing.T) {
	deadlineErr := context.DeadlineExceeded

	if got := Classify(ClassifyInput{Err: deadlineErr, Kind: OpKindWrite}); got != ClassUncertainWrite {
		t.Fatalf("expected write timeout to be uncertain-write, got %v", got)
	}

	if got := Classify(ClassifyInput{Err: deadlineErr, Kind: OpKindRead}); got != ClassTransient {
		t.Fatalf("expected read timeout to be transient, got %v", got)
	}
}

func TestDo_429ThenSuccessRecovers(t *testing.T) {
	clock := NewFakeClock(time.Now())
	policy := Policy{
		MaxAttempts: 5,
		BaseDelay:   100 * time.Millisecond,
		MaxDelay:    1 * time.Second,
		MaxWait:     10 * time.Second,
		Clock:       clock,
		JitterFn:    func(d time.Duration) time.Duration { return d },
	}

	var calls int
	val, err := Do(context.Background(), policy, OpKindRead, func(ctx context.Context) (string, error) {
		calls++
		if calls < 3 {
			return "", fmt.Errorf("api error (status 429): rate limit exceeded")
		}
		return "recovered", nil
	})

	if err != nil {
		t.Fatalf("expected success after retries, got err: %v", err)
	}
	if val != "recovered" {
		t.Fatalf("expected 'recovered', got %q", val)
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls, got %d", calls)
	}
	if len(clock.Sleeps()) != 2 {
		t.Fatalf("expected 2 sleeps, got %d", len(clock.Sleeps()))
	}
}

func TestDo_401NeverRetried(t *testing.T) {
	clock := NewFakeClock(time.Now())
	policy := Policy{
		MaxAttempts: 5,
		Clock:       clock,
	}

	var calls int
	_, err := Do(context.Background(), policy, OpKindRead, func(ctx context.Context) (string, error) {
		calls++
		return "", &github.ErrorResponse{
			Response: &http.Response{StatusCode: 401},
			Message:  "Bad credentials",
		}
	})

	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 call for 401, got %d", calls)
	}
	if len(clock.Sleeps()) != 0 {
		t.Fatalf("expected 0 sleeps, got %d", len(clock.Sleeps()))
	}
}

func TestDo_TimedOutWriteReturnsUncertainWrite_SingleAttempt(t *testing.T) {
	clock := NewFakeClock(time.Now())
	policy := Policy{
		MaxAttempts: 5,
		Clock:       clock,
	}

	var calls int
	_, err := Do(context.Background(), policy, OpKindWrite, func(ctx context.Context) (string, error) {
		calls++
		return "", context.DeadlineExceeded
	})

	if err == nil {
		t.Fatalf("expected error for write timeout, got nil")
	}
	if !errors.Is(err, ErrUncertainWrite) {
		t.Fatalf("expected ErrUncertainWrite, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 attempt on uncertain write, got %d", calls)
	}
}

func TestBackoff_CeilingsAndExceedsWorkerBudget(t *testing.T) {
	clock := NewFakeClock(time.Now())
	policy := Policy{
		MaxAttempts: 5,
		BaseDelay:   1 * time.Second,
		MaxDelay:    30 * time.Second,
		MaxWait:     60 * time.Second,
		Clock:       clock,
		JitterFn:    func(d time.Duration) time.Duration { return d },
	}

	// 1. Verify exponential backoff sequence capped at MaxDelay
	for attempt := 1; attempt <= 6; attempt++ {
		d, err := CalculateDelay(attempt, policy, nil)
		if err != nil {
			t.Fatalf("unexpected error at attempt %d: %v", attempt, err)
		}
		expected := time.Duration(1<<(attempt-1)) * time.Second
		if expected > 30*time.Second {
			expected = 30 * time.Second
		}
		if d != expected {
			t.Errorf("attempt %d: expected delay %v, got %v", attempt, expected, d)
		}
	}

	// 2. Retry-After exceeding MaxWait returns ExceedsWorkerBudget
	headers := http.Header{"Retry-After": []string{"120"}} // 120s > MaxWait 60s
	_, err := CalculateDelay(1, policy, headers)
	if err == nil {
		t.Fatalf("expected ExceedsWorkerBudget error, got nil")
	}
	var budgetErr *ExceedsWorkerBudgetError
	if !errors.As(err, &budgetErr) {
		t.Fatalf("expected *ExceedsWorkerBudgetError, got %T: %v", err, err)
	}
	if budgetErr.RequiredWait != 120*time.Second {
		t.Errorf("expected RequiredWait 120s, got %v", budgetErr.RequiredWait)
	}
	if budgetErr.MaxWait != 60*time.Second {
		t.Errorf("expected MaxWait 60s, got %v", budgetErr.MaxWait)
	}
}

func TestCancel_ImmediateContextStop(t *testing.T) {
	clock := NewFakeClock(time.Now())
	policy := Policy{
		MaxAttempts: 5,
		BaseDelay:   1 * time.Second,
		Clock:       clock,
	}

	ctx, cancel := context.WithCancel(context.Background())

	var calls int
	clock.onSleepHook = func(d time.Duration) {
		// Cancel context during first sleep
		cancel()
	}

	_, err := Do(ctx, policy, OpKindRead, func(ctx context.Context) (string, error) {
		calls++
		return "", fmt.Errorf("api error (status 503): service unavailable")
	})

	if err == nil {
		t.Fatalf("expected error on cancelled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 call before cancel took effect, got %d", calls)
	}
}

func TestBudget_SharingAcrossOperations(t *testing.T) {
	clock := NewFakeClock(time.Now())
	sharedBudget := NewBudget(3*time.Second, clock)

	policy := Policy{
		MaxAttempts: 5,
		BaseDelay:   1 * time.Second,
		MaxDelay:    5 * time.Second,
		MaxWait:     10 * time.Second,
		Clock:       clock,
		JitterFn:    func(d time.Duration) time.Duration { return d },
	}

	ctx := WithBudget(context.Background(), sharedBudget)

	// First operation consumes 1s + 2s = 3s budget over 2 retries
	var op1Calls int
	_, err1 := Do(ctx, policy, OpKindRead, func(ctx context.Context) (string, error) {
		op1Calls++
		if op1Calls < 3 {
			return "", fmt.Errorf("api error (status 429): rate limit")
		}
		return "op1-done", nil
	})

	if err1 != nil {
		t.Fatalf("expected op1 to succeed, got %v", err1)
	}
	if sharedBudget.Remaining() != 0 {
		t.Fatalf("expected shared budget to be 0, got %v", sharedBudget.Remaining())
	}

	// Second operation must immediately short-circuit due to exhausted budget
	var op2Calls int
	_, err2 := Do(ctx, policy, OpKindRead, func(ctx context.Context) (string, error) {
		op2Calls++
		return "", fmt.Errorf("api error (status 429): rate limit")
	})

	if !errors.Is(err2, ErrJobBudgetExhausted) {
		t.Fatalf("expected ErrJobBudgetExhausted for op2, got %v", err2)
	}
}

func TestJitter_DistributionBounds(t *testing.T) {
	policy := Policy{
		BaseDelay: 2 * time.Second,
		MaxDelay:  30 * time.Second,
		MaxWait:   60 * time.Second,
	}

	// Over 100 samples, delay with default jitter must stay within [base, base + 25%]
	for i := 0; i < 100; i++ {
		d, err := CalculateDelay(1, policy, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if d < 2*time.Second {
			t.Errorf("delay %v below base delay 2s", d)
		}
		if d > 2500*time.Millisecond {
			t.Errorf("delay %v exceeds base + 25%% (2.5s)", d)
		}
	}
}

func TestHTTPIntegration_ServerSimulation(t *testing.T) {
	var requestCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&requestCount, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error": "rate limit exceeded"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"result": "ok"}`))
	}))
	defer server.Close()

	clock := NewFakeClock(time.Now())
	policy := Policy{
		MaxAttempts: 3,
		BaseDelay:   100 * time.Millisecond,
		MaxDelay:    2 * time.Second,
		MaxWait:     5 * time.Second,
		Clock:       clock,
	}

	client := server.Client()
	val, err := Do(context.Background(), policy, OpKindRead, func(ctx context.Context) (string, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", server.URL, nil)
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			return "", fmt.Errorf("status %d", resp.StatusCode)
		}
		return "success", nil
	})

	if err != nil {
		t.Fatalf("expected success on retry, got %v", err)
	}
	if val != "success" {
		t.Fatalf("expected 'success', got %q", val)
	}
	if atomic.LoadInt32(&requestCount) != 2 {
		t.Fatalf("expected 2 server requests, got %d", atomic.LoadInt32(&requestCount))
	}
}
