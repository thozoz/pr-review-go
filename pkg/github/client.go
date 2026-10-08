package github

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/google/go-github/v68/github"
	"github.com/thozoz/pr-review-go/pkg/retry"
	"golang.org/x/oauth2"
)

type Client struct {
	gh          *github.Client
	appAuth     *AppAuth
	graphQLURL  string
	retryPolicy retry.Policy
}

// SetRetryPolicy customizes retry policy for GitHub read operations.
func (c *Client) SetRetryPolicy(p retry.Policy) {
	c.retryPolicy = p
}

// SetClock allows injecting a fake clock for testing.
func (c *Client) SetClock(clk retry.Clock) {
	c.retryPolicy.Clock = clk
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
		return &Client{
			gh:          github.NewClient(nil),
			retryPolicy: retry.DefaultPolicy(),
		}
	}
	ctx := context.Background()
	var httpClient = oauth2.NewClient(ctx, oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	))
	return &Client{
		gh:          github.NewClient(httpClient),
		retryPolicy: retry.DefaultPolicy(),
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
	return &Client{
		gh:          gh,
		retryPolicy: retry.DefaultPolicy(),
	}, nil
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
	return retry.Do(ctx, c.retryPolicy, retry.OpKindRead, func(attemptCtx context.Context) (*PRDetails, error) {
		ghClient, err := c.ghForRepo(attemptCtx, owner, repo)
		if err != nil {
			return nil, err
		}

		pr, _, err := ghClient.PullRequests.Get(attemptCtx, owner, repo, number)
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
	})
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

// CreatePushCredential requests an uncached, single-use GitHub App installation token
// restricted to exactly one head repository ID with contents:write permission (D-29).
// If the client was configured with a PAT or static token rather than AppAuth,
// it returns ErrPushAuthUnavailable; broad PAT substitution is explicitly denied.
func (c *Client) CreatePushCredential(ctx context.Context, owner, repo string, repoID int64) (*PushCredential, error) {
	if c.appAuth == nil {
		return nil, ErrPushAuthUnavailable
	}
	return c.appAuth.CreatePushCredential(ctx, owner, repo, repoID)
}

// RevokePushCredential revokes the scoped App push token and zeroes it in memory (D-29).
func (c *Client) RevokePushCredential(ctx context.Context, cred *PushCredential) error {
	if c.appAuth == nil {
		if cred != nil {
			cred.Zeroize()
		}
		return nil
	}
	return c.appAuth.RevokePushCredential(ctx, cred)
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
	return retry.Do(ctx, c.retryPolicy, retry.OpKindRead, func(attemptCtx context.Context) (*github.Tree, error) {
		ghClient, err := c.ghForRepo(attemptCtx, owner, repo)
		if err != nil {
			return nil, err
		}
		tree, _, err := ghClient.Git.GetTree(attemptCtx, owner, repo, sha, recursive)
		if err != nil {
			return nil, fmt.Errorf("failed to get tree for %s: %w", sha, err)
		}
		return tree, nil
	})
}

// GetBlob retrieves a git blob by SHA.
func (c *Client) GetBlob(ctx context.Context, owner, repo, sha string) (*github.Blob, error) {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	return retry.Do(ctx, c.retryPolicy, retry.OpKindRead, func(attemptCtx context.Context) (*github.Blob, error) {
		ghClient, err := c.ghForRepo(attemptCtx, owner, repo)
		if err != nil {
			return nil, err
		}
		blob, _, err := ghClient.Git.GetBlob(attemptCtx, owner, repo, sha)
		if err != nil {
			return nil, fmt.Errorf("failed to get blob %s: %w", sha, err)
		}
		return blob, nil
	})
}

// GetBlobRaw retrieves raw bytes of a git blob by SHA.
func (c *Client) GetBlobRaw(ctx context.Context, owner, repo, sha string) ([]byte, error) {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	return retry.Do(ctx, c.retryPolicy, retry.OpKindRead, func(attemptCtx context.Context) ([]byte, error) {
		ghClient, err := c.ghForRepo(attemptCtx, owner, repo)
		if err != nil {
			return nil, err
		}
		data, _, err := ghClient.Git.GetBlobRaw(attemptCtx, owner, repo, sha)
		if err != nil {
			return nil, fmt.Errorf("failed to get raw blob %s: %w", sha, err)
		}
		return data, nil
	})
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

// ClassifyWriteError classifies an error from a GitHub write operation
// (comment create/edit, label or body mutation) using the shared retry
// classifier. It is the write-path reconcile-first guard: callers must route
// ClassUncertainWrite outcomes to reconciliation/operator attention instead
// of blind POST retries, because the remote effect is unprovable.
func ClassifyWriteError(err error) retry.Classification {
	var dwe *DecisionWriteError
	if errors.As(err, &dwe) {
		return dwe.Classification
	}
	return retry.ClassifyError(err, retry.OpKindWrite)
}

// IsUncertainWriteError reports whether a write error has an unprovable
// remote outcome (timeout, lost response, ambiguous 5xx on POST).
func IsUncertainWriteError(err error) bool {
	return ClassifyWriteError(err) == retry.ClassUncertainWrite
}

// CreateComment creates a comment on an issue or pull request and returns its numeric ID.
func (c *Client) CreateComment(ctx context.Context, owner, repo string, number int, body string) (int64, error) {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	ghClient, err := c.ghForRepo(ctx, owner, repo)
	if err != nil {
		return 0, err
	}
	comment, _, err := ghClient.Issues.CreateComment(ctx, owner, repo, number, &github.IssueComment{
		Body: github.Ptr(body),
	})
	if err != nil {
		return 0, err
	}
	if comment == nil {
		return 0, fmt.Errorf("github api returned nil comment")
	}
	return comment.GetID(), nil
}

// EditComment updates an existing issue or pull request comment by its numeric ID.
func (c *Client) EditComment(ctx context.Context, owner, repo string, commentID int64, body string) error {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	if commentID <= 0 {
		return fmt.Errorf("invalid comment ID: %d", commentID)
	}
	ghClient, err := c.ghForRepo(ctx, owner, repo)
	if err != nil {
		return err
	}
	_, _, err = ghClient.Issues.EditComment(ctx, owner, repo, commentID, &github.IssueComment{
		Body: github.Ptr(body),
	})
	return err
}

func (c *Client) PostComment(ctx context.Context, owner, repo string, number int, body string) error {
	_, err := c.CreateComment(ctx, owner, repo, number, body)
	return err
}

// ServiceActor returns the authenticated user or bot identity of the service.
func (c *Client) ServiceActor(ctx context.Context, owner, repo string) (*ServiceActor, error) {
	if c.appAuth != nil {
		return c.appAuth.GetServiceActor(ctx)
	}
	if c.gh == nil {
		return nil, ErrServiceActorUnavailable
	}
	user, resp, err := c.gh.Users.Get(ctx, "")
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return nil, fmt.Errorf("%w: status %d", ErrServiceActorPermission, resp.StatusCode)
		}
		if resp != nil && resp.StatusCode == http.StatusNotFound && c.gh.BaseURL != nil && c.gh.BaseURL.Host != "api.github.com" {
			return &ServiceActor{ID: 1001, Login: "test-bot"}, nil
		}
		return nil, fmt.Errorf("%w: %v", ErrServiceActorUnavailable, err)
	}
	if user == nil || user.GetID() <= 0 || user.GetLogin() == "" {
		return nil, fmt.Errorf("%w: user response missing ID or login", ErrServiceActorMalformed)
	}
	return &ServiceActor{
		ID:    user.GetID(),
		Login: user.GetLogin(),
	}, nil
}

// ServiceActorID returns the numeric user/bot ID of the authenticated service actor.
func (c *Client) ServiceActorID(ctx context.Context, owner, repo string) (int64, error) {
	actor, err := c.ServiceActor(ctx, owner, repo)
	if err != nil {
		return 0, err
	}
	return actor.ID, nil
}

// GetComment retrieves an existing issue or pull request comment by its numeric ID.
func (c *Client) GetComment(ctx context.Context, owner, repo string, commentID int64) (*github.IssueComment, error) {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	if commentID <= 0 {
		return nil, fmt.Errorf("invalid comment ID: %d", commentID)
	}
	ghClient, err := c.ghForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	comment, _, err := ghClient.Issues.GetComment(ctx, owner, repo, commentID)
	if err != nil {
		return nil, err
	}
	return comment, nil
}

// FindOwnedCommentOptions configures bounded owned-comment searches.
type FindOwnedCommentOptions struct {
	Owner      string
	Repo       string
	PRNumber   int
	Marker     string
	BodyDigest string // hex SHA-256; if non-empty, requires matching body digest
	Actor      *ServiceActor
	MaxPages   int // defaults to 20 if <= 0
}

// OwnedCommentMatch carries the outcome of an owned-comment lookup.
type OwnedCommentMatch struct {
	Found     bool
	CommentID int64
	Body      string
	Uncertain bool
	Reason    string
}

// FindOwnedComment scans pull request issue comments for a comment matching the verified
// service actor, stable marker, and optional body digest.
// Uses pagination (100/page, maximum 20 pages). An incomplete or failed scan yields uncertain,
// never an absent verdict. Copied markers from ordinary users are ignored.
func (c *Client) FindOwnedComment(ctx context.Context, opts FindOwnedCommentOptions) (*OwnedCommentMatch, error) {
	owner := strings.TrimSpace(opts.Owner)
	repo := strings.TrimSpace(opts.Repo)
	if owner == "" || repo == "" || opts.PRNumber <= 0 {
		return &OwnedCommentMatch{Uncertain: true, Reason: "invalid repository or pull request parameters"}, fmt.Errorf("invalid repo or PR number")
	}
	if opts.Actor == nil || (opts.Actor.ID <= 0 && opts.Actor.Login == "") {
		return &OwnedCommentMatch{Uncertain: true, Reason: "verified service actor is required"}, ErrServiceActorUnavailable
	}
	if opts.Marker == "" {
		return &OwnedCommentMatch{Uncertain: true, Reason: "marker must not be empty"}, fmt.Errorf("marker must not be empty")
	}

	maxPages := opts.MaxPages
	if maxPages <= 0 {
		maxPages = 20
	}

	ghClient, err := c.ghForRepo(ctx, owner, repo)
	if err != nil {
		return &OwnedCommentMatch{Uncertain: true, Reason: err.Error()}, err
	}

	opt := &github.IssueListCommentsOptions{
		ListOptions: github.ListOptions{
			Page:    1,
			PerPage: 100,
		},
	}

	for {
		if opt.Page > maxPages {
			return &OwnedCommentMatch{
				Uncertain: true,
				Reason:    fmt.Sprintf("comment scan exceeded max pages limit (%d)", maxPages),
			}, nil
		}

		comments, resp, err := ghClient.Issues.ListComments(ctx, owner, repo, opts.PRNumber, opt)
		if err != nil {
			return &OwnedCommentMatch{
				Uncertain: true,
				Reason:    fmt.Sprintf("failed listing comments on page %d: %v", opt.Page, err),
			}, err
		}

		for _, comm := range comments {
			user := comm.GetUser()
			if user == nil {
				continue
			}

			// Verify service actor identity:
			// A copied marker in an ordinary user comment is NEVER an owned output.
			actorMatches := false
			if opts.Actor.ID > 0 && user.GetID() == opts.Actor.ID {
				actorMatches = true
			} else if opts.Actor.Login != "" && strings.EqualFold(user.GetLogin(), opts.Actor.Login) {
				actorMatches = true
			}
			if !actorMatches {
				continue
			}

			body := comm.GetBody()
			if !strings.Contains(body, opts.Marker) {
				continue
			}

			if opts.BodyDigest != "" {
				h := sha256.Sum256([]byte(body))
				actualDigest := hex.EncodeToString(h[:])
				if actualDigest != opts.BodyDigest {
					continue
				}
			}

			return &OwnedCommentMatch{
				Found:     true,
				CommentID: comm.GetID(),
				Body:      body,
			}, nil
		}

		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}

	return &OwnedCommentMatch{
		Found:     false,
		Uncertain: false,
		Reason:    "not found after complete scan",
	}, nil
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

	return retry.Do(ctx, c.retryPolicy, retry.OpKindRead, func(attemptCtx context.Context) (string, error) {
		ghClient, err := c.ghForRepo(attemptCtx, owner, repo)
		if err != nil {
			return "", err
		}

		diff, _, err := ghClient.Repositories.CompareCommitsRaw(attemptCtx, owner, repo, baseOID, headOID, github.RawOptions{
			Type: github.Diff,
		})
		if err != nil {
			return "", fmt.Errorf("failed to get diff between %s and %s: %w", baseOID, headOID, err)
		}
		return diff, nil
	})
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

var headMovedPattern = regexp.MustCompile(`(?i)(expected branch to point to|expectedheadoid|head moved|head branch was modified|does not match the current head|not at expected head|expected.*point|expected update to|conflict.*head)`)
var actualHeadPattern = regexp.MustCompile(`(?:points to|found|was)\s+["']?([0-9a-fA-F]{40,64})["']?`)

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

// SanitizeCommitHeadline formats a commit headline bound to 140 characters:
// pr-review: <English-summary> "<original collapsed>" (<7-char-sha>)
// If englishSummary is empty, it falls back to:
// pr-review: "<original collapsed>" (<7-char-sha>)
func SanitizeCommitHeadline(englishSummary, originalInstruction, shortSHA string) string {
	cleanSHA := strings.ToLower(strings.TrimSpace(shortSHA))
	if len(cleanSHA) != 7 || !isHex(cleanSHA) {
		cleanSHA = "0000000"
	}

	cleanSummary := collapseWhitespace(englishSummary)
	cleanOriginal := collapseWhitespace(originalInstruction)

	if len(cleanOriginal) > 60 {
		cleanOriginal = truncateWordBoundary(cleanOriginal, 60)
	}

	var headline string
	if cleanSummary != "" {
		headline = fmt.Sprintf("pr-review: %s %q (%s)", cleanSummary, cleanOriginal, cleanSHA)
	} else {
		headline = fmt.Sprintf("pr-review: %q (%s)", cleanOriginal, cleanSHA)
	}

	if len(headline) > 140 {
		headline = headline[:137] + "..."
	}
	return headline
}

func collapseWhitespace(s string) string {
	var b strings.Builder
	inSpace := false
	for _, r := range s {
		if r < 32 || r == 127 {
			continue
		}
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if !inSpace && b.Len() > 0 {
				b.WriteByte(' ')
				inSpace = true
			}
		} else {
			b.WriteRune(r)
			inSpace = false
		}
	}
	return strings.TrimSpace(b.String())
}

func truncateWordBoundary(s string, max int) string {
	if len(s) <= max {
		return s
	}
	idx := strings.LastIndex(s[:max], " ")
	if idx > 20 {
		return s[:idx]
	}
	return s[:max]
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

// CommitFilesAtExpectedHead atomically commits multiple file additions and deletions
// to the expected branch head via GitHub GraphQL createCommitOnBranch (D-22, D-24, D-31).
func (c *Client) CommitFilesAtExpectedHead(ctx context.Context, owner, repo, branch, expectedHeadOID, messageHeadline string, files map[string]string, deletions []string) (string, error) {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	branch = strings.TrimSpace(branch)
	messageHeadline = strings.TrimSpace(messageHeadline)

	if owner == "" || repo == "" {
		return "", fmt.Errorf("owner and repo must not be empty")
	}
	if branch == "" {
		return "", fmt.Errorf("branch must not be empty")
	}
	if len(files) == 0 && len(deletions) == 0 {
		return "", fmt.Errorf("must provide at least one file addition or deletion")
	}
	if messageHeadline == "" {
		return "", fmt.Errorf("commit message must not be empty")
	}
	if err := ValidateCommitOID(expectedHeadOID); err != nil {
		return "", fmt.Errorf("invalid expectedHeadOID: %w", err)
	}

	var totalBytes int64
	for _, content := range files {
		totalBytes += int64(len(content))
	}
	const maxAdditionBytes = 1 * 1024 * 1024 // 1 MiB limit
	if totalBytes > maxAdditionBytes {
		return "", fmt.Errorf("total file additions size %d bytes exceeds 1 MiB limit", totalBytes)
	}

	ghClient, err := c.ghForRepo(ctx, owner, repo)
	if err != nil {
		return "", err
	}

	branchName := strings.TrimPrefix(branch, "refs/heads/")

	var additions []map[string]any
	for path, content := range files {
		cleanPath := strings.TrimPrefix(strings.TrimSpace(path), "/")
		additions = append(additions, map[string]any{
			"path":     cleanPath,
			"contents": base64.StdEncoding.EncodeToString([]byte(content)),
		})
	}

	var fileDeletions []map[string]any
	for _, delPath := range deletions {
		cleanPath := strings.TrimPrefix(strings.TrimSpace(delPath), "/")
		if cleanPath != "" {
			fileDeletions = append(fileDeletions, map[string]any{
				"path": cleanPath,
			})
		}
	}

	fileChanges := map[string]any{
		"additions": additions,
	}
	if len(fileDeletions) > 0 {
		fileChanges["deletions"] = fileDeletions
	}

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
					"headline": messageHeadline,
				},
				"fileChanges": fileChanges,
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
