package awareness

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/agent-fox-dev/parley/internal/server"
)

// Overlaps are the overlap entries of the change number of workspace, read
// through q (nil for the store): one per other draft or open change of the
// workspace with the same target branch and at least one indexed file in
// common, outside the change's stack, the most files in common first and,
// among equals, the lower number first. A change that is neither draft
// nor open has none, also before the index has followed it out of flight.
// A change the workspace does not have is ErrChangeNotFound.
//
// The partners are those of Partners, and each one's summary is read from
// its record.
func (c *Component) Overlaps(ctx context.Context, q Querier, workspace string, number int64) ([]Entry, error) {
	q = c.querier(q)
	ch, err := c.deps.Changes.ReadChange(ctx, q, workspace, number)
	if err != nil {
		return nil, err
	}
	if !inFlight(ch.State) {
		return []Entry{}, nil
	}
	partners, err := c.partners(ctx, q, workspace, number)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(partners))
	for _, p := range partners {
		out = append(out, EntryOf(p.record, p.Paths))
	}
	return out, nil
}

// inFlight reports whether a change in state is draft or open.
func inFlight(state string) bool { return state == StateDraft || state == StateOpen }

// overlapPosition is an overlap entry's place in the order of the
// overlaps list: count descending, then number ascending.
type overlapPosition struct {
	Count  int   `json:"count"`
	Number int64 `json:"number"`
}

// compare orders p before o as the overlaps list does.
func (p overlapPosition) compare(o overlapPosition) int {
	return cmp.Or(cmp.Compare(o.Count, p.Count), cmp.Compare(p.Number, o.Number))
}

// overlapsRoute answers the overlaps of the change :number of the
// workspace the route acts on, in pages of the list envelope; none for a
// landed or abandoned change. A :number that is not a positive integer, or
// that the workspace does not have, is 404 change_not_found.
func (c *Component) overlapsRoute(w http.ResponseWriter, r *http.Request) error {
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	var after overlapPosition
	paged, err := page.After(&after)
	if err != nil {
		return err
	}
	workspace := actedOn(r)
	number, ok := server.Positive(server.Param(r, "number"))
	if !ok {
		return server.NewError(errChangeNotFound, http.StatusNotFound, "no such change")
	}
	entries, err := c.Overlaps(r.Context(), nil, workspace, number)
	if errors.Is(err, ErrChangeNotFound) {
		return server.NewError(errChangeNotFound, http.StatusNotFound,
			"no change "+workspace+"#"+strconv.FormatInt(number, 10))
	}
	if err != nil {
		return err
	}
	if paged {
		i := 0
		for i < len(entries) && positionOf(entries[i]).compare(after) <= 0 {
			i++
		}
		entries = entries[i:]
	}
	entries = entries[:min(len(entries), page.Limit+1)]
	return server.WritePage(w, page, entries, func(e Entry) any { return positionOf(e) })
}

// positionOf is the place of the entry e in the overlaps list.
func positionOf(e Entry) overlapPosition {
	return overlapPosition{Count: e.Count, Number: e.Change.Number}
}

// errChangeNotFound is the error type of a :number that names no change of
// the workspace a route acts on.
const errChangeNotFound = "change_not_found"
