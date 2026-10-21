package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// chatRequestBody is the subset of the outgoing payload these tests assert on.
type chatRequestBody struct {
	Model     string        `json:"model"`
	Messages  []ChatMessage `json:"messages"`
	MaxTokens *int          `json:"max_tokens"`
}

// writeChatResponse renders one OpenAI-compatible completion body.
func writeChatResponse(w http.ResponseWriter, content any, finishReason, reasoning string) {
	message := map[string]any{"role": "assistant", "content": content}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	body := map[string]any{
		"choices": []map[string]any{{
			"message":       message,
			"finish_reason": finishReason,
		}},
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		panic(err)
	}
}

// newReasoningServer serves the given per-attempt responses in order and records
// the decoded request bodies.
func newReasoningServer(t *testing.T, attempts *int32, requests *[]chatRequestBody, handler func(attempt int32, w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequestBody
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		*requests = append(*requests, req)
		attempt := atomic.AddInt32(attempts, 1)
		handler(attempt, w)
	}))
}

func TestChatCompletionReasoningRetrySucceeds(t *testing.T) {
	var attempts int32
	var requests []chatRequestBody
	ts := newReasoningServer(t, &attempts, &requests, func(attempt int32, w http.ResponseWriter) {
		if attempt == 1 {
			writeChatResponse(w, nil, "length", "Thinking through the pull request...")
			return
		}
		writeChatResponse(w, "Final assistant answer", "stop", "Thought process")
	})
	defer ts.Close()

	client := NewClient(ts.URL, "key", "reasoning-model")
	client.SetMaxTokens(4096)

	got, err := client.ChatCompletion(context.Background(), "system", "user")
	if err != nil {
		t.Fatalf("ChatCompletion() error = %v, want nil", err)
	}
	if got != "Final assistant answer" {
		t.Fatalf("ChatCompletion() = %q, want %q", got, "Final assistant answer")
	}
	if attempts != 2 {
		t.Fatalf("provider attempts = %d, want 2 (one recovery retry)", attempts)
	}
	if requests[0].MaxTokens == nil || *requests[0].MaxTokens != 4096 {
		t.Fatalf("first request max_tokens = %v, want 4096", requests[0].MaxTokens)
	}
	if requests[1].MaxTokens == nil || *requests[1].MaxTokens != 8192 {
		t.Fatalf("retry request max_tokens = %v, want doubled 8192", requests[1].MaxTokens)
	}
}

func TestChatCompletionReasoningRetryFailsTyped(t *testing.T) {
	var attempts int32
	var requests []chatRequestBody
	ts := newReasoningServer(t, &attempts, &requests, func(_ int32, w http.ResponseWriter) {
		writeChatResponse(w, nil, "length", "Still thinking...")
	})
	defer ts.Close()

	client := NewClient(ts.URL, "key", "reasoning-model")
	client.SetMaxTokens(4096)

	_, err := client.ChatCompletion(context.Background(), "system", "user")
	if err == nil {
		t.Fatal("ChatCompletion() error = nil, want typed empty-content failure")
	}
	if !errors.Is(err, ErrEmptyAssistantContent) {
		t.Fatalf("errors.Is(err, ErrEmptyAssistantContent) = false; err = %v", err)
	}
	var emptyErr *EmptyAssistantContentError
	if !errors.As(err, &emptyErr) {
		t.Fatalf("errors.As(*EmptyAssistantContentError) = false; err = %v", err)
	}
	if emptyErr.FinishReason != "length" {
		t.Errorf("FinishReason = %q, want %q", emptyErr.FinishReason, "length")
	}
	if !emptyErr.HasReasoning {
		t.Error("HasReasoning = false, want true")
	}
	if !emptyErr.Retried {
		t.Error("Retried = false, want true")
	}
	if emptyErr.MaxTokensAttempted != 8192 {
		t.Errorf("MaxTokensAttempted = %d, want 8192", emptyErr.MaxTokensAttempted)
	}
	for _, want := range []string{"reasoning-model", "length", "8192", "LLM_MAX_TOKENS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message %q missing %q", err.Error(), want)
		}
	}
	if attempts != 2 {
		t.Fatalf("provider attempts = %d, want 2 (exactly one retry)", attempts)
	}
}

func TestChatCompletionEmptyContentWithoutReasoning(t *testing.T) {
	var attempts int32
	var requests []chatRequestBody
	ts := newReasoningServer(t, &attempts, &requests, func(_ int32, w http.ResponseWriter) {
		writeChatResponse(w, "", "stop", "")
	})
	defer ts.Close()

	client := NewClient(ts.URL, "key", "plain-model")
	client.SetMaxTokens(4096)

	_, err := client.ChatCompletion(context.Background(), "system", "user")
	var emptyErr *EmptyAssistantContentError
	if !errors.As(err, &emptyErr) {
		t.Fatalf("errors.As(*EmptyAssistantContentError) = false; err = %v", err)
	}
	if emptyErr.HasReasoning {
		t.Error("HasReasoning = true, want false")
	}
	if emptyErr.Retried {
		t.Error("Retried = true, want false for non-length finish reason")
	}
	if attempts != 1 {
		t.Fatalf("provider attempts = %d, want 1 (no needless retry)", attempts)
	}
}

func TestChatCompletionEmptyContentWithReasoningButStopDoesNotRetry(t *testing.T) {
	var attempts int32
	var requests []chatRequestBody
	ts := newReasoningServer(t, &attempts, &requests, func(_ int32, w http.ResponseWriter) {
		writeChatResponse(w, nil, "stop", "Deliberation finished, no answer")
	})
	defer ts.Close()

	client := NewClient(ts.URL, "key", "reasoning-model")

	_, err := client.ChatCompletion(context.Background(), "system", "user")
	var emptyErr *EmptyAssistantContentError
	if !errors.As(err, &emptyErr) {
		t.Fatalf("errors.As(*EmptyAssistantContentError) = false; err = %v", err)
	}
	if !emptyErr.HasReasoning {
		t.Error("HasReasoning = false, want true")
	}
	if attempts != 1 {
		t.Fatalf("provider attempts = %d, want 1 (retry requires finish_reason=length)", attempts)
	}
}

func TestChatCompletionReasoningContentNeverLeaks(t *testing.T) {
	var attempts int32
	var requests []chatRequestBody
	ts := newReasoningServer(t, &attempts, &requests, func(_ int32, w http.ResponseWriter) {
		writeChatResponse(w, "Public review comment", "stop", "Confidential thinking trace")
	})
	defer ts.Close()

	client := NewClient(ts.URL, "key", "reasoning-model")

	got, err := client.ChatCompletion(context.Background(), "system", "user")
	if err != nil {
		t.Fatalf("ChatCompletion() error = %v", err)
	}
	if got != "Public review comment" {
		t.Fatalf("ChatCompletion() = %q, want the published content only", got)
	}
	if strings.Contains(got, "Confidential thinking trace") {
		t.Fatal("reasoning_content leaked into the returned completion")
	}
}

func TestChatCompletionMaxTokensSerialization(t *testing.T) {
	var attempts int32
	var requests []chatRequestBody
	ts := newReasoningServer(t, &attempts, &requests, func(_ int32, w http.ResponseWriter) {
		writeChatResponse(w, "ok", "stop", "")
	})
	defer ts.Close()

	client := NewClient(ts.URL, "key", "plain-model")
	client.SetMaxTokens(4096)
	if _, err := client.ChatCompletion(context.Background(), "system", "user"); err != nil {
		t.Fatalf("ChatCompletion() error = %v", err)
	}
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(requests))
	}
	if requests[0].MaxTokens == nil || *requests[0].MaxTokens != 4096 {
		t.Fatalf("max_tokens = %v, want 4096 when configured", requests[0].MaxTokens)
	}

	// Unset budget must omit the field entirely so provider defaults apply.
	var attempts2 int32
	var requests2 []chatRequestBody
	ts2 := newReasoningServer(t, &attempts2, &requests2, func(_ int32, w http.ResponseWriter) {
		writeChatResponse(w, "ok", "stop", "")
	})
	defer ts2.Close()
	client2 := NewClient(ts2.URL, "key", "plain-model")
	client2.SetMaxTokens(0)
	if _, err := client2.ChatCompletion(context.Background(), "system", "user"); err != nil {
		t.Fatalf("ChatCompletion() error = %v", err)
	}
	if len(requests2) != 1 {
		t.Fatalf("requests = %d, want 1", len(requests2))
	}
	if requests2[0].MaxTokens != nil {
		t.Fatalf("max_tokens = %v, want omitted when unset", requests2[0].MaxTokens)
	}
}

func TestMaxTokensFromEnv(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{name: "unset", raw: "", want: 0},
		{name: "valid", raw: "16384", want: 16384},
		{name: "whitespace", raw: "  8192  ", want: 8192},
		{name: "non-numeric", raw: "abc", want: 0},
		{name: "zero", raw: "0", want: 0},
		{name: "negative", raw: "-5", want: 0},
		{name: "clamped above ceiling", raw: "999999", want: MaxLLMMaxTokens},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LLM_MAX_TOKENS", tc.raw)
			if got := maxTokensFromEnv(); got != tc.want {
				t.Fatalf("maxTokensFromEnv() = %d, want %d", got, tc.want)
			}
		})
	}
}
