package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/google/go-github/v68/github"
	"golang.org/x/oauth2"
)

type Client struct {
	gh         *github.Client
	appAuth    *AppAuth
	graphQLURL string
}

// InlineSuggestion is a one-click replacement attached to a changed PR line.
type InlineSuggestion struct {
	Path string
	Line int
	Body string
}

type FileContent struct {
	Content string
	SHA     string
}

func (c *Client) ghForRepo(ctx context.Context, owner, repo string) (*github.Client, error) {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	if c.appAuth != nil {
		if owner == "" || repo == "" {
			return nil, fmt.Errorf("owner and repo must not be empty for repository-scoped client")
		}
		return c.appAuth.getClient(ctx, owner, repo)
	}
	if c.gh == nil {
		return nil, fmt.Errorf("github client is not initialized")
	}
	return c.gh, nil
}

func NewClient(token string) *Client {
	if token == "" {
		return &Client{gh: github.NewClient(nil)}
	}
	ctx := context.Background()
	var httpClient = oauth2.NewClient(ctx, oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	))
	return &Client{
		gh: github.NewClient(httpClient),
	}
}

// NewTestClient creates a GitHub client pointed at a custom base URL for testing.
func NewTestClient(baseURL string) (*Client, error) {
	gh := github.NewClient(nil)
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	gh.BaseURL = u
	return &Client{gh: gh}, nil
}

// NewTestClientWithToken creates a GitHub client pointed at a custom base URL with a static token for testing.
func NewTestClientWithToken(baseURL, token string) (*Client, error) {
	client := NewClient(token)
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	client.gh.BaseURL = u
	return client, nil
}

// ParseRepoOwner splits "owner/repo" into [owner, repo]
func ParseRepoOwner(repoFlag string) []string {
	parts := strings.Split(strings.TrimSpace(repoFlag), "/")
	if len(parts) != 2 {
		return nil
	}
	return parts
}

// ParsePRURL extracts owner, repo, and PR number from a standard GitHub PR URL
// Supports various URL formats:
// - https://github.com/owner/repo/pull/123
// - http://github.com/owner/repo/pull/123
// - https://github.com/owner/repo/pull/123/
// - https://github.com/owner/repo/pull/123?query=params
func ParsePRURL(prURL string) (owner string, repo string, number int, err error) {
	// Remove query parameters
	if idx := strings.Index(prURL, "?"); idx != -1 {
		prURL = prURL[:idx]
	}

	// Remove trailing slash
	prURL = strings.TrimSuffix(prURL, "/")

	// Remove protocol and domain
	trimmed := strings.TrimPrefix(prURL, "https://github.com/")
	trimmed = strings.TrimPrefix(trimmed, "http://github.com/")

	parts := strings.Split(trimmed, "/")
	if len(parts) < 4 || parts[2] != "pull" {
		return "", "", 0, fmt.Errorf("invalid PR URL format: %s (expected https://github.com/owner/repo/pull/number)", prURL)
	}

	owner = parts[0]
	repo = parts[1]
	_, err = fmt.Sscanf(parts[3], "%d", &number)
	if err != nil {
		return "", "", 0, fmt.Errorf("failed to parse PR number from %s: %w", parts[3], err)
	}

	if owner == "" || repo == "" {
		return "", "", 0, fmt.Errorf("owner or repo is empty in URL: %s", prURL)
	}

	return owner, repo, number, nil
}

func (c *Client) GetPR(ctx context.Context, owner, repo string, number int) (*PRDetails, error) {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	ghClient, err := c.ghForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}

	pr, _, err := ghClient.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("failed to get pull request: %w", err)
	}

	var headRepoOwner string
	var headRepoName string
	var headRepoID int64
	var cloneURL string
	var headRef string
	var headSHA string

	if head := pr.GetHead(); head != nil {
		headRef = head.GetRef()
		headSHA = head.GetSHA()
		if hr := head.GetRepo(); hr != nil {
			cloneURL = hr.GetCloneURL()
			headRepoName = hr.GetName()
			headRepoID = hr.GetID()
			if hrOwner := hr.GetOwner(); hrOwner != nil {
				headRepoOwner = hrOwner.GetLogin()
			}
		}
	}

	return &PRDetails{
		Owner:         owner,
		Repo:          repo,
		Number:        number,
		Title:         pr.GetTitle(),
		Body:          pr.GetBody(),
		Author:        pr.GetUser().GetLogin(),
		BaseRef:       pr.GetBase().GetRef(),
		BaseSHA:       pr.GetBase().GetSHA(),
		HeadRef:       headRef,
		HeadSHA:       headSHA,
		HeadRepoOwner: headRepoOwner,
		HeadRepoName:  headRepoName,
		HeadRepoID:    headRepoID,
		CloneURL:      cloneURL,
		CreatedAt:     pr.GetCreatedAt().Time,
	}, nil
}

// GetRepoID returns the numeric GitHub repository ID for owner/repo.
func (c *Client) GetRepoID(ctx context.Context, owner, repo string) (int64, error) {
	ghClient, err := c.ghForRepo(ctx, owner, repo)
	if err != nil {
		return 0, err
	}
	r, _, err := ghClient.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return 0, fmt.Errorf("failed to get repository %s/%s: %w", owner, repo, err)
	}
	if r.GetID() == 0 {
		return 0, fmt.Errorf("%w: repository %s/%s has zero ID", ErrInvalidRepoID, owner, repo)
	}
	return r.GetID(), nil
}

// CreateRetrievalCredential creates a short-lived App token scoped to exactly one head repository
// with contents:read permission. If the client is configured with a PAT instead of a GitHub App,
// it returns ErrRetrievalAuthUnavailable; broad PAT substitution is explicitly denied (D-10, SAFE-01).
func (c *Client) CreateRetrievalCredential(ctx context.Context, owner, repo string, repoID int64) (*RetrievalCredential, error) {
	if c.appAuth == nil {
		return nil, ErrRetrievalAuthUnavailable
	}
	return c.appAuth.CreateRetrievalCredential(ctx, owner, repo, repoID)
}

// RevokeRetrievalCredential revokes the scoped App retrieval token and zeroes it in memory.
func (c *Client) RevokeRetrievalCredential(ctx context.Context, cred *RetrievalCredential) error {
	if c.appAuth == nil {
		if cred != nil {
			cred.Zeroize()
		}
		return nil
	}
	return c.appAuth.RevokeRetrievalCredential(ctx, cred)
}

// AppAuth returns the underlying AppAuth instance if configured, or nil.
func (c *Client) AppAuth() *AppAuth {
	return c.appAuth
}

// NewScopedClient returns a new Client configured with the provided access token
// while preserving custom BaseURL if set on this client.
func (c *Client) NewScopedClient(token string) *Client {
	client := NewClient(token)
	if c != nil && c.gh != nil && c.gh.BaseURL != nil {
		client.gh.BaseURL = c.gh.BaseURL
	}
	if c != nil {
		client.graphQLURL = c.graphQLURL
	}
	return client
}

// GetCommit retrieves a commit by full SHA from the repository.
func (c *Client) GetCommit(ctx context.Context, owner, repo, sha string) (*github.RepositoryCommit, error) {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	ghClient, err := c.ghForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	commit, _, err := ghClient.Repositories.GetCommit(ctx, owner, repo, sha, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get commit %s: %w", sha, err)
	}
	return commit, nil
}

// GetTree retrieves a git tree by SHA, optionally recursive.
func (c *Client) GetTree(ctx context.Context, owner, repo, sha string, recursive bool) (*github.Tree, error) {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	ghClient, err := c.ghForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	tree, _, err := ghClient.Git.GetTree(ctx, owner, repo, sha, recursive)
	if err != nil {
		return nil, fmt.Errorf("failed to get tree for %s: %w", sha, err)
	}
	return tree, nil
}

// GetBlob retrieves a git blob by SHA.
func (c *Client) GetBlob(ctx context.Context, owner, repo, sha string) (*github.Blob, error) {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	ghClient, err := c.ghForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	blob, _, err := ghClient.Git.GetBlob(ctx, owner, repo, sha)
	if err != nil {
		return nil, fmt.Errorf("failed to get blob %s: %w", sha, err)
	}
	return blob, nil
}

// GetBlobRaw retrieves raw bytes of a git blob by SHA.
func (c *Client) GetBlobRaw(ctx context.Context, owner, repo, sha string) ([]byte, error) {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	ghClient, err := c.ghForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	data, _, err := ghClient.Git.GetBlobRaw(ctx, owner, repo, sha)
	if err != nil {
		return nil, fmt.Errorf("failed to get raw blob %s: %w", sha, err)
	}
	return data, nil
}

func (c *Client) GetRawDiff(ctx context.Context, owner, repo string, number int) (string, error) {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	ghClient, err := c.ghForRepo(ctx, owner, repo)
	if err != nil {
		return "", err
	}

	diff, _, err := ghClient.PullRequests.GetRaw(ctx, owner, repo, number, github.RawOptions{
		Type: github.Diff,
	})
	if err != nil {
		return "", fmt.Errorf("failed to get raw diff: %w", err)
	}
	return diff, nil
}

func (c *Client) GetComments(ctx context.Context, owner, repo string, number int) ([]Comment, []DiscussionThread, error) {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	ghClient, err := c.ghForRepo(ctx, owner, repo)
	if err != nil {
		return nil, nil, err
	}

	// 1. Fetch general issue comments with pagination
	var generalComments []Comment
	opt := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	for {
		issueComments, resp, err := ghClient.Issues.ListComments(ctx, owner, repo, number, opt)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get issue comments: %w", err)
		}
		for _, ic := range issueComments {
			generalComments = append(generalComments, Comment{
				ID:        ic.GetID(),
				User:      ic.GetUser().GetLogin(),
				Body:      ic.GetBody(),
				CreatedAt: ic.GetCreatedAt().Time,
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}

	// 2. Fetch inline review comments with pagination
	var reviewComments []*github.PullRequestComment
	prOpt := &github.PullRequestListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	for {
		rcPage, resp, err := ghClient.PullRequests.ListComments(ctx, owner, repo, number, prOpt)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get review comments: %w", err)
		}
		reviewComments = append(reviewComments, rcPage...)
		if resp.NextPage == 0 {
			break
		}
		prOpt.Page = resp.NextPage
	}

	// Group inline comments into threads by root comment or file+line
	threadMap := make(map[string]*DiscussionThread)
	for _, rc := range reviewComments {
		comm := Comment{
			ID:          rc.GetID(),
			User:        rc.GetUser().GetLogin(),
			Body:        rc.GetBody(),
			CreatedAt:   rc.GetCreatedAt().Time,
			Path:        rc.GetPath(),
			Line:        rc.GetLine(),
			DiffHunk:    rc.GetDiffHunk(),
			InReplyToID: rc.GetInReplyTo(),
		}

		// Use a more robust key that includes start line for multi-line comments
		key := fmt.Sprintf("%s:%d:%d", comm.Path, comm.Line, comm.InReplyToID)
		if comm.InReplyToID == 0 {
			key = fmt.Sprintf("%s:%d", comm.Path, comm.Line)
		}

		if thread, exists := threadMap[key]; exists {
			thread.Comments = append(thread.Comments, comm)
		} else {
			threadMap[key] = &DiscussionThread{
				Path:     comm.Path,
				Line:     comm.Line,
				DiffHunk: comm.DiffHunk,
				Comments: []Comment{comm},
			}
		}
	}

	var threads []DiscussionThread
	for _, thread := range threadMap {
		threads = append(threads, *thread)
	}

	return generalComments, threads, nil
}

// CanWriteRepository checks the comment author's current repository permission.
func (c *Client) CanWriteRepository(ctx context.Context, owner, repo, username string) (bool, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return false, nil
	}
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	ghClient, err := c.ghForRepo(ctx, owner, repo)
	if err != nil {
		return false, err
	}
	permission, _, err := ghClient.Repositories.GetPermissionLevel(ctx, owner, repo, username)
	if err != nil {
		return false, err
	}
	if permission == nil {
		return false, nil
	}
	switch permission.GetPermission() {
	case "admin", "maintain", "write":
		return true, nil
	default:
		return false, nil
	}
}

func (c *Client) PostComment(ctx context.Context, owner, repo string, number int, body string) error {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	ghClient, err := c.ghForRepo(ctx, owner, repo)
	if err != nil {
		return err
	}
	_, _, err = ghClient.Issues.CreateComment(ctx, owner, repo, number, &github.IssueComment{
		Body: github.Ptr(body),
	})
	return err
}

// UpdatePRBody replaces only the PR description, not its title or other metadata.
func (c *Client) UpdatePRBody(ctx context.Context, owner, repo string, number int, body string) error {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	ghClient, err := c.ghForRepo(ctx, owner, repo)
	if err != nil {
		return err
	}
	_, _, err = ghClient.PullRequests.Edit(ctx, owner, repo, number, &github.PullRequest{Body: github.Ptr(body)})
	return err
}

func (c *Client) GetFileContent(ctx context.Context, owner, repo, path, ref string) (*FileContent, error) {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	ghClient, err := c.ghForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	file, _, _, err := ghClient.Repositories.GetContents(ctx, owner, repo, path, &github.RepositoryContentGetOptions{Ref: ref})
	if err != nil {
		return nil, err
	}
	content, err := file.GetContent()
	if err != nil {
		return nil, err
	}
	return &FileContent{Content: content, SHA: file.GetSHA()}, nil
}

// UpdateFile creates one commit on branch containing only path.
func (c *Client) UpdateFile(ctx context.Context, owner, repo, path, branch, sha, content, message string) (string, error) {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	ghClient, err := c.ghForRepo(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	result, _, err := ghClient.Repositories.UpdateFile(ctx, owner, repo, path, &github.RepositoryContentFileOptions{
		Message: github.Ptr(message),
		Content: []byte(content),
		SHA:     github.Ptr(sha),
		Branch:  github.Ptr(branch),
	})
	if err != nil {
		return "", err
	}
	return result.Commit.GetSHA(), nil
}

// PostSuggestions creates one COMMENT review containing inline GitHub suggestion blocks.
func (c *Client) PostSuggestions(ctx context.Context, owner, repo string, number int, commitSHA string, suggestions []InlineSuggestion) error {
	if len(suggestions) == 0 {
		return nil
	}

	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	ghClient, err := c.ghForRepo(ctx, owner, repo)
	if err != nil {
		return err
	}

	comments := make([]*github.DraftReviewComment, 0, len(suggestions))
	for _, suggestion := range suggestions {
		comments = append(comments, &github.DraftReviewComment{
			Path: github.Ptr(suggestion.Path),
			Line: github.Ptr(suggestion.Line),
			Side: github.Ptr("RIGHT"),
			Body: github.Ptr(suggestion.Body),
		})
	}

	_, _, err = ghClient.PullRequests.CreateReview(ctx, owner, repo, number, &github.PullRequestReviewRequest{
		CommitID: github.Ptr(commitSHA),
		Event:    github.Ptr("COMMENT"),
		Body:     github.Ptr("## 🤖 PR Improvement Suggestions\n\nOne-click suggestions for changed lines."),
		Comments: comments,
	})
	return err
}

func (c *Client) GetLabels(ctx context.Context, owner, repo string, number int) ([]string, error) {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	ghClient, err := c.ghForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	labels, _, err := ghClient.Issues.ListLabelsByIssue(ctx, owner, repo, number, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list labels: %w", err)
	}

	var names []string
	for _, l := range labels {
		names = append(names, l.GetName())
	}
	return names, nil
}

func (c *Client) AddLabels(ctx context.Context, owner, repo string, number int, labels []string) error {
	if len(labels) == 0 {
		return nil
	}
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	ghClient, err := c.ghForRepo(ctx, owner, repo)
	if err != nil {
		return err
	}
	_, _, err = ghClient.Issues.AddLabelsToIssue(ctx, owner, repo, number, labels)
	if err != nil {
		return fmt.Errorf("failed to add labels: %w", err)
	}
	return nil
}

// SetGraphQLURL sets an explicit GraphQL endpoint override (for testing or custom enterprise setups).
func (c *Client) SetGraphQLURL(u string) {
	c.graphQLURL = u
}

func (c *Client) graphQLEndpointForClient(ghClient *github.Client) string {
	if c != nil && c.graphQLURL != "" {
		return c.graphQLURL
	}
	if ghClient != nil && ghClient.BaseURL != nil {
		base := ghClient.BaseURL.String()
		if strings.HasSuffix(base, "/api/v3/") {
			return strings.TrimSuffix(base, "/api/v3/") + "/api/graphql"
		}
		return strings.TrimSuffix(base, "/") + "/graphql"
	}
	return "https://api.github.com/graphql"
}

var (
	// ErrHeadMoved indicates that the branch head moved away from expectedHeadOid.
	ErrHeadMoved = errors.New("branch head moved")
	// ErrInvalidCommitOID indicates that a commit SHA is not a 40- or 64-character hex string.
	ErrInvalidCommitOID = errors.New("commit SHA must be 40 or 64 hex characters (full OID required)")
)

// HeadMovedError indicates that an expected-head conditional commit mutation failed
// because the branch head on GitHub moved away from the expected commit OID (D-16, SAFE-03).
type HeadMovedError struct {
	ExpectedHeadOID string
	ActualHeadOID   string
	Message         string
}

func (e *HeadMovedError) Error() string {
	if e.ActualHeadOID != "" {
		return fmt.Sprintf("branch head moved: expected %s, found %s (%s)", e.ExpectedHeadOID, e.ActualHeadOID, e.Message)
	}
	return fmt.Sprintf("branch head moved: expected %s (%s)", e.ExpectedHeadOID, e.Message)
}

func (e *HeadMovedError) Is(target error) bool {
	return target == ErrHeadMoved
}

// ValidateCommitOID ensures the commit hash is a full 40-char (SHA-1) or 64-char (SHA-256) hex string.
// Short prefixes, non-hex characters, and empty strings are rejected (SAFE-01, SAFE-03).
func ValidateCommitOID(sha string) error {
	shaLen := len(sha)
	if shaLen != 40 && shaLen != 64 {
		return fmt.Errorf("%w: got length %d (%q)", ErrInvalidCommitOID, shaLen, sha)
	}
	for i := 0; i < shaLen; i++ {
		c := sha[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return fmt.Errorf("%w: invalid character %q at index %d", ErrInvalidCommitOID, string(c), i)
		}
	}
	return nil
}

// GetDiffAtCommits retrieves the diff between baseOID and headOID using the GitHub commit comparison endpoint.
// It requires complete 40 or 64 hex character OIDs (SAFE-03) and sends the GitHub diff media type.
// Never generates from the mutable PR diff endpoint to prevent ABA head movement risks (D-16).
func (c *Client) GetDiffAtCommits(ctx context.Context, owner, repo, baseOID, headOID string) (string, error) {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	if owner == "" || repo == "" {
		return "", fmt.Errorf("owner and repo must not be empty")
	}
	if err := ValidateCommitOID(baseOID); err != nil {
		return "", fmt.Errorf("invalid base commit OID: %w", err)
	}
	if err := ValidateCommitOID(headOID); err != nil {
		return "", fmt.Errorf("invalid head commit OID: %w", err)
	}

	ghClient, err := c.ghForRepo(ctx, owner, repo)
	if err != nil {
		return "", err
	}

	diff, _, err := ghClient.Repositories.CompareCommitsRaw(ctx, owner, repo, baseOID, headOID, github.RawOptions{
		Type: github.Diff,
	})
	if err != nil {
		return "", fmt.Errorf("failed to get diff between %s and %s: %w", baseOID, headOID, err)
	}
	return diff, nil
}

const maxGraphQLResponseBytes = 1 << 20 // 1 MiB bound for response decoding

type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

type graphQLError struct {
	Message    string         `json:"message"`
	Type       string         `json:"type,omitempty"`
	Path       []any          `json:"path,omitempty"`
	Locations  []any          `json:"locations,omitempty"`
	Extensions map[string]any `json:"extensions,omitempty"`
}

type graphQLResponse struct {
	Data *struct {
		CreateCommitOnBranch *struct {
			Commit *struct {
				OID string `json:"oid"`
				URL string `json:"url"`
			} `json:"commit"`
			Ref *struct {
				Name   string `json:"name"`
				Target *struct {
					OID string `json:"oid"`
				} `json:"target"`
			} `json:"ref"`
		} `json:"createCommitOnBranch"`
	} `json:"data"`
	Errors []graphQLError `json:"errors"`
}

var headMovedPattern = regexp.MustCompile(`(?i)(expected branch to point to|expectedheadoid|head moved|head branch was modified|does not match the current head|not at expected head|expected.*point|conflict.*head)`)
var actualHeadPattern = regexp.MustCompile(`(?:points to|found)\s+["']?([0-9a-fA-F]{40,64})["']?`)

// CommitFileAtExpectedHead creates a single commit on branch containing only path with content,
// conditional on the branch head matching expectedHeadOID exactly, using GitHub GraphQL createCommitOnBranch (D-16, SAFE-03).
// If the branch head does not match expectedHeadOID, it maps the conflict to *HeadMovedError.
func (c *Client) CommitFileAtExpectedHead(ctx context.Context, owner, repo, branch, expectedHeadOID, path, content, message string) (string, error) {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	branch = strings.TrimSpace(branch)
	path = strings.TrimSpace(path)
	message = strings.TrimSpace(message)

	if owner == "" || repo == "" {
		return "", fmt.Errorf("owner and repo must not be empty")
	}
	if branch == "" {
		return "", fmt.Errorf("branch must not be empty")
	}
	if path == "" {
		return "", fmt.Errorf("file path must not be empty")
	}
	if message == "" {
		return "", fmt.Errorf("commit message must not be empty")
	}
	if err := ValidateCommitOID(expectedHeadOID); err != nil {
		return "", fmt.Errorf("invalid expectedHeadOID: %w", err)
	}

	ghClient, err := c.ghForRepo(ctx, owner, repo)
	if err != nil {
		return "", err
	}

	branchName := strings.TrimPrefix(branch, "refs/heads/")
	cleanPath := strings.TrimPrefix(path, "/")

	reqPayload := graphQLRequest{
		Query: `mutation CreateCommitOnBranch($input: CreateCommitOnBranchInput!) {
  createCommitOnBranch(input: $input) {
    commit {
      oid
      url
    }
    ref {
      name
      target {
        oid
      }
    }
  }
}`,
		Variables: map[string]any{
			"input": map[string]any{
				"branch": map[string]any{
					"repositoryNameWithOwner": fmt.Sprintf("%s/%s", owner, repo),
					"branchName":              branchName,
				},
				"expectedHeadOid": expectedHeadOID,
				"message": map[string]any{
					"headline": message,
				},
				"fileChanges": map[string]any{
					"additions": []map[string]any{
						{
							"path":     cleanPath,
							"contents": base64.StdEncoding.EncodeToString([]byte(content)),
						},
					},
				},
			},
		},
	}

	reqBody, err := json.Marshal(reqPayload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal graphql mutation request: %w", err)
	}

	endpoint := c.graphQLEndpointForClient(ghClient)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("failed to create graphql http request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	httpClient := ghClient.Client()
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("graphql request failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxGraphQLResponseBytes))
	if err != nil {
		return "", fmt.Errorf("failed to read bounded graphql response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		trimmedBody := strings.TrimSpace(string(bodyBytes))
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return "", fmt.Errorf("graphql auth error (status %d): %s", resp.StatusCode, trimmedBody)
		}
		return "", fmt.Errorf("graphql unexpected http status %d: %s", resp.StatusCode, trimmedBody)
	}

	var gqlResp graphQLResponse
	if err := json.Unmarshal(bodyBytes, &gqlResp); err != nil {
		return "", fmt.Errorf("failed to decode graphql response: %w", err)
	}

	if len(gqlResp.Errors) > 0 {
		var errorMsgs []string
		isHeadMoved := false
		var actualHead string

		for _, e := range gqlResp.Errors {
			errorMsgs = append(errorMsgs, e.Message)
			if headMovedPattern.MatchString(e.Message) {
				isHeadMoved = true
				if match := actualHeadPattern.FindStringSubmatch(e.Message); len(match) > 1 {
					actualHead = match[1]
				}
			}
		}
		combinedErrMsg := strings.Join(errorMsgs, "; ")
		if isHeadMoved {
			return "", &HeadMovedError{
				ExpectedHeadOID: expectedHeadOID,
				ActualHeadOID:   actualHead,
				Message:         combinedErrMsg,
			}
		}
		return "", fmt.Errorf("graphql error: %s", combinedErrMsg)
	}

	if gqlResp.Data == nil || gqlResp.Data.CreateCommitOnBranch == nil {
		return "", fmt.Errorf("graphql response missing createCommitOnBranch data")
	}
	mutationData := gqlResp.Data.CreateCommitOnBranch
	if mutationData.Commit == nil || mutationData.Commit.OID == "" {
		return "", fmt.Errorf("graphql response missing created commit OID")
	}
	if err := ValidateCommitOID(mutationData.Commit.OID); err != nil {
		return "", fmt.Errorf("graphql response contains invalid commit OID: %w", err)
	}
	if mutationData.Ref == nil {
		return "", fmt.Errorf("graphql response missing ref data")
	}

	return mutationData.Commit.OID, nil
}
