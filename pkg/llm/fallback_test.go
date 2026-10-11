package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

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
