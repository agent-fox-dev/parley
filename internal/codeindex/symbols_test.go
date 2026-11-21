package codeindex_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// symbol is a declaration of the symbols route's answer.
type symbol struct {
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Container string `json:"container"`
	Signature string `json:"signature"`
	Exported  bool   `json:"exported"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

// symbolsAnswer is the answer of the symbols route.
type symbolsAnswer struct {
	Workspace  string   `json:"workspace"`
	IndexedSHA *string  `json:"indexed_sha"`
	Partial    bool     `json:"partial"`
	Truncated  bool     `json:"truncated"`
	Symbols    []symbol `json:"symbols"`
}

// names are the answer's symbols as "path name", sorted.
func (a symbolsAnswer) names() []string {
	names := []string{}
	for _, s := range a.Symbols {
		names = append(names, s.Path+" "+s.Name)
	}
	slices.Sort(names)
	return names
}

// symbols is the answer of a symbol lookup in the workspace slug by the
// owner; it must be 200.
func (w *world) symbols(t *testing.T, slug, query string) symbolsAnswer {
	t.Helper()
	a := w.ask(t, slug, "symbols", query, w.key)
	var s symbolsAnswer
	if err := json.Unmarshal(a.body, &s); a.code != http.StatusOK || err != nil || s.Symbols == nil {
		t.Fatalf("GET scm/symbols?%s of %s = %d %s, want 200 with symbols", query, slug, a.code, a.body)
	}
	return s
}

// symbolFiles are the files of the workspace the symbol tests read.
func symbolFiles() map[string]string {
	var bulk strings.Builder
	bulk.WriteString("package bulk\n")
	for i := range 30 {
		fmt.Fprintf(&bulk, "\nfunc Bulk%02d() {}\n", i)
	}
	return map[string]string{
		"router.go":             "package api\n\nfunc NewRouter() *Router { return &Router{} }\n\ntype newRouterOptions struct{}\n",
		"internal/api/route.go": "package api\n\ntype Router struct{}\n\nfunc (r *Router) New() {}\n",
		"bulk/bulk.go":          bulk.String(),
	}
}

func TestSymbols_FindsTheDeclarationsWhoseNameStartsWithTheGivenOne(t *testing.T) {
	t.Parallel()
	w := newWorld(t, enabled)
	head := w.addWorkspace(t, "demo", symbolFiles())
	w.indexed(t, "demo")
	for _, tc := range []struct {
		name string
		want []string
	}{
		{"newrouter", []string{"router.go NewRouter", "router.go newRouterOptions"}},
		{"NewRouter", []string{"router.go NewRouter"}},
		{"New", []string{"internal/api/route.go New", "router.go NewRouter"}},
		{"zzz", []string{}},
	} {
		got := w.symbols(t, "demo", "name="+tc.name)
		if got.Workspace != "demo" || got.IndexedSHA == nil || *got.IndexedSHA != head || got.Partial || got.Truncated {
			t.Errorf("name=%s: the answer = %+v, want workspace demo, indexed_sha %s, not partial, not truncated", tc.name, got, head)
		}
		if names := got.names(); !slices.Equal(names, tc.want) {
			t.Errorf("name=%s finds %q, want %q", tc.name, names, tc.want)
		}
	}
	got := w.symbols(t, "demo", "name=New")
	want := []symbol{
		{"internal/api/route.go", "method", "New", "Router", "func (r *Router) New()", true, 5, 5},
		{"router.go", "func", "NewRouter", "", "func NewRouter() *Router", true, 3, 3},
	}
	if !slices.Equal(got.Symbols, want) {
		t.Errorf("name=New answers\n%+v\nwant each declaration with its members, by path and line:\n%+v", got.Symbols, want)
	}
}

func TestSymbols_NarrowsTheLookupByExactKindPathAndLimit(t *testing.T) {
	t.Parallel()
	w := newWorld(t, enabled)
	w.addWorkspace(t, "demo", symbolFiles())
	w.indexed(t, "demo")
	for _, tc := range []struct {
		query     string
		want      []string
		truncated bool
	}{
		{"name=New&exact=true", []string{"internal/api/route.go New"}, false},
		{"name=new&kind=type", []string{"router.go newRouterOptions"}, false},
		{"name=New&kind=METHOD", []string{"internal/api/route.go New"}, false},
		{"name=New&path=internal/api", []string{"internal/api/route.go New"}, false},
		{"name=New&path=internal", []string{"internal/api/route.go New"}, false},
		{"name=New&path=router.go", []string{"router.go NewRouter"}, false},
		{"name=New&path=internal/api/route.go", []string{"internal/api/route.go New"}, false},
		{"name=New&path=nowhere", []string{}, false},
		{"name=New&limit=1", []string{"internal/api/route.go New"}, true},
	} {
		got := w.symbols(t, "demo", tc.query)
		if names := got.names(); !slices.Equal(names, tc.want) || got.Truncated != tc.truncated {
			t.Errorf("%s finds %q, truncated %t; want %q and %t", tc.query, names, got.Truncated, tc.want, tc.truncated)
		}
	}
	if got := w.symbols(t, "demo", "name=Bulk"); len(got.Symbols) != 20 || !got.Truncated || got.Symbols[0].Name != "Bulk00" || got.Symbols[19].Name != "Bulk19" {
		t.Errorf("a name that 30 declarations start with answers %d symbols, truncated %t; want the first 20 by line and truncated", len(got.Symbols), got.Truncated)
	}
	if got := w.symbols(t, "demo", "name=Bulk&limit=50"); len(got.Symbols) != 30 || got.Truncated {
		t.Errorf("with limit=50 the 30 declarations answer %d symbols, truncated %t; want 30 and false", len(got.Symbols), got.Truncated)
	}
}

func TestSymbols_RefusesAParameterItDoesNotTake(t *testing.T) {
	t.Parallel()
	w := newWorld(t, enabled)
	w.addWorkspace(t, "demo", symbolFiles())
	w.indexed(t, "demo")
	for _, tc := range []struct{ query, names string }{
		{"", "name"},
		{"name=", "name"},
		{"name=" + strings.Repeat("a", 201), "name"},
		{"name=a&exact=maybe", "exact"},
		{"name=a&kind=gadget", "kind"},
		{"name=a&path=/abs", "path"},
		{"name=a&path=../x", "path"},
		{"name=a&path=.git/x", "path"},
		{"name=a&limit=0", "limit"},
		{"name=a&limit=51", "limit"},
		{"name=a&limit=x", "limit"},
	} {
		a := w.ask(t, "demo", "symbols", tc.query, w.key)
		if r := a.refusal(); a.code != http.StatusBadRequest || r.Error.ErrorType != "invalid_request" || !strings.Contains(r.Error.Message, tc.names) {
			t.Errorf("%q: %d %s, want 400 invalid_request with a message naming %s", tc.query, a.code, a.body, tc.names)
		}
	}
	if got := w.symbols(t, "demo", "name=NewRouter&kind=FUNC"); !slices.Equal(got.names(), []string{"router.go NewRouter"}) {
		t.Errorf("kind=FUNC finds %q, want it taken as func", got.names())
	}
}

func TestSymbols_AreRefusedByAPartialIndexWhichCodeSearchAnswers(t *testing.T) {
	t.Parallel()
	files := map[string]string{}
	for i := range 10 {
		files[fmt.Sprintf("f%d.go", i)] = fmt.Sprintf("package f\n\nfunc Apple%d() {}\n", i)
	}
	w := newWorld(t, "[code_index]\nmax_files = 3\n")
	head := w.addWorkspace(t, "demo", files)
	w.indexed(t, "demo")
	a := w.ask(t, "demo", "symbols", "name=a", w.key)
	r := a.refusal()
	if a.code != http.StatusConflict || r.Error.ErrorType != "code_index_partial" || r.Error.IndexedSHA == nil || *r.Error.IndexedSHA != head ||
		!strings.Contains(r.Error.Message, "complete index") || !strings.Contains(r.Error.Message, "code-search") {
		t.Errorf("%d %s, want 409 code_index_partial with indexed_sha %s and a message that points at the code search", a.code, a.body, head)
	}
	if got, _ := w.search(t, "demo", "Apple"); !got.Partial {
		t.Errorf("the code search of the same workspace = %+v, want it answered, partial", got)
	}
}

func TestSymbols_RefuseANameThatTooManyDeclarationsMatch(t *testing.T) {
	t.Parallel()
	var src strings.Builder
	src.WriteString("package gen\n")
	for i := range 1001 {
		fmt.Fprintf(&src, "\nfunc gen%04d() {}\n", i)
	}
	w := newWorld(t, enabled)
	w.addWorkspace(t, "demo", map[string]string{"gen.go": src.String()})
	w.indexed(t, "demo")
	a := w.ask(t, "demo", "symbols", "name=gen", w.key)
	if r := a.refusal(); a.code != http.StatusUnprocessableEntity || r.Error.ErrorType != "too_many_matches" ||
		!strings.Contains(r.Error.Message, "longer name") || !strings.Contains(r.Error.Message, "kind") || !strings.Contains(r.Error.Message, "path") {
		t.Errorf("%d %s, want 422 too_many_matches saying to give a longer name, a kind or a path", a.code, a.body)
	}
	if got := w.symbols(t, "demo", "name=gen1000"); !slices.Equal(got.names(), []string{"gen.go gen1000"}) {
		t.Errorf("name=gen1000 finds %q, want the one function", got.names())
	}
}

// The two query routes with a query each takes.
var queryRoutes = []struct{ route, query string }{{"code-search", "q=NewRouter"}, {"symbols", "name=NewRouter"}}

func TestQuery_IsRefusedWhileTheWorkspacesSettingIsOff(t *testing.T) {
	t.Parallel()
	w := newWorld(t, enabled)
	head := w.addWorkspace(t, "demo", symbolFiles())
	w.indexed(t, "demo")
	if code, body := w.Call(t, http.MethodPost, "/api/v1/workspaces/demo/forks", w.key, map[string]any{"name": "x", "auto_change": false}); code != http.StatusCreated {
		t.Fatalf("create the fork demo--x = %d %s, want 201", code, body)
	}
	set := func(on bool) {
		t.Helper()
		if code, body := w.Call(t, http.MethodPatch, "/api/v1/workspaces/demo", w.key, map[string]any{"settings": map[string]any{"code_index": on}}); code != http.StatusOK {
			t.Fatalf("PATCH code_index %t = %d %s, want 200", on, code, body)
		}
	}
	set(false)
	for _, rt := range queryRoutes {
		for _, slug := range []string{"demo", "demo--x"} {
			a := w.ask(t, slug, rt.route, rt.query, w.key)
			if r := a.refusal(); a.code != http.StatusConflict || r.Error.ErrorType != "code_index_off" || !strings.Contains(r.Error.Message, "code_index") {
				t.Errorf("%s of %s with the setting off: %d %s, want 409 code_index_off naming the setting", rt.route, slug, a.code, a.body)
			}
		}
	}
	set(true)
	w.reaches(t, "demo", head)
	for _, rt := range queryRoutes {
		if a := w.ask(t, "demo", rt.route, rt.query, w.key); a.code != http.StatusOK {
			t.Errorf("%s with the setting on again: %d %s, want 200", rt.route, a.code, a.body)
		}
	}
}

func TestQuery_AppliesTheAccessRuleOfTheSourceControlReads(t *testing.T) {
	t.Parallel()
	w := newWorld(t, enabled)
	w.addWorkspace(t, "demo", symbolFiles())
	w.indexed(t, "demo")
	outsider := w.IssueKey(t, w.CreateUser(t, "outsider"))
	pat := func(perm string) string {
		return w.IssuePAT(t, identity.PATSpec{UserID: w.owner, Permissions: []string{perm}, CreatedAt: time.Now().UTC()})
	}
	for _, rt := range queryRoutes {
		for _, tc := range []struct {
			name, cred string
			code       int
			errorType  string
		}{
			{"no credential", "", http.StatusUnauthorized, "unauthenticated"},
			{"a PAT without git:read", pat("changes:read"), http.StatusForbidden, "insufficient_scope"},
			{"a caller who may not read demo", outsider, http.StatusNotFound, "not_found"},
			{"a PAT with git:read", pat("git:read"), http.StatusOK, ""},
		} {
			a := w.ask(t, "demo", rt.route, rt.query, tc.cred)
			if a.code != tc.code || a.refusal().Error.ErrorType != tc.errorType {
				t.Errorf("%s, %s: %d %s, want %d %s", rt.route, tc.name, a.code, a.body, tc.code, tc.errorType)
			}
		}
	}
	// A workspace that does not exist answers the outsider's answer.
	missing := w.ask(t, "nope", "code-search", "q=a", outsider)
	if hidden := w.ask(t, "demo", "code-search", "q=a", outsider); missing.code != hidden.code || missing.refusal().Error != hidden.refusal().Error {
		t.Errorf("a caller who may not read demo gets %s, want the answer of a workspace that does not exist, %s", hidden.body, missing.body)
	}
	text := w.metricsText(t)
	for _, op := range []string{"code_search", "symbols"} {
		if !strings.Contains(text, `parley_scm_requests_total{op="`+op+`",result="ok"} 1`) {
			t.Errorf("GET /metrics does not count the one answered request under the op %s:\n%s", op, grep(text, `op="`+op+`"`))
		}
	}
}

func TestQuery_AnswersAWorkspaceWithoutAHeadAsTheTreeRouteDoes(t *testing.T) {
	t.Parallel()
	w := newWorld(t, enabled)
	ctx, now, dir := context.Background(), time.Now().UTC(), w.workspaces.RepoPath("empty")
	gitrepotest.InitBare(t, dir, "main")
	rec := workspace.Workspace{
		Slug: "empty", Kind: access.KindRepository, Mode: workspace.ModeStandard, GitURL: new("https://origin.test/empty.git"),
		DefaultBranch: new("main"), OwnerID: w.owner, OrgID: w.org, Status: workspace.StatusReady, DisplayName: "empty",
		CreatedAt: now, UpdatedAt: now, InstanceID: workspace.NewInstanceID(),
	}
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error { return workspace.Insert(ctx, tx, rec) }); err != nil {
		t.Fatal(err)
	}
	tree := w.ask(t, "empty", "tree", "", w.key)
	if tree.code < 400 {
		t.Fatalf("GET scm/tree of a repository without a commit = %d %s, want its refusal", tree.code, tree.body)
	}
	for _, rt := range queryRoutes {
		a := w.ask(t, "empty", rt.route, rt.query, w.key)
		if a.code != tree.code || a.refusal().Error.ErrorType != tree.refusal().Error.ErrorType {
			t.Errorf("%s of a repository without a commit: %d %s, want what the tree route answers, %d %s", rt.route, a.code, a.body, tree.code, tree.body)
		}
	}
}
