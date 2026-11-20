package harness

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Issue is an open issue the fake forge holds, with the bodies of its
// comments, oldest first.
type Issue struct {
	Number   int
	Title    string
	Comments []string
}

// Pull is an open pull request the fake forge holds: the bodies of the
// comments of its conversation, the names of the files it changes and of
// the check runs of its head commit, and the bodies of its reviews, each
// oldest first.
type Pull struct {
	Number                           int
	Title                            string
	Comments, Files, Checks, Reviews []string
}

// forgeRepo is a repository of the fake forge. Issues and pull requests
// share one sequence of numbers, as on GitHub.
type forgeRepo struct {
	issues []Issue
	pulls  []Pull
	labels []string
}

// Forge is a fake GitHub API over TLS on a loopback port, serving
// repositories, their issues, pull requests and labels below URL, paged as
// GitHub pages, by per_page and page with a Link header naming the next
// page. A workspace reaches it with the setting forge_api_url = URL and an
// origin whose path is /<owner>/<repo>.git (WithRepoPath), and writes to
// it with a GIT_PAT secret, whatever its value; the hub must be started
// with Env to trust it.
type Forge struct {
	// URL is the API's base, https://127.0.0.1:<port>/github.
	URL    string
	caFile string
	mu     sync.Mutex
	repos  map[string]*forgeRepo
}

// forgeTime is the time of everything the fake forge holds.
var forgeTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)

// NewForge starts a fake forge holding no repository; it is closed when
// the test ends.
func NewForge(tb testing.TB) *Forge {
	tb.Helper()
	f := &Forge{repos: map[string]*forgeRepo{}}
	mux := http.NewServeMux()
	f.handle(mux, "GET "+repoPath, func(w http.ResponseWriter, r *http.Request, full string, _ *forgeRepo) {
		writeJSON(w, http.StatusOK, map[string]any{
			"full_name": full, "name": r.PathValue("repo"), "default_branch": "main", "private": false,
			"html_url": "https://github.example/" + full, "owner": map[string]any{"login": r.PathValue("owner")},
		})
	})
	f.serveIssues(mux)
	f.servePulls(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { notFound(w) })
	srv := httptest.NewTLSServer(mux)
	tb.Cleanup(srv.Close)
	f.URL = srv.URL + "/github"
	f.caFile = filepath.Join(tb.TempDir(), "forge-ca.pem")
	if err := os.WriteFile(f.caFile, certPEM(srv), 0o644); err != nil {
		tb.Fatal(err)
	}
	return f
}

// repoPath is the API path of a repository.
const repoPath = "/github/repos/{owner}/{repo}"

// serveIssues serves the issues of a repository, their comments, which
// hold a pull request's conversation too, and its labels.
func (f *Forge) serveIssues(mux *http.ServeMux) {
	f.handle(mux, "GET "+repoPath+"/issues", func(w http.ResponseWriter, r *http.Request, full string, fr *forgeRepo) {
		var items []map[string]any
		// Every issue is open.
		if state := r.URL.Query().Get("state"); state == "" || state == "open" || state == "all" {
			for _, is := range fr.issues {
				items = append(items, issueJSON(full, is))
			}
		}
		writeJSON(w, http.StatusOK, paged(w, r, items))
	})
	f.handle(mux, "POST "+repoPath+"/issues", func(w http.ResponseWriter, r *http.Request, full string, fr *forgeRepo) {
		var in struct{ Title string }
		if json.NewDecoder(r.Body).Decode(&in) != nil || in.Title == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Validation Failed"})
			return
		}
		is := Issue{Number: fr.nextNumber(), Title: in.Title}
		fr.issues = append(fr.issues, is)
		writeJSON(w, http.StatusCreated, issueJSON(full, is))
	})
	f.handle(mux, "GET "+repoPath+"/issues/{number}/comments", func(w http.ResponseWriter, r *http.Request, full string, fr *forgeRepo) {
		comments, ok := fr.comments(r.PathValue("number"))
		if !ok {
			notFound(w)
			return
		}
		var items []map[string]any
		for i, body := range *comments {
			items = append(items, commentJSON(full, i+1, body))
		}
		writeJSON(w, http.StatusOK, paged(w, r, items))
	})
	f.handle(mux, "POST "+repoPath+"/issues/{number}/comments", func(w http.ResponseWriter, r *http.Request, full string, fr *forgeRepo) {
		comments, ok := fr.comments(r.PathValue("number"))
		var in struct{ Body string }
		switch {
		case !ok:
			notFound(w)
		case json.NewDecoder(r.Body).Decode(&in) != nil || in.Body == "":
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Validation Failed"})
		default:
			*comments = append(*comments, in.Body)
			writeJSON(w, http.StatusCreated, commentJSON(full, len(*comments), in.Body))
		}
	})
	f.handle(mux, "GET "+repoPath+"/labels/{name}", func(w http.ResponseWriter, r *http.Request, _ string, fr *forgeRepo) {
		if !slices.Contains(fr.labels, r.PathValue("name")) {
			notFound(w)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"name": r.PathValue("name"), "color": "ededed", "description": nil})
	})
	f.handle(mux, "POST "+repoPath+"/labels", func(w http.ResponseWriter, r *http.Request, _ string, fr *forgeRepo) {
		var in struct {
			Name, Color string
			Description *string
		}
		switch {
		case json.NewDecoder(r.Body).Decode(&in) != nil || in.Name == "":
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Validation Failed"})
		case slices.Contains(fr.labels, in.Name):
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Validation Failed",
				"errors": []any{map[string]any{"resource": "Label", "code": "already_exists", "field": "name"}}})
		default:
			fr.labels = append(fr.labels, in.Name)
			writeJSON(w, http.StatusCreated, map[string]any{"name": in.Name, "color": in.Color, "description": in.Description})
		}
	})
}

// servePulls serves the pull requests of a repository, their files, check
// runs and reviews.
func (f *Forge) servePulls(mux *http.ServeMux) {
	f.handle(mux, "POST "+repoPath+"/pulls", func(w http.ResponseWriter, r *http.Request, full string, fr *forgeRepo) {
		var in struct{ Title, Head, Base string }
		if json.NewDecoder(r.Body).Decode(&in) != nil || in.Title == "" || in.Head == "" || in.Base == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Validation Failed"})
			return
		}
		p := Pull{Number: fr.nextNumber(), Title: in.Title}
		fr.pulls = append(fr.pulls, p)
		writeJSON(w, http.StatusCreated, pullJSON(full, p))
	})
	f.handle(mux, "GET "+repoPath+"/pulls/{number}", func(w http.ResponseWriter, r *http.Request, full string, fr *forgeRepo) {
		if p := fr.pull(r.PathValue("number")); p != nil {
			writeJSON(w, http.StatusOK, pullJSON(full, *p))
			return
		}
		notFound(w)
	})
	f.handle(mux, "GET "+repoPath+"/pulls/{number}/files", func(w http.ResponseWriter, r *http.Request, _ string, fr *forgeRepo) {
		p := fr.pull(r.PathValue("number"))
		if p == nil {
			notFound(w)
			return
		}
		var items []map[string]any
		for _, name := range p.Files {
			items = append(items, map[string]any{"filename": name, "status": "added", "additions": 1, "deletions": 0})
		}
		writeJSON(w, http.StatusOK, paged(w, r, items))
	})
	f.handle(mux, "GET "+repoPath+"/pulls/{number}/reviews", func(w http.ResponseWriter, r *http.Request, _ string, fr *forgeRepo) {
		p := fr.pull(r.PathValue("number"))
		if p == nil {
			notFound(w)
			return
		}
		var items []map[string]any
		for _, body := range p.Reviews {
			items = append(items, map[string]any{"user": map[string]any{"login": "reviewer"}, "state": "COMMENTED",
				"body": body, "submitted_at": forgeTime})
		}
		writeJSON(w, http.StatusOK, paged(w, r, items))
	})
	f.handle(mux, "GET "+repoPath+"/commits/{sha}/check-runs", func(w http.ResponseWriter, r *http.Request, _ string, fr *forgeRepo) {
		var runs []map[string]any
		for _, p := range fr.pulls {
			if headSHA(p.Number) != r.PathValue("sha") {
				continue
			}
			for _, name := range p.Checks {
				runs = append(runs, map[string]any{"name": name, "status": "completed", "conclusion": "success",
					"output": map[string]any{"summary": name + " passed"}, "html_url": "https://github.example/checks/" + name})
			}
		}
		page := paged(w, r, runs)
		writeJSON(w, http.StatusOK, map[string]any{"total_count": len(runs), "check_runs": page})
	})
}

// handle serves pattern with fn, holding the forge's lock, for a
// repository the forge holds; any other is not found.
func (f *Forge) handle(mux *http.ServeMux, pattern string, fn func(http.ResponseWriter, *http.Request, string, *forgeRepo)) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		full := r.PathValue("owner") + "/" + r.PathValue("repo")
		f.mu.Lock()
		defer f.mu.Unlock()
		fr, ok := f.repos[full]
		if !ok {
			notFound(w)
			return
		}
		fn(w, r, full, fr)
	})
}

// repo is the repository owner/name, created empty the first time.
func (f *Forge) repo(owner, name string) *forgeRepo {
	full := owner + "/" + name
	if f.repos[full] == nil {
		f.repos[full] = &forgeRepo{}
	}
	return f.repos[full]
}

// SetIssues makes the forge hold the repository owner/repo with the issues
// given in place of those it held.
func (f *Forge) SetIssues(owner, repo string, issues ...Issue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repo(owner, repo).issues = cloneEach(issues, func(is Issue) Issue {
		is.Comments = slices.Clone(is.Comments)
		return is
	})
}

// SetPulls makes the forge hold the repository owner/repo with the pull
// requests given in place of those it held.
func (f *Forge) SetPulls(owner, repo string, pulls ...Pull) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repo(owner, repo).pulls = cloneEach(pulls, clonePull)
}

// Issues are the issues of owner/repo the forge holds now, those created
// through it included.
func (f *Forge) Issues(owner, repo string) []Issue {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneEach(f.repo(owner, repo).issues, func(is Issue) Issue {
		is.Comments = slices.Clone(is.Comments)
		return is
	})
}

// Pulls are the pull requests of owner/repo the forge holds now.
func (f *Forge) Pulls(owner, repo string) []Pull {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneEach(f.repo(owner, repo).pulls, clonePull)
}

// Labels are the names of the labels of owner/repo the forge holds now.
func (f *Forge) Labels(owner, repo string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.repo(owner, repo).labels)
}

// Env is what a hub's environment needs to trust the forge (Options.Env).
func (f *Forge) Env() []string { return []string{"SSL_CERT_FILE=" + f.caFile} }

func cloneEach[T any](in []T, clone func(T) T) []T {
	out := make([]T, 0, len(in))
	for _, v := range in {
		out = append(out, clone(v))
	}
	return out
}

func clonePull(p Pull) Pull {
	p.Comments, p.Files, p.Checks, p.Reviews = slices.Clone(p.Comments), slices.Clone(p.Files), slices.Clone(p.Checks), slices.Clone(p.Reviews)
	return p
}

// nextNumber is the number the next issue or pull request of the
// repository gets.
func (fr *forgeRepo) nextNumber() int {
	n := 0
	for _, is := range fr.issues {
		n = max(n, is.Number)
	}
	for _, p := range fr.pulls {
		n = max(n, p.Number)
	}
	return n + 1
}

// comments are the comments of the issue or pull request number, false
// when the repository holds neither.
func (fr *forgeRepo) comments(number string) (*[]string, bool) {
	for i := range fr.issues {
		if strconv.Itoa(fr.issues[i].Number) == number {
			return &fr.issues[i].Comments, true
		}
	}
	if p := fr.pull(number); p != nil {
		return &p.Comments, true
	}
	return nil, false
}

// pull is the pull request number, nil when the repository holds none.
func (fr *forgeRepo) pull(number string) *Pull {
	for i := range fr.pulls {
		if strconv.Itoa(fr.pulls[i].Number) == number {
			return &fr.pulls[i]
		}
	}
	return nil
}

// headSHA is the head commit the fake forge gives pull request number.
func headSHA(number int) string { return fmt.Sprintf("%040x", number) }

func issueJSON(full string, is Issue) map[string]any {
	return map[string]any{
		"number": is.Number, "title": is.Title, "body": "", "state": "open",
		"html_url": "https://github.example/" + full + "/issues/" + strconv.Itoa(is.Number),
		"user":     map[string]any{"login": "reporter"}, "labels": []any{},
		"created_at": forgeTime, "updated_at": forgeTime,
	}
}

// commentJSON is the comment body, the id-th of its issue or pull request.
func commentJSON(full string, id int, body string) map[string]any {
	return map[string]any{
		"id": id, "body": body, "user": map[string]any{"login": "commenter"}, "created_at": forgeTime, "updated_at": forgeTime,
		"html_url": "https://github.example/" + full + "#comment-" + strconv.Itoa(id),
	}
}

func pullJSON(full string, p Pull) map[string]any {
	return map[string]any{
		"number": p.Number, "title": p.Title, "body": "", "state": "open", "draft": false, "merged": false,
		"html_url":   "https://github.example/" + full + "/pull/" + strconv.Itoa(p.Number),
		"user":       map[string]any{"login": "author"},
		"head":       map[string]any{"sha": headSHA(p.Number), "ref": "topic-" + strconv.Itoa(p.Number)},
		"base":       map[string]any{"sha": headSHA(0), "ref": "main"},
		"created_at": forgeTime, "updated_at": forgeTime,
	}
}

// paged is the page of items the request asks for by per_page and page,
// never nil, with a Link header naming the next page when there is one.
func paged[T any](w http.ResponseWriter, r *http.Request, items []T) []T {
	q := r.URL.Query()
	size, _ := strconv.Atoi(q.Get("per_page"))
	if size < 1 {
		size = 30
	}
	page, _ := strconv.Atoi(q.Get("page"))
	page = max(page, 1)
	from := min((page-1)*size, len(items))
	to := min(from+size, len(items))
	if to < len(items) {
		q.Set("page", strconv.Itoa(page+1))
		w.Header().Set("Link", fmt.Sprintf(`<https://%s%s?%s>; rel="next"`, r.Host, r.URL.Path, q.Encode()))
	}
	return append([]T{}, items[from:to]...)
}

func notFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
