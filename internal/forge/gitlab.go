package forge

import (
	"context"
	"net/url"
	"time"
)

// gitlab is GitLab's REST API (v4).
type gitlab struct{}

// gitlabProject is the API path of the project r: its whole path, the
// groups included, as one escaped segment, which a request sends as it is.
func gitlabProject(r Repository) string {
	return "/projects/" + url.PathEscape(r.Owner+"/"+r.Name)
}

// The GitLab access levels that may push to a project and administer it:
// developer and maintainer.
const (
	gitlabDeveloper  = 30
	gitlabMaintainer = 40
)

type gitlabAccess struct {
	AccessLevel int `json:"access_level"`
}

type gitlabProjectDoc struct {
	PathWithNamespace string  `json:"path_with_namespace"`
	DefaultBranch     *string `json:"default_branch"`
	Visibility        string  `json:"visibility"`
	Archived          bool    `json:"archived"`
	Permissions       struct {
		ProjectAccess *gitlabAccess `json:"project_access"`
		GroupAccess   *gitlabAccess `json:"group_access"`
	} `json:"permissions"`
	MergeMethod  *string `json:"merge_method"`
	SquashOption *string `json:"squash_option"`
}

func (gitlab) repository(ctx context.Context, call *Call) (RepositoryDetails, error) {
	var g gitlabProjectDoc
	if err := getJSON(ctx, call, gitlabProject(call.Resolution.Repository), &g); err != nil {
		return RepositoryDetails{}, err
	}
	level := 0
	for _, a := range []*gitlabAccess{g.Permissions.ProjectAccess, g.Permissions.GroupAccess} {
		if a != nil {
			level = max(level, a.AccessLevel)
		}
	}
	d := RepositoryDetails{
		FullName: g.PathWithNamespace, Private: g.Visibility != "public", Archived: g.Archived,
		Permissions: Permissions{Pull: true, Push: level >= gitlabDeveloper, Admin: level >= gitlabMaintainer},
	}
	if g.DefaultBranch != nil {
		d.DefaultBranch = *g.DefaultBranch
	}
	if m := g.MergeMethod; m != nil {
		d.AllowMergeCommit = new(*m == "merge")
		d.AllowRebaseMerge = new(*m == "rebase_merge" || *m == "ff")
	}
	if s := g.SquashOption; s != nil {
		d.AllowSquashMerge = new(*s != "never")
	}
	return d, nil
}

type gitlabIssue struct {
	IID         int64   `json:"iid"`
	Title       string  `json:"title"`
	Description *string `json:"description"`
	State       string  `json:"state"`
	WebURL      string  `json:"web_url"`
	Author      struct {
		Username string `json:"username"`
	} `json:"author"`
	Labels    []string   `json:"labels"`
	CreatedAt *time.Time `json:"created_at"`
	UpdatedAt *time.Time `json:"updated_at"`
	ClosedAt  *time.Time `json:"closed_at"`
}

func (g gitlabIssue) issue() Issue {
	i := Issue{
		Number: g.IID, Title: g.Title, State: IssueOpen, URL: g.WebURL, Author: Author{Login: g.Author.Username},
		Labels: append(make([]string, 0, len(g.Labels)), g.Labels...), CreatedAt: stamp(g.CreatedAt),
		UpdatedAt: stamp(g.UpdatedAt), ClosedAt: stamp(g.ClosedAt),
	}
	if g.Description != nil {
		i.Body = *g.Description
	}
	// GitLab's opened and locked issues are both open.
	if g.State == IssueClosed {
		i.State = IssueClosed
	}
	return i
}

func (gitlab) issues(ctx context.Context, call *Call, f IssueFilter, p Paging) ([]Issue, *Position, error) {
	q := url.Values{"order_by": {f.Sort + "_at"}, "sort": {f.Direction}}
	switch f.State {
	case IssueOpen:
		q.Set("state", "opened")
	case IssueClosed:
		q.Set("state", IssueClosed)
	}
	if f.Labels != "" {
		q.Set("labels", f.Labels)
	}
	if f.Assignee != "" {
		q.Set("assignee_username", f.Assignee)
	}
	p.Query(q)
	var page []gitlabIssue
	resp, err := getJSONResponse(ctx, call, gitlabProject(call.Resolution.Repository)+"/issues?"+q.Encode(), &page)
	if err != nil {
		return nil, nil, err
	}
	issues := make([]Issue, 0, len(page))
	for _, g := range page {
		issues = append(issues, g.issue())
	}
	return issues, p.Next(resp.Header), nil
}
