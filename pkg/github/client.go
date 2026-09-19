package github

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/go-github/v68/github"
	"golang.org/x/oauth2"
)

type Client struct {
	gh *github.Client
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
	pr, _, err := c.gh.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("failed to get pull request: %w", err)
	}

	return &PRDetails{
		Owner:     owner,
		Repo:      repo,
		Number:    number,
		Title:     pr.GetTitle(),
		Body:      pr.GetBody(),
		Author:    pr.GetUser().GetLogin(),
		BaseRef:   pr.GetBase().GetRef(),
		HeadRef:   pr.GetHead().GetRef(),
		HeadSHA:   pr.GetHead().GetSHA(),
		CloneURL:  pr.GetHead().GetRepo().GetCloneURL(),
		CreatedAt: pr.GetCreatedAt().Time,
	}, nil
}

func (c *Client) GetRawDiff(ctx context.Context, owner, repo string, number int) (string, error) {
	diff, _, err := c.gh.PullRequests.GetRaw(ctx, owner, repo, number, github.RawOptions{
		Type: github.Diff,
	})
	if err != nil {
		return "", fmt.Errorf("failed to get raw diff: %w", err)
	}
	return diff, nil
}

func (c *Client) GetComments(ctx context.Context, owner, repo string, number int) ([]Comment, []DiscussionThread, error) {
	// 1. Fetch general issue comments with pagination
	var generalComments []Comment
	opt := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	for {
		issueComments, resp, err := c.gh.Issues.ListComments(ctx, owner, repo, number, opt)
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
		rcPage, resp, err := c.gh.PullRequests.ListComments(ctx, owner, repo, number, prOpt)
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

func (c *Client) PostComment(ctx context.Context, owner, repo string, number int, body string) error {
	_, _, err := c.gh.Issues.CreateComment(ctx, owner, repo, number, &github.IssueComment{
		Body: github.Ptr(body),
	})
	return err
}

func (c *Client) GetLabels(ctx context.Context, owner, repo string, number int) ([]string, error) {
	labels, _, err := c.gh.Issues.ListLabelsByIssue(ctx, owner, repo, number, nil)
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
	_, _, err := c.gh.Issues.AddLabelsToIssue(ctx, owner, repo, number, labels)
	if err != nil {
		return fmt.Errorf("failed to add labels: %w", err)
	}
	return nil
}
