package github

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/go-github/v68/github"
	"github.com/thozoz/pr-review-go/pkg/retry"
)

var (
	// ErrInvalidDecisionEvent indicates that the decision event is not APPROVE or REQUEST_CHANGES.
	ErrInvalidDecisionEvent = errors.New("decision event must be APPROVE or REQUEST_CHANGES")
	// ErrMissingOwnerOrRepo indicates that owner or repo is empty.
	ErrMissingOwnerOrRepo = errors.New("owner and repo must not be empty")
)

const (
	// EventApprove represents GitHub review event APPROVE.
	EventApprove = "APPROVE"
	// EventRequestChanges represents GitHub review event REQUEST_CHANGES.
	EventRequestChanges = "REQUEST_CHANGES"
)

// DecisionWriteError wraps an error from a decision review write and captures its retry classification.
type DecisionWriteError struct {
	Err            error
	Classification retry.Classification
}

func (e *DecisionWriteError) Error() string {
	return fmt.Sprintf("decision write failed (%s): %v", e.Classification, e.Err)
}

func (e *DecisionWriteError) Unwrap() error {
	return e.Err
}

// IsUncertain reports whether the write error has an unprovable remote outcome (D-16).
func (e *DecisionWriteError) IsUncertain() bool {
	return e.Classification == retry.ClassUncertainWrite
}

// PostDecisionReview creates ONE review with event APPROVE or REQUEST_CHANGES containing
// summary body, inline comments, and bound to the exact commit SHA (D-02, D-15).
func (c *Client) PostDecisionReview(ctx context.Context, owner, repo string, number int, commitSHA string, event string, body string, suggestions []InlineSuggestion) (*github.PullRequestReview, error) {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	if owner == "" || repo == "" {
		return nil, ErrMissingOwnerOrRepo
	}

	if err := ValidateCommitOID(commitSHA); err != nil {
		return nil, fmt.Errorf("invalid decision commit SHA: %w", err)
	}

	eventUpper := strings.ToUpper(strings.TrimSpace(event))
	if eventUpper != EventApprove && eventUpper != EventRequestChanges {
		return nil, fmt.Errorf("%w: got %q", ErrInvalidDecisionEvent, event)
	}

	ghClient, err := c.ghForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}

	var comments []*github.DraftReviewComment
	if len(suggestions) > 0 {
		comments = make([]*github.DraftReviewComment, 0, len(suggestions))
		for _, suggestion := range suggestions {
			comments = append(comments, &github.DraftReviewComment{
				Path: github.Ptr(suggestion.Path),
				Line: github.Ptr(suggestion.Line),
				Side: github.Ptr("RIGHT"),
				Body: github.Ptr(suggestion.Body),
			})
		}
	}

	req := &github.PullRequestReviewRequest{
		CommitID: github.Ptr(commitSHA),
		Event:    github.Ptr(eventUpper),
		Body:     github.Ptr(body),
		Comments: comments,
	}

	review, _, err := ghClient.PullRequests.CreateReview(ctx, owner, repo, number, req)
	if err != nil {
		classification := ClassifyWriteError(err)
		return nil, &DecisionWriteError{
			Err:            err,
			Classification: classification,
		}
	}

	return review, nil
}
