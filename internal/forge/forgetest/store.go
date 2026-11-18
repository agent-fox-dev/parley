package forgetest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// Login is the user the fake forge says wrote what it creates, and Stamp
// the time it says everything was created and last updated at.
const (
	Login = "kim"
	Stamp = "2026-10-01T08:00:00Z"
)

// Issue is an issue or a pull request of an in-memory repository.
type Issue struct {
	Number int64
	Title  string
	Body   string
	// State is open or closed.
	State     string
	Labels    []string
	Assignees []string
	// Pull marks a pull request, a merge request on GitLab, from the
	// branch Head into Base.
	Pull       bool
	Head, Base string
	Draft      bool
	Comments   []Comment
}

// Comment is a comment on an issue or a pull request, a note on GitLab.
type Comment struct {
	ID   int64
	Body string
}

// Label is a label of an in-memory repository, its color as it was sent.
type Label struct {
	Name, Color, Description string
}

// repository is an in-memory GitHub repository or GitLab project. On both
// forges its issues and pull requests share one sequence of numbers.
type repository struct {
	gitlab bool
	// web is the URL of the repository's web page.
	web    string
	next   int64
	issues map[int64]*Issue
	labels []Label
	// comments is the id of the latest comment.
	comments int64
}

// The in-memory repositories serve the writes a forge route sends, and the
// reads of what they hold, below the API path of a repository, which the
// methods below name as it is sent: GitHubPrefix+"/repos/<owner>/<name>",
// or GitLabPrefix+"/projects/<owner%2Fname>". A repository exists from its
// first request or method call on, empty, its first number 1. A request
// that a scripted answer fits, or a GET of a path SetObject or SetList set,
// never reaches them.
//
// GitHub: POST /issues, GET and PATCH /issues/<n>, POST /issues/<n>/comments,
// POST /issues/<n>/labels, DELETE /issues/<n>/labels/<name>, GET
// /labels/<name>, POST /labels and POST /pulls. GitLab: POST /issues, GET
// and PUT /issues/<n> (with state_event, labels, add_labels and
// remove_labels, a label list being one string of names separated by
// commas), POST /issues/<n>/notes, POST /merge_requests/<n>/notes, GET
// /labels/<name>, POST /labels and POST /merge_requests. A request for an
// issue or label that does not exist is 404; a label created twice is 422
// on GitHub and 409 on GitLab.

// SetNext makes n the number of the next issue or pull request created in
// the repository repo.
func (f *Fake) SetNext(repo string, n int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repository(repo).next = n
}

// AddIssue stores issue in the repository repo, in place of the one with
// its number; the next number created is above it.
func (f *Fake) AddIssue(repo string, issue Issue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repository(repo)
	issue.Labels, issue.Assignees, issue.Comments = slices.Clone(issue.Labels), slices.Clone(issue.Assignees), slices.Clone(issue.Comments)
	r.issues[issue.Number] = &issue
	r.next = max(r.next, issue.Number+1)
}

// Issue is the issue or pull request number of the repository repo.
func (f *Fake) Issue(repo string, number int64) (Issue, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i, ok := f.repository(repo).issues[number]
	if !ok {
		return Issue{}, false
	}
	c := *i
	c.Labels, c.Assignees, c.Comments = slices.Clone(i.Labels), slices.Clone(i.Assignees), slices.Clone(i.Comments)
	return c, true
}

// AddLabel stores label in the repository repo, in place of the one with
// its name.
func (f *Fake) AddLabel(repo string, label Label) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repository(repo)
	if i := r.label(label.Name); i >= 0 {
		r.labels[i] = label
		return
	}
	r.labels = append(r.labels, label)
}

// Labels are the labels of the repository repo.
func (f *Fake) Labels(repo string) []Label {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.repository(repo).labels)
}

// repository is the repository at the API path repo, created when it does
// not exist. f.mu is held.
func (f *Fake) repository(repo string) *repository {
	if r, ok := f.repositories[repo]; ok {
		return r
	}
	r := &repository{next: 1, issues: map[int64]*Issue{}}
	if project, ok := strings.CutPrefix(repo, GitLabPrefix+"/projects/"); ok {
		path, _ := url.PathUnescape(project)
		r.gitlab, r.web = true, f.URL+"/"+path
	} else {
		r.web = "https://github.com/" + strings.TrimPrefix(repo, GitHubPrefix+"/repos/")
	}
	if f.repositories == nil {
		f.repositories = map[string]*repository{}
	}
	f.repositories[repo] = r
	return r
}

// serveRepository answers a request for a path below a repository's API
// path with the in-memory repository, and is false for any other path.
func (f *Fake) serveRepository(w http.ResponseWriter, r *http.Request, body []byte) bool {
	path := r.URL.EscapedPath()
	var repo, rest string
	if p, ok := strings.CutPrefix(path, GitHubPrefix+"/repos/"); ok {
		parts := strings.SplitN(p, "/", 3)
		if len(parts) < 3 {
			return false
		}
		repo, rest = GitHubPrefix+"/repos/"+parts[0]+"/"+parts[1], parts[2]
	} else if p, ok := strings.CutPrefix(path, GitLabPrefix+"/projects/"); ok {
		project, more, found := strings.Cut(p, "/")
		if !found {
			return false
		}
		repo, rest = GitLabPrefix+"/projects/"+project, more
	} else {
		return false
	}
	var in map[string]json.RawMessage
	if len(body) > 0 && json.Unmarshal(body, &in) != nil {
		writeAnswer(w, Answer{Status: http.StatusBadRequest, Body: `{"message":"Problems parsing JSON"}`})
		return true
	}
	f.mu.Lock()
	status, answer := f.repository(repo).serve(r.Method, strings.Split(rest, "/"), in)
	f.mu.Unlock()
	writeAnswer(w, Answer{Status: status, Body: string(mustJSON(answer))})
	return true
}

// notFound is a forge's answer to a request for what does not exist.
var notFound = map[string]string{"message": "Not Found"}

// serve answers the request method for the path segments below the
// repository, with the members of its JSON body in.
func (r *repository) serve(method string, path []string, in map[string]json.RawMessage) (int, any) {
	route := method + " " + path[0]
	var issue *Issue
	if len(path) > 1 && path[0] != "labels" {
		n, err := strconv.ParseInt(path[1], 10, 64)
		if issue = r.issues[n]; err != nil || issue == nil {
			return http.StatusNotFound, notFound
		}
		route += "/n"
		if len(path) > 2 {
			route += "/" + path[2]
		}
	}
	switch route {
	case "POST issues", "POST pulls", "POST merge_requests":
		return r.create(path[0] != "issues", in)
	case "GET issues/n", "GET merge_requests/n":
		return http.StatusOK, r.issueJSON(issue)
	case "PATCH issues/n", "PUT issues/n":
		r.update(issue, in)
		return http.StatusOK, r.issueJSON(issue)
	case "POST issues/n/comments", "POST issues/n/notes", "POST merge_requests/n/notes":
		if r.gitlab && issue.Pull != (path[0] == "merge_requests") {
			return http.StatusNotFound, notFound
		}
		r.comments++
		c := Comment{ID: r.comments, Body: str(in, "body")}
		issue.Comments = append(issue.Comments, c)
		return http.StatusCreated, r.commentJSON(issue, c)
	case "POST issues/n/labels":
		issue.Labels = union(issue.Labels, list(in, "labels"))
		return http.StatusOK, r.labelsJSON(issue.Labels)
	case "DELETE issues/n/labels":
		name, _ := url.PathUnescape(strings.Join(path[3:], "/"))
		if !slices.Contains(issue.Labels, name) {
			return http.StatusNotFound, map[string]string{"message": "Label does not exist"}
		}
		issue.Labels = slices.DeleteFunc(issue.Labels, func(l string) bool { return l == name })
		return http.StatusOK, r.labelsJSON(issue.Labels)
	case "GET labels":
		name, _ := url.PathUnescape(strings.Join(path[1:], "/"))
		if i := r.label(name); i >= 0 {
			return http.StatusOK, r.labelJSON(r.labels[i])
		}
		return http.StatusNotFound, notFound
	case "POST labels":
		l := Label{Name: str(in, "name"), Color: str(in, "color"), Description: str(in, "description")}
		if r.label(l.Name) >= 0 {
			if r.gitlab {
				return http.StatusConflict, map[string]string{"message": "Label already exists"}
			}
			return http.StatusUnprocessableEntity, map[string]any{"message": "Validation Failed",
				"errors": []map[string]string{{"resource": "Label", "code": "already_exists", "field": "name"}}}
		}
		r.labels = append(r.labels, l)
		return http.StatusCreated, r.labelJSON(l)
	}
	return http.StatusNotFound, notFound
}

// create creates an issue, or a pull request when pull is true.
func (r *repository) create(pull bool, in map[string]json.RawMessage) (int, any) {
	i := &Issue{Number: r.next, Title: str(in, "title"), State: "open", Pull: pull,
		Assignees: list(in, "assignees"), Labels: list(in, "labels")}
	r.next++
	i.Body = str(in, "body")
	if r.gitlab {
		i.Body = str(in, "description")
	}
	if pull {
		i.Head, i.Base, i.Draft = str(in, "head"), str(in, "base"), str(in, "draft") == "true"
		if r.gitlab {
			i.Head, i.Base = str(in, "source_branch"), str(in, "target_branch")
			i.Draft = strings.HasPrefix(i.Title, "Draft:")
		}
	}
	r.issues[i.Number] = i
	return http.StatusCreated, r.issueJSON(i)
}

// update sets the members of in on the issue i.
func (r *repository) update(i *Issue, in map[string]json.RawMessage) {
	if _, ok := in["title"]; ok {
		i.Title = str(in, "title")
	}
	for _, name := range []string{"body", "description"} {
		if _, ok := in[name]; ok {
			i.Body = str(in, name)
		}
	}
	if _, ok := in["state"]; ok {
		i.State = str(in, "state")
	}
	switch str(in, "state_event") {
	case "close":
		i.State = "closed"
	case "reopen":
		i.State = "open"
	}
	if _, ok := in["labels"]; ok {
		i.Labels = list(in, "labels")
	}
	if _, ok := in["assignees"]; ok {
		i.Assignees = list(in, "assignees")
	}
	i.Labels = union(i.Labels, list(in, "add_labels"))
	for _, l := range list(in, "remove_labels") {
		i.Labels = slices.DeleteFunc(i.Labels, func(have string) bool { return have == l })
	}
}

// label is the index of the label name, whose letter case does not count,
// and -1 when there is none.
func (r *repository) label(name string) int {
	return slices.IndexFunc(r.labels, func(l Label) bool { return strings.EqualFold(l.Name, name) })
}

// issueJSON is the issue or pull request i as the forge answers it.
func (r *repository) issueJSON(i *Issue) map[string]any {
	var closedAt any
	if i.State == "closed" {
		closedAt = Stamp
	}
	out := map[string]any{"title": i.Title, "state": i.State, "created_at": Stamp, "updated_at": Stamp, "closed_at": closedAt}
	if r.gitlab {
		page := "issues"
		if i.Pull {
			page = "merge_requests"
			out["source_branch"], out["target_branch"], out["draft"], out["merged_at"] = i.Head, i.Base, i.Draft, nil
		}
		out["id"], out["iid"], out["description"] = 1000+i.Number, i.Number, i.Body
		out["web_url"] = fmt.Sprintf("%s/-/%s/%d", r.web, page, i.Number)
		out["author"], out["labels"] = map[string]string{"username": Login}, append([]string{}, i.Labels...)
		out["assignees"] = users(i.Assignees, "username")
		if i.State == "open" {
			out["state"] = "opened"
		}
		return out
	}
	page := "issues"
	if i.Pull {
		page = "pull"
		out["head"], out["base"], out["draft"], out["merged"], out["merged_at"] = map[string]string{"ref": i.Head},
			map[string]string{"ref": i.Base}, i.Draft, false, nil
	}
	out["number"], out["body"] = i.Number, i.Body
	out["html_url"] = fmt.Sprintf("%s/%s/%d", r.web, page, i.Number)
	if i.Pull {
		out["pull_request"] = map[string]string{"html_url": out["html_url"].(string)}
	}
	out["user"], out["labels"] = map[string]string{"login": Login}, r.labelsJSON(i.Labels)
	out["assignees"] = users(i.Assignees, "login")
	return out
}

// commentJSON is the comment c on the issue i as the forge answers it.
func (r *repository) commentJSON(i *Issue, c Comment) map[string]any {
	out := map[string]any{"id": c.ID, "body": c.Body, "created_at": Stamp, "updated_at": Stamp}
	if r.gitlab {
		out["author"], out["system"] = map[string]string{"username": Login}, false
		return out
	}
	out["user"] = map[string]string{"login": Login}
	out["html_url"] = fmt.Sprintf("%s/issues/%d#issuecomment-%d", r.web, i.Number, c.ID)
	return out
}

// labelsJSON is the labels names as GitHub answers an issue's labels.
func (r *repository) labelsJSON(names []string) []map[string]any {
	out := []map[string]any{}
	for _, name := range names {
		l := Label{Name: name}
		if i := r.label(name); i >= 0 {
			l = r.labels[i]
		}
		out = append(out, r.labelJSON(l))
	}
	return out
}

// labelJSON is the label l as the forge answers it.
func (r *repository) labelJSON(l Label) map[string]any {
	return map[string]any{"name": l.Name, "color": l.Color, "description": l.Description}
}

// users are the users names as the forge names a user, by the member key.
func users(names []string, key string) []map[string]string {
	out := []map[string]string{}
	for _, n := range names {
		out = append(out, map[string]string{key: n})
	}
	return out
}

// str is the member name of in as text: a string's value, or the JSON
// text of any other value; "" when it is absent or null.
func str(in map[string]json.RawMessage, name string) string {
	raw, ok := in[name]
	if !ok || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// list is the member name of in as a list of names: a JSON array of
// strings, or a string of names separated by commas as GitLab takes them.
func list(in map[string]json.RawMessage, name string) []string {
	var names []string
	if json.Unmarshal(in[name], &names) == nil && names != nil {
		return names
	}
	out := []string{}
	if s := str(in, name); s != "" {
		out = strings.Split(s, ",")
	}
	return out
}

// union is have with the names of add it lacks appended.
func union(have, add []string) []string {
	for _, a := range add {
		if !slices.Contains(have, a) {
			have = append(have, a)
		}
	}
	return have
}
