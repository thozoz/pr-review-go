package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/thozoz/pr-review-go/pkg/retry"
)

func TestFallbackHTTPStatus(t *testing.T) {
	for _, status := range []int{429, 500, 502, 503, 504, 401, 403, 400, 404} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var models []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req ChatRequest
				json.NewDecoder(r.Body).Decode(&req)
				models = append(models, req.Model)
				if req.Model == "primary" {
					w.WriteHeader(status)
					w.Write([]byte(`{"error":"unavailable"}`))
					return
				}
				w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
			}))
			defer srv.Close()
			c := NewClient(srv.URL, "key", "primary")
			c.SetFallbackModels([]string{" primary ", "fallback", "fallback", ""})
			c.SetRetryPolicy(retry.Policy{MaxAttempts: 1})
			got, err := c.ChatCompletion(context.Background(), "system", "user")
			if status == 429 || status >= 500 {
				if err != nil || got != "ok" || !reflect.DeepEqual(models, []string{"primary", "fallback"}) {
					t.Fatalf("got=%q err=%v models=%v", got, err, models)
				}
			} else if err == nil || !reflect.DeepEqual(models, []string{"primary"}) {
				t.Fatalf("should not switch: err=%v models=%v", err, models)
			}
		})
	}
}

// An empty completion from the primary model must fall through to the next model
// in LLM_FALLBACK_MODELS. Live: agy/gemini-3.8-flash-high returned
// finish_reason=stop with no content on turn 3 while evren/deepseek-v4.1-flash
// answered the same prompt, and the job failed instead of falling back.
func TestFallbackOnEmptyCompletion(t *testing.T) {
	var seen []string
	mux := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen = append(seen, body.Model)
		w.Header().Set("Content-Type", "application/json")
		if body.Model == "primary-model" {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":null},"finish_reason":"stop"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"review from the fallback model"},"finish_reason":"stop"}]}`))
	}))
	defer mux.Close()

	c := NewClient(mux.URL, "key", "primary-model")
	c.SetFallbackModels([]string{"fallback-model"})
	c.SetRetryPolicy(retry.Policy{MaxAttempts: 1, BaseDelay: time.Millisecond})

	got, err := c.ChatCompletion(context.Background(), "system", "user")
	if err != nil {
		t.Fatalf("ChatCompletion() error = %v, want the fallback model to answer", err)
	}
	if got != "review from the fallback model" {
		t.Fatalf("ChatCompletion() = %q, want the fallback model's answer", got)
	}
	if want := []string{"primary-model", "fallback-model"}; !reflect.DeepEqual(seen, want) {
		t.Fatalf("model call order = %v, want %v", seen, want)
	}
}
