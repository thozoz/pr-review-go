package github

import (
	"strings"
	"time"
)

// ServiceActor represents the verified authenticated identity of the service.
type ServiceActor struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

// PRDetails holds basic metadata of the pull request
type PRDetails struct {
	Owner         string
	Repo          string
	Number        int
	Title         string
	Body          string
	Author        string
	BaseRef       string
	BaseSHA       string // Captured immutable base commit OID (D-16, SAFE-03)
	HeadRef       string
	HeadSHA       string
	HeadRepoOwner string
	HeadRepoName  string
	HeadRepoID    int64
	CloneURL      string
	CreatedAt     time.Time
}

// IsFork returns true if the pull request originates from a fork or if head repository metadata is missing.
func (p *PRDetails) IsFork() bool {
	if p == nil || p.HeadRepoOwner == "" || p.HeadRepoName == "" {
		return true
	}
	return !strings.EqualFold(p.HeadRepoOwner, p.Owner) || !strings.EqualFold(p.HeadRepoName, p.Repo)
}

// Comment represents either an issue comment or inline review comment
type Comment struct {
	ID        int64     `json:"id"`
	User      string    `json:"user"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`

	// Inline review comment specific fields
	Path        string `json:"path,omitempty"`
	Line        int    `json:"line,omitempty"`
	DiffHunk    string `json:"diff_hunk,omitempty"`
	InReplyToID int64  `json:"in_reply_to_id,omitempty"`
}

// DiscussionThread groups inline comments on the same code location
type DiscussionThread struct {
	Path     string    `json:"path"`
	Line     int       `json:"line"`
	DiffHunk string    `json:"diff_hunk"`
	Comments []Comment `json:"comments"`
}
