package github

import "time"

// PRDetails holds basic metadata of the pull request
type PRDetails struct {
	Owner     string
	Repo      string
	Number    int
	Title     string
	Body      string
	Author    string
	BaseRef   string
	HeadRef   string
	HeadSHA   string
	CloneURL  string
	CreatedAt time.Time
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
