package forge

import (
	"context"
	"net/url"
	"strconv"
	"strings"
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

// gitlabIssuePath is the API path of the issue iid of the project r.
func gitlabIssuePath(r Repository, iid int64) string {
	return gitlabProject(r) + "/issues/" + strconv.FormatInt(iid, 10)
}

// issue reads the issue iid. GitLab's issues are never merge requests.
func (gitlab) issue(ctx context.Context, call *Call, iid int64) (Issue, error) {
	var g gitlabIssue
	if err := getJSON(ctx, call, gitlabIssuePath(call.Resolution.Repository, iid), &g); err != nil {
		return Issue{}, err
	}
	return g.issue(), nil
}

type gitlabNote struct {
	ID     int64   `json:"id"`
	Body   *string `json:"body"`
	System bool    `json:"system"`
	Author struct {
		Username string `json:"username"`
	} `json:"author"`
	CreatedAt *time.Time `json:"created_at"`
	UpdatedAt *time.Time `json:"updated_at"`
}

// comment is the note g as the hub answers it, on the issue or merge request
// whose web page is at page: GitLab gives a note no web URL, so it is the
// note's anchor on that page.
func (g gitlabNote) comment(page string) Comment {
	c := Comment{ID: g.ID, Author: Author{Login: g.Author.Username}, CreatedAt: stamp(g.CreatedAt),
		UpdatedAt: stamp(g.UpdatedAt), URL: page + "#note_" + strconv.FormatInt(g.ID, 10)}
	if g.Body != nil {
		c.Body = *g.Body
	}
	return c
}

// gitlabPage is the web URL of the issue iid of the project of res, or of
// its merge request iid when mr is true.
func gitlabPage(res Resolution, iid int64, mr bool) string {
	kind := "/-/issues/"
	if mr {
		kind = "/-/merge_requests/"
	}
	return gitlabWebURL(res, kind+strconv.FormatInt(iid, 10))
}

// comments reads the notes of the issue iid, oldest first. The notes
// GitLab writes itself, such as a label change, are left out once the forge
// has paged, so a page may hold fewer than its size.
func (gitlab) comments(ctx context.Context, call *Call, iid int64, p Paging) ([]Comment, *Position, error) {
	q := url.Values{"sort": {"asc"}, "order_by": {"created_at"}}
	p.Query(q)
	var page []gitlabNote
	resp, err := getJSONResponse(ctx, call, gitlabIssuePath(call.Resolution.Repository, iid)+"/notes?"+q.Encode(), &page)
	if err != nil {
		return nil, nil, err
	}
	issueURL := gitlabPage(call.Resolution, iid, false)
	comments := make([]Comment, 0, len(page))
	for _, g := range page {
		if !g.System {
			comments = append(comments, g.comment(issueURL))
		}
	}
	return comments, p.Next(resp.Header), nil
}

// gitlabWebURL is the URL of the page path below the project of res on
// GitLab's web site, which a note does not give: the scheme and host of the
// API URL, then the project's path.
func gitlabWebURL(res Resolution, path string) string {
	u := url.URL{Path: "/" + res.Repository.Owner + "/" + res.Repository.Name + path}
	if api, err := url.Parse(res.APIURL); err == nil {
		u.Scheme, u.Host = api.Scheme, api.Host
	}
	return u.String()
}

// gitlabMRPath is the API path of the merge request iid of the project r.
func gitlabMRPath(r Repository, iid int64) string {
	return gitlabProject(r) + "/merge_requests/" + strconv.FormatInt(iid, 10)
}

type gitlabMergeRequest struct {
	IID         int64   `json:"iid"`
	Title       string  `json:"title"`
	Description *string `json:"description"`
	State       string  `json:"state"`
	WebURL      string  `json:"web_url"`
	Draft       bool    `json:"draft"`
	Author      struct {
		Username string `json:"username"`
	} `json:"author"`
	SHA          string     `json:"sha"`
	SourceBranch string     `json:"source_branch"`
	TargetBranch string     `json:"target_branch"`
	CreatedAt    *time.Time `json:"created_at"`
	UpdatedAt    *time.Time `json:"updated_at"`
	ClosedAt     *time.Time `json:"closed_at"`
	MergedAt     *time.Time `json:"merged_at"`
}

func (gitlab) pull(ctx context.Context, call *Call, iid int64) (PullRequest, error) {
	var g gitlabMergeRequest
	if err := getJSON(ctx, call, gitlabMRPath(call.Resolution.Repository, iid), &g); err != nil {
		return PullRequest{}, err
	}
	return g.pull(), nil
}

// pull is the merge request g as the hub answers a pull request, numbered
// by its iid.
func (g gitlabMergeRequest) pull() PullRequest {
	p := PullRequest{
		Number: g.IID, Title: g.Title, State: IssueOpen, URL: g.WebURL, Draft: g.Draft,
		Merged: g.State == PullMerged || g.MergedAt != nil, HeadSHA: g.SHA, HeadBranch: g.SourceBranch,
		BaseBranch: g.TargetBranch, Author: Author{Login: g.Author.Username}, CreatedAt: stamp(g.CreatedAt),
		UpdatedAt: stamp(g.UpdatedAt), ClosedAt: stamp(g.ClosedAt), MergedAt: stamp(g.MergedAt),
	}
	if g.Description != nil {
		p.Body = *g.Description
	}
	// GitLab's opened and locked merge requests are both open.
	switch {
	case p.Merged:
		p.State = PullMerged
	case g.State == IssueClosed:
		p.State = IssueClosed
	}
	return p
}

// files reads the diffs of the merge request iid. GitLab gives no line
// counts, so they are counted from each file's diff text.
func (gitlab) files(ctx context.Context, call *Call, iid int64, p Paging) ([]ChangedFile, *Position, error) {
	q := url.Values{}
	p.Query(q)
	var page []struct {
		NewPath     string `json:"new_path"`
		NewFile     bool   `json:"new_file"`
		DeletedFile bool   `json:"deleted_file"`
		RenamedFile bool   `json:"renamed_file"`
		Diff        string `json:"diff"`
	}
	resp, err := getJSONResponse(ctx, call, gitlabMRPath(call.Resolution.Repository, iid)+"/diffs?"+q.Encode(), &page)
	if err != nil {
		return nil, nil, err
	}
	files := make([]ChangedFile, 0, len(page))
	for _, g := range page {
		f := ChangedFile{Filename: g.NewPath, Status: FileModified}
		switch {
		case g.NewFile:
			f.Status = FileAdded
		case g.DeletedFile:
			f.Status = FileDeleted
		case g.RenamedFile:
			f.Status = FileRenamed
		}
		for line := range strings.SplitSeq(g.Diff, "\n") {
			switch {
			case strings.HasPrefix(line, "+"):
				f.Additions++
			case strings.HasPrefix(line, "-"):
				f.Deletions++
			}
		}
		files = append(files, f)
	}
	return files, p.Next(resp.Header), nil
}

// gitlabJobDone maps the statuses of GitLab's finished jobs to the
// conclusions of a completed check run; every other status but running is
// queued.
var gitlabJobDone = map[string]string{
	"success": CheckSuccess, "failed": CheckFailure, "canceled": CheckCancelled, "skipped": CheckSkipped,
}

// checkAnchor accepts the id of the pipeline the first page read the jobs
// of: a positive decimal integer, without sign or leading zero.
func (gitlab) checkAnchor(anchor string) bool {
	id, err := strconv.ParseInt(anchor, 10, 64)
	return err == nil && id > 0 && strconv.FormatInt(id, 10) == anchor
}

// checks reads the jobs of the merge request's latest pipeline, the first
// its pipeline list names. The first page finds the pipeline; later pages
// read the one it found, so a pipeline started meanwhile does not mix two
// listings.
func (gitlab) checks(ctx context.Context, call *Call, iid int64, p Paging) ([]CheckRun, *Position, error) {
	if p.Anchor == "" {
		var pipelines []struct {
			ID int64 `json:"id"`
		}
		latest := Paging{Position: Position{Page: 1, Size: 1}}
		q := url.Values{}
		latest.Query(q)
		if err := getJSON(ctx, call, gitlabMRPath(call.Resolution.Repository, iid)+"/pipelines?"+q.Encode(), &pipelines); err != nil {
			return nil, nil, err
		}
		if len(pipelines) == 0 {
			return []CheckRun{}, nil, nil
		}
		p.Anchor = strconv.FormatInt(pipelines[0].ID, 10)
	}
	q := url.Values{}
	p.Query(q)
	var page []struct {
		Name         string `json:"name"`
		Status       string `json:"status"`
		AllowFailure bool   `json:"allow_failure"`
		WebURL       string `json:"web_url"`
	}
	path := gitlabProject(call.Resolution.Repository) + "/pipelines/" + url.PathEscape(p.Anchor) + "/jobs?" + q.Encode()
	resp, err := getJSONResponse(ctx, call, path, &page)
	if err != nil {
		return nil, nil, err
	}
	runs := make([]CheckRun, 0, len(page))
	for _, g := range page {
		run := CheckRun{Name: g.Name, Status: CheckQueued, URL: g.WebURL}
		if g.Status == "running" {
			run.Status = CheckInProgress
		}
		if c, ok := gitlabJobDone[g.Status]; ok {
			if c == CheckFailure && g.AllowFailure {
				c = CheckNeutral
			}
			run.Status, run.Conclusion = CheckCompleted, &c
		}
		runs = append(runs, run)
	}
	return runs, p.Next(resp.Header), nil
}

// phaseNotes is the second phase of GitLab's reviews, its notes.
const phaseNotes = "notes"

// reviews joins the approvals of the merge request iid, which GitLab gives
// without a time and so come first, in the forge's order, with the notes of
// people, oldest first. The approvals fill pages first; the page on which
// they end is filled up with notes, and the notes are then read in pages of
// the size that page had left, so that no page is longer than the limit.
// When the approvals fill that page, the notes are read in pages of the
// limit, and a next page is named only if a note page of one entry finds a
// note. System notes are left out once the forge has paged.
func (gitlab) reviews(ctx context.Context, call *Call, iid int64, p Paging) ([]Review, *Position, error) {
	mr := gitlabMRPath(call.Resolution.Repository, iid)
	reviews := []Review{}
	notes := Paging{Position: Position{Page: p.Page, Size: p.PhaseSize}}
	if p.Phase == "" {
		var approvals struct {
			ApprovedBy []struct {
				User struct {
					Username string `json:"username"`
				} `json:"user"`
			} `json:"approved_by"`
		}
		if err := getJSON(ctx, call, mr+"/approvals", &approvals); err != nil {
			return nil, nil, err
		}
		all := approvals.ApprovedBy
		start := min(len(all), (p.Page-1)*p.Size)
		end := min(len(all), start+p.Size)
		for _, a := range all[start:end] {
			reviews = append(reviews, Review{Author: Author{Login: a.User.Username}, State: ReviewApproved})
		}
		left := p.Size - len(reviews)
		switch {
		case end < len(all):
			return reviews, &Position{Page: p.Page + 1, Size: p.Size}, nil
		case left == 0:
			// The approvals fill the page: the notes follow on the next
			// one only if there is a note, which the first note page of
			// one entry tells.
			first := Paging{Position: Position{Page: 1, Size: 1}}
			page, resp, err := gitlabNotes(ctx, call, mr, first)
			if err != nil {
				return nil, nil, err
			}
			if len(page) == 0 && first.Next(resp.Header) == nil {
				return reviews, nil, nil
			}
			return reviews, &Position{Page: 1, Size: p.Size, Phase: phaseNotes, PhaseSize: p.Size}, nil
		}
		notes.Position = Position{Page: 1, Size: left}
	}
	page, resp, err := gitlabNotes(ctx, call, mr, notes)
	if err != nil {
		return nil, nil, err
	}
	for _, g := range page {
		if g.System {
			continue
		}
		r := Review{Author: Author{Login: g.Author.Username}, State: ReviewCommented, SubmittedAt: stamp(g.CreatedAt)}
		if g.Body != nil {
			r.Body = *g.Body
		}
		reviews = append(reviews, r)
	}
	next := notes.Next(resp.Header)
	if next != nil {
		next = &Position{Page: next.Page, Size: p.Size, Phase: phaseNotes, PhaseSize: notes.Size}
	}
	return reviews, next, nil
}

// gitlabNotes reads the page p of the notes of the merge request at the API
// path mr, oldest first.
func gitlabNotes(ctx context.Context, call *Call, mr string, p Paging) ([]gitlabNote, *Response, error) {
	q := url.Values{"sort": {"asc"}, "order_by": {"created_at"}}
	p.Query(q)
	var page []gitlabNote
	resp, err := getJSONResponse(ctx, call, mr+"/notes?"+q.Encode(), &page)
	return page, resp, err
}
