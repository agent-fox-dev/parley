package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Default bounds of a list's limit parameter.
const (
	DefaultPageLimit = 50
	MinPageLimit     = 1
	MaxPageLimit     = 200
)

// PageBounds are a list route's own default and bounds for limit; a zero
// field means the default of 50, 1 or 200.
type PageBounds struct {
	Default, Min, Max int
}

func (b PageBounds) withDefaults() PageBounds {
	if b.Default == 0 {
		b.Default = DefaultPageLimit
	}
	if b.Min == 0 {
		b.Min = MinPageLimit
	}
	if b.Max == 0 {
		b.Max = MaxPageLimit
	}
	return b
}

// Page is a list request's page: how many items to answer and the position
// to continue after. The handler fetches Limit+1 items after After in its
// own total order (After is "" on the first page) and passes them to
// WritePage, which answers at most Limit of them.
type Page struct {
	// Limit is the number of items to answer.
	Limit int
	// After is the position, of the handler's own choosing, of the last
	// item of the previous page; "" on the first page.
	After string
	// fingerprint identifies the query the cursor belongs to.
	fingerprint string
}

// cursorDoc is what a cursor holds: the position after the last item of its
// page and the fingerprint of its query.
type cursorDoc struct {
	Position *string `json:"p"`
	Query    *string `json:"q"`
}

// ParsePage reads limit and cursor from a list request. A limit that is not
// an integer or is outside the route's bounds (1 to 200 unless the route
// states others) is 400 invalid_request, and so is a cursor that does not
// decode, lacks a position, or was issued for another query: another route,
// another path parameter value, or other query parameters apart from cursor
// and limit.
func ParsePage(r *http.Request) (Page, error) {
	var bounds PageBounds
	if m := MatchFrom(r.Context()); m != nil {
		bounds = m.Route.Paging
	}
	bounds = bounds.withDefaults()
	q := r.URL.Query()

	page := Page{Limit: bounds.Default, fingerprint: queryFingerprint(r)}
	if values, ok := q["limit"]; ok {
		n, err := strconv.Atoi(values[0])
		if err != nil || n < bounds.Min || n > bounds.Max {
			return Page{}, InvalidRequest("limit must be an integer from " + strconv.Itoa(bounds.Min) + " to " + strconv.Itoa(bounds.Max))
		}
		page.Limit = n
	}
	if values, ok := q["cursor"]; ok {
		raw, err := base64.RawURLEncoding.DecodeString(values[0])
		if err != nil {
			return Page{}, InvalidRequest("cursor is malformed")
		}
		var c cursorDoc
		if err := json.Unmarshal(raw, &c); err != nil || c.Position == nil || c.Query == nil {
			return Page{}, InvalidRequest("cursor is malformed")
		}
		if *c.Query != page.fingerprint {
			return Page{}, InvalidRequest("cursor was issued for another query")
		}
		page.After = *c.Position
	}
	return page, nil
}

// queryFingerprint identifies a list query: the matched route's pattern,
// its path parameter values and every query parameter except cursor and
// limit.
func queryFingerprint(r *http.Request) string {
	var b strings.Builder
	if m := MatchFrom(r.Context()); m != nil {
		b.WriteString(m.Route.Method + " " + m.Route.Pattern)
		names := make([]string, 0, len(m.Params))
		for name := range m.Params {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			b.WriteString("\x00p:" + name + "=" + m.Params[name])
		}
	} else {
		b.WriteString(r.URL.Path)
	}
	q := r.URL.Query()
	q.Del("cursor")
	q.Del("limit")
	b.WriteString("\x00q:" + q.Encode())
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:12])
}

// listDoc is the body of every list answer.
type listDoc[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

// WritePage answers a page of a list: {"items":[…],"next_cursor":…}. items
// are what the handler fetched, Limit+1 at most: when there are more than
// page.Limit, only Limit are written and next_cursor holds the position of
// the last one written; otherwise next_cursor is null. items is never null.
func WritePage[T any](w http.ResponseWriter, page Page, items []T, position func(T) string) {
	doc := listDoc[T]{Items: items}
	if page.Limit > 0 && len(items) > page.Limit {
		doc.Items = items[:page.Limit]
		pos := position(doc.Items[len(doc.Items)-1])
		fp := page.fingerprint
		raw, _ := json.Marshal(cursorDoc{Position: &pos, Query: &fp})
		cursor := base64.RawURLEncoding.EncodeToString(raw)
		doc.NextCursor = &cursor
	}
	if doc.Items == nil {
		doc.Items = []T{}
	}
	WriteJSON(w, http.StatusOK, doc)
}
