package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// dashboardFS embeds the zero-dependency operations UI. Every style and script
// is inlined in the template so the binary never fetches a CDN asset at runtime
// (T-14-SC).
//
//go:embed templates/*
var dashboardFS embed.FS

var dashboardTemplate = template.Must(template.ParseFS(dashboardFS, "templates/*"))

// dashboardPRURLPattern is the canonical GitHub pull request URL shape. It is
// intentionally strict: only https, only github.com, no ports, no userinfo, no
// query strings, no path traversal (T-14-06).
var dashboardPRURLPattern = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/pull/([0-9]+)/?$`)

const (
	maxDashboardDispatchBody = 64 << 10
	dashboardZeroSHA         = "0000000000000000000000000000000000000000"
	dashboardDefaultRepoID   = int64(1)
	// dashboardCSP pins the page to same-origin inline assets only: no
	// external scripts, stylesheets, frames, or fetch origins.
	dashboardCSP = "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; " +
		"img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
)

// dashboardDeliveryCounter keeps manual delivery IDs unique even when two
// dispatches share the same nanosecond timestamp.
var dashboardDeliveryCounter atomic.Uint64

// dashboardPageData is the server-side render payload. *DashboardMetrics is
// embedded so the template reads {{.TotalPRs}} and friends directly while the
// UI still refreshes every value from GET /api/v1/metrics.
type dashboardPageData struct {
	*DashboardMetrics
	GeneratedAt     string
	TokenConfigured bool
	Dispatched      bool
}

type dashboardDispatchRequest struct {
	PRURL   string `json:"pr_url"`
	Action  string `json:"action"`
	Token   string `json:"token"`
	RepoID  int64  `json:"repo_id"`
	HeadSHA string `json:"head_sha"`
	BaseSHA string `json:"base_sha"`
	Author  string `json:"author"`
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	metrics, err := s.dashboardMetrics(r.Context())
	if err != nil {
		http.Error(w, "failed to compute dashboard metrics", http.StatusServiceUnavailable)
		return
	}
	data := dashboardPageData{
		DashboardMetrics: metrics,
		GeneratedAt:      time.Now().UTC().Format("2006-01-02 15:04:05 UTC"),
		TokenConfigured:  s.cfg != nil && s.cfg.DashboardToken != "",
		Dispatched:       r.URL.Query().Get("dispatched") == "success",
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", dashboardCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if err := dashboardTemplate.Execute(w, data); err != nil {
		log.Printf("[dashboard] template render failed: %v", err)
	}
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	metrics, err := s.dashboardMetrics(r.Context())
	if err != nil {
		writeDashboardJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "metrics unavailable"})
		return
	}
	writeDashboardJSON(w, http.StatusOK, metrics)
}

// dashboardMetrics returns a non-nil metrics snapshot so the template never
// dereferences a nil embedded pointer.
func (s *Server) dashboardMetrics(ctx context.Context) (*DashboardMetrics, error) {
	if s.store == nil {
		return nil, fmt.Errorf("durable store is not initialized")
	}
	metrics, err := s.store.GetDashboardMetrics(ctx)
	if err != nil {
		return nil, err
	}
	if metrics == nil {
		return emptyDashboardMetrics(), nil
	}
	if metrics.Decisions == nil {
		metrics.Decisions = map[string]int{}
	}
	if metrics.JobsByKind == nil {
		metrics.JobsByKind = map[string]int{}
	}
	if metrics.JobsByStatus == nil {
		metrics.JobsByStatus = map[string]int{}
	}
	if metrics.RecentJobs == nil {
		metrics.RecentJobs = []*JobSummary{}
	}
	return metrics, nil
}

func emptyDashboardMetrics() *DashboardMetrics {
	return &DashboardMetrics{
		Decisions:    map[string]int{},
		JobsByKind:   map[string]int{},
		JobsByStatus: map[string]int{},
		RecentJobs:   []*JobSummary{},
	}
}

// handleDashboardDispatch admits one operator-triggered job for a canonical
// GitHub PR URL. Authentication is fail-closed: a configured token must match
// in constant time, and without a token only loopback callers are admitted.
// Client-supplied forwarding headers are never consulted (T-14-01).
func (s *Server) handleDashboardDispatch(w http.ResponseWriter, r *http.Request) {
	req, err := readDashboardDispatchRequest(w, r)
	if err != nil {
		writeDashboardJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if status, message := s.authorizeDashboardDispatch(r, req.Token); status != 0 {
		writeDashboardJSON(w, status, map[string]string{"error": message})
		return
	}

	owner, repo, prNumber, err := parseDashboardPRURL(req.PRURL)
	if err != nil {
		writeDashboardJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid PR URL: must be https://github.com/<owner>/<repo>/pull/<number>",
		})
		return
	}

	action := strings.ToLower(strings.TrimSpace(req.Action))
	if !isDashboardDispatchAction(action) {
		writeDashboardJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid action: must be review, summary, describe, or labels",
		})
		return
	}

	repoID, headSHA, baseSHA, author, status, message := s.resolveDashboardPRMetadata(r.Context(), req, owner, repo, prNumber)
	if status != 0 {
		writeDashboardJSON(w, status, map[string]string{"error": message})
		return
	}

	prKey, err := MakePRKey("github.com", repoID, prNumber)
	if err != nil {
		writeDashboardJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid PR identifier"})
		return
	}

	job := Job{
		Kind:     action,
		Trigger:  "manual",
		Author:   author,
		PRKey:    prKey,
		Owner:    owner,
		Repo:     repo,
		PRNumber: prNumber,
		BaseSHA:  baseSHA,
		HeadSHA:  headSHA,
	}
	delivery := Delivery{
		Host:       "github.com",
		RepoID:     repoID,
		DeliveryID: fmt.Sprintf("manual-%d-%d", time.Now().UnixNano(), dashboardDeliveryCounter.Add(1)),
		EventKind:  "manual_dispatch",
		ReceivedAt: time.Now().UTC(),
	}

	// Manual jobs route through the same durable admission gate as webhooks so
	// backlog, capacity, byte bounds, and dedup all still apply (T-14-05).
	res, err := s.store.Admit(r.Context(), delivery, []Job{job})
	if err != nil {
		log.Printf("[dashboard] manual dispatch admission failed: %v", err)
		w.Header().Set("Retry-After", "30")
		writeDashboardJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "storage failure"})
		return
	}

	switch res.Status {
	case AdmitAccepted:
		if s.scheduler != nil {
			s.scheduler.Wake()
		}
		if s.statusOutbox != nil {
			s.statusOutbox.Wake()
		}
		if s.dispatchHook != nil {
			s.dispatchHook(action, owner, repo, prNumber)
		}
		jobID := s.latestManualJobID(r.Context(), prKey, action)
		if prefersHTMLResponse(r) {
			http.Redirect(w, r, "/dashboard?dispatched=success", http.StatusSeeOther)
			return
		}
		payload := map[string]string{"status": "accepted"}
		if jobID != "" {
			payload["job_id"] = jobID
		}
		writeDashboardJSON(w, http.StatusOK, payload)
	case AdmitDuplicate:
		if prefersHTMLResponse(r) {
			http.Redirect(w, r, "/dashboard?dispatched=success", http.StatusSeeOther)
			return
		}
		writeDashboardJSON(w, http.StatusOK, map[string]string{"status": "duplicate"})
	case AdmitCollision:
		writeDashboardJSON(w, http.StatusConflict, map[string]string{"error": "delivery collision"})
	default:
		w.Header().Set("Retry-After", "30")
		message := strings.TrimSpace(res.Reason)
		if message == "" {
			message = "queue capacity reached"
		}
		writeDashboardJSON(w, http.StatusServiceUnavailable, map[string]string{"error": message})
	}
}

// authorizeDashboardDispatch returns (0, "") when the caller may dispatch.
// A non-zero HTTP status plus message means reject.
func (s *Server) authorizeDashboardDispatch(r *http.Request, bodyToken string) (int, string) {
	expected := ""
	if s.cfg != nil {
		expected = s.cfg.DashboardToken
	}
	if expected != "" {
		candidates := []string{
			bearerToken(r),
			strings.TrimSpace(r.Header.Get("X-Dashboard-Token")),
			strings.TrimSpace(bodyToken),
		}
		for _, candidate := range candidates {
			if candidate != "" && constantTimeTokenMatch(candidate, expected) {
				return 0, ""
			}
		}
		return http.StatusUnauthorized, "unauthorized: invalid dashboard token"
	}
	if isLoopbackRemote(r.RemoteAddr) {
		return 0, ""
	}
	return http.StatusForbidden, "forbidden: manual dispatch without DASHBOARD_TOKEN is restricted to loopback callers"
}

func (s *Server) resolveDashboardPRMetadata(ctx context.Context, req *dashboardDispatchRequest, owner, repo string, prNumber int) (repoID int64, headSHA, baseSHA, author string, status int, message string) {
	repoID = req.RepoID
	if repoID <= 0 {
		repoID = dashboardDefaultRepoID
	}
	headSHA = strings.TrimSpace(req.HeadSHA)
	if headSHA == "" {
		headSHA = dashboardZeroSHA
	}
	baseSHA = strings.TrimSpace(req.BaseSHA)
	if baseSHA == "" {
		baseSHA = dashboardZeroSHA
	}
	author = strings.TrimSpace(req.Author)
	if author == "" {
		author = "dashboard"
	}

	if s.gh == nil {
		return repoID, headSHA, baseSHA, author, 0, ""
	}

	pr, err := s.gh.GetPR(ctx, owner, repo, prNumber)
	if err != nil || pr == nil {
		log.Printf("[dashboard] manual dispatch PR lookup failed for %s/%s#%d: %v", owner, repo, prNumber, err)
		return 0, "", "", "", http.StatusBadGateway, "failed to resolve pull request from GitHub"
	}
	resolvedRepoID, err := s.gh.GetRepoID(ctx, owner, repo)
	if err != nil || resolvedRepoID <= 0 {
		log.Printf("[dashboard] manual dispatch repo lookup failed for %s/%s: %v", owner, repo, err)
		return 0, "", "", "", http.StatusBadGateway, "failed to resolve repository from GitHub"
	}
	if pr.HeadSHA != "" {
		headSHA = pr.HeadSHA
	}
	if pr.BaseSHA != "" {
		baseSHA = pr.BaseSHA
	}
	if pr.Author != "" {
		author = pr.Author
	}
	return resolvedRepoID, headSHA, baseSHA, author, 0, ""
}

// latestManualJobID resolves the admitted job ID for the response. Admit
// assigns IDs to its own copy, so the admitted row is read back from the
// durable queue (best effort: an absent ID never fails the accepted response).
func (s *Server) latestManualJobID(ctx context.Context, prKey PRKey, kind string) string {
	jobs, err := s.store.ListQueuedJobs(ctx)
	if err != nil {
		return ""
	}
	var best *Job
	for _, job := range jobs {
		if job == nil || job.PRKey != prKey || job.Kind != kind || job.Trigger != "manual" {
			continue
		}
		if best == nil || job.CreatedAt.After(best.CreatedAt) {
			best = job
		}
	}
	if best == nil {
		return ""
	}
	return best.ID
}

func readDashboardDispatchRequest(w http.ResponseWriter, r *http.Request) (*dashboardDispatchRequest, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxDashboardDispatchBody)
	req := &dashboardDispatchRequest{}
	contentType := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.Contains(contentType, "application/json") {
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(req); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
	} else {
		if err := r.ParseForm(); err != nil {
			return nil, err
		}
		req.PRURL = r.PostForm.Get("pr_url")
		req.Action = r.PostForm.Get("action")
		req.Token = r.PostForm.Get("token")
	}
	if req.Token == "" {
		req.Token = r.URL.Query().Get("token")
	}
	return req, nil
}

func parseDashboardPRURL(raw string) (owner, repo string, number int, err error) {
	trimmed := strings.TrimSpace(raw)
	matches := dashboardPRURLPattern.FindStringSubmatch(trimmed)
	if matches == nil {
		return "", "", 0, fmt.Errorf("invalid PR URL")
	}
	if _, scanErr := fmt.Sscanf(matches[3], "%d", &number); scanErr != nil || number <= 0 {
		return "", "", 0, fmt.Errorf("invalid PR number")
	}
	return matches[1], matches[2], number, nil
}

func isDashboardDispatchAction(action string) bool {
	switch action {
	case "review", "summary", "describe", "labels":
		return true
	default:
		return false
	}
}

func constantTimeTokenMatch(candidate, expected string) bool {
	candidateHash := sha256.Sum256([]byte(candidate))
	expectedHash := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(candidateHash[:], expectedHash[:]) == 1
}

func bearerToken(r *http.Request) string {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// isLoopbackRemote inspects only the transport peer address. Forwarding headers
// are attacker-controlled and are deliberately ignored (T-14-01).
func isLoopbackRemote(remoteAddr string) bool {
	remoteAddr = strings.TrimSpace(remoteAddr)
	if remoteAddr == "" {
		return false
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func prefersHTMLResponse(r *http.Request) bool {
	contentType := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.Contains(contentType, "application/json") {
		return false
	}
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html")
}

func writeDashboardJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
