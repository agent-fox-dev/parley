package forge

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"time"
)

// github is GitHub's REST API (v3).
type github struct{}

// githubRepo is the API path of the repository r.
func githubRepo(r Repository) string {
	return "/repos/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Name)
}

type githubRepository struct {
	FullName      string       `json:"full_name"`
	DefaultBranch string       `json:"default_branch"`
	Private       bool         `json:"private"`
	Archived      bool         `json:"archived"`
	Permissions   *Permissions `json:"permissions"`
	// GitHub gives the allow_* members only to a credential that may
	// administer the repository.
	AllowMergeCommit *bool `json:"allow_merge_commit"`
	AllowSquashMerge *bool `json:"allow_squash_merge"`
	AllowRebaseMerge *bool `json:"allow_rebase_merge"`
}

func (github) repository(ctx context.Context, call *Call) (RepositoryDetails, error) {
	var g githubRepository
	if err := getJSON(ctx, call, githubRepo(call.Resolution.Repository), &g); err != nil {
		return RepositoryDetails{}, err
	}
	// GitHub leaves permissions out for an anonymous request, which may
	// read a repository it can see and nothing more.
	permissions := Permissions{Pull: true}
	if g.Permissions != nil {
		permissions = *g.Permissions
	}
	return RepositoryDetails{
		FullName: g.FullName, DefaultBranch: g.DefaultBranch, Private: g.Private, Archived: g.Archived,
		Permissions: permissions, AllowMergeCommit: g.AllowMergeCommit, AllowSquashMerge: g.AllowSquashMerge,
		AllowRebaseMerge: g.AllowRebaseMerge,
	}, nil
}

type githubIssue struct {
	Number  int64   `json:"number"`
	Title   string  `json:"title"`
	Body    *string `json:"body"`
	State   string  `json:"state"`
	HTMLURL string  `json:"html_url"`
	User    struct {
		Login string `json:"login"`
	} `json:"user"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	CreatedAt *time.Time `json:"created_at"`
	UpdatedAt *time.Time `json:"updated_at"`
	ClosedAt  *time.Time `json:"closed_at"`
	// PullRequest is present on an issue that is a pull request.
	PullRequest json.RawMessage `json:"pull_request"`
}

func (g githubIssue) isPR() bool {
	return len(g.PullRequest) > 0 && string(g.PullRequest) != "null"
}

func (g githubIssue) issue() Issue {
	i := Issue{
		Number: g.Number, Title: g.Title, State: IssueOpen, URL: g.HTMLURL, Author: Author{Login: g.User.Login},
		Labels: make([]string, 0, len(g.Labels)), CreatedAt: stamp(g.CreatedAt), UpdatedAt: stamp(g.UpdatedAt),
		ClosedAt: stamp(g.ClosedAt), IsPR: g.isPR(),
	}
	if g.Body != nil {
		i.Body = *g.Body
	}
	if g.State == IssueClosed {
		i.State = IssueClosed
	}
	for _, l := range g.Labels {
		i.Labels = append(i.Labels, l.Name)
	}
	return i
}

func (github) issues(ctx context.Context, call *Call, f IssueFilter, p Paging) ([]Issue, *Position, error) {
	q := url.Values{"state": {f.State}, "sort": {f.Sort}, "direction": {f.Direction}}
	if f.Labels != "" {
		q.Set("labels", f.Labels)
	}
	if f.Assignee != "" {
		q.Set("assignee", f.Assignee)
	}
	p.Query(q)
	var page []githubIssue
	resp, err := getJSONResponse(ctx, call, githubRepo(call.Resolution.Repository)+"/issues?"+q.Encode(), &page)
	if err != nil {
		return nil, nil, err
	}
	// GitHub lists pull requests among the issues. They are left out once
	// the forge has paged, so a page may hold fewer than its size.
	issues := make([]Issue, 0, len(page))
	for _, g := range page {
		if !g.isPR() {
			issues = append(issues, g.issue())
		}
	}
	return issues, p.Next(resp.Header), nil
}

// githubIssuePath is the API path of the issue number of the repository r.
func githubIssuePath(r Repository, number int64) string {
	return githubRepo(r) + "/issues/" + strconv.FormatInt(number, 10)
}

func (github) issue(ctx context.Context, call *Call, number int64) (Issue, error) {
	var g githubIssue
	if err := getJSON(ctx, call, githubIssuePath(call.Resolution.Repository, number), &g); err != nil {
		return Issue{}, err
	}
	return g.issue(), nil
}

type githubComment struct {
	ID   int64   `json:"id"`
	Body *string `json:"body"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	CreatedAt *time.Time `json:"created_at"`
	UpdatedAt *time.Time `json:"updated_at"`
	HTMLURL   string     `json:"html_url"`
}

// comments reads GitHub's issue comments, which it lists oldest first.
func (github) comments(ctx context.Context, call *Call, number int64, p Paging) ([]Comment, *Position, error) {
	q := url.Values{}
	p.Query(q)
	var page []githubComment
	resp, err := getJSONResponse(ctx, call, githubIssuePath(call.Resolution.Repository, number)+"/comments?"+q.Encode(), &page)
	if err != nil {
		return nil, nil, err
	}
	comments := make([]Comment, 0, len(page))
	for _, g := range page {
		c := Comment{ID: g.ID, Author: Author{Login: g.User.Login}, CreatedAt: stamp(g.CreatedAt),
			UpdatedAt: stamp(g.UpdatedAt), URL: g.HTMLURL}
		if g.Body != nil {
			c.Body = *g.Body
		}
		comments = append(comments, c)
	}
	return comments, p.Next(resp.Header), nil
}
