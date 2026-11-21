package codeindex_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/codeindex"
)

// searchAnswer is the answer of the code search route.
type searchAnswer struct {
	Workspace  string  `json:"workspace"`
	IndexedSHA *string `json:"indexed_sha"`
	Partial    bool    `json:"partial"`
	Truncated  bool    `json:"truncated"`
	Files      []struct {
		Path     string `json:"path"`
		Language string `json:"language"`
		Matches  []struct {
			Line int    `json:"line"`
			Text string `json:"text"`
		} `json:"matches"`
	} `json:"files"`
}

// paths are the paths of the answer's files, in its order.
func (a searchAnswer) paths() []string {
	paths := []string{}
	for _, f := range a.Files {
		paths = append(paths, f.Path)
	}
	return paths
}

// refusal is an error envelope of the hub with the members its error
// object may carry.
type refusal struct {
	Error struct {
		Code       int     `json:"code"`
		Message    string  `json:"message"`
		ErrorType  string  `json:"error_type"`
		State      string  `json:"state"`
		HeadSHA    *string `json:"head_sha"`
		IndexedSHA *string `json:"indexed_sha"`
	} `json:"error"`
}

// answered is one answer of the hub.
type answered struct {
	code   int
	header http.Header
	body   []byte
}

// refusal is the answer's error envelope.
func (a answered) refusal() refusal {
	var r refusal
	_ = json.Unmarshal(a.body, &r)
	return r
}

// ask sends GET …/scm/<route>?query of the workspace slug with cred and the
// headers header names and values in turn.
func (w *world) ask(t *testing.T, slug, route, query, cred string, header ...string) answered {
	t.Helper()
	h := http.Header{}
	for i := 0; i+1 < len(header); i += 2 {
		h.Set(header[i], header[i+1])
	}
	code, got, body := w.Exchange(t, http.MethodGet, "/api/v1/workspaces/"+slug+"/scm/"+route+"?"+query, cred, nil, h)
	return answered{code, got, body}
}

// search is the answer of a code search of the workspace slug for q by the
// owner, with more query parameters in more; it must be 200.
func (w *world) search(t *testing.T, slug, q string, more ...string) (searchAnswer, answered) {
	t.Helper()
	query := "q=" + url.QueryEscape(q)
	for _, m := range more {
		query += "&" + m
	}
	a := w.ask(t, slug, "code-search", query, w.key)
	var s searchAnswer
	if err := json.Unmarshal(a.body, &s); a.code != http.StatusOK || err != nil || s.Files == nil {
		t.Fatalf("GET scm/code-search?%s of %s = %d %s, want 200 with files", query, slug, a.code, a.body)
	}
	return s, a
}

// The files of the workspace the search tests read.
const (
	searchRouter  = "package api\n\n// NewRouter builds a router.\nfunc NewRouter() *Router { return &Router{} }\n\ntype Router struct{}\n"
	searchHandler = "package api\n\nfunc handle() {\n\tr := NewRouter()\n\t_ = r\n\t_ = NewRouter\n}\n"
)

// linesWith are the lines of content that hold word, as the matches of a
// search for it: number and text.
func linesWith(content, word string) []string {
	var lines []string
	for i, line := range strings.Split(content, "\n") {
		if strings.Contains(line, word) {
			lines = append(lines, fmt.Sprintf("%d %s", i+1, line))
		}
	}
	return lines
}

func TestSearch_AnswersTheFilesThatMatchBestFirstWithTheirLines(t *testing.T) {
	t.Parallel()
	w := newWorld(t, enabled)
	head := w.addWorkspace(t, "demo", map[string]string{"router.go": searchRouter, "handler.go": searchHandler, "notes.md": "# Notes\n\nnothing here\n"})
	w.indexed(t, "demo")

	got, _ := w.search(t, "demo", "NewRouter")
	if got.Workspace != "demo" || got.IndexedSHA == nil || *got.IndexedSHA != head || got.Partial || got.Truncated {
		t.Errorf("the answer = %+v, want workspace demo, indexed_sha %s, not partial and not truncated", got, head)
	}
	if paths := got.paths(); !slices.Equal(slices.Sorted(slices.Values(paths)), []string{"handler.go", "router.go"}) {
		t.Errorf("the files = %q, want router.go and handler.go and not notes.md", paths)
	}
	for _, f := range got.Files {
		content := map[string]string{"router.go": searchRouter, "handler.go": searchHandler}[f.Path]
		var lines []string
		for _, m := range f.Matches {
			lines = append(lines, fmt.Sprintf("%d %s", m.Line, m.Text))
		}
		if want := linesWith(content, "NewRouter"); f.Language != "Go" || !slices.Equal(lines, want) {
			t.Errorf("%s: language %q and matches %q, want Go and the file's lines that hold NewRouter, %q", f.Path, f.Language, lines, want)
		}
	}

	if got, _ := w.search(t, "demo", "sym:NewRouter"); len(got.Files) == 0 || got.Files[0].Path != "router.go" {
		t.Errorf("sym:NewRouter answers %q, want router.go, which declares it, first", got.paths())
	}
	if got, _ := w.search(t, "demo", "lang:go NewRouter file:handler"); !slices.Equal(got.paths(), []string{"handler.go"}) {
		t.Errorf("lang:go NewRouter file:handler answers %q, want handler.go alone", got.paths())
	}
	if got, a := w.search(t, "demo", "zzzznothing"); len(got.Files) != 0 || !strings.Contains(string(a.body), `"files":[]`) || got.Truncated {
		t.Errorf("a query that matches nothing answers %s, want files [] and not truncated", a.body)
	}
}

func TestSearch_BoundsTheFilesTheMatchesAndTheirText(t *testing.T) {
	t.Parallel()
	files := map[string]string{}
	for i := range 28 {
		files[fmt.Sprintf("f%02d.go", i)] = fmt.Sprintf("package f\n\n// needle number %d\n", i)
	}
	files["many.go"] = "package f\n\n" + strings.Repeat("// needle again\n", 40)
	files["long.go"] = "package f\n\n// needle " + strings.Repeat("x", 1990) + "\n"
	files["f00.go"] += "// pairword\n"
	files["f01.go"] += "// pairword\n"
	w := newWorld(t, enabled)
	w.addWorkspace(t, "demo", files)
	w.indexed(t, "demo")

	for _, tc := range []struct {
		more      string
		files     int
		truncated bool
	}{{"", 20, true}, {"limit=25", 25, true}, {"limit=3", 3, true}, {"limit=1", 1, true}} {
		var more []string
		if tc.more != "" {
			more = []string{tc.more}
		}
		if got, _ := w.search(t, "demo", "needle", more...); len(got.Files) != tc.files || got.Truncated != tc.truncated {
			t.Errorf("needle with %q answers %d files, truncated %t; want %d and %t", tc.more, len(got.Files), got.Truncated, tc.files, tc.truncated)
		}
	}
	if got, _ := w.search(t, "demo", "pairword"); len(got.Files) != 2 || got.Truncated {
		t.Errorf("a query that matches two files answers %d files, truncated %t; want 2 and false", len(got.Files), got.Truncated)
	}
	if got, _ := w.search(t, "demo", "needle file:many"); len(got.Files) != 1 || len(got.Files[0].Matches) == 0 || len(got.Files[0].Matches) > 20 {
		t.Errorf("the file with 40 matching lines answers %+v, want it with at most 20 matches", got.Files)
	}
	got, _ := w.search(t, "demo", "needle file:long")
	if len(got.Files) != 1 || len(got.Files[0].Matches) != 1 || len([]rune(got.Files[0].Matches[0].Text)) != 500 {
		t.Errorf("the file with a line of 2,000 characters answers %d files; want its one match with a text of 500 characters", len(got.Files))
	}
}

func TestSearch_RefusesAQueryOrALimitItDoesNotTake(t *testing.T) {
	t.Parallel()
	s := newSeam()
	w := newWorld(t, enabled, s.adjust)
	w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n"})
	w.indexed(t, "demo")
	for _, tc := range []struct{ name, query, names string }{
		{"q absent", "", "q"},
		{"q empty", "q=", "q"},
		{"q of blanks alone", "q=+++", "q"},
		{"q of 501 characters", "q=" + strings.Repeat("a", 501), "q"},
		{"q of 400 three-byte characters", "q=" + url.QueryEscape(strings.Repeat("€", 400)), "q"},
		{"limit 0", "q=a&limit=0", "limit"},
		{"limit 26", "q=a&limit=26", "limit"},
		{"limit not a number", "q=a&limit=x", "limit"},
	} {
		a := w.ask(t, "demo", "code-search", tc.query, w.key)
		if r := a.refusal(); a.code != http.StatusBadRequest || r.Error.ErrorType != "invalid_request" || !strings.Contains(r.Error.Message, tc.names) {
			t.Errorf("%s: %d %s, want 400 invalid_request with a message naming %s", tc.name, a.code, a.body, tc.names)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.searches != 0 {
		t.Errorf("%d searches reached the index, want none for a refused request", s.searches)
	}
}

func TestSearch_RefusesAQueryTheIndexCannotParseWithItsParsersMessage(t *testing.T) {
	t.Parallel()
	w := newWorld(t, enabled)
	w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n"})
	w.indexed(t, "demo")
	for _, q := range []string{"(unclosed", "file:[", "a)"} {
		a := w.ask(t, "demo", "code-search", "q="+url.QueryEscape(q), w.key)
		r := a.refusal()
		if a.code != http.StatusBadRequest || r.Error.ErrorType != "invalid_query" || r.Error.Message == "" || a.header.Get("ETag") != "" {
			t.Errorf("q=%s: %d %s, want 400 invalid_query with the parser's message and no ETag", q, a.code, a.body)
		}
	}
}

func TestSearch_AnswersATimeoutWhenTheIndexDoesNotFinish(t *testing.T) {
	t.Parallel()
	s := newSeam()
	w := newWorld(t, enabled, s.adjust)
	w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n"})
	w.indexed(t, "demo")
	s.mu.Lock()
	s.searchErr = codeindex.ErrSearchTimeout
	s.mu.Unlock()

	a := w.ask(t, "demo", "code-search", "q=package", w.key)
	if r := a.refusal(); a.code != http.StatusServiceUnavailable || r.Error.ErrorType != "scm_timeout" || !strings.Contains(r.Error.Message, "narrow the query") ||
		a.header.Get("ETag") != "" {
		t.Errorf("%d %s (ETag %q), want 503 scm_timeout saying to narrow the query, without an ETag", a.code, a.body, a.header.Get("ETag"))
	}
}

func TestSearch_AnswersFromAPartialIndexAndSaysSo(t *testing.T) {
	t.Parallel()
	files := map[string]string{}
	for i := range 10 {
		files[fmt.Sprintf("f%d.go", i)] = "package f\n\n// needle\n"
	}
	w := newWorld(t, "[code_index]\nmax_files = 3\n")
	w.addWorkspace(t, "demo", files)
	w.indexed(t, "demo")
	if got, _ := w.search(t, "demo", "needle"); !got.Partial || len(got.Files) == 0 || len(got.Files) > 3 {
		t.Errorf("the search of a partial index answers partial %t with %d files, want true and the files it holds, at most 3", got.Partial, len(got.Files))
	}
}

func TestSearch_IsRefusedOnAHubThatKeepsNoCodeIndex(t *testing.T) {
	t.Parallel()
	w := newWorld(t, "")
	w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n\nfunc A() {}\n"})
	for route, query := range map[string]string{"code-search": "q=a", "symbols": "name=a"} {
		a := w.ask(t, "demo", route, query, w.key)
		if r := a.refusal(); a.code != http.StatusNotFound || r.Error.ErrorType != "code_index_disabled" || !strings.Contains(r.Error.Message, "no code index") {
			t.Errorf("%s: %d %s, want 404 code_index_disabled saying the hub keeps no code index", route, a.code, a.body)
		}
	}
	// The other reads are as they were.
	if a := w.ask(t, "demo", "outline", "path=a.go", w.key); a.code != http.StatusOK || !strings.Contains(string(a.body), `"name":"A"`) {
		t.Errorf("scm/outline = %d %s, want 200 with the declaration A", a.code, a.body)
	}
	if a := w.ask(t, "demo", "search", "q=package", w.key); a.code != http.StatusOK {
		t.Errorf("scm/search = %d %s, want 200", a.code, a.body)
	}
}
