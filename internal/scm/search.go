package scm

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/server"
)

// searchBounds are the default and the bounds of a search's limit.
var searchBounds = server.PageBounds{Default: 50, Min: 1, Max: 200}

// maxSearchQuery is the most characters a search's q may have, and
// maxMatchText the most characters of a matching line a search answers.
const (
	maxSearchQuery = 500
	maxMatchText   = 500
)

// searchMatch is a matching line as the search route answers it.
type searchMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// searchPosition is where a search continues: the commit its first page
// resolved, so that every page searches the same files when the ref moves,
// and the path and line of the last match the pages before held.
type searchPosition struct {
	SHA  string `json:"sha"`
	Path string `json:"path"`
	Line int    `json:"line"`
}

// after reports whether m comes after p in the order of a search's
// matches: by path, compared byte by byte, then by line.
func (p searchPosition) after(m gitrepo.GrepMatch) bool {
	return m.Path > p.Path || (m.Path == p.Path && m.Line > p.Line)
}

// search answers GET …/scm/search: a page of the lines of the text files
// at ref that q matches, as a fixed string with regard to case unless
// regex or case_sensitive say otherwise, in the files whose path one of the
// path globs matches (every file without one), ordered by path then line,
// each line cut to its first maxMatchText characters, with the commit the
// files were read from.
func (c *Component) search(w http.ResponseWriter, r *http.Request) error {
	page, err := server.ReadPage(r, searchBounds)
	if err != nil {
		return err
	}
	var pos searchPosition
	continuing, err := page.After(&pos)
	if err != nil {
		return err
	}
	if continuing && (!isCommitID(pos.SHA) || len(pos.SHA) != commitIDLen || pos.Path == "" || pos.Line < 1) {
		return server.InvalidRequest("cursor is malformed or belongs to another query")
	}
	q := r.URL.Query()
	pattern := q.Get("q")
	if n := utf8.RuneCountInString(pattern); n < 1 || n > maxSearchQuery {
		return server.InvalidRequest(fmt.Sprintf("q must have 1 to %d characters", maxSearchQuery))
	}
	if strings.ContainsRune(pattern, 0) {
		return server.InvalidRequest("q must not contain a NUL character")
	}
	regex, err := boolParam(q, "regex", false)
	if err != nil {
		return err
	}
	caseSensitive, err := boolParam(q, "case_sensitive", true)
	if err != nil {
		return err
	}
	globs := q["path"]

	rd, err := c.open(r)
	if err != nil {
		return err
	}
	res, err := listedCommit(r, rd, continuing, pos.SHA)
	if err != nil {
		return err
	}
	if notModified(w, r, res.Full, res.SHA) {
		return nil
	}
	sha := res.SHA
	items := make([]searchMatch, 0, page.Limit+1)
	err = rd.repo.Grep(r.Context(), sha, gitrepo.GrepQuery{Pattern: pattern, Regexp: regex, IgnoreCase: !caseSensitive}, func(m gitrepo.GrepMatch) bool {
		if (continuing && !pos.after(m)) || !matchesAny(globs, m.Path) {
			return true
		}
		items = append(items, searchMatch{Path: m.Path, Line: m.Line, Text: firstRunes(m.Text, maxMatchText)})
		// One match past the page tells that there is a next page.
		return len(items) <= page.Limit
	})
	if errors.Is(err, gitrepo.ErrInvalidPattern) {
		return server.InvalidRequest(fmt.Sprintf("q %q is not a valid extended regular expression", pattern))
	}
	if err != nil {
		return err
	}
	return server.WritePageWith(w, page, items, func(m searchMatch) any { return searchPosition{SHA: sha, Path: m.Path, Line: m.Line} },
		server.Member{Name: "resolved_sha", Value: sha})
}

// firstRunes is s cut to its first n characters, so that no character is
// split.
func firstRunes(s string, n int) string {
	for i := range s {
		if n == 0 {
			return s[:i]
		}
		n--
	}
	return s
}
