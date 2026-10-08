package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thozoz/pr-review-go/pkg/retry"
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

type fixtureTransport struct {
	allowedHost string
	rt          http.RoundTripper
}

func (f *fixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != f.allowedHost {
		return nil, fmt.Errorf("fixture transport rejected non-fixture URL host: %s", req.URL.Host)
	}
	base := f.rt
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

func newFixtureClient(serverURL string) *http.Client {
	u, err := url.Parse(serverURL)
	if err != nil {
		panic(err)
	}
	return &http.Client{
		Transport: &fixtureTransport{
			allowedHost: u.Host,
		},
		Timeout: 5 * time.Second,
	}
}

func TestChatCompletion_ProtocolAndSuccess(t *testing.T) {
	var (
		receivedMethod      string
		receivedPath        string
		receivedContentType string
		receivedAuth        string
		receivedBody        ChatRequest
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		receivedPath = r.URL.Path
		receivedContentType = r.Header.Get("Content-Type")
		receivedAuth = r.Header.Get("Authorization")

		_ = json.NewDecoder(r.Body).Decode(&receivedBody)

		resp := ChatResponse{
			Choices: []ChatChoice{
				{
					Message: ChatMessage{
						Role:    "assistant",
						Content: "  Verified output response  ",
					},
					FinishReason: "stop",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "secret-key-42", "custom-model")
	client.SetHTTPClient(newFixtureClient(ts.URL))

	res, err := client.ChatCompletion(context.Background(), "System prompt here", "User prompt here")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if receivedMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", receivedMethod)
	}
	if receivedPath != "/chat/completions" {
		t.Errorf("expected /chat/completions, got %s", receivedPath)
	}
	if receivedContentType != "application/json" {
		t.Errorf("expected application/json, got %s", receivedContentType)
	}
	if receivedAuth != "Bearer secret-key-42" {
		t.Errorf("expected Bearer secret-key-42, got %s", receivedAuth)
	}
	if receivedBody.Model != "custom-model" {
		t.Errorf("expected custom-model, got %s", receivedBody.Model)
	}
	if len(receivedBody.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(receivedBody.Messages))
	}
	if receivedBody.Messages[0].Role != "system" || receivedBody.Messages[0].Content != "System prompt here" {
		t.Errorf("unexpected system message: %+v", receivedBody.Messages[0])
	}
	if receivedBody.Messages[1].Role != "user" || receivedBody.Messages[1].Content != "User prompt here" {
		t.Errorf("unexpected user message: %+v", receivedBody.Messages[1])
	}
	if res != "  Verified output response  " {
		t.Errorf("unexpected result: %q", res)
	}
}

func TestChatCompletion_InputValidation(t *testing.T) {
	cases := []struct {
		name         string
		baseURL      string
		apiKey       string
		model        string
		systemPrompt string
		userPrompt   string
		errSubstring string
	}{
		{
			name:         "missing base URL",
			baseURL:      "",
			apiKey:       "key",
			model:        "model",
			systemPrompt: "sys",
			userPrompt:   "usr",
			errSubstring: "base URL is not configured",
		},
		{
			name:         "missing api key",
			baseURL:      "http://localhost",
			apiKey:       "",
			model:        "model",
			systemPrompt: "sys",
			userPrompt:   "usr",
			errSubstring: "API key is not configured",
		},
		{
			name:         "missing model",
			baseURL:      "http://localhost",
			apiKey:       "key",
			model:        "",
			systemPrompt: "sys",
			userPrompt:   "usr",
			errSubstring: "model is not configured",
		},
		{
			name:         "empty system prompt",
			baseURL:      "http://localhost",
			apiKey:       "key",
			model:        "model",
			systemPrompt: "",
			userPrompt:   "usr",
			errSubstring: "system prompt cannot be empty",
		},
		{
			name:         "whitespace-only system prompt",
			baseURL:      "http://localhost",
			apiKey:       "key",
			model:        "model",
			systemPrompt: "  \t \n ",
			userPrompt:   "usr",
			errSubstring: "system prompt cannot be empty",
		},
		{
			name:         "empty user prompt",
			baseURL:      "http://localhost",
			apiKey:       "key",
			model:        "model",
			systemPrompt: "sys",
			userPrompt:   "",
			errSubstring: "user prompt cannot be empty",
		},
		{
			name:         "whitespace-only user prompt",
			baseURL:      "http://localhost",
			apiKey:       "key",
			model:        "model",
			systemPrompt: "sys",
			userPrompt:   " \r\n  ",
			errSubstring: "user prompt cannot be empty",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := NewClient(tc.baseURL, tc.apiKey, tc.model)
			_, err := client.ChatCompletion(context.Background(), tc.systemPrompt, tc.userPrompt)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.errSubstring)
			}
			if !strings.Contains(err.Error(), tc.errSubstring) {
				t.Fatalf("expected error containing %q, got %v", tc.errSubstring, err)
			}
		})
	}
}

func TestChatCompletion_MalformedURL(t *testing.T) {
	client := NewClient("http://[::1]:namedport", "key", "model")
	_, err := client.ChatCompletion(context.Background(), "sys", "usr")
	if err == nil {
		t.Fatal("expected error on malformed URL, got nil")
	}
	if !strings.Contains(err.Error(), "failed to create http request") {
		t.Fatalf("expected 'failed to create http request', got: %v", err)
	}
}

func TestChatCompletion_TransportFailure(t *testing.T) {
	// Point to a closed port / unrouted local address
	client := NewClient("http://127.0.0.1:59998", "key", "model")
	_, err := client.ChatCompletion(context.Background(), "sys", "usr")
	if err == nil {
		t.Fatal("expected transport failure error, got nil")
	}
	if !strings.Contains(err.Error(), "http request failed") {
		t.Fatalf("expected 'http request failed', got: %v", err)
	}
}

func TestChatCompletion_CancelledContext(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "key", "model")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before request

	_, err := client.ChatCompletion(ctx, "sys", "usr")
	if err == nil {
		t.Fatal("expected context error, got nil")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("expected 'context canceled', got: %v", err)
	}
}

func TestChatCompletion_ReadFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hijack connection to abruptly close it mid-body
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijacking not supported", http.StatusInternalServerError)
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 500\r\n\r\nPartial"))
		_ = conn.Close()
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "key", "model")
	_, err := client.ChatCompletion(context.Background(), "sys", "usr")
	if err == nil {
		t.Fatal("expected read failure error, got nil")
	}
	if !strings.Contains(err.Error(), "failed to read response") {
		t.Fatalf("expected 'failed to read response', got: %v", err)
	}
}

func TestChatCompletion_HTTPStatusesAndRetries(t *testing.T) {
	tests := []struct {
		code          int
		expectedCalls int32
	}{
		{code: http.StatusUnauthorized, expectedCalls: 1}, // 401 permanent: no retry
		{code: http.StatusTooManyRequests, expectedCalls: 5}, // 429 transient: retries up to 5 attempts
		{code: http.StatusInternalServerError, expectedCalls: 5}, // 500 transient: retries up to 5 attempts
	}

	for _, tc := range tests {
		t.Run(fmt.Sprintf("status_%d", tc.code), func(t *testing.T) {
			var requestCount int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&requestCount, 1)
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(fmt.Sprintf("simulated error %d", tc.code)))
			}))
			defer ts.Close()

			clock := retry.NewBudget(time.Hour, nil) // dummy
			_ = clock
			client := NewClient(ts.URL, "key", "model")
			client.SetHTTPClient(newFixtureClient(ts.URL))
			// Speed up test timing
			client.SetRetryPolicy(retry.Policy{
				MaxAttempts: 5,
				BaseDelay:   time.Millisecond,
				MaxDelay:    5 * time.Millisecond,
				MaxWait:     time.Second,
			})

			_, err := client.ChatCompletion(context.Background(), "sys", "usr")
			if err == nil {
				t.Fatalf("expected error for status %d, got nil", tc.code)
			}
			expectedSubstr := fmt.Sprintf("api error (status %d)", tc.code)
			if !strings.Contains(err.Error(), expectedSubstr) {
				t.Fatalf("expected error to contain %q, got: %v", expectedSubstr, err)
			}

			finalCount := atomic.LoadInt32(&requestCount)
			if finalCount != tc.expectedCalls {
				t.Fatalf("expected %d requests for status %d, got %d", tc.expectedCalls, tc.code, finalCount)
			}
		})
	}
}

func TestChatCompletion_RetryInsideSingleGatePermit(t *testing.T) {
	var requestCount int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cnt := atomic.AddInt32(&requestCount, 1)
		if cnt < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("rate limit"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ChatResponse{
			Choices: []ChatChoice{
				{Message: ChatMessage{Role: "assistant", Content: "recovered under gate"}},
			},
		})
	}))
	defer ts.Close()

	gate, err := NewRequestGate(1, time.Millisecond, 1048576, nil)
	if err != nil {
		t.Fatalf("failed to create gate: %v", err)
	}

	client := NewClient(ts.URL, "key", "model")
	client.SetHTTPClient(newFixtureClient(ts.URL))
	client.SetRetryPolicy(retry.Policy{
		MaxAttempts: 5,
		BaseDelay:   time.Millisecond,
		MaxDelay:    5 * time.Millisecond,
		MaxWait:     time.Second,
	})

	ctx := WithAdmission(context.Background(), gate)
	res, err := client.ChatCompletion(ctx, "sys", "usr")
	if err != nil {
		t.Fatalf("expected success on 3rd attempt, got: %v", err)
	}
	if res != "recovered under gate" {
		t.Fatalf("expected 'recovered under gate', got %q", res)
	}
	if atomic.LoadInt32(&requestCount) != 3 {
		t.Fatalf("expected 3 attempts, got %d", atomic.LoadInt32(&requestCount))
	}

	// Verify gate permit was released after the retry sequence completes
	release, err := gate.Acquire(context.Background())
	if err != nil {
		t.Fatalf("expected gate permit to be released and re-acquirable: %v", err)
	}
	release()
}

func TestChatCompletion_ErrorEnvelope(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := ChatResponse{
			Error: &struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			}{
				Message: "rate limit quota exhausted",
				Type:    "insufficient_quota",
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "key", "model")
	_, err := client.ChatCompletion(context.Background(), "sys", "usr")
	if err == nil {
		t.Fatal("expected error envelope error, got nil")
	}
	if !strings.Contains(err.Error(), "llm returned error: rate limit quota exhausted") {
		t.Fatalf("expected error envelope message, got: %v", err)
	}
}

func TestChatCompletion_MalformedJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices": [ { "message": { "content": `)) // malformed JSON
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "key", "model")
	_, err := client.ChatCompletion(context.Background(), "sys", "usr")
	if err == nil {
		t.Fatal("expected malformed JSON error, got nil")
	}
	if !strings.Contains(err.Error(), "failed to unmarshal chat response") {
		t.Fatalf("expected unmarshal error, got: %v", err)
	}
}

func TestChatCompletion_EmptyChoices(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := ChatResponse{
			Choices: []ChatChoice{},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	client := NewClient(ts.URL, "key", "model")
	_, err := client.ChatCompletion(context.Background(), "sys", "usr")
	if err == nil {
		t.Fatal("expected empty choices error, got nil")
	}
	if !strings.Contains(err.Error(), "no response choices returned from model") {
		t.Fatalf("expected 'no response choices returned from model', got: %v", err)
	}
}

func TestChatCompletion_BlankAssistantContent(t *testing.T) {
	blankContents := []string{
		"",
		" ",
		"\t\n  \r\n",
	}

	for i, blank := range blankContents {
		t.Run(fmt.Sprintf("blank_content_%d", i), func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				resp := ChatResponse{
					Choices: []ChatChoice{
						{
							Message: ChatMessage{
								Role:    "assistant",
								Content: blank,
							},
						},
					},
				}
				_ = json.NewEncoder(w).Encode(resp)
			}))
			defer ts.Close()

			client := NewClient(ts.URL, "key", "model")
			_, err := client.ChatCompletion(context.Background(), "sys", "usr")
			if err == nil {
				t.Fatalf("expected error on blank content %q, got nil", blank)
			}
			if !strings.Contains(err.Error(), "empty assistant response content") {
				t.Fatalf("expected 'empty assistant response content', got: %v", err)
			}
		})
	}
}

func TestChatCompletion_FixtureTransportRejectsExternal(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	fixtureClient := newFixtureClient(ts.URL)

	req, err := http.NewRequestWithContext(context.Background(), "GET", "https://api.openai.com/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = fixtureClient.Do(req)
	if err == nil {
		t.Fatal("expected fixture transport to reject non-fixture host, got nil error")
	}
	if !strings.Contains(err.Error(), "fixture transport rejected non-fixture URL host") {
		t.Fatalf("unexpected error: %v", err)
	}
}

