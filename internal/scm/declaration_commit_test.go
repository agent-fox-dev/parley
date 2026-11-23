package scm_test

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/scm"
)

// declKinds is kinds.go on the main of the dc workspace: a type and a
// method of one name.
const declKinds = "package svc\n\ntype Close struct{}\n\nfunc (Close) Close() {}\n"

// declWorld is a commitWorld with the workspace dc of the owner, whose
// main holds svc.go (declSvc), kinds.go (declKinds), many.go (25 functions
// init), the executable bin/tool.go, docs/guide.md and the submodule entry
// vendor/lib; dir is its git directory, main its main and path its commit
// route.
type declWorld struct {
	*commitWorld
	dir, main, path string
}

func newDeclWorld(t *testing.T) *declWorld {
	t.Helper()
	w := &declWorld{commitWorld: newCommitWorld(t, ""), path: scmPath("dc", "/commits")}
	data := func(s string) string { return "data " + strconv.Itoa(len(s)) + "\n" + s + "\n" }
	file := func(path, content string) string { return "M 100644 inline " + path + "\n" + data(content) }
	many := "package svc\n" + strings.Repeat("\nfunc init() {}\n", 25)
	w.dir = w.addWorkspace(t, "dc", w.org, "commit refs/heads/main\n"+testCommitter+data("dc")+
		file("svc.go", declSvc)+file("kinds.go", declKinds)+file("many.go", many)+file("docs/guide.md", "guide\n")+
		"M 100755 inline bin/tool.go\n"+data("package main\n\nfunc main() {}\n")+
		"M 160000 "+strings.Repeat("5", 40)+" vendor/lib\n\n")
	w.main = strings.TrimSpace(gitrepotest.Git(t, w.dir, "rev-parse", "main"))
	return w
}

// at makes the branch name at main and returns its head.
func (w *declWorld) at(t *testing.T, name string) string {
	t.Helper()
	gitrepotest.Git(t, w.dir, "update-ref", "refs/heads/"+name, w.main)
	return w.main
}

// commit sends a commit of ops to the branch name at head.
func (w *declWorld) commit(t *testing.T, name, head string, ops ...any) reply {
	t.Helper()
	return w.sendJSON(t, http.MethodPost, w.path, w.ownerKey, map[string]any{"branch": name, "expected_head_sha": head,
		"message": "m", "operations": ops})
}

// file is the content of path on the branch name.
func (w *declWorld) file(t *testing.T, name, path string) string {
	t.Helper()
	return string(gitrepotest.Blob(t, w.dir, name+":"+path))
}

// branchHead is the commit the branch name of dc is at.
func (w *declWorld) branchHead(t *testing.T, name string) string {
	t.Helper()
	return gitrepotest.Refs(t, w.dir, "refs/heads/")[name]
}

func replaceDecl(path, name, content string, more ...string) map[string]any {
	op := map[string]any{"op": "replace_declaration", "path": path, "name": name, "content": content}
	for i := 0; i+1 < len(more); i += 2 {
		op[more[i]] = more[i+1]
	}
	return op
}

func deleteDecl(path, name string, more ...string) map[string]any {
	op := map[string]any{"op": "delete_declaration", "path": path, "name": name}
	for i := 0; i+1 < len(more); i += 2 {
		op[more[i]] = more[i+1]
	}
	return op
}

func insertDecl(path, where string, at map[string]any, content string) map[string]any {
	return map[string]any{"op": "insert_declaration", "path": path, where: at, "content": content}
}

func TestDeclarationCommit(t *testing.T) {
	t.Parallel()
	w := newDeclWorld(t)
	newGreet := "func Greet(name string) string {\n\treturn \"hi \" + name\n}\n"

	t.Run("replaces a declaration's lines and leaves every other byte", func(t *testing.T) {
		head := w.at(t, "replace")
		r := w.commit(t, "replace", head, replaceDecl("svc.go", "Greet", newGreet))
		if r.status != http.StatusCreated || answerCommit(r) == nil {
			t.Fatalf("%d %s, want 201 with a commit", r.status, r.body)
		}
		for _, k := range []string{"branch", "change", "change_error"} {
			if _, ok := r.json[k]; !ok {
				t.Errorf("the answer has no %s: %s", k, r.body)
			}
		}
		if got, want := w.file(t, "replace", "svc.go"), lines(declSvc, 1, 3)+newGreet+lines(declSvc, 7, 20); got != want {
			t.Errorf("svc.go =\n%s\nwant\n%s", got, want)
		}
		if parents := answerCommit(r)["parents"].([]any); len(parents) != 1 || parents[0] != head {
			t.Errorf("parents %v, want [%s]", parents, head)
		}

		head = w.at(t, "exec")
		if r := w.commit(t, "exec", head, replaceDecl("bin/tool.go", "main", "func main() { run() }\n")); r.status != http.StatusCreated {
			t.Fatalf("bin/tool.go: %d %s, want 201", r.status, r.body)
		}
		if tree := gitrepotest.Git(t, w.dir, "ls-tree", "exec", "bin/tool.go"); !strings.HasPrefix(tree, "100755 ") {
			t.Errorf("bin/tool.go is %q, want it still executable", tree)
		}
	})

	t.Run("deletes a declaration's lines", func(t *testing.T) {
		head := w.at(t, "delete")
		if r := w.commit(t, "delete", head, deleteDecl("svc.go", "greet")); r.status != http.StatusCreated {
			t.Fatalf("%d %s, want 201", r.status, r.body)
		}
		if got, want := w.file(t, "delete", "svc.go"), lines(declSvc, 1, 19); got != want {
			t.Errorf("svc.go =\n%s\nwant\n%s", got, want)
		}
		if _, a := outlineOf(t, w.world, "dc", "path=svc.go&ref=delete"); slices.Contains(a.names(), "greet") {
			t.Errorf("the outline still lists greet: %v", a.names())
		}
	})

	t.Run("inserts after or before a declaration", func(t *testing.T) {
		const added = "func Added() {}\n"
		for i, tc := range []struct {
			where string
			at    map[string]any
			want  string
		}{
			{"after", map[string]any{"name": "Greet"}, lines(declSvc, 1, 6) + added + lines(declSvc, 7, 20)},
			{"before", map[string]any{"name": "Greet"}, lines(declSvc, 1, 3) + added + lines(declSvc, 4, 20)},
			{"after", map[string]any{"name": "Close", "container": "Writer"}, lines(declSvc, 1, 18) + added + lines(declSvc, 19, 20)},
		} {
			name := "insert-" + strconv.Itoa(i)
			head := w.at(t, name)
			if r := w.commit(t, name, head, insertDecl("svc.go", tc.where, tc.at, added)); r.status != http.StatusCreated {
				t.Fatalf("%s %v: %d %s, want 201", tc.where, tc.at, r.status, r.body)
			}
			if got := w.file(t, name, "svc.go"); got != tc.want {
				t.Errorf("%s %v: svc.go =\n%s\nwant\n%s", tc.where, tc.at, got, tc.want)
			}
		}
	})

	t.Run("applies each operation to the file as the operations before it left it", func(t *testing.T) {
		longer := "func Greet() string {\n\ts := \"hi\"\n\ts += \"!\"\n\ts += \"!\"\n\ts += \"!\"\n\treturn s\n}\n"
		for i, tc := range []struct {
			name string
			ops  []any
			path string
			want string
		}{
			{"two replacements", []any{replaceDecl("svc.go", "Greet", longer), replaceDecl("svc.go", "greet", "func greet() { _ = 1 }\n")},
				"svc.go", lines(declSvc, 1, 3) + longer + lines(declSvc, 7, 19) + "func greet() { _ = 1 }\n"},
			{"a put, then a replacement in it", []any{put("new.go", "package svc\n\nfunc F() {}\n"), replaceDecl("new.go", "F", "func F() { g() }\n")},
				"new.go", "package svc\n\nfunc F() { g() }\n"},
			{"a move, then a replacement at the new path", []any{map[string]any{"op": "move", "from": "svc.go", "to": "pkg/svc.go"},
				replaceDecl("pkg/svc.go", "Greet", newGreet)}, "pkg/svc.go", lines(declSvc, 1, 3) + newGreet + lines(declSvc, 7, 20)},
			{"a rename, then an insert after the new name", []any{replaceDecl("svc.go", "Greet", "func Hello() {}\n"),
				insertDecl("svc.go", "after", map[string]any{"name": "Hello"}, "func Added() {}\n")},
				"svc.go", lines(declSvc, 1, 3) + "func Hello() {}\nfunc Added() {}\n" + lines(declSvc, 7, 20)},
		} {
			name := "order-" + strconv.Itoa(i)
			head := w.at(t, name)
			if r := w.commit(t, name, head, tc.ops...); r.status != http.StatusCreated {
				t.Fatalf("%s: %d %s, want 201", tc.name, r.status, r.body)
			}
			if got := w.file(t, name, tc.path); got != tc.want {
				t.Errorf("%s: %s =\n%s\nwant\n%s", tc.name, tc.path, got, tc.want)
			}
		}
	})

	t.Run("accepts a replacement that no longer declares the name", func(t *testing.T) {
		for i, content := range []string{
			"func Other() string {\n\treturn \"x\"\n}\n",
			"func A() {}\n\nfunc B() {}\n",
			"// gone\n",
		} {
			name := "free-" + strconv.Itoa(i)
			head := w.at(t, name)
			if r := w.commit(t, name, head, replaceDecl("svc.go", "Greet", content)); r.status != http.StatusCreated {
				t.Fatalf("%q: %d %s, want 201", content, r.status, r.body)
			}
			if got, want := w.file(t, name, "svc.go"), lines(declSvc, 1, 3)+content+lines(declSvc, 7, 20); got != want {
				t.Errorf("%q: svc.go =\n%s\nwant\n%s", content, got, want)
			}
		}
	})

	t.Run("refuses an address that matches no declaration", func(t *testing.T) {
		head := w.at(t, "nomatch")
		for _, tc := range []struct {
			name  string
			ops   []any
			index float64
		}{
			{"a name the file does not declare", []any{replaceDecl("svc.go", "Nope", "x\n")}, 0},
			{"a name in another case", []any{replaceDecl("svc.go", "GREET", "x\n")}, 0},
			{"a container nothing has", []any{deleteDecl("svc.go", "Close", "container", "Nobody")}, 0},
			{"a kind the name does not have", []any{replaceDecl("svc.go", "Greet", "x\n", "kind", "method")}, 0},
			{"insert after nothing", []any{insertDecl("svc.go", "after", map[string]any{"name": "Nope"}, "x\n")}, 0},
			{"insert before nothing", []any{insertDecl("svc.go", "before", map[string]any{"name": "Nope"}, "x\n")}, 0},
			{"a name an earlier operation replaced", []any{replaceDecl("svc.go", "Greet", "func Hello() {}\n"), deleteDecl("svc.go", "Greet")}, 1},
		} {
			seq := w.lastSeq(t)
			r := w.commit(t, "nomatch", head, tc.ops...)
			d := errorDetails(r)
			if r.status != http.StatusNotFound || r.errorType() != "declaration_not_found" || d["index"] != tc.index || d["path"] != "svc.go" {
				t.Errorf("%s: %d %s, want 404 declaration_not_found with details index %v and path svc.go", tc.name, r.status, r.body, tc.index)
			}
			if got := w.branchHead(t, "nomatch"); got != head {
				t.Errorf("%s: the branch moved to %s", tc.name, got)
			}
			if types, _ := w.eventsAfter(t, seq); len(types) != 0 {
				t.Errorf("%s: events %q, want none", tc.name, types)
			}
		}
	})

	t.Run("refuses an address that matches more than one declaration and lists them", func(t *testing.T) {
		head := w.at(t, "ambiguous")
		_, o := outlineOf(t, w.world, "dc", "path=svc.go")
		var want []any
		for _, d := range o.Declarations {
			if d.Name == "Close" {
				want = append(want, map[string]any{"kind": d.Kind, "name": d.Name, "container": d.Container, "signature": d.Signature,
					"start_line": float64(d.StartLine), "end_line": float64(d.EndLine)})
			}
		}
		for _, op := range []any{
			replaceDecl("svc.go", "Close", "x\n"),
			deleteDecl("svc.go", "Close"),
			insertDecl("svc.go", "after", map[string]any{"name": "Close"}, "x\n"),
		} {
			seq := w.lastSeq(t)
			r := w.commit(t, "ambiguous", head, op)
			d := errorDetails(r)
			candidates, _ := d["candidates"].([]any)
			if r.status != http.StatusConflict || r.errorType() != "ambiguous_declaration" || d["index"] != float64(0) || d["path"] != "svc.go" ||
				len(want) != 2 || !slices.EqualFunc(candidates, want, func(a, b any) bool { return equalJSON(a, b) }) {
				t.Errorf("%v: %d %s, want 409 ambiguous_declaration with index 0, path svc.go and the candidates %v", op, r.status, r.body, want)
			}
			if got := w.branchHead(t, "ambiguous"); got != head {
				t.Errorf("%v: the branch moved to %s", op, got)
			}
			if types, _ := w.eventsAfter(t, seq); len(types) != 0 {
				t.Errorf("%v: events %q, want none", op, types)
			}
		}
		r := w.commit(t, "ambiguous", head, deleteDecl("many.go", "init"))
		candidates, _ := errorDetails(r)["candidates"].([]any)
		if r.status != http.StatusConflict || len(candidates) != 20 {
			t.Fatalf("many.go: %d %.300s, want 409 with 20 candidates", r.status, r.body)
		}
		for i, c := range candidates {
			if line := c.(map[string]any)["start_line"]; line != float64(3+2*i) {
				t.Errorf("candidate %d starts on line %v, want %d: the first 20 in line order", i, line, 3+2*i)
			}
		}
	})

	t.Run("resolves an ambiguous name by its container or its kind", func(t *testing.T) {
		head := w.at(t, "narrow")
		if r := w.commit(t, "narrow", head, replaceDecl("svc.go", "Close", "func (w *Writer) Close() error {\n\treturn w.flush()\n}\n",
			"container", "Writer")); r.status != http.StatusCreated {
			t.Fatalf("container Writer: %d %s, want 201", r.status, r.body)
		}
		if got, want := w.file(t, "narrow", "svc.go"), lines(declSvc, 1, 15)+"func (w *Writer) Close() error {\n\treturn w.flush()\n}\n"+lines(declSvc, 19, 20); got != want {
			t.Errorf("svc.go =\n%s\nwant\n%s", got, want)
		}
		head = w.at(t, "bykind")
		if r := w.commit(t, "bykind", head, replaceDecl("kinds.go", "Close", "type Close int\n", "kind", "type")); r.status != http.StatusCreated {
			t.Fatalf("kind type: %d %s, want 201", r.status, r.body)
		}
		if got, want := w.file(t, "bykind", "kinds.go"), "package svc\n\ntype Close int\n\nfunc (Close) Close() {}\n"; got != want {
			t.Errorf("kinds.go = %q, want %q", got, want)
		}
	})
}

// equalJSON reports whether two decoded JSON values are equal.
func equalJSON(a, b any) bool {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if !aok || !bok || len(am) != len(bm) {
		return false
	}
	for k, v := range am {
		if bm[k] != v {
			return false
		}
	}
	return true
}

func TestReplaceLines_EndsTheContentWithTheFilesOwnLineEnding(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, src  string
		start, end int
		content    string
		want       string
	}{
		{"an LF file", "a\nb\nc\n", 2, 2, "x", "a\nx\nc\n"},
		{"a CRLF file", "a\r\nb\r\nc\r\n", 2, 2, "x", "a\r\nx\r\nc\r\n"},
		{"a content with its own line ending", "a\r\nb\r\nc\r\n", 2, 2, "x\n", "a\r\nx\nc\r\n"},
		{"a file of one line without a line ending", "a", 1, 1, "x", "x\n"},
		{"the last line, without a line ending", "a\nb", 2, 2, "x", "a\nx\n"},
		{"several lines", "a\nb\nc\nd\n", 2, 3, "x\ny", "a\nx\ny\nd\n"},
		{"an empty content", "a\nb\nc\n", 2, 2, "", "a\nc\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := scm.ReplaceLines(tc.src, tc.start, tc.end, tc.content)
			if !ok || got != tc.want {
				t.Errorf("ReplaceLines(%q, %d, %d, %q) = %q, %v; want %q", tc.src, tc.start, tc.end, tc.content, got, ok, tc.want)
			}
		})
	}
}
