package github

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/go-github/v68/github"
	"golang.org/x/oauth2"
)

type Client struct {
	gh      *github.Client
	appAuth *AppAuth
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
	var cloneURL string
	var headRef string
	var headSHA string

	if head := pr.GetHead(); head != nil {
		headRef = head.GetRef()
		headSHA = head.GetSHA()
		if hr := head.GetRepo(); hr != nil {
			cloneURL = hr.GetCloneURL()
			headRepoName = hr.GetName()
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
		HeadRef:       headRef,
		HeadSHA:       headSHA,
		HeadRepoOwner: headRepoOwner,
		HeadRepoName:  headRepoName,
		CloneURL:      cloneURL,
		CreatedAt:     pr.GetCreatedAt().Time,
	}, nil
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
