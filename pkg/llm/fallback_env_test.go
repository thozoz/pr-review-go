package llm

import (
	"context"
	"github.com/thozoz/pr-review-go/pkg/retry"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestFallbackEnvAndExhaustion(t *testing.T) {
	t.Setenv("LLM_FALLBACK_MODELS", " primary,backup,backup, ,last ")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) }))
	defer srv.Close()
	c := NewClient(srv.URL, "key", "primary")
	if !reflect.DeepEqual(c.fallbackModels, []string{"backup", "last"}) {
		t.Fatal(c.fallbackModels)
	}
	c.SetRetryPolicy(retry.Policy{MaxAttempts: 1})
	if _, err := c.ChatCompletion(context.Background(), "sys", "usr"); err == nil {
		t.Fatal("exhaustion must fail")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.ChatCompletion(ctx, "sys", "usr"); err != context.Canceled {
		t.Fatalf("got %v", err)
	}
}
