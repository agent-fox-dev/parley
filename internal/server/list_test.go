package server

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type item struct {
	ID string `json:"id"`
}

// collection is an in-memory collection ordered by id.
type collection struct {
	mu    sync.Mutex
	ids   []string
	reads atomic.Int32
}

func newCollection(n int) *collection {
	c := &collection{}
	for i := range n {
		c.ids = append(c.ids, fmt.Sprintf("item-%04d", i*10))
	}
	return c
}

func (c *collection) insert(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ids = append(c.ids, id)
	sort.Strings(c.ids)
}

// after returns up to n items whose id sorts after the position.
func (c *collection) after(pos string, n int) []item {
	c.reads.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []item
	for _, id := range c.ids {
		if id > pos && len(out) < n {
			out = append(out, item{ID: id})
		}
	}
	return out
}

func (c *collection) handler(w http.ResponseWriter, r *http.Request) error {
	page, err := ParsePage(r)
	if err != nil {
		return err
	}
	WritePage(w, page, c.after(page.After, page.Limit+1), func(it item) string { return it.ID })
	return nil
}

func listServer(routes map[string]*collection, bounds map[string]PageBounds) http.Handler {
	s := New(Options{})
	s.SetAuthenticator(bearerAuth(nil))
	for pattern, c := range routes {
		s.Register(Route{Method: "GET", Pattern: pattern, Handler: c.handler, Paging: bounds[pattern]})
	}
	return s.Handler()
}

type pageDoc struct {
	items []string
	next  *string
}

func getPage(t *testing.T, h http.Handler, target string) (apiResponse, pageDoc) {
	t.Helper()
	r := do(t, h, "GET", target, bearer("a"))
	if r.status != 200 {
		return r, pageDoc{}
	}
	doc := r.json(t)
	raw, ok := doc["items"].([]any)
	if !ok {
		t.Fatalf("items = %v, want an array", doc["items"])
	}
	var p pageDoc
	for _, it := range raw {
		p.items = append(p.items, it.(map[string]any)["id"].(string))
	}
	if s, ok := doc["next_cursor"].(string); ok {
		p.next = &s
	} else if doc["next_cursor"] != nil {
		t.Fatalf("next_cursor = %v, want a string or null", doc["next_cursor"])
	}
	if _, present := doc["next_cursor"]; !present {
		t.Fatal("next_cursor is missing")
	}
	return r, p
}

func withCursor(target, cursor string) string {
	sep := "?"
	if strings.Contains(target, "?") {
		sep = "&"
	}
	return target + sep + "cursor=" + url.QueryEscape(cursor)
}

func TestWritePage_ItemsIsAnArrayAndNextCursorNullOnTheLastPage(t *testing.T) {
	empty := listServer(map[string]*collection{"/things": newCollection(0)}, nil)
	r := do(t, empty, "GET", "/api/v1/things", bearer("a"))
	if r.status != 200 || r.header.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("status %d, Content-Type %q", r.status, r.header.Get("Content-Type"))
	}
	if !strings.Contains(string(r.body), `"items":[]`) || !strings.Contains(string(r.body), `"next_cursor":null`) {
		t.Errorf("empty page = %s, want items [] and next_cursor null", r.body)
	}

	rec := do(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WritePage[item](w, Page{Limit: 5}, nil, func(it item) string { return it.ID })
	}), "GET", "/")
	if !strings.Contains(string(rec.body), `"items":[]`) || !strings.Contains(string(rec.body), `"next_cursor":null`) {
		t.Errorf("nil slice page = %s, want items [] and next_cursor null", rec.body)
	}

	h := listServer(map[string]*collection{"/things": newCollection(3)}, nil)
	_, p := getPage(t, h, "/api/v1/things?limit=2")
	if len(p.items) != 2 || p.next == nil || *p.next == "" {
		t.Fatalf("first page = %+v, want 2 items and a cursor", p)
	}
	_, p = getPage(t, h, withCursor("/api/v1/things?limit=2", *p.next))
	if len(p.items) != 1 || p.next != nil {
		t.Fatalf("second page = %v next %v, want 1 item and null", p.items, p.next)
	}
}

func TestParsePage_DefaultLimitIsFifty(t *testing.T) {
	_, p := getPage(t, listServer(map[string]*collection{"/things": newCollection(51)}, nil), "/api/v1/things")
	if len(p.items) != 50 || p.next == nil {
		t.Errorf("got %d items, next %v; want 50 and a cursor", len(p.items), p.next)
	}
}

func TestParsePage_RefusesALimitOutOfBounds(t *testing.T) {
	c := newCollection(300)
	h := listServer(map[string]*collection{"/things": c}, nil)
	for _, limit := range []string{"0", "-1", "201", "1000", "abc", "1.5", ""} {
		before := c.reads.Load()
		r := do(t, h, "GET", "/api/v1/things?limit="+limit, bearer("a"))
		if r.status != 400 || r.errorType(t) != "invalid_request" {
			t.Errorf("limit=%q = %d, want 400 invalid_request", limit, r.status)
		}
		if c.reads.Load() != before {
			t.Errorf("limit=%q read the collection", limit)
		}
	}
	for _, limit := range []int{1, 200} {
		_, p := getPage(t, h, fmt.Sprintf("/api/v1/things?limit=%d", limit))
		if len(p.items) != limit {
			t.Errorf("limit=%d gave %d items", limit, len(p.items))
		}
	}
}

func TestParsePage_ARouteStatesItsOwnBounds(t *testing.T) {
	h := listServer(map[string]*collection{"/wide": newCollection(1200)}, map[string]PageBounds{"/wide": {Default: 100, Min: 1, Max: 1000}})
	if _, p := getPage(t, h, "/api/v1/wide?limit=1000"); len(p.items) != 1000 {
		t.Errorf("limit=1000 gave %d items", len(p.items))
	}
	if r, _ := getPage(t, h, "/api/v1/wide?limit=1001"); r.status != 400 {
		t.Errorf("limit=1001 = %d, want 400", r.status)
	}
	if _, p := getPage(t, h, "/api/v1/wide"); len(p.items) != 100 {
		t.Errorf("no limit gave %d items, want 100", len(p.items))
	}
}

func TestParsePage_RefusesACursorOfAnotherQuery(t *testing.T) {
	things, owners, others := newCollection(20), newCollection(20), newCollection(20)
	h := listServer(map[string]*collection{"/things": things, "/owners/:owner/things": owners, "/others": others}, nil)
	_, p := getPage(t, h, "/api/v1/things?state=open&limit=5")
	c := *p.next
	_, op := getPage(t, h, "/api/v1/owners/a/things?limit=5")
	oc := *op.next

	bad := []struct{ name, target string }{
		{"not base64", withCursor("/api/v1/things?state=open", "%%%")},
		{"not JSON", withCursor("/api/v1/things?state=open", "AAAA")},
		{"no position", withCursor("/api/v1/things?state=open", "e30")},
		{"truncated", withCursor("/api/v1/things?state=open", c[:len(c)-1])},
		{"another route", withCursor("/api/v1/others", c)},
		{"another filter", withCursor("/api/v1/things?state=closed", c)},
		{"no filter", withCursor("/api/v1/things", c)},
		{"another path parameter", withCursor("/api/v1/owners/b/things", oc)},
	}
	for _, tt := range bad {
		reads := things.reads.Load() + owners.reads.Load() + others.reads.Load()
		r := do(t, h, "GET", tt.target, bearer("a"))
		if r.status != 400 || r.errorType(t) != "invalid_request" {
			t.Errorf("%s: %d %s, want 400 invalid_request", tt.name, r.status, r.body)
		}
		if things.reads.Load()+owners.reads.Load()+others.reads.Load() != reads {
			t.Errorf("%s: the collection was read", tt.name)
		}
	}
	_, next := getPage(t, h, withCursor("/api/v1/things?state=open&limit=7", c))
	if len(next.items) != 7 || next.items[0] != "item-0050" {
		t.Errorf("same query with another limit = %v, want 7 items from item-0050", next.items)
	}
}

func TestPaging_FollowingNextCursorListsEveryItemOnceInOrder(t *testing.T) {
	for n := 0; n <= 120; n++ {
		for _, limit := range []int{1, 2, 7, 50, 60, 200} {
			c := newCollection(n)
			h := listServer(map[string]*collection{"/things": c}, nil)
			var got []string
			pages := 0
			target := fmt.Sprintf("/api/v1/things?limit=%d", limit)
			next := target
			for {
				r, p := getPage(t, h, next)
				if r.status != 200 {
					t.Fatalf("n=%d limit=%d: %d %s", n, limit, r.status, r.body)
				}
				pages++
				got = append(got, p.items...)
				if p.next == nil {
					break
				}
				next = withCursor(target, *p.next)
			}
			want := max(1, (n+limit-1)/limit)
			if pages != want {
				t.Fatalf("n=%d limit=%d: %d pages, want %d", n, limit, pages, want)
			}
			if strings.Join(got, ",") != strings.Join(c.ids, ",") {
				t.Fatalf("n=%d limit=%d: walked %v", n, limit, got)
			}
		}
	}

	c := newCollection(10)
	h := listServer(map[string]*collection{"/things": c}, nil)
	original := append([]string(nil), c.ids...)
	_, p := getPage(t, h, "/api/v1/things?limit=4")
	got := p.items
	c.insert("item-0005") // before the current position
	for p.next != nil {
		_, p = getPage(t, h, withCursor("/api/v1/things?limit=4", *p.next))
		got = append(got, p.items...)
	}
	if strings.Join(got, ",") != strings.Join(original, ",") {
		t.Errorf("after an insert before the position: %v, want %v", got, original)
	}
}
