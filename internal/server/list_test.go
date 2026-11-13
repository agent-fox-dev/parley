package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// thing is an item of the test collection; its id is its place in the
// collection's order.
type thing struct {
	ID int `json:"id"`
}

// collection is an in-memory collection ordered by id that counts how often
// it is read. It is safe for concurrent requests.
type collection struct {
	mu    sync.Mutex
	ids   []int
	reads int
}

// newCollection holds n items with the ids 2, 4, …, 2n, so that a test can
// insert an item between any two of them.
func newCollection(n int) *collection {
	c := &collection{}
	for i := range n {
		c.ids = append(c.ids, 2*(i+1))
	}
	return c
}

// insert adds an item with id in its place in the order.
func (c *collection) insert(id int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	i, _ := slices.BinarySearch(c.ids, id)
	c.ids = slices.Insert(c.ids, i, id)
}

// all returns the ids of every item, in order.
func (c *collection) all() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.ids)
}

// read returns up to n items, those after the id after when continuing, or
// from the start, and counts the read.
func (c *collection) read(after int, continuing bool, n int) []thing {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
	var out []thing
	for _, id := range c.ids {
		if continuing && id <= after {
			continue
		}
		if len(out) == n {
			break
		}
		out = append(out, thing{ID: id})
	}
	return out
}

func (c *collection) readCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}

// handler pages the collection by id through the list helpers, with bounds
// when the route states its own.
func (c *collection) handler(bounds ...PageBounds) HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		page, err := ReadPage(r, bounds...)
		if err != nil {
			return err
		}
		var after int
		continuing, err := page.After(&after)
		if err != nil {
			return err
		}
		items := c.read(after, continuing, page.Limit+1)
		return WritePage(w, page, items, func(t thing) any { return t.ID })
	}
}

// listServer serves the collection c on several list routes, behind a
// passing authenticator and without a rate limit, since a walk sends many
// requests: GET /things, /owners/:owner/things and /others with the standard
// bounds, GET /wide with bounds 1–1000 and default 100, and GET /empty, whose
// handler gives the helper a nil slice.
func listServer(c *collection) http.Handler {
	s := New(Options{RateLimitDisabled: true})
	s.SetAuthenticator(func(*http.Request) (Caller, error) { return testCaller, nil })
	s.Register(Route{Method: http.MethodGet, Pattern: "/things", Handler: c.handler()})
	s.Register(Route{Method: http.MethodGet, Pattern: "/owners/:owner/things", Handler: c.handler()})
	s.Register(Route{Method: http.MethodGet, Pattern: "/others", Handler: c.handler()})
	s.Register(Route{Method: http.MethodGet, Pattern: "/wide", Handler: c.handler(PageBounds{Default: 100, Min: 1, Max: 1000})})
	s.Register(Route{Method: http.MethodGet, Pattern: "/empty", Handler: func(w http.ResponseWriter, r *http.Request) error {
		page, err := ReadPage(r)
		if err != nil {
			return err
		}
		return WritePage[thing](w, page, nil, func(t thing) any { return t.ID })
	}})
	return s.Handler()
}

// listPage is one page a list route answered.
type listPage struct {
	ids  []int
	next *string
}

// getPage requests target and returns the page it answered, failing the test
// unless the answer is 200 JSON with exactly the members items, an array,
// and next_cursor.
func getPage(t *testing.T, h http.Handler, target string) listPage {
	t.Helper()
	rec := do(h, newRequest(http.MethodGet, target, nil, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200 (body %s)", target, rec.Code, rec.Body.Bytes())
	}
	if ct := rec.Header().Get("Content-Type"); ct != jsonContentType {
		t.Errorf("GET %s: Content-Type = %q, want %q", target, ct, jsonContentType)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("GET %s: body %s is not a JSON object: %v", target, rec.Body.Bytes(), err)
	}
	if len(doc) != 2 || doc["items"] == nil || doc["next_cursor"] == nil {
		t.Fatalf("GET %s: body %s, want exactly the members items and next_cursor", target, rec.Body.Bytes())
	}
	var items []thing
	if !strings.HasPrefix(string(doc["items"]), "[") {
		t.Fatalf("GET %s: items = %s, want an array", target, doc["items"])
	}
	if err := json.Unmarshal(doc["items"], &items); err != nil {
		t.Fatalf("GET %s: items %s: %v", target, doc["items"], err)
	}
	var p listPage
	for _, it := range items {
		p.ids = append(p.ids, it.ID)
	}
	if err := json.Unmarshal(doc["next_cursor"], &p.next); err != nil {
		t.Fatalf("GET %s: next_cursor = %s, want a string or null", target, doc["next_cursor"])
	}
	if p.next != nil && *p.next == "" {
		t.Errorf("GET %s: next_cursor is an empty string, want a cursor or null", target)
	}
	return p
}

// withCursor adds cursor to target's query.
func withCursor(target, cursor string) string {
	sep := "?"
	if strings.Contains(target, "?") {
		sep = "&"
	}
	return target + sep + "cursor=" + url.QueryEscape(cursor)
}

// walk follows next_cursor from target until it is null and returns every
// page. between, when not nil, runs after each page that has a next one.
func walk(t *testing.T, h http.Handler, target string, between func()) []listPage {
	t.Helper()
	pages := []listPage{getPage(t, h, target)}
	for pages[len(pages)-1].next != nil {
		if len(pages) > 10000 {
			t.Fatalf("walk of %s does not end", target)
		}
		if between != nil {
			between()
		}
		pages = append(pages, getPage(t, h, withCursor(target, *pages[len(pages)-1].next)))
	}
	return pages
}

func concat(pages []listPage) []int {
	var ids []int
	for _, p := range pages {
		ids = append(ids, p.ids...)
	}
	return ids
}

func TestList_WritesItemsAndANextCursorOnlyWhileMoreFollow(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"/api/v1/things", "/api/v1/empty"} {
		t.Run("empty "+target, func(t *testing.T) {
			t.Parallel()
			rec := do(listServer(newCollection(0)), newRequest(http.MethodGet, target, nil, ""))
			body := rec.Body.String()
			if rec.Code != http.StatusOK || !strings.Contains(body, `"items":[]`) || !strings.Contains(body, `"next_cursor":null`) {
				t.Errorf("GET %s = %d %s, want 200 with items [] and next_cursor null", target, rec.Code, body)
			}
			getPage(t, listServer(newCollection(0)), target)
		})
	}
	t.Run("three items by two", func(t *testing.T) {
		t.Parallel()
		h := listServer(newCollection(3))
		first := getPage(t, h, "/api/v1/things?limit=2")
		if !slices.Equal(first.ids, []int{2, 4}) || first.next == nil {
			t.Fatalf("first page = %v with next_cursor %v, want [2 4] and a cursor", first.ids, first.next)
		}
		last := getPage(t, h, withCursor("/api/v1/things?limit=2", *first.next))
		if !slices.Equal(last.ids, []int{6}) || last.next != nil {
			t.Errorf("last page = %v with next_cursor %v, want [6] and null", last.ids, last.next)
		}
	})
}

func TestList_UsesALimitOf50WhenTheRequestStatesNone(t *testing.T) {
	t.Parallel()
	p := getPage(t, listServer(newCollection(51)), "/api/v1/things")
	if len(p.ids) != 50 || p.next == nil {
		t.Errorf("page has %d items and next_cursor %v, want 50 and a cursor", len(p.ids), p.next)
	}
}

func TestList_RefusesALimitThatIsNotAnIntegerWithinTheBounds(t *testing.T) {
	t.Parallel()
	for _, limit := range []string{"0", "-1", "201", "1000", "abc", "1.5", ""} {
		t.Run("limit="+limit, func(t *testing.T) {
			t.Parallel()
			c := newCollection(10)
			rec := do(listServer(c), newRequest(http.MethodGet, "/api/v1/things?limit="+url.QueryEscape(limit), nil, ""))
			assertError(t, rec, http.StatusBadRequest, "invalid_request")
			if n := c.readCount(); n != 0 {
				t.Errorf("collection read %d times, want 0", n)
			}
		})
	}
	for _, limit := range []int{1, 200} {
		t.Run("limit="+strconv.Itoa(limit), func(t *testing.T) {
			t.Parallel()
			p := getPage(t, listServer(newCollection(250)), "/api/v1/things?limit="+strconv.Itoa(limit))
			if len(p.ids) != limit {
				t.Errorf("limit=%d answered %d items, want %d", limit, len(p.ids), limit)
			}
		})
	}
}

func TestList_AppliesTheDefaultAndBoundsARouteStates(t *testing.T) {
	t.Parallel()
	c := newCollection(1200)
	h := listServer(c)

	if p := getPage(t, h, "/api/v1/wide?limit=1000"); len(p.ids) != 1000 {
		t.Errorf("limit=1000 answered %d items, want 1000", len(p.ids))
	}
	before := c.readCount()
	assertError(t, do(h, newRequest(http.MethodGet, "/api/v1/wide?limit=1001", nil, "")), http.StatusBadRequest, "invalid_request")
	if c.readCount() != before {
		t.Errorf("limit=1001 read the collection")
	}
	if p := getPage(t, h, "/api/v1/wide"); len(p.ids) != 100 {
		t.Errorf("no limit answered %d items, want 100", len(p.ids))
	}
}

func TestList_RefusesAMalformedCursorOrOneIssuedForAnotherQuery(t *testing.T) {
	t.Parallel()
	c := newCollection(5)
	h := listServer(c)
	first := getPage(t, h, "/api/v1/things?state=open&limit=2")
	if first.next == nil {
		t.Fatal("first page of /things?state=open has no cursor")
	}
	cur := *first.next
	owners := getPage(t, h, "/api/v1/owners/a/things?limit=2")
	if owners.next == nil {
		t.Fatal("first page of /owners/a/things has no cursor")
	}

	cases := []struct {
		name   string
		target string
	}{
		{"not base64", withCursor("/api/v1/things?state=open", "%%%")},
		{"not JSON", withCursor("/api/v1/things?state=open", "AAAA")},
		{"no position", withCursor("/api/v1/things?state=open", "e30")},
		{"truncated", withCursor("/api/v1/things?state=open", cur[:len(cur)-1])},
		{"query that does not parse", "/api/v1/things?state=open&cursor=%%%"},
		{"another route", withCursor("/api/v1/others", cur)},
		{"another query value", withCursor("/api/v1/things?state=closed", cur)},
		{"a query parameter missing", withCursor("/api/v1/things", cur)},
		{"another path parameter", withCursor("/api/v1/owners/b/things?limit=2", *owners.next)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := c.readCount()
			assertError(t, do(h, newRequest(http.MethodGet, tc.target, nil, "")), http.StatusBadRequest, "invalid_request")
			if c.readCount() != before {
				t.Errorf("GET %s read the collection", tc.target)
			}
		})
	}

	t.Run("another limit", func(t *testing.T) {
		p := getPage(t, h, withCursor("/api/v1/things?state=open&limit=7", cur))
		if !slices.Equal(p.ids, []int{6, 8, 10}) || p.next != nil {
			t.Errorf("page = %v with next_cursor %v, want [6 8 10] and null", p.ids, p.next)
		}
	})
}

func TestList_WalksEveryItemOnceInOrder(t *testing.T) {
	t.Parallel()
	for n := range 121 {
		for _, limit := range []int{1, 2, 7, 50, 60, 200} {
			c := newCollection(n)
			h := listServer(c)
			target := "/api/v1/things?limit=" + strconv.Itoa(limit)

			pages := walk(t, h, target, nil)
			if got, want := concat(pages), c.all(); !slices.Equal(got, want) {
				t.Fatalf("n=%d limit=%d: walk returned %v, want %v", n, limit, got, want)
			}
			if want := max(1, (n+limit-1)/limit); len(pages) != want {
				t.Errorf("n=%d limit=%d: %d pages, want %d", n, limit, len(pages), want)
			}
			for i, p := range pages[:len(pages)-1] {
				if p.next == nil {
					t.Errorf("n=%d limit=%d: page %d of %d has next_cursor null", n, limit, i+1, len(pages))
				}
			}

			if n <= limit {
				continue
			}
			// An item inserted before the position reached between two
			// pages must neither repeat nor hide an item that was there
			// throughout.
			existing := c.all()
			inserted := false
			pages = walk(t, h, target, func() {
				if !inserted {
					c.insert(1)
					inserted = true
				}
			})
			got := slices.DeleteFunc(concat(pages), func(id int) bool { return id == 1 })
			if !slices.Equal(got, existing) {
				t.Fatalf("n=%d limit=%d: walk with an insertion returned %v, want %v", n, limit, got, existing)
			}
		}
	}
}
