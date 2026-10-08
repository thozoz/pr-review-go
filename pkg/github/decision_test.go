package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/thozoz/pr-review-go/pkg/retry"
)

func TestPostDecisionReview_Approve(t *testing.T) {
	var requestCount atomic.Int32
	var receivedBody map[string]any

	const validSHA = "0123456789abcdef0123456789abcdef01234567"

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/pulls/10/reviews" {
			requestCount.Add(1)
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_ = json.Unmarshal(bodyBytes, &receivedBody)

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":        1001,
				"commit_id": validSHA,
				"state":     "APPROVED",
				"body":      "Approved via PR Review",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	client, err := NewTestClient(ts.URL)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	suggestions := []InlineSuggestion{
		{
			Path: "main.go",
			Line: 42,
			Body: "suggestion comment",
		},
	}

	review, err := client.PostDecisionReview(
		context.Background(),
		"owner",
		"repo",
		10,
		validSHA,
		EventApprove,
		"## Verdict: Approved",
		suggestions,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if review == nil || review.GetID() != 1001 {
		t.Fatalf("expected review ID 1001, got %v", review)
	}

	if requestCount.Load() != 1 {
		t.Fatalf("expected exactly 1 CreateReview request, got %d", requestCount.Load())
	}

	if receivedBody["commit_id"] != validSHA {
		t.Errorf("expected commit_id %q, got %v", validSHA, receivedBody["commit_id"])
	}
	if receivedBody["event"] != "APPROVE" {
		t.Errorf("expected event APPROVE, got %v", receivedBody["event"])
	}
	if receivedBody["body"] != "## Verdict: Approved" {
		t.Errorf("expected body %q, got %v", "## Verdict: Approved", receivedBody["body"])
	}

	comments, ok := receivedBody["comments"].([]any)
	if !ok || len(comments) != 1 {
		t.Fatalf("expected 1 draft comment, got %v", receivedBody["comments"])
	}
	comm := comments[0].(map[string]any)
	if comm["path"] != "main.go" || comm["line"] != float64(42) || comm["side"] != "RIGHT" || comm["body"] != "suggestion comment" {
		t.Errorf("unexpected comment shape: %v", comm)
	}
}

func TestPostDecisionReview_RequestChanges(t *testing.T) {
	var requestCount atomic.Int32
	var receivedBody map[string]any

	const validSHA = "0123456789abcdef0123456789abcdef01234567"

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/pulls/10/reviews" {
			requestCount.Add(1)
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_ = json.Unmarshal(bodyBytes, &receivedBody)

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":        1002,
				"commit_id": validSHA,
				"state":     "CHANGES_REQUESTED",
				"body":      "Changes requested",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	client, err := NewTestClient(ts.URL)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	review, err := client.PostDecisionReview(
		context.Background(),
		"owner",
		"repo",
		10,
		validSHA,
		EventRequestChanges,
		"## Verdict: Changes Requested",
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if review == nil || review.GetID() != 1002 {
		t.Fatalf("expected review ID 1002, got %v", review)
	}
	if requestCount.Load() != 1 {
		t.Fatalf("expected exactly 1 request, got %d", requestCount.Load())
	}
	if receivedBody["event"] != "REQUEST_CHANGES" {
		t.Errorf("expected event REQUEST_CHANGES, got %v", receivedBody["event"])
	}
}

func TestPostDecisionReview_ValidationFailClosed(t *testing.T) {
	client, err := NewTestClient("http://127.0.0.1:9")
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	const validSHA = "0123456789abcdef0123456789abcdef01234567"

	cases := []struct {
		name      string
		owner     string
		repo      string
		commitSHA string
		event     string
		errCheck  func(error) bool
	}{
		{
			name:      "empty owner",
			owner:     "",
			repo:      "repo",
			commitSHA: validSHA,
			event:     EventApprove,
			errCheck:  func(err error) bool { return errors.Is(err, ErrMissingOwnerOrRepo) },
		},
		{
			name:      "empty repo",
			owner:     "owner",
			repo:      "",
			commitSHA: validSHA,
			event:     EventApprove,
			errCheck:  func(err error) bool { return errors.Is(err, ErrMissingOwnerOrRepo) },
		},
		{
			name:      "empty commitSHA",
			owner:     "owner",
			repo:      "repo",
			commitSHA: "",
			event:     EventApprove,
			errCheck:  func(err error) bool { return errors.Is(err, ErrInvalidCommitOID) },
		},
		{
			name:      "short prefix commitSHA",
			owner:     "owner",
			repo:      "repo",
			commitSHA: "0123456",
			event:     EventApprove,
			errCheck:  func(err error) bool { return errors.Is(err, ErrInvalidCommitOID) },
		},
		{
			name:      "uppercase commitSHA",
			owner:     "owner",
			repo:      "repo",
			commitSHA: "0123456789ABCDEF0123456789ABCDEF01234567",
			event:     EventApprove,
			errCheck:  func(err error) bool { return errors.Is(err, ErrInvalidCommitOID) },
		},
		{
			name:      "invalid event COMMENT",
			owner:     "owner",
			repo:      "repo",
			commitSHA: validSHA,
			event:     "COMMENT",
			errCheck:  func(err error) bool { return errors.Is(err, ErrInvalidDecisionEvent) },
		},
		{
			name:      "invalid event empty",
			owner:     "owner",
			repo:      "repo",
			commitSHA: validSHA,
			event:     "",
			errCheck:  func(err error) bool { return errors.Is(err, ErrInvalidDecisionEvent) },
		},
		{
			name:      "invalid event DISMISS",
			owner:     "owner",
			repo:      "repo",
			commitSHA: validSHA,
			event:     "DISMISS",
			errCheck:  func(err error) bool { return errors.Is(err, ErrInvalidDecisionEvent) },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.PostDecisionReview(
				context.Background(),
				tc.owner,
				tc.repo,
				1,
				tc.commitSHA,
				tc.event,
				"body",
				nil,
			)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.errCheck(err) {
				t.Fatalf("error check failed for error: %v", err)
			}
		})
	}
}

func TestPostDecisionReview_WriteErrorClassification(t *testing.T) {
	const validSHA = "0123456789abcdef0123456789abcdef01234567"

	t.Run("500ServerError_ClassifiedUncertain", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}))
		defer ts.Close()

		client, err := NewTestClient(ts.URL)
		if err != nil {
			t.Fatalf("failed to create client: %v", err)
		}

		_, err = client.PostDecisionReview(
			context.Background(),
			"owner",
			"repo",
			1,
			validSHA,
			EventApprove,
			"body",
			nil,
		)
		if err == nil {
			t.Fatalf("expected error, got nil")
		}

		var writeErr *DecisionWriteError
		if !errors.As(err, &writeErr) {
			t.Fatalf("expected *DecisionWriteError, got %T: %v", err, err)
		}
		if !writeErr.IsUncertain() {
			t.Errorf("expected write error to be uncertain, got %v", writeErr.Classification)
		}
		if !IsUncertainWriteError(err) {
			t.Errorf("expected IsUncertainWriteError(err) to be true")
		}
	})

	t.Run("422UnprocessableEntity_ClassifiedPermanent", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "Unprocessable Entity", http.StatusUnprocessableEntity)
		}))
		defer ts.Close()

		client, err := NewTestClient(ts.URL)
		if err != nil {
			t.Fatalf("failed to create client: %v", err)
		}

		_, err = client.PostDecisionReview(
			context.Background(),
			"owner",
			"repo",
			1,
			validSHA,
			EventApprove,
			"body",
			nil,
		)
		if err == nil {
			t.Fatalf("expected error, got nil")
		}

		var writeErr *DecisionWriteError
		if !errors.As(err, &writeErr) {
			t.Fatalf("expected *DecisionWriteError, got %T: %v", err, err)
		}
		if writeErr.Classification != retry.ClassPermanent {
			t.Errorf("expected ClassPermanent, got %v", writeErr.Classification)
		}
		if IsUncertainWriteError(err) {
			t.Errorf("expected IsUncertainWriteError to be false for 422")
		}
	})
}
