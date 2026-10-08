package github

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/go-github/v68/github"
	"github.com/thozoz/pr-review-go/pkg/retry"
)

func writeErrWithStatus(status int) error {
	return &github.ErrorResponse{
		Response: &http.Response{StatusCode: status, Header: http.Header{}},
		Message:  fmt.Sprintf("api error (status %d)", status),
	}
}

// TestClassifyWriteErrorGuardsAmbiguousWrites: timeouts and ambiguous 5xx on
// writes are uncertain (reconcile-first, never blind retry); auth/input
// failures stay permanent; plain rate limiting stays transient.
func TestClassifyWriteErrorGuardsAmbiguousWrites(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want retry.Classification
	}{
		{"timeout uncertain", context.DeadlineExceeded, retry.ClassUncertainWrite},
		{"server 500 uncertain", writeErrWithStatus(500), retry.ClassUncertainWrite},
		{"bad gateway uncertain", writeErrWithStatus(502), retry.ClassUncertainWrite},
		{"not found permanent", writeErrWithStatus(404), retry.ClassPermanent},
		{"unauthorized permanent", writeErrWithStatus(401), retry.ClassPermanent},
		{"unprocessable permanent", writeErrWithStatus(422), retry.ClassPermanent},
		{"not implemented permanent", writeErrWithStatus(501), retry.ClassPermanent},
		{"rate limit transient", writeErrWithStatus(429), retry.ClassTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyWriteError(tc.err); got != tc.want {
				t.Errorf("ClassifyWriteError = %q, want %q", got, tc.want)
			}
			if want := tc.want == retry.ClassUncertainWrite; IsUncertainWriteError(tc.err) != want {
				t.Errorf("IsUncertainWriteError mismatch for %q", tc.name)
			}
		})
	}
}
