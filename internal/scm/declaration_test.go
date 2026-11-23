package scm_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agent-fox-dev/agentkit-go/outline"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/scm"
)

// declSvc is svc.go on the main of the decl workspace: a function with a
// doc comment, two methods of one name on two types, and a function whose
// name differs from the first only in case.
const declSvc = `package svc

// Greet greets.
func Greet() string {
	return "hi"
}

type Reader struct{}

type Writer struct{}

func (r *Reader) Close() error {
	return nil
}

func (w *Writer) Close() error {
	return nil
}

func greet() {}
`

// declBroken is broken.go on the main of the decl workspace: Open lacks its
// closing brace, so the outline does not know where it ends.
const declBroken = "package a\n\nfunc Whole() {}\n\nfunc Open() string {\n\treturn \"x\"\n\nfunc Other() {}\n"

// addDeclarations adds the ready workspace decl of the owner and returns
// the id of its main, which holds svc.go (declSvc), crlf.go (a Go file
// with CRLF line endings), tail.go (whose last line, a one-line function,
// has no line ending), broken.go (declBroken), README.md, the symlink
// link.go to svc.go, and the executable run.go.
func (w *world) addDeclarations(t testing.TB) string {
	t.Helper()
	data := func(s string) string { return "data " + strconv.Itoa(len(s)) + "\n" + s + "\n" }
	file := func(path, content string) string { return "M 100644 inline " + path + "\n" + data(content) }
	dir := w.addWorkspace(t, "decl", w.org, "commit refs/heads/main\n"+testCommitter+data("decl")+
		file("svc.go", declSvc)+file("crlf.go", "package a\r\n\r\nfunc Greet() {\r\n}\r\n")+
		file("tail.go", "package a\n\nfunc Tail() {}")+file("broken.go", declBroken)+file("README.md", "# Decl\n")+
		"M 120000 inline link.go\n"+data("svc.go")+"M 100755 inline run.go\n"+data("package main\n\nfunc main() {}\n")+"\n")
	return strings.TrimSpace(gitrepotest.Git(t, dir, "rev-parse", "main"))
}

// declarationFound is one declaration of the declaration route's answer;
// Content is nil when the answer's member is null.
type declarationFound struct {
	declaration
	Content *string `json:"content"`
}

// declarationsAnswer is the answer of the declaration route.
type declarationsAnswer struct {
	Path         string             `json:"path"`
	ResolvedSHA  string             `json:"resolved_sha"`
	BlobSHA      string             `json:"blob_sha"`
	Language     string             `json:"language"`
	Declarations []declarationFound `json:"declarations"`
}

// names are the names of the answer's declarations, in its order, each
// with its container before a dot when it has one.
func (a declarationsAnswer) names() []string {
	names := []string{}
	for _, d := range a.Declarations {
		if d.Container != "" {
			names = append(names, d.Container+"."+d.Name)
		} else {
			names = append(names, d.Name)
		}
	}
	return names
}

// declarationOf is the answer of GET …/scm/declaration?query of the
// workspace slug to the owner, decoded when it is 200.
func declarationOf(t *testing.T, w *world, slug, query string) (reply, declarationsAnswer) {
	t.Helper()
	r := w.get(t, scmPath(slug, "/declaration?"+query), w.ownerKey)
	var a declarationsAnswer
	if r.status == http.StatusOK {
		if err := json.Unmarshal(r.body, &a); err != nil {
			t.Fatalf("GET /declaration?%s: the answer is not the declaration object: %v\n%s", query, err, r.body)
		}
	}
	return r, a
}

// lines are the lines start to end, 1-based and inclusive, of content
// split after each LF.
func lines(content string, start, end int) string {
	all := strings.SplitAfter(content, "\n")
	return strings.Join(all[start-1:end], "")
}

func TestDeclaration_AnswersTheDeclarationsOfANameWithTheirLines(t *testing.T) {
	t.Parallel()
	w := readOnlyWorld(t)
	r, a := declarationOf(t, w.world, "decl", "path=svc.go&name=Greet")
	blob := strings.TrimSpace(gitrepotest.Git(t, w.workspaces.RepoPath("decl"), "rev-parse", "main:svc.go"))
	if r.status != http.StatusOK || a.Path != "svc.go" || a.ResolvedSHA != w.decl || a.BlobSHA != blob || a.Language != "Go" {
		t.Fatalf("GET /declaration?path=svc.go&name=Greet = %d %s, want 200 with path svc.go, resolved_sha %s, blob_sha %s and language Go",
			r.status, r.body, w.decl, blob)
	}
	_, o := outlineOf(t, w.world, "decl", "path=svc.go")
	var greet declaration
	for _, d := range o.Declarations {
		if d.Name == "Greet" {
			greet = d
		}
	}
	want := lines(declSvc, greet.StartLine, greet.EndLine)
	if len(a.Declarations) != 1 || a.Declarations[0].declaration != greet || a.Declarations[0].Content == nil || *a.Declarations[0].Content != want {
		t.Errorf("declarations = %+v, want the outline's Greet %+v with the content %q", a.Declarations, greet, want)
	}
	if strings.Contains(want, "Greet greets") {
		t.Errorf("the content %q holds the doc comment", want)
	}

	for _, c := range []struct {
		query string
		want  []string
	}{
		{"path=svc.go&name=Close", []string{"Reader.Close", "Writer.Close"}},
		{"path=svc.go&name=greet", []string{"greet"}},
	} {
		if r, a := declarationOf(t, w.world, "decl", c.query); r.status != http.StatusOK || !slices.Equal(a.names(), c.want) {
			t.Errorf("GET /declaration?%s = %d %s, want 200 with %q in line order", c.query, r.status, r.body, c.want)
		}
	}

	if r, a := declarationOf(t, w.world, "decl", "path=crlf.go&name=Greet"); r.status != http.StatusOK || len(a.Declarations) != 1 ||
		a.Declarations[0].Content == nil || *a.Declarations[0].Content != "func Greet() {\r\n}\r\n" {
		t.Errorf("crlf.go: %d %s, want the content with its CRLF line endings", r.status, r.body)
	}
	if r, a := declarationOf(t, w.world, "decl", "path=tail.go&name=Tail"); r.status != http.StatusOK || len(a.Declarations) != 1 ||
		a.Declarations[0].Content == nil || *a.Declarations[0].Content != "func Tail() {}" {
		t.Errorf("tail.go: %d %s, want the last line as content, without a line ending", r.status, r.body)
	}
}

func TestDeclaration_NarrowsTheMatchesByContainerAndKind(t *testing.T) {
	t.Parallel()
	w := readOnlyWorld(t)
	for _, c := range []struct {
		query string
		want  []string
	}{
		{"name=Close&container=Writer", []string{"Writer.Close"}},
		{"name=Close&kind=method", []string{"Reader.Close", "Writer.Close"}},
		{"name=Close&container=Writer&kind=func", nil},
		{"name=Greet&container=", []string{"Greet"}},
	} {
		t.Run(c.query, func(t *testing.T) {
			r, a := declarationOf(t, w.world, "decl", "path=svc.go&"+c.query)
			switch {
			case c.want == nil && (r.status != http.StatusNotFound || r.errorType() != "declaration_not_found"):
				t.Errorf("%d %s, want 404 declaration_not_found", r.status, r.body)
			case c.want != nil && (r.status != http.StatusOK || !slices.Equal(a.names(), c.want)):
				t.Errorf("%d %s, want 200 with %q", r.status, r.body, c.want)
			}
		})
	}
}

func TestDeclaration_ReadsTheCommitItsRefNames(t *testing.T) {
	t.Parallel()
	w := readOnlyWorld(t)
	for _, c := range []struct{ ref, sha, name string }{
		{"", w.outline["main"], "NewRouter"},
		{"main", w.outline["main"], "NewRouter"},
		{"feat", w.outline["feat"], "Feat"},
		{"v1", w.outline["v1"], "Second"},
		{w.outline["first"], w.outline["first"], "First"},
		{w.outline["first"][:8], w.outline["first"], "First"},
	} {
		t.Run("ref "+c.ref, func(t *testing.T) {
			query := "path=router.go&name=" + c.name
			if c.ref != "" {
				query += "&ref=" + c.ref
			}
			r, a := declarationOf(t, w.world, "outline", query)
			if r.status != http.StatusOK || a.ResolvedSHA != c.sha || !slices.Equal(a.names(), []string{c.name}) {
				t.Errorf("%d %s, want 200 with resolved_sha %s and %s", r.status, r.body, c.sha, c.name)
			}
		})
	}
	if _, a := declarationOf(t, w.world, "outline", "path=router.go&name=Second&ref=v1"); len(a.Declarations) == 1 &&
		(a.Declarations[0].Content == nil || *a.Declarations[0].Content != "func Second() {}\n") {
		t.Errorf("Second at v1: content %v, want that commit's line", a.Declarations[0].Content)
	}

	t.Run("on a fork", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		base := commit(t, w.repo, w.id["c2"], "a.go", "package a\n\nfunc Parent() {}\n")
		gitrepotest.Git(t, w.repo, "update-ref", "refs/heads/main", base, w.id["c2"])
		forkRepo := w.workspaces.RepoPath("demo--t1")
		own := commit(t, forkRepo, base, "a.go", "package a\n\nfunc Parent() { own() }\n")
		gitrepotest.Git(t, forkRepo, "update-ref", "refs/heads/work", own)

		r, a := declarationOf(t, w, "demo--t1", "path=a.go&name=Parent")
		if r.status != http.StatusOK || a.ResolvedSHA != base || len(a.Declarations) != 1 || *a.Declarations[0].Content != "func Parent() {}\n" {
			t.Errorf("without a ref: %d %s, want the file of the fork's base branch at %s", r.status, r.body, base)
		}
		r, a = declarationOf(t, w, "demo--t1", "path=a.go&name=Parent&ref=work")
		if r.status != http.StatusOK || a.ResolvedSHA != own || len(a.Declarations) != 1 || *a.Declarations[0].Content != "func Parent() { own() }\n" {
			t.Errorf("ref=work: %d %s, want the file of the fork's own branch at %s", r.status, r.body, own)
		}
	})
}

func TestDeclaration_RefusesANameTheFileDoesNotDeclare(t *testing.T) {
	t.Parallel()
	w := readOnlyWorld(t)
	for _, name := range []string{"Nope", "GREET"} {
		r := w.get(t, scmPath("decl", "/declaration?path=svc.go&name="+name), w.ownerKey)
		if r.status != http.StatusNotFound || r.errorType() != "declaration_not_found" ||
			!strings.Contains(r.message(), name) || !strings.Contains(r.message(), "svc.go") {
			t.Errorf("name=%s: %d %s, want 404 declaration_not_found naming %s and svc.go", name, r.status, r.body, name)
		}
	}
}

func TestDeclaration_RefusesAFileItCannotOutline(t *testing.T) {
	t.Parallel()
	w := readOnlyWorld(t)
	for _, path := range []string{"README.md", "data.csv", "Makefile", "LICENSE", "link.go"} {
		t.Run(path, func(t *testing.T) {
			r := w.get(t, scmPath("outline", "/declaration?name=x&path="+path), w.ownerKey)
			e, _ := r.json["error"].(map[string]any)
			details, _ := e["details"].(map[string]any)
			if language, ok := details["language"]; r.status != http.StatusUnprocessableEntity || r.errorType() != "unsupported_language" || !ok || language != "" {
				t.Errorf("%d %s, want 422 unsupported_language with details.language \"\"", r.status, r.body)
			}
		})
	}

	t.Run("a language this build has no backend for", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t, func(d *scm.Deps) {
			d.Outline = func(context.Context, string, []byte) (outline.File, error) {
				return outline.File{Lang: outline.LangPython, Backend: outline.BackendNone, Decls: []outline.Decl{},
					NotParsed: outline.NotParsedUnsupported, SyntaxErrors: []outline.SyntaxError{}}, nil
			}
		})
		x := commit(t, w.repo, w.id["c2"], "app.py", "def serve():\n    pass\n")
		r := w.get(t, scmPath("demo", "/declaration?path=app.py&name=serve&ref="+x), w.ownerKey)
		e, _ := r.json["error"].(map[string]any)
		details, _ := e["details"].(map[string]any)
		if r.status != http.StatusUnprocessableEntity || r.errorType() != "unsupported_language" || details["language"] != "Python" {
			t.Errorf("%d %s, want 422 unsupported_language with details.language Python", r.status, r.body)
		}
	})
}

func TestDeclaration_RefusesWhatItCannotReadBeforeParsing(t *testing.T) {
	t.Parallel()
	var parses atomic.Int32
	w := newWorld(t, func(d *scm.Deps) {
		d.Outline = func(ctx context.Context, path string, src []byte) (outline.File, error) {
			parses.Add(1)
			return outline.Outline(ctx, path, src, outline.Options{})
		}
	})
	w.addOutline(t)
	long := strings.Repeat("a", 201)
	for _, c := range []struct {
		query  string
		status int
		want   string
	}{
		{"name=x", http.StatusBadRequest, "invalid_request"},
		{"path=../x.go&name=x", http.StatusBadRequest, "invalid_request"},
		{"path=router.go", http.StatusBadRequest, "invalid_request"},
		{"path=router.go&name=", http.StatusBadRequest, "invalid_request"},
		{"path=router.go&name=" + long, http.StatusBadRequest, "invalid_request"},
		{"path=router.go&name=x&kind=function", http.StatusBadRequest, "invalid_request"},
		{"path=nope.go&name=x", http.StatusNotFound, "path_not_found"},
		{"path=internal&name=x", http.StatusBadRequest, "invalid_request"},
		{"path=vendor/sub&name=x", http.StatusBadRequest, "invalid_request"},
		{"path=big.go&name=x", http.StatusRequestEntityTooLarge, "payload_too_large"},
		{"path=logo.png&name=x", http.StatusUnprocessableEntity, "binary_file"},
		{"path=blob.go&name=x", http.StatusUnprocessableEntity, "binary_file"},
		{"path=router.go&name=x&ref=nope", http.StatusNotFound, "ref_not_found"},
	} {
		t.Run(c.query, func(t *testing.T) {
			before := parses.Load()
			r := w.get(t, scmPath("outline", "/declaration?"+c.query), w.ownerKey)
			if r.status != c.status || r.errorType() != c.want {
				t.Fatalf("%d %.300s, want %d %s", r.status, r.body, c.status, c.want)
			}
			if c.want == "payload_too_large" {
				if e, _ := r.json["error"].(map[string]any); e["size"] != float64(1<<20+1) {
					t.Errorf("size = %v, want the file's 1048577 bytes", e["size"])
				}
			}
			if n := parses.Load() - before; n != 0 {
				t.Errorf("%d parses, want none", n)
			}
		})
	}
	if r := w.get(t, scmPath("outline", "/declaration?path=router.go&name="+long[:200]), w.ownerKey); r.errorType() != "declaration_not_found" {
		t.Errorf("a name of 200 characters: %d %s, want 404 declaration_not_found", r.status, r.body)
	}
	if r := w.get(t, scmPath("outline", "/declaration?path=edge.go&name=Edge"), w.ownerKey); r.status != http.StatusOK {
		t.Errorf("edge.go, exactly 1 MiB: %d %.200s, want 200", r.status, r.body)
	}
}

func TestDeclaration_AnswersAFileWithSyntaxErrorsAsFarAsItsOutlineGoes(t *testing.T) {
	t.Parallel()
	w := readOnlyWorld(t)
	r, a := declarationOf(t, w.world, "decl", "path=broken.go&name=Whole")
	if r.status != http.StatusOK || len(a.Declarations) != 1 || a.Declarations[0].Content == nil || *a.Declarations[0].Content != "func Whole() {}\n" {
		t.Errorf("Whole: %d %s, want 200 with its one line", r.status, r.body)
	}
	r, a = declarationOf(t, w.world, "decl", "path=broken.go&name=Open")
	if r.status != http.StatusOK || len(a.Declarations) != 1 || a.Declarations[0].StartLine != 5 || a.Declarations[0].EndLine != 0 ||
		a.Declarations[0].Content != nil || !strings.Contains(string(r.body), `"content":null`) {
		t.Errorf("Open: %d %s, want 200 with start_line 5, end_line 0 and content null", r.status, r.body)
	}
}
