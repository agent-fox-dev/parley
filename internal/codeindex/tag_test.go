package codeindex_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/identity"
)

func TestQuery_AnswersAForkFromItsParentsIndexAndSaysSo(t *testing.T) {
	t.Parallel()
	s := newSeam()
	w := newWorld(t, enabled, s.adjust)
	head := w.addWorkspace(t, "demo", symbolFiles())
	w.indexed(t, "demo")
	if code, body := w.Call(t, http.MethodPost, "/api/v1/workspaces/demo/forks", w.key, map[string]any{"name": "x", "auto_change": false}); code != http.StatusCreated {
		t.Fatalf("create the fork demo--x = %d %s, want 201", code, body)
	}
	w.commit(t, "demo--x", "work", map[string]string{"fork.go": "package api\n\n// forkword\n"})
	// A token bound to the fork reads the fork and not its parent.
	bound := w.IssuePAT(t, identity.PATSpec{UserID: w.owner, Permissions: []string{"git:read"}, Workspace: "demo--x", CreatedAt: time.Now().UTC()})
	if a := w.ask(t, "demo", "code-search", "q=NewRouter", bound); a.code == http.StatusOK {
		t.Fatalf("the token bound to the fork reads its parent: %d %s", a.code, a.body)
	}

	var found searchAnswer
	if a := w.ask(t, "demo--x", "code-search", "q=NewRouter", bound); a.code != http.StatusOK || json.Unmarshal(a.body, &found) != nil ||
		found.Workspace != "demo" || str(found.IndexedSHA) != head || !slices.Equal(found.paths(), []string{"router.go"}) {
		t.Errorf("the fork's search = %d %s, want 200 from demo's index: workspace demo, indexed_sha %s and router.go", a.code, a.body, head)
	}
	if a := w.ask(t, "demo--x", "code-search", "q=forkword", bound); a.code != http.StatusOK || json.Unmarshal(a.body, &found) != nil || len(found.Files) != 0 {
		t.Errorf("the fork's search for a word of its own branch = %d %s, want files []: the parent's index does not hold it", a.code, a.body)
	}
	var syms symbolsAnswer
	if a := w.ask(t, "demo--x", "symbols", "name=NewRouter", bound); a.code != http.StatusOK || json.Unmarshal(a.body, &syms) != nil ||
		syms.Workspace != "demo" || str(syms.IndexedSHA) != head || len(syms.Symbols) != 1 {
		t.Errorf("the fork's symbol lookup = %d %s, want 200 from demo's index with workspace demo", a.code, a.body)
	}

	// The waiting rule is the parent's: a commit to demo's main is waited for.
	release := s.hold(demoPart)
	t.Cleanup(release)
	next := w.commit(t, "demo", "main", map[string]string{"later.go": "package api\n\n// laterword\n"})
	s.waitBegan(t, demoPart)
	ch := make(chan answered, 1)
	go func() { ch <- w.ask(t, "demo--x", "code-search", "q=laterword", bound) }()
	waits(t, ch, "the fork's search")
	release()
	if a := arrives(t, ch, "the fork's search"); a.code != http.StatusOK || json.Unmarshal(a.body, &found) != nil ||
		str(found.IndexedSHA) != next || !slices.Equal(found.paths(), []string{"later.go"}) {
		t.Errorf("once demo's build ended the fork's search = %d %s, want demo's index of %s with later.go", a.code, a.body, next)
	}
}

func TestQuery_TagsAnAnswerByTheIndexedCommitAndTheQuery(t *testing.T) {
	t.Parallel()
	w := newWorld(t, enabled)
	w.addWorkspace(t, "demo", symbolFiles())
	w.indexed(t, "demo")
	tag := func(route, query string) string {
		t.Helper()
		a := w.ask(t, "demo", route, query, w.key)
		if a.code != http.StatusOK || a.header.Get("ETag") == "" {
			t.Fatalf("GET scm/%s?%s = %d with ETag %q, want 200 with a tag", route, query, a.code, a.header.Get("ETag"))
		}
		return a.header.Get("ETag")
	}
	first := tag("code-search", "q=NewRouter")
	if again := tag("code-search", "q=NewRouter"); again != first {
		t.Errorf("the same search twice carries %s and %s, want one tag", first, again)
	}
	others := []string{tag("code-search", "q=Router"), tag("code-search", "q=NewRouter&limit=5"), tag("symbols", "name=NewRouter"), tag("symbols", "name=NewRouter&exact=true")}
	if all := append([]string{first}, others...); len(slices.Compact(slices.Sorted(slices.Values(all)))) != len(all) {
		t.Errorf("another query, another limit and the symbols route carry %q beside %s, want a tag each", others, first)
	}
	tree := w.ask(t, "demo", "tree", "ref=main", w.key)
	search := w.ask(t, "demo", "code-search", "q=NewRouter", w.key)
	if got, want := search.header.Get("Cache-Control"), tree.header.Get("Cache-Control"); got != want || want == "" {
		t.Errorf("Cache-Control %q, want the one a read to revalidate carries, %q", got, want)
	}

	head := w.commit(t, "demo", "main", map[string]string{"more.go": "package api\n"})
	w.reaches(t, "demo", head)
	if after := tag("code-search", "q=NewRouter"); after == first {
		t.Errorf("after a commit and its build the same search carries %s still, want another tag", after)
	}
}

func TestQuery_AnswersNotModifiedWithoutAskingTheIndex(t *testing.T) {
	t.Parallel()
	s := newSeam()
	w := newWorld(t, enabled, s.adjust)
	w.addWorkspace(t, "demo", symbolFiles())
	w.indexed(t, "demo")
	first := w.ask(t, "demo", "code-search", "q=NewRouter", w.key)
	tag := first.header.Get("ETag")
	s.mu.Lock()
	before := s.searches
	s.mu.Unlock()

	a := w.ask(t, "demo", "code-search", "q=NewRouter", w.key, "If-None-Match", tag)
	s.mu.Lock()
	asked := s.searches - before
	s.mu.Unlock()
	if a.code != http.StatusNotModified || len(a.body) != 0 || a.header.Get("ETag") != tag || asked != 0 {
		t.Errorf("If-None-Match %s: %d with %d bytes, ETag %q, and %d searches of the index; want 304, no body, the tag and none", tag, a.code, len(a.body), a.header.Get("ETag"), asked)
	}
	if syms := w.ask(t, "demo", "symbols", "name=NewRouter", w.key); w.ask(t, "demo", "symbols", "name=NewRouter", w.key, "If-None-Match", syms.header.Get("ETag")).code != http.StatusNotModified {
		t.Errorf("the symbol lookup with its own tag is not answered 304")
	}

	// The tag is of the commit before: the same request waits for the
	// build of the new head and is answered anew.
	release := s.hold(demoPart)
	t.Cleanup(release)
	head := w.commit(t, "demo", "main", map[string]string{"more.go": "package api\n\nfunc NewRouterToo() {}\n"})
	s.waitBegan(t, demoPart)
	ch := make(chan answered, 1)
	go func() { ch <- w.ask(t, "demo", "code-search", "q=NewRouter", w.key, "If-None-Match", tag) }()
	waits(t, ch, "the search with the old tag")
	release()
	var found searchAnswer
	if a := arrives(t, ch, "the search with the old tag"); a.code != http.StatusOK || json.Unmarshal(a.body, &found) != nil || str(found.IndexedSHA) != head || a.header.Get("ETag") == tag {
		t.Errorf("after a commit the request with the old tag = %d %s, want 200 from the index of %s with another tag", a.code, a.body, head)
	}
}

func TestQuery_LeavesARefusalWithoutATagAndUncached(t *testing.T) {
	t.Parallel()
	s := newSeam()
	w := newWorld(t, "[code_index]\n[scm]\nread_timeout = \"1s\"\n", s.adjust)
	w.addWorkspace(t, "demo", symbolFiles())
	w.addWorkspace(t, "off", symbolFiles())
	w.indexed(t, "demo")
	w.indexed(t, "off")
	if code, body := w.Call(t, http.MethodPatch, "/api/v1/workspaces/off", w.key, map[string]any{"settings": map[string]any{"code_index": false}}); code != http.StatusOK {
		t.Fatalf("PATCH code_index false = %d %s, want 200", code, body)
	}
	refused := w.ask(t, "demo", "tree", "ref=nope", w.key)
	check := func(a answered, code int, errorType string) {
		t.Helper()
		if a.code != code || a.refusal().Error.ErrorType != errorType || a.header.Get("ETag") != "" ||
			a.header.Get("Cache-Control") != refused.header.Get("Cache-Control") {
			t.Errorf("%d %s with ETag %q and Cache-Control %q; want %d %s without a tag and with the Cache-Control of a refused read, %q",
				a.code, a.body, a.header.Get("ETag"), a.header.Get("Cache-Control"), code, errorType, refused.header.Get("Cache-Control"))
		}
	}
	check(w.ask(t, "demo", "code-search", "q=(unclosed", w.key), http.StatusBadRequest, "invalid_query")
	check(w.ask(t, "off", "code-search", "q=a", w.key), http.StatusConflict, "code_index_off")
	check(w.ask(t, "off", "symbols", "name=a", w.key), http.StatusConflict, "code_index_off")
	t.Cleanup(s.hold(demoPart))
	w.commit(t, "demo", "main", map[string]string{"more.go": "package api\n"})
	check(w.ask(t, "demo", "code-search", "q=a", w.key), http.StatusServiceUnavailable, "code_index_not_ready")
}
