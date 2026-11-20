package scm_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/agent-fox-dev/agentkit-go/outline"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/scm"
)

// outlineRouter is router.go on the main of the outline workspace.
const outlineRouter = `package api

// MaxRoutes bounds the table.
const MaxRoutes = 64

type Router struct {
	routes []string
}

func (r *Router) Handle(pattern string) {
	r.routes = append(r.routes, pattern)
}

func NewRouter() *Router {
	return &Router{}
}

func match(pattern, path string) bool {
	return pattern == path
}
`

// outlineEdge is a Go file of exactly the 1 MiB an outline takes: one
// function and a comment that fills the rest.
var outlineEdge = func() string {
	const head = "package api\n\nfunc Edge() {}\n\n//"
	return head + strings.Repeat("e", 1<<20-len(head)-1) + "\n"
}()

// outlineLate is a Go file whose only NUL byte is its byte 9,000, past what
// is looked at to call a file binary.
var outlineLate = func() string {
	const head = "package api\n\nfunc Late() {}\n\n/*"
	return head + strings.Repeat("l", 9000-len(head)) + "\x00*/\n"
}()

// addOutline adds the ready workspace outline of the owner and returns the
// ids of its commits first, v1, main and feat. Its main is first <- v1 <-
// main, each with a router.go of its own (the functions First, Second and,
// on main, outlineRouter); the tag v1 names v1. main also holds empty.go (a
// package clause alone), README.md, data.csv, Makefile, LICENSE, the
// symlink link.go to router.go, internal/api/x.go, the submodule entry
// vendor/sub, edge.go (outlineEdge), big.go (one byte more), late.go
// (outlineLate), logo.png with a NUL in its header and blob.go whose third
// byte is NUL. feat, one commit on top of main, adds the function Feat to
// router.go and adds only.go.
func (w *world) addOutline(t testing.TB) map[string]string {
	t.Helper()
	data := func(s string) string { return "data " + strconv.Itoa(len(s)) + "\n" + s + "\n" }
	file := func(path, content string) string { return "M 100644 inline " + path + "\n" + data(content) }
	dir := w.addWorkspace(t, "outline", w.org, "commit refs/heads/main\nmark :1\n"+testCommitter+data("first")+
		file("router.go", "package api\n\nfunc First() {}\n")+
		"\ncommit refs/heads/main\nmark :2\n"+testCommitter+data("v1")+"from :1\n"+
		file("router.go", "package api\n\nfunc Second() {}\n")+
		"\ncommit refs/heads/main\nmark :3\n"+testCommitter+data("main")+"from :2\n"+
		file("router.go", outlineRouter)+file("empty.go", "package api\n")+file("README.md", "# Outline\n\n## Use\n")+
		file("data.csv", "a,b\n1,2\n")+file("Makefile", "all:\n\ttrue\n")+file("LICENSE", "func free() {}\n")+
		"M 120000 inline link.go\n"+data("router.go")+file("internal/api/x.go", "package api\n")+
		"M 160000 "+strings.Repeat("5", 40)+" vendor/sub\n"+
		file("edge.go", outlineEdge)+file("big.go", outlineEdge+"\n")+file("late.go", outlineLate)+
		file("logo.png", "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")+file("blob.go", "pa\x00ckage api\n\nfunc Blob() {}\n")+
		"\ncommit refs/heads/feat\n"+testCommitter+data("feat")+"from :3\n"+
		file("router.go", outlineRouter+"\nfunc Feat() {}\n")+file("only.go", "package api\n\nfunc Only() {}\n")+
		"\nreset refs/tags/v1\nfrom :2\n\n")
	ids := map[string]string{}
	for i, id := range strings.Fields(gitrepotest.Git(t, dir, "rev-parse", "main~2", "main~1", "main", "feat")) {
		ids[[]string{"first", "v1", "main", "feat"}[i]] = id
	}
	return ids
}

// declaration is one declaration of an outline's answer.
type declaration struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Container string `json:"container"`
	Signature string `json:"signature"`
	Exported  bool   `json:"exported"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

// outlineAnswer is the answer of the outline route; Declarations is nil
// when the answer's member is null or missing.
type outlineAnswer struct {
	Path         string        `json:"path"`
	ResolvedSHA  string        `json:"resolved_sha"`
	Language     *string       `json:"language"`
	Supported    *bool         `json:"supported"`
	Declarations []declaration `json:"declarations"`
}

// names are the names of the answer's declarations, in its order.
func (a outlineAnswer) names() []string {
	names := []string{}
	for _, d := range a.Declarations {
		names = append(names, d.Name)
	}
	return names
}

// is reports whether the answer says supported, in language, with
// declarations that are a list.
func (a outlineAnswer) is(supported bool, language string) bool {
	return a.Supported != nil && *a.Supported == supported && a.Language != nil && *a.Language == language && a.Declarations != nil
}

// outlineOf is the answer of GET …/scm/outline?query of the workspace slug
// to the owner, decoded when it is 200.
func outlineOf(t *testing.T, w *world, slug, query string) (reply, outlineAnswer) {
	t.Helper()
	r := w.get(t, scmPath(slug, "/outline?"+query), w.ownerKey)
	var a outlineAnswer
	if r.status == http.StatusOK {
		if err := json.Unmarshal(r.body, &a); err != nil {
			t.Fatalf("GET /outline?%s: the answer is not the outline object: %v\n%s", query, err, r.body)
		}
	}
	return r, a
}

func TestOutline_AnswersTheDeclarationsOfAFileInLineOrder(t *testing.T) {
	t.Parallel()
	w := readOnlyWorld(t)
	r, a := outlineOf(t, w.world, "outline", "path=router.go")
	want := []declaration{
		{"const", "MaxRoutes", "", "const MaxRoutes", true, 4, 4},
		{"type", "Router", "", "type Router struct", true, 6, 8},
		{"method", "Handle", "Router", "func (r *Router) Handle(pattern string)", true, 10, 12},
		{"func", "NewRouter", "", "func NewRouter() *Router", true, 14, 16},
		{"func", "match", "", "func match(pattern, path string) bool", false, 18, 20},
	}
	if r.status != http.StatusOK || a.Path != "router.go" || a.ResolvedSHA != w.outline["main"] || !a.is(true, "Go") {
		t.Fatalf("GET /outline?path=router.go = %d %s, want 200 with path router.go, resolved_sha %s, language Go and supported true",
			r.status, r.body, w.outline["main"])
	}
	if !slices.Equal(a.Declarations, want) {
		t.Errorf("declarations =\n%+v\nwant\n%+v", a.Declarations, want)
	}

	r, a = outlineOf(t, w.world, "outline", "path=empty.go")
	if r.status != http.StatusOK || !a.is(true, "Go") || len(a.Declarations) != 0 || !strings.Contains(string(r.body), `"declarations":[]`) {
		t.Errorf("GET /outline?path=empty.go = %d %s, want 200, supported true and declarations []", r.status, r.body)
	}
}

func TestOutline_ReadsTheCommitItsRefNames(t *testing.T) {
	t.Parallel()
	w := readOnlyWorld(t)
	onMain := []string{"MaxRoutes", "Router", "Handle", "NewRouter", "match"}
	for _, c := range []struct {
		ref, sha string
		want     []string
	}{
		{"", w.outline["main"], onMain},
		{"main", w.outline["main"], onMain},
		{"feat", w.outline["feat"], append(slices.Clone(onMain), "Feat")},
		{"v1", w.outline["v1"], []string{"Second"}},
		{w.outline["first"], w.outline["first"], []string{"First"}},
		{w.outline["first"][:8], w.outline["first"], []string{"First"}},
	} {
		t.Run("ref "+c.ref, func(t *testing.T) {
			query := "path=router.go"
			if c.ref != "" {
				query += "&ref=" + c.ref
			}
			r, a := outlineOf(t, w.world, "outline", query)
			if r.status != http.StatusOK || a.ResolvedSHA != c.sha || !slices.Equal(a.names(), c.want) {
				t.Errorf("%d %s, want 200 with resolved_sha %s and the declarations %q", r.status, r.body, c.sha, c.want)
			}
		})
	}

	t.Run("on a fork", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		base := commit(t, w.repo, w.id["c2"], "a.go", "package a\n\nfunc Parent() {}\n")
		gitrepotest.Git(t, w.repo, "update-ref", "refs/heads/main", base, w.id["c2"])
		forkRepo := w.workspaces.RepoPath("demo--t1")
		own := commit(t, forkRepo, base, "a.go", "package a\n\nfunc Own() {}\n")
		gitrepotest.Git(t, forkRepo, "update-ref", "refs/heads/work", own)

		if r, a := outlineOf(t, w, "demo--t1", "path=a.go"); r.status != http.StatusOK || a.ResolvedSHA != base || !slices.Equal(a.names(), []string{"Parent"}) {
			t.Errorf("without a ref: %d %s, want the file of the fork's base branch at %s, declaring Parent", r.status, r.body, base)
		}
		if r := w.get(t, scmPath("demo--t1", "/outline?path=a.go"), w.memberKey); r.status != http.StatusOK {
			t.Errorf("the fork's owner, a member of the parent's org: %d %s, want 200", r.status, r.body)
		}
		if r, a := outlineOf(t, w, "demo--t1", "path=a.go&ref=work"); r.status != http.StatusOK || a.ResolvedSHA != own || !slices.Equal(a.names(), []string{"Own"}) {
			t.Errorf("ref=work: %d %s, want the file of the fork's own branch at %s, declaring Own", r.status, r.body, own)
		}
	})
}

func TestOutline_AnswersAFileItHasNoBackendForAsNotSupported(t *testing.T) {
	t.Parallel()
	w := readOnlyWorld(t)
	for _, path := range []string{"README.md", "data.csv", "Makefile", "LICENSE", "link.go"} {
		t.Run(path, func(t *testing.T) {
			r, a := outlineOf(t, w.world, "outline", "path="+path)
			if r.status != http.StatusOK || a.Path != path || a.ResolvedSHA != w.outline["main"] || !a.is(false, "") || len(a.Declarations) != 0 {
				t.Errorf("%d %s, want 200 with supported false, language \"\" and declarations []", r.status, r.body)
			}
		})
	}
}

func TestOutline_RefusesWhatItCannotOutline(t *testing.T) {
	t.Parallel()
	w := readOnlyWorld(t)
	for _, c := range []struct {
		query  string
		status int
		want   string
	}{
		{"", http.StatusBadRequest, "invalid_request"},
		{"path=", http.StatusBadRequest, "invalid_request"},
		{"path=/a.go", http.StatusBadRequest, "invalid_request"},
		{"path=a//b.go", http.StatusBadRequest, "invalid_request"},
		{"path=../a.go", http.StatusBadRequest, "invalid_request"},
		{"path=.git/config", http.StatusBadRequest, "invalid_request"},
		{"path=a%00.go", http.StatusBadRequest, "invalid_request"},
		{"path=router.go&ref=nope", http.StatusNotFound, "ref_not_found"},
		{"path=nope.go", http.StatusNotFound, "path_not_found"},
		{"path=only.go", http.StatusNotFound, "path_not_found"},
		{"path=internal", http.StatusBadRequest, "invalid_request"},
		{"path=vendor/sub", http.StatusBadRequest, "invalid_request"},
		{"path=big.go", http.StatusRequestEntityTooLarge, "payload_too_large"},
		{"path=logo.png", http.StatusUnprocessableEntity, "binary_file"},
		{"path=blob.go", http.StatusUnprocessableEntity, "binary_file"},
	} {
		t.Run(c.query, func(t *testing.T) {
			r := w.get(t, scmPath("outline", "/outline?"+c.query), w.ownerKey)
			e, _ := r.json["error"].(map[string]any)
			if r.status != c.status || r.errorType() != c.want || e["code"] != float64(c.status) {
				t.Fatalf("%d %s, want the %d %s envelope", r.status, r.body, c.status, c.want)
			}
			path := url.Values{}
			if q, err := url.ParseQuery(c.query); err == nil {
				path = q
			}
			switch {
			case c.status == http.StatusBadRequest && !strings.Contains(c.query, "internal") && !strings.Contains(c.query, "vendor"):
				// The file route's own words for the same path.
				if f := w.get(t, scmPath("outline", "/file?"+c.query), w.ownerKey); f.status != http.StatusBadRequest || f.message() != r.message() {
					t.Errorf("message %q, want the file route's for the same path, %q", r.message(), f.message())
				}
			case c.query == "path=internal" || c.query == "path=vendor/sub":
				if !strings.Contains(r.message(), "not a file") {
					t.Errorf("message %q does not say that the path is not a file", r.message())
				}
			case c.want == "payload_too_large":
				if e["size"] != float64(1<<20+1) {
					t.Errorf("size = %v, want the file's 1048577 bytes (body %s)", e["size"], r.body)
				}
			case c.want == "binary_file":
				if !strings.Contains(r.message(), path.Get("path")) {
					t.Errorf("message %q does not name the path %s", r.message(), path.Get("path"))
				}
			}
		})
	}

	if r, a := outlineOf(t, w.world, "outline", "path=only.go&ref=feat"); r.status != http.StatusOK || !slices.Equal(a.names(), []string{"Only"}) {
		t.Errorf("only.go at feat, where it exists: %d %s, want 200 declaring Only", r.status, r.body)
	}
	if r, a := outlineOf(t, w.world, "outline", "path=edge.go"); r.status != http.StatusOK || !a.is(true, "Go") || !slices.Equal(a.names(), []string{"Edge"}) {
		t.Errorf("edge.go, exactly 1 MiB: %d %.200s, want 200 declaring Edge", r.status, r.body)
	}
	if r, _ := outlineOf(t, w.world, "outline", "path=late.go"); r.status != http.StatusOK {
		t.Errorf("late.go, whose only NUL is byte 9,000: %d %.200s, want 200", r.status, r.body)
	}
}

// The outline package reads a file from disk when it is handed no content
// at all, so an empty file's content must still be content.
func TestOutline_OutlinesAnEmptyFileFromItsContentAlone(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var got [][]byte
	w := newWorld(t, func(d *scm.Deps) {
		open := d.Open
		d.Open = func(ctx context.Context, slug string) (scm.Repository, error) {
			repo, err := open(ctx, slug)
			return emptyBlobs{repo}, err
		}
		d.Outline = func(ctx context.Context, path string, src []byte) (outline.File, error) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, src)
			return outline.Outline(ctx, path, src, outline.Options{})
		}
	})
	x := commit(t, w.repo, w.id["c2"], "etc/hosts.sh", "")
	r, a := outlineOf(t, w, "demo", "path=etc/hosts.sh&ref="+x)
	if r.status != http.StatusOK || a.Language == nil || *a.Language != "Shell" || len(a.Declarations) != 0 {
		t.Errorf("%d %s, want 200 with the language Shell and no declarations", r.status, r.body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] == nil {
		t.Errorf("the outline was handed %v, want the file's content once, empty and not nil", got)
	}
}
