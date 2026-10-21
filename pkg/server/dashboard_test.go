package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thozoz/pr-review-go/pkg/config"
)

// newDashboardTestServer builds a real bbolt-backed server in normal mode with
// an optional dashboard token. The GitHub client stays nil so manual dispatch
// uses the deterministic offline metadata defaults.
func newDashboardTestServer(t *testing.T, token string) (*Server, *BoltJobStore) {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenJobStore(filepath.Join(dir, "jobs.db"), StoreOptions{
		BacklogLimit:  50,
		DeliveryLimit: 100,
	})
	if err != nil {
		t.Fatalf("OpenJobStore failed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := &config.Config{
		Port:            3000,
		GitHubToken:     "test-token",
		WebhookSecret:   "test-secret",
		LLMAPIKey:       "test-key",
		LLMModel:        "test-model",
		LLMBaseURL:      "http://example.invalid",
		WebhookStateDir: dir,
		DashboardToken:  token,
	}
	return &Server{cfg: cfg, store: store}, store
}

func serveDashboardRequest(srv *Server, req *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rr, req)
	return rr
}

func TestDashboardMetricsEndpoint(t *testing.T) {
	srv, _ := newDashboardTestServer(t, "")

	req := httptest.NewRequest("GET", "/api/v1/metrics", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rr := serveDashboardRequest(srv, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/metrics status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if contentType := rr.Header().Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", contentType)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("metrics response is not valid JSON: %v", err)
	}
	for _, field := range []string{
		"total_prs", "total_jobs", "active_workers", "running_jobs", "queued_jobs",
		"deferred_jobs", "needs_attention_jobs", "autonomous_commits", "decisions",
		"jobs_by_kind", "jobs_by_status", "success_rate", "recent_jobs",
	} {
		if _, ok := raw[field]; !ok {
			t.Errorf("metrics response missing field %q", field)
		}
	}

	var metrics DashboardMetrics
	if err := json.Unmarshal(rr.Body.Bytes(), &metrics); err != nil {
		t.Fatalf("decode metrics: %v", err)
	}
	if metrics.TotalJobs != 0 || metrics.TotalPRs != 0 {
		t.Errorf("empty store metrics = %+v, want zero counters", metrics)
	}
	if metrics.RecentJobs == nil {
		t.Error("recent_jobs must be an empty array, not null")
	}
}

func TestDashboardPagesRender(t *testing.T) {
	srv, _ := newDashboardTestServer(t, "")

	for _, path := range []string{"/dashboard", "/dashboard/"} {
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = "127.0.0.1:1234"
		rr := serveDashboardRequest(srv, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200 (body %s)", path, rr.Code, rr.Body.String())
		}
		if contentType := rr.Header().Get("Content-Type"); contentType != "text/html; charset=utf-8" {
			t.Fatalf("GET %s Content-Type = %q, want text/html; charset=utf-8", path, contentType)
		}
		body := rr.Body.String()
		if !strings.Contains(body, "<!DOCTYPE html>") {
			t.Errorf("GET %s body is missing the HTML doctype", path)
		}
		if !strings.Contains(body, "PR Review Operations Dashboard") {
			t.Errorf("GET %s body is missing the dashboard title", path)
		}
	}
}

func TestDashboardHTMLStructure(t *testing.T) {
	srv, _ := newDashboardTestServer(t, "")

	req := httptest.NewRequest("GET", "/dashboard", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rr := serveDashboardRequest(srv, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	for _, needle := range []string{
		// Dark-mode palette.
		"#0d1117", "#161b22", "#30363d", "#c9d1d9", "#58a6ff",
		"#238636", "#d29922", "#da3633", "#8957e5",
		// Summary stat cards, live monitor, ledger, dispatch form.
		`id="stat-total-prs"`,
		`id="stat-autonomous-commits"`,
		`id="stat-approvals"`,
		`id="stat-queued"`,
		`id="stat-deferred"`,
		`id="stat-needs-attention"`,
		`id="running-jobs"`,
		`id="queued-jobs"`,
		`id="attention-jobs"`,
		`id="ledger-table"`,
		`id="ledger-body"`,
		`id="dispatch-form"`,
		`id="dispatch-pr-url"`,
		`id="dispatch-action"`,
		`/api/v1/metrics`,
		`/api/v1/dashboard/dispatch`,
		"setInterval",
		"refresh-interval",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("dashboard HTML missing %q", needle)
		}
	}

	// Zero external dependencies: every asset is inlined.
	lower := strings.ToLower(body)
	for _, forbidden := range []string{
		"http://", "https://cdn", "unpkg", "cdnjs", "fonts.googleapis", "fonts.gstatic",
		"<script src=", "<link rel=\"stylesheet\"", "<link href=",
	} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("dashboard HTML must not reference external dependency %q", forbidden)
		}
	}

	// Dispatch action options are the four supported operator actions.
	for _, action := range []string{`value="review"`, `value="summary"`, `value="describe"`, `value="labels"`} {
		if !strings.Contains(body, action) {
			t.Errorf("dashboard HTML missing dispatch action option %q", action)
		}
	}
}
