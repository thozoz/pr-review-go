package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thozoz/pr-review-go/pkg/config"
)

func TestHealthEndpoint(t *testing.T) {
	cfg := &config.Config{Port: 3000}
	srv := NewServer(cfg)

	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()

	srv.Routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}

	body := rr.Body.String()
	if !strings.Contains(body, `"status":"ok"`) {
		t.Fatalf("expected response to contain 'ok', got %s", body)
	}
}

func TestWebhookPing(t *testing.T) {
	cfg := &config.Config{Port: 3000}
	srv := NewServer(cfg)

	req := httptest.NewRequest("POST", "/api/v1/github_webhooks", strings.NewReader(`{"zen":"Keep it logically awesome."}`))
	req.Header.Set("X-GitHub-Event", "ping")
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}
}
