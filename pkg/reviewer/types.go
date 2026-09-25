package reviewer

import "github.com/thozoz/pr-review-go/pkg/github"

type Finding struct {
	File          string `json:"file"`
	Line          int    `json:"line"`
	Severity      string `json:"severity"` // "CRITICAL", "WARNING", "NOTE"
	Title         string `json:"title"`
	Description   string `json:"description"`
	Suggestion    string `json:"suggestion,omitempty"`
	SuggestedCode string `json:"suggested_code,omitempty"`
}

type CommentTracking struct {
	ThreadKey string `json:"thread_key"`
	Status    string `json:"status"` // "ADDRESSED", "STILL_OPEN", "DISMISSED"
	Note      string `json:"note"`
}

type LLMReviewOutput struct {
	Score            int               `json:"score"` // 0 - 100
	Summary          string            `json:"summary"`
	CommentFollowups []CommentTracking `json:"comment_followups,omitempty"`
	Findings         []Finding         `json:"findings"`
}

type ReviewReport struct {
	PRNumber            int
	PRTitle             string
	HeadSHA             string
	Score               int
	Summary             string
	VerificationSummary string
	RulesSource         string
	DeduplicatedCount   int
	CommentFollowups    []CommentTracking
	Findings            []Finding
	Suggestions         []github.InlineSuggestion
	RawMarkdown         string
}
