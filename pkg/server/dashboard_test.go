package server

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thozoz/pr-review-go/pkg/config"
	"go.etcd.io/bbolt"
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

func postDashboardDispatch(srv *Server, body, contentType, remoteAddr string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/v1/dashboard/dispatch", strings.NewReader(body))
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	return serveDashboardRequest(srv, req)
}

func dispatchJSONBody(prURL, action string, token string) string {
	payload := map[string]string{"pr_url": prURL, "action": action}
	if token != "" {
		payload["token"] = token
	}
	raw, _ := json.Marshal(payload)
	return string(raw)
}

const validDashboardPRURL = "https://github.com/acme/widgets/pull/42"

func TestDashboardDispatch_AuthWithToken(t *testing.T) {
	const token = "s3cret-dashboard-token"

	t.Run("bearer header", func(t *testing.T) {
		srv, _ := newDashboardTestServer(t, token)
		rr := postDashboardDispatch(srv, dispatchJSONBody(validDashboardPRURL, "review", ""), "application/json", "203.0.113.9:1234", map[string]string{
			"Authorization": "Bearer " + token,
		})
		if rr.Code != http.StatusOK {
			t.Fatalf("bearer dispatch status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
		}
	})

	t.Run("x-dashboard-token header", func(t *testing.T) {
		srv, _ := newDashboardTestServer(t, token)
		rr := postDashboardDispatch(srv, dispatchJSONBody(validDashboardPRURL, "summary", ""), "application/json", "203.0.113.9:1234", map[string]string{
			"X-Dashboard-Token": token,
		})
		if rr.Code != http.StatusOK {
			t.Fatalf("header token dispatch status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
		}
	})

	t.Run("token field", func(t *testing.T) {
		srv, _ := newDashboardTestServer(t, token)
		rr := postDashboardDispatch(srv, dispatchJSONBody(validDashboardPRURL, "describe", token), "application/json", "203.0.113.9:1234", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("body token dispatch status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
		}
	})

	t.Run("form token", func(t *testing.T) {
		srv, _ := newDashboardTestServer(t, token)
		form := "pr_url=" + validDashboardPRURL + "&action=labels&token=" + token
		rr := postDashboardDispatch(srv, form, "application/x-www-form-urlencoded", "203.0.113.9:1234", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("form token dispatch status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
		}
	})

	t.Run("mismatched token rejected", func(t *testing.T) {
		srv, _ := newDashboardTestServer(t, token)
		rr := postDashboardDispatch(srv, dispatchJSONBody(validDashboardPRURL, "review", "wrong-token"), "application/json", "127.0.0.1:1234", map[string]string{
			"Authorization": "Bearer wrong-token",
		})
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("mismatched token status = %d, want 401", rr.Code)
		}
	})

	t.Run("missing token rejected even on loopback", func(t *testing.T) {
		srv, _ := newDashboardTestServer(t, token)
		rr := postDashboardDispatch(srv, dispatchJSONBody(validDashboardPRURL, "review", ""), "application/json", "127.0.0.1:1234", nil)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("missing token status = %d, want 401", rr.Code)
		}
	})
}

func TestDashboardDispatch_LoopbackAuth(t *testing.T) {
	t.Run("ipv4 loopback accepted", func(t *testing.T) {
		srv, _ := newDashboardTestServer(t, "")
		rr := postDashboardDispatch(srv, dispatchJSONBody(validDashboardPRURL, "review", ""), "application/json", "127.0.0.1:1234", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("loopback dispatch status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
		}
	})

	t.Run("ipv6 loopback accepted", func(t *testing.T) {
		srv, _ := newDashboardTestServer(t, "")
		rr := postDashboardDispatch(srv, dispatchJSONBody(validDashboardPRURL, "review", ""), "application/json", "[::1]:1234", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("ipv6 loopback dispatch status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
		}
	})

	t.Run("remote address rejected fail-closed", func(t *testing.T) {
		srv, store := newDashboardTestServer(t, "")
		rr := postDashboardDispatch(srv, dispatchJSONBody(validDashboardPRURL, "review", ""), "application/json", "192.168.1.100:1234", nil)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("remote dispatch status = %d, want 403 (body %s)", rr.Code, rr.Body.String())
		}
		if jobs, err := store.ListQueuedJobs(t.Context()); err != nil || len(jobs) != 0 {
			t.Fatalf("rejected dispatch must not enqueue jobs: jobs=%d err=%v", len(jobs), err)
		}
	})

	t.Run("spoofed forwarding headers do not bypass loopback", func(t *testing.T) {
		srv, _ := newDashboardTestServer(t, "")
		rr := postDashboardDispatch(srv, dispatchJSONBody(validDashboardPRURL, "review", ""), "application/json", "192.168.1.100:1234", map[string]string{
			"X-Forwarded-For": "127.0.0.1",
			"X-Real-IP":       "127.0.0.1",
			"Forwarded":       "for=127.0.0.1",
		})
		if rr.Code != http.StatusForbidden {
			t.Fatalf("spoofed forwarding headers status = %d, want 403", rr.Code)
		}
	})
}

func TestDashboardDispatch_URLValidation(t *testing.T) {
	srv, _ := newDashboardTestServer(t, "")
	invalid := []string{
		"",
		"github.com/acme/widgets/pull/42",
		"http://github.com/acme/widgets/pull/42",
		"https://gitlab.com/acme/widgets/pull/42",
		"https://github.com/acme/widgets/pull/",
		"https://github.com/acme/widgets/pull/abc",
		"https://github.com/acme/widgets/pull/0",
		"https://github.com/acme/widgets/pull/-1",
		"https://github.com/acme/widgets/pull/42/extra",
		"https://github.com/acme/widgets/pull/42?x=1",
		"https://github.com/acme/widgets/issues/42",
		"https://user@github.com/acme/widgets/pull/42",
	}
	for _, prURL := range invalid {
		rr := postDashboardDispatch(srv, dispatchJSONBody(prURL, "review", ""), "application/json", "127.0.0.1:1234", nil)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("PR URL %q status = %d, want 400 (body %s)", prURL, rr.Code, rr.Body.String())
		}
	}

	// A single trailing slash is canonical-equivalent and accepted.
	rr := postDashboardDispatch(srv, dispatchJSONBody(validDashboardPRURL+"/", "review", ""), "application/json", "127.0.0.1:1234", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("trailing-slash PR URL status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
}

func TestDashboardDispatch_ActionValidation(t *testing.T) {
	srv, _ := newDashboardTestServer(t, "")
	for _, action := range []string{"", "merge", "approve", "edit", "request_changes", "delete"} {
		rr := postDashboardDispatch(srv, dispatchJSONBody(validDashboardPRURL, action, ""), "application/json", "127.0.0.1:1234", nil)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("action %q status = %d, want 400 (body %s)", action, rr.Code, rr.Body.String())
		}
	}
}

func TestDashboardDispatch_FormRedirect(t *testing.T) {
	const token = "form-token"
	srv, _ := newDashboardTestServer(t, token)
	form := "pr_url=" + validDashboardPRURL + "&action=review&token=" + token
	rr := postDashboardDispatch(srv, form, "application/x-www-form-urlencoded", "203.0.113.9:1234", map[string]string{
		"Accept": "text/html",
	})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("form post status = %d, want 303 (body %s)", rr.Code, rr.Body.String())
	}
	if location := rr.Header().Get("Location"); location != "/dashboard?dispatched=success" {
		t.Fatalf("form redirect Location = %q, want /dashboard?dispatched=success", location)
	}
}

func TestDashboardDispatch_JobEnqueued(t *testing.T) {
	srv, store := newDashboardTestServer(t, "")

	rr := postDashboardDispatch(srv, dispatchJSONBody(validDashboardPRURL, "review", ""), "application/json", "127.0.0.1:1234", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("dispatch status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	var accepted map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("decode dispatch response: %v", err)
	}
	if accepted["status"] != "accepted" {
		t.Fatalf("dispatch status payload = %q, want accepted", accepted["status"])
	}
	if accepted["job_id"] == "" {
		t.Fatalf("dispatch response missing job_id: %s", rr.Body.String())
	}

	queued, err := store.ListQueuedJobs(t.Context())
	if err != nil {
		t.Fatalf("ListQueuedJobs: %v", err)
	}
	if len(queued) != 1 {
		t.Fatalf("queued jobs = %d, want 1", len(queued))
	}
	job := queued[0]
	if job.ID != accepted["job_id"] {
		t.Errorf("job ID = %q, response job_id = %q", job.ID, accepted["job_id"])
	}
	if job.Kind != "review" || job.Trigger != "manual" || job.Author != "dashboard" {
		t.Errorf("job = %+v, want manual dashboard review", job)
	}
	if job.Owner != "acme" || job.Repo != "widgets" || job.PRNumber != 42 {
		t.Errorf("job target = %s/%s#%d, want acme/widgets#42", job.Owner, job.Repo, job.PRNumber)
	}
	if job.HeadSHA != dashboardZeroSHA || job.BaseSHA != dashboardZeroSHA {
		t.Errorf("offline dispatch SHAs = head %q base %q, want zero SHA defaults", job.HeadSHA, job.BaseSHA)
	}

	metricsReq := httptest.NewRequest("GET", "/api/v1/metrics", nil)
	metricsReq.RemoteAddr = "127.0.0.1:1234"
	metricsRR := serveDashboardRequest(srv, metricsReq)
	var metrics DashboardMetrics
	if err := json.Unmarshal(metricsRR.Body.Bytes(), &metrics); err != nil {
		t.Fatalf("decode metrics: %v", err)
	}
	if metrics.TotalJobs != 1 || metrics.QueuedJobs != 1 || metrics.JobsByKind["review"] != 1 {
		t.Errorf("metrics after dispatch = %+v, want one queued review", metrics)
	}
	if len(metrics.RecentJobs) != 1 {
		t.Fatalf("recent jobs = %d, want 1", len(metrics.RecentJobs))
	}
	row := metrics.RecentJobs[0]
	if row.ID != job.ID || row.Trigger != "manual" || row.PRURL != validDashboardPRURL {
		t.Errorf("recent job row = %+v, want the dispatched manual job", row)
	}
}

func TestDashboardMetrics_Calculations(t *testing.T) {
	_, store := newDashboardTestServer(t, "")
	base := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	finished := base.Add(5 * time.Minute)

	seedDashboardJob(t, store, 1, Job{Kind: "edit", Trigger: "automatic", Status: "completed", Owner: "acme", Repo: "widgets", PRNumber: 1, CreatedAt: base.Add(1 * time.Minute)})
	seedDashboardJob(t, store, 2, Job{Kind: "changelog", Trigger: "automatic", Status: "completed", Owner: "acme", Repo: "widgets", PRNumber: 1, CreatedAt: base.Add(2 * time.Minute)})
	seedDashboardJob(t, store, 3, Job{Kind: "review", Trigger: "automatic", Status: "completed", Owner: "acme", Repo: "widgets", PRNumber: 2, CreatedAt: base.Add(3 * time.Minute), FinishedAt: &finished})
	seedDashboardJob(t, store, 4, Job{Kind: "review", Trigger: "automatic", Status: "failed", Owner: "acme", Repo: "widgets", PRNumber: 2, CreatedAt: base.Add(4 * time.Minute), Error: "boom"})
	seedDashboardJob(t, store, 5, Job{Kind: "review", Trigger: "automatic", Status: "running", Owner: "acme", Repo: "widgets", PRNumber: 3, CreatedAt: base.Add(5 * time.Minute)})
	seedDashboardJob(t, store, 6, Job{Kind: "review", Trigger: "automatic", Status: "queued", Owner: "acme", Repo: "widgets", PRNumber: 4, CreatedAt: base.Add(6 * time.Minute)})
	seedDashboardJob(t, store, 7, Job{Kind: "review", Trigger: "automatic", Status: "queued", Owner: "acme", Repo: "widgets", PRNumber: 5, CreatedAt: base.Add(7 * time.Minute), NotBeforeAt: time.Now().UTC().Add(2 * time.Hour)})
	seedDashboardJob(t, store, 8, Job{Kind: "review", Trigger: "automatic", Status: "needs_attention", Owner: "acme", Repo: "widgets", PRNumber: 6, CreatedAt: base.Add(8 * time.Minute)})
	seedDashboardJob(t, store, 9, Job{Kind: "review", Trigger: "automatic", Status: "uncertain", Owner: "acme", Repo: "widgets", PRNumber: 7, CreatedAt: base.Add(9 * time.Minute)})

	// Two tracked PR rows.
	if err := store.DB().Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketPRs)
		for _, key := range []string{"github.com/1/1", "github.com/1/2"} {
			raw, _ := json.Marshal(PRState{PRKey: PRKey{Host: "github.com", RepoID: 1, Number: 1}})
			if err := b.Put([]byte(key), raw); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed PR states: %v", err)
	}

	// Decision ledger rows: 2 approvals, 1 request changes.
	if err := store.DB().Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketDecisions)
		records := []DecisionRecord{
			{Event: "APPROVE", HeadSHA: "aaa"},
			{Event: "APPROVE", HeadSHA: "bbb"},
			{Event: "REQUEST_CHANGES", HeadSHA: "ccc"},
		}
		for i, rec := range records {
			raw, _ := json.Marshal(rec)
			if err := b.Put([]byte(fmt.Sprintf("decision-%d", i)), raw); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed decisions: %v", err)
	}

	metrics, err := store.GetDashboardMetrics(t.Context())
	if err != nil {
		t.Fatalf("GetDashboardMetrics: %v", err)
	}

	if metrics.TotalPRs != 2 {
		t.Errorf("TotalPRs = %d, want 2", metrics.TotalPRs)
	}
	if metrics.TotalJobs != 9 {
		t.Errorf("TotalJobs = %d, want 9", metrics.TotalJobs)
	}
	if metrics.RunningJobs != 1 || metrics.ActiveWorkers != 1 {
		t.Errorf("running/active = %d/%d, want 1/1", metrics.RunningJobs, metrics.ActiveWorkers)
	}
	if metrics.QueuedJobs != 2 || metrics.DeferredJobs != 1 {
		t.Errorf("queued/deferred = %d/%d, want 2/1", metrics.QueuedJobs, metrics.DeferredJobs)
	}
	if metrics.NeedsAttentionJobs != 1 {
		t.Errorf("NeedsAttentionJobs = %d, want 1", metrics.NeedsAttentionJobs)
	}
	if metrics.AutonomousCommits != 2 {
		t.Errorf("AutonomousCommits = %d, want 2 (completed edit + changelog)", metrics.AutonomousCommits)
	}
	if metrics.JobsByKind["review"] != 7 || metrics.JobsByKind["edit"] != 1 || metrics.JobsByKind["changelog"] != 1 {
		t.Errorf("JobsByKind = %v, want review=7 edit=1 changelog=1", metrics.JobsByKind)
	}
	if metrics.JobsByStatus["completed"] != 3 || metrics.JobsByStatus["failed"] != 1 || metrics.JobsByStatus["uncertain"] != 1 {
		t.Errorf("JobsByStatus = %v, want completed=3 failed=1 uncertain=1", metrics.JobsByStatus)
	}
	if metrics.SuccessRate < 74.99 || metrics.SuccessRate > 75.01 {
		t.Errorf("SuccessRate = %f, want 75", metrics.SuccessRate)
	}
	if metrics.Decisions["APPROVE"] != 2 || metrics.Decisions["REQUEST_CHANGES"] != 1 {
		t.Errorf("Decisions = %v, want APPROVE=2 REQUEST_CHANGES=1", metrics.Decisions)
	}
	if len(metrics.RecentJobs) != 9 {
		t.Fatalf("RecentJobs = %d, want 9", len(metrics.RecentJobs))
	}
	if metrics.RecentJobs[0].PRNumber != 7 {
		t.Errorf("newest recent job PR = %d, want 7 (descending CreatedAt)", metrics.RecentJobs[0].PRNumber)
	}
	if metrics.RecentJobs[0].PRURL != "https://github.com/acme/widgets/pull/7" {
		t.Errorf("PRURL = %q, want canonical GitHub URL", metrics.RecentJobs[0].PRURL)
	}
}

func TestDashboardMetrics_DoNotLeakSecrets(t *testing.T) {
	srv, store := newDashboardTestServer(t, "")
	seedDashboardJob(t, store, 1, Job{
		Kind: "review", Trigger: "explicit", Status: "failed",
		Owner: "acme", Repo: "widgets", PRNumber: 9,
		Payload:   "TOPSECRET-PAYLOAD",
		Error:     "review failed",
		CreatedAt: time.Now().UTC(),
	})

	req := httptest.NewRequest("GET", "/api/v1/metrics", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rr := serveDashboardRequest(srv, req)
	body := rr.Body.String()

	for _, secret := range []string{"TOPSECRET-PAYLOAD", srv.cfg.GitHubToken, srv.cfg.LLMAPIKey, srv.cfg.WebhookSecret} {
		if strings.Contains(body, secret) {
			t.Errorf("metrics response leaked secret %q", secret)
		}
	}
}

// seedDashboardJob writes a job row directly into the durable ledger so metric
// aggregation can be tested against arbitrary status/kind/timestamp mixes.
func seedDashboardJob(t *testing.T, store *BoltJobStore, seq uint64, job Job) {
	t.Helper()
	job.Sequence = seq
	if job.ID == "" {
		job.ID = fmt.Sprintf("job-%08d", seq)
	}
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, seq)
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal seeded job: %v", err)
	}
	if err := store.DB().Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketJobs).Put(key, raw)
	}); err != nil {
		t.Fatalf("seed job %d: %v", seq, err)
	}
}
