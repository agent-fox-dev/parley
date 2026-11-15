package scm_test

import (
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// hits are the matches of a search answer, each as its path and line
// joined by a colon.
func (r reply) hits() []string {
	items, _ := r.json["items"].([]any)
	hits := []string{}
	for _, it := range items {
		o, _ := it.(map[string]any)
		hits = append(hits, fmt.Sprintf("%v:%v", o["path"], o["line"]))
	}
	return hits
}

func TestSearch_FindsALiteralStringInEveryTextFileAtARef(t *testing.T) {
	w := newWorld(t)
	_, main := w.addSearch(t)
	want := []any{
		map[string]any{"path": "api/greeting.go", "line": 3.0, "text": "func Greeting() string {"},
		map[string]any{"path": "docs/a.md", "line": 1.0, "text": "# Greeting"},
	}
	for _, q := range []string{"ref=main&q=Greeting", "q=Greeting"} {
		r := w.get(t, scmPath("search", "/search?"+q), w.ownerKey)
		if r.status != http.StatusOK || !reflect.DeepEqual(r.json["items"], want) || r.json["resolved_sha"] != main {
			t.Errorf("%s: %d %s, want the items %v and resolved_sha %s", q, r.status, r.body, want, main)
		}
	}
	for _, c := range []struct {
		q    string
		want []string
	}{
		{"greeting", []string{"api/greeting.go:4"}},
		{"a.b(c)*", []string{"literal.txt:1"}},
	} {
		if r := w.get(t, scmPath("search", "/search?q="+url.QueryEscape(c.q)), w.ownerKey); !slices.Equal(r.hits(), c.want) {
			t.Errorf("q=%s: %d %s, want the matches %v", c.q, r.status, r.body, c.want)
		}
	}
	r := w.get(t, scmPath("search", "/search?q=zzz-not-there"), w.ownerKey)
	if want := map[string]any{"items": []any{}, "next_cursor": nil, "resolved_sha": main}; !reflect.DeepEqual(r.json, want) {
		t.Errorf("q=zzz-not-there: %d %s, want %v", r.status, r.body, want)
	}
	if r := w.get(t, scmPath("search", "/search?q=x&ref=main~1"), w.ownerKey); r.status != http.StatusNotFound || r.errorType() != "ref_not_found" {
		t.Errorf("ref=main~1: %d %s, want 404 ref_not_found", r.status, r.body)
	}
}

func TestSearch_TakesARegularExpressionIgnoresCaseAndKeepsThePathsGlobsMatch(t *testing.T) {
	w := newWorld(t)
	w.addSearch(t)
	for _, c := range []struct {
		query string
		want  []string
	}{
		{"q=" + url.QueryEscape(`func +[A-Z]\w*\(`) + "&regex=true", []string{"api/greeting.go:3", "api/greeting.go:7"}},
		{"q=greeting&case_sensitive=false", []string{"api/greeting.go:3", "api/greeting.go:4", "docs/a.md:1"}},
		{"q=Greeting&path=docs/**", []string{"docs/a.md:1"}},
		{"q=Greeting&path=*.go&path=api/*", []string{"api/greeting.go:3"}},
		{"q=a.b&regex=true", []string{"literal.txt:1", "literal.txt:2"}},
		{"q=a.b&regex=false", []string{"literal.txt:1"}},
		{"q=greeting&case_sensitive=true", []string{"api/greeting.go:4"}},
	} {
		if r := w.get(t, scmPath("search", "/search?"+c.query), w.ownerKey); r.status != http.StatusOK || !slices.Equal(r.hits(), c.want) {
			t.Errorf("%s: %d %s, want the matches %v", c.query, r.status, r.body, c.want)
		}
	}
}

func TestSearch_SkipsBinaryFilesAndCutsALongLine(t *testing.T) {
	w := newWorld(t)
	w.addSearch(t)
	if r := w.get(t, scmPath("search", "/search?q=Greeting"), w.ownerKey); r.item("blob.bin") != nil {
		t.Errorf("q=Greeting: %s, want no match in the binary blob.bin", r.body)
	}
	r := w.get(t, scmPath("search", "/search?q=needle"), w.ownerKey)
	line := r.item("long.txt")
	text, _ := line["text"].(string)
	if want := string([]rune(gitrepotest.SearchLong)[:500]); line["line"] != 1.0 || text != want {
		t.Errorf("q=needle: %s, want long.txt's line 1 cut to its first 500 characters", r.body)
	}
}

func TestSearch_PagesMatchesFromTheCommitTheFirstPageResolved(t *testing.T) {
	const query = "/search?q=hit&limit=200"
	pages := func(t *testing.T, w *world, between func()) []reply {
		t.Helper()
		var got []reply
		path := query
		for len(got) < 3 {
			r := w.get(t, scmPath("search", path), w.ownerKey)
			if r.status != http.StatusOK {
				t.Fatalf("GET %s = %d %s", path, r.status, r.body)
			}
			got = append(got, r)
			next, _ := r.json["next_cursor"].(string)
			if next == "" {
				break
			}
			if between != nil {
				between()
				between = nil
			}
			path = query + "&cursor=" + url.QueryEscape(next)
		}
		return got
	}

	w := newWorld(t)
	dir, main := w.addSearch(t)
	got := pages(t, w, nil)
	var all, want []string
	var sizes []int
	for _, p := range got {
		all = append(all, p.hits()...)
		sizes = append(sizes, len(p.hits()))
	}
	for i := 1; i <= 450; i++ {
		want = append(want, fmt.Sprintf("hits.txt:%d", i))
	}
	if !slices.Equal(sizes, []int{200, 200, 50}) || got[len(got)-1].json["next_cursor"] != nil || !slices.Equal(all, want) {
		t.Errorf("pages of %d %v matches, last next_cursor %v, want 200, 200 and 50 of hits.txt's lines 1 to 450 in order, then null",
			len(all), sizes, got[len(got)-1].json["next_cursor"])
	}
	if r := w.get(t, scmPath("search", "/search?q=hit"), w.ownerKey); len(r.hits()) != 50 {
		t.Errorf("without limit: %d matches, want 50", len(r.hits()))
	}

	moved := commit(t, dir, main, "hits.txt", strings.Repeat("hit\n", 3))
	got = pages(t, w, func() { gitrepotest.Git(t, dir, "update-ref", "refs/heads/main", moved, main) })
	if len(got) != 3 || got[1].json["resolved_sha"] != main || got[0].json["resolved_sha"] != main || len(got[1].hits()) != 200 {
		t.Errorf("main moved after the first page: resolved_sha %v then %v over %d pages, want %s on each and 200 matches on the second",
			got[0].json["resolved_sha"], got[len(got)-1].json["resolved_sha"], len(got), main)
	}
}

func TestSearch_RefusesAMalformedQuery(t *testing.T) {
	w := newWorld(t)
	w.addSearch(t)
	for _, query := range []string{
		"", "q=", "q=" + url.QueryEscape(strings.Repeat("é", 501)), "q=" + url.QueryEscape("(unclosed") + "&regex=true",
		"q=x&regex=1", "q=x&case_sensitive=no", "q=x&limit=201", "q=x&limit=0", "q=a%00b",
	} {
		if r := w.get(t, scmPath("search", "/search?"+query), w.ownerKey); r.status != http.StatusBadRequest || r.errorType() != "invalid_request" {
			t.Errorf("GET /search?%.40s = %d %s, want 400 invalid_request", query, r.status, r.body)
		}
	}
	for _, query := range []string{"q=" + url.QueryEscape(strings.Repeat("é", 500)), "q=" + url.QueryEscape("(unclosed")} {
		if r := w.get(t, scmPath("search", "/search?"+query), w.ownerKey); r.status != http.StatusOK {
			t.Errorf("GET /search?%.40s = %d %s, want 200", query, r.status, r.body)
		}
	}
}
