package forge

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/agent-fox-dev/parley/internal/server"
)

// maxPageSize is the largest page a forge list reads, the bound of a list's
// limit.
const maxPageSize = 200

// Position is where a forge list continues: the number of the forge's page,
// from 1, the page size, and the anchor of a list that is read from one
// thing its first page found, such as a commit, "" for none.
type Position struct {
	Page   int    `json:"page"`
	Size   int    `json:"size"`
	Anchor string `json:"anchor,omitempty"`
}

// Paging is the page of a forge list a request asks for. The forges page by
// number and size, so the page size is kept in the cursor: a request with a
// cursor reads the cursor's page with the cursor's size, since a size that
// changed between two pages would skip or repeat entries.
type Paging struct {
	// Position is the forge page to read: the first page of limit
	// entries, or the cursor's.
	Position
	// Continued is true when the request carried a cursor.
	Continued bool

	page server.Page
}

// ReadPaging reads the limit and the cursor of a forge list request, as
// server.ReadPage does (a limit from 1 to 200, by default 50, and a cursor
// issued for the same route, path parameters and query parameters other
// than cursor and limit). A limit given with a cursor must equal the
// cursor's page size. A refusal is 400 invalid_request.
func ReadPaging(r *http.Request) (Paging, error) {
	page, err := server.ReadPage(r)
	if err != nil {
		return Paging{}, err
	}
	p := Paging{Position: Position{Page: 1, Size: page.Limit}, page: page}
	var pos Position
	continued, err := page.After(&pos)
	if err != nil || !continued {
		return p, err
	}
	if pos.Page < 1 || pos.Size < 1 || pos.Size > maxPageSize {
		return Paging{}, server.InvalidRequest("cursor is malformed or belongs to another query")
	}
	if r.URL.Query().Has("limit") && page.Limit != pos.Size {
		return Paging{}, server.InvalidRequest(fmt.Sprintf("limit must be left out or equal the cursor's page size, %d", pos.Size))
	}
	p.Position, p.Continued = pos, true
	return p, nil
}

// Query sets the page of p on the query of a forge request, in the names
// both forges use: per_page and page.
func (p Paging) Query(q url.Values) {
	q.Set("per_page", strconv.Itoa(p.Size))
	q.Set("page", strconv.Itoa(p.Page))
}

// Next is the position of the forge page after p's, with p's size and
// anchor, when the forge's answer with header h names a next page in its
// Link header, else nil.
func (p Paging) Next(h http.Header) *Position {
	if !namesNext(h) {
		return nil
	}
	next := p.Position
	next.Page++
	return &next
}

// WriteList answers the page p of a forge list: items, never null, and a
// next_cursor that continues at next, null when next is nil, followed by
// members.
func WriteList[T any](w http.ResponseWriter, p Paging, items []T, next *Position, members ...server.Member) error {
	var cursor *string
	if next != nil {
		c, err := p.page.Cursor(next)
		if err != nil {
			return err
		}
		cursor = &c
	}
	return server.WriteList(w, items, cursor, members...)
}

// linkTarget is the target of a link in a Link header, which may hold
// commas and semicolons of its own.
var linkTarget = regexp.MustCompile(`<[^>]*>`)

// namesNext reports whether the Link header of h names a link with the
// relation next (RFC 8288).
func namesNext(h http.Header) bool {
	for _, value := range h.Values("Link") {
		for link := range strings.SplitSeq(linkTarget.ReplaceAllString(value, "<>"), ",") {
			params := strings.Split(link, ";")
			for _, param := range params[1:] {
				name, v, ok := strings.Cut(strings.TrimSpace(param), "=")
				if !ok || !strings.EqualFold(strings.TrimSpace(name), "rel") {
					continue
				}
				for rel := range strings.FieldsSeq(strings.Trim(strings.TrimSpace(v), `"`)) {
					if strings.EqualFold(rel, "next") {
						return true
					}
				}
			}
		}
	}
	return false
}
