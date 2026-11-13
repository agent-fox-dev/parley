package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// PageBounds is the default and the bounds of a list's limit. A route whose
// list has other bounds than the standard ones passes its own to ReadPage.
type PageBounds struct {
	// Default is the limit of a request that states none.
	Default int
	// Min and Max are the smallest and the largest limit accepted.
	Min, Max int
}

// standardPageBounds are the bounds of every list that states none: a
// default of 50 and a limit from 1 to 200.
var standardPageBounds = PageBounds{Default: 50, Min: 1, Max: 200}

// Page is what a list request asks for: how many items, and where to
// continue. A handler reads it with ReadPage, decodes the position with
// After, fetches up to Limit+1 items after it in the list's order, and
// answers with WritePage.
type Page struct {
	// Limit is the most items the page holds.
	Limit int

	// position is the encoded position after the last item of the previous
	// page; nil on the first page.
	position json.RawMessage
	// fingerprint identifies the query the page belongs to: its route, path
	// parameter values and query parameters other than cursor and limit.
	fingerprint string
}

// cursor is the document a next_cursor encodes. It is not signed: a cursor
// grants nothing, since every page passes the full order of checks again.
type cursor struct {
	Position    json.RawMessage `json:"p"`
	Fingerprint string          `json:"f"`
}

// ReadPage reads the limit and the cursor of a list request. The limit
// defaults to 50 and must be an integer from 1 to 200, or the default and
// bounds the route passes as bounds. The cursor must be one a page of the
// same query issued: the same route, the same path parameter values and the
// same query parameters apart from cursor and limit, so a client may change
// the limit between pages. Anything else is answered 400 invalid_request, and
// the handler returns that error before it reads its collection.
//
// It panics when bounds holds more than one value or bounds that admit no
// limit, since both are programming errors.
func ReadPage(r *http.Request, bounds ...PageBounds) (Page, error) {
	b := standardPageBounds
	switch len(bounds) {
	case 0:
	case 1:
		b = bounds[0]
		if b.Min < 1 || b.Max < b.Min || b.Default < b.Min || b.Default > b.Max {
			panic(fmt.Sprintf("server.ReadPage: bounds %+v admit no default limit of at least 1", b))
		}
	default:
		panic("server.ReadPage: more than one PageBounds")
	}

	// A query that does not parse would otherwise lose its cursor or limit
	// silently and restart the list from its first page.
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return Page{}, InvalidRequest("the query string is malformed")
	}

	page := Page{Limit: b.Default, fingerprint: fingerprint(r, query)}
	if query.Has("limit") {
		n, err := strconv.Atoi(query.Get("limit"))
		if err != nil || n < b.Min || n > b.Max {
			return Page{}, InvalidRequest(fmt.Sprintf("limit must be an integer from %d to %d", b.Min, b.Max))
		}
		page.Limit = n
	}
	if query.Has("cursor") {
		pos, ok := decodeCursor(query.Get("cursor"), page.fingerprint)
		if !ok {
			return Page{}, InvalidRequest("cursor is malformed or belongs to another query")
		}
		page.position = pos
	}
	return page, nil
}

// After decodes into pos the position of the last item the previous page
// held, which the handler wrote through WritePage's position function. It
// reports false, leaving pos unchanged, on the first page. A position that
// does not decode into pos is answered 400 invalid_request.
func (p Page) After(pos any) (bool, error) {
	if p.position == nil {
		return false, nil
	}
	if err := json.Unmarshal(p.position, pos); err != nil {
		return false, InvalidRequest("cursor is malformed or belongs to another query")
	}
	return true, nil
}

// WritePage answers 200 with {"items":[…],"next_cursor":…}. items are what
// the handler fetched after the page's position, up to p.Limit+1 of them, in
// the list's order: the first p.Limit are written, and when there are more,
// next_cursor holds the position of the last item written, else it is null.
// items is never written as null. position returns an item's place in the
// list's total order (its keyset), so the next page continues after it even
// when items are added or removed earlier in the order; it must encode as
// JSON, and After decodes it on the next page.
func WritePage[T any](w http.ResponseWriter, p Page, items []T, position func(T) any) error {
	var next *string
	if len(items) > p.Limit {
		items = items[:p.Limit]
		c, err := encodeCursor(position(items[len(items)-1]), p.fingerprint)
		if err != nil {
			return err
		}
		next = &c
	}
	if items == nil {
		items = []T{}
	}
	WriteJSON(w, http.StatusOK, struct {
		Items      []T     `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}{items, next})
	return nil
}

// fingerprint is a short hash of the route r matched (its method and its
// pattern as registered, or the request's path when it matched none), its
// path parameter values and every query parameter except cursor and limit.
func fingerprint(r *http.Request, query url.Values) string {
	route := r.Method + " " + r.URL.Path
	var params map[string]string
	if m, ok := MatchedRoute(r); ok {
		route = m.Method + " " + m.Pattern
		params = m.Params
	}
	q := make(url.Values, len(query))
	for k, vs := range query {
		if k != "cursor" && k != "limit" {
			q[k] = vs
		}
	}
	// Maps encode with sorted keys and Encode sorts by key, so the same
	// query always gives the same bytes.
	doc, _ := json.Marshal([]any{route, params, q.Encode()})
	sum := sha256.Sum256(doc)
	return base64.RawURLEncoding.EncodeToString(sum[:16])
}

func encodeCursor(position any, fingerprint string) (string, error) {
	pos, err := json.Marshal(position)
	if err != nil {
		return "", fmt.Errorf("encode list position: %w", err)
	}
	doc, err := json.Marshal(cursor{Position: pos, Fingerprint: fingerprint})
	if err != nil {
		return "", fmt.Errorf("encode list cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(doc), nil
}

// decodeCursor returns the position s holds when s is a cursor issued for
// the query with fingerprint want.
func decodeCursor(s, want string) (json.RawMessage, bool) {
	doc, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	var c cursor
	if err := json.Unmarshal(doc, &c); err != nil {
		return nil, false
	}
	if c.Fingerprint != want || len(c.Position) == 0 || bytes.Equal(c.Position, []byte("null")) {
		return nil, false
	}
	return c.Position, true
}
