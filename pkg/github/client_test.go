package github

import (
	"testing"
)

func TestParsePRURL(t *testing.T) {
	tests := []struct {
		url        string
		wantOwner  string
		wantRepo   string
		wantNumber int
		wantErr    bool
	}{
		{
			url:        "https://github.com/thozoz/pr-review-go/pull/42",
			wantOwner:  "thozoz",
			wantRepo:   "pr-review-go",
			wantNumber: 42,
			wantErr:    false,
		},
		{
			url:        "http://github.com/golang/go/pull/100",
			wantOwner:  "golang",
			wantRepo:   "go",
			wantNumber: 100,
			wantErr:    false,
		},
		{
			url:     "https://github.com/invalid/url",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		owner, repo, num, err := ParsePRURL(tt.url)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParsePRURL(%q) error = %v, wantErr %v", tt.url, err, tt.wantErr)
			continue
		}
		if !tt.wantErr {
			if owner != tt.wantOwner || repo != tt.wantRepo || num != tt.wantNumber {
				t.Errorf("ParsePRURL(%q) = (%s, %s, %d), want (%s, %s, %d)",
					tt.url, owner, repo, num, tt.wantOwner, tt.wantRepo, tt.wantNumber)
			}
		}
	}
}
