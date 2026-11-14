package event

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/agent-fox-dev/parley/internal/server"
)

// The bounds of a request to the log.
const (
	// defaultLogLimit and maxLogLimit are the list rule's default and upper
	// bound of limit.
	defaultLogLimit = 50
	maxLogLimit     = 200
	// maxLogWait is the longest wait, in seconds, a request may ask for.
	maxLogWait = 60
	// logBatch is how many events of the log a page reads at a time while
	// it skips the events the caller may not see or the filters exclude.
	logBatch = 200
)

// Routes returns the event routes, for the hub to register.
func (c *Component) Routes() []server.Route {
	return []server.Route{{Method: http.MethodGet, Pattern: "/events/log", Handler: c.serveLog, Scope: "audit:read"}}
}

// logQuery is a request to the log.
type logQuery struct {
	filter filter
	// after is the position the page starts after, when hasAfter.
	after    int64
	hasAfter bool
	limit    int
	wait     time.Duration
}

// parseLogQuery reads a request to the log, refusing every bad parameter at
// once with 400 invalid_request. It parses limit itself rather than through
// server.ReadPage, which would read cursor as the generic list's opaque
// cursor: here cursor is another name for after.
func parseLogQuery(rawQuery string) (logQuery, error) {
	q, err := parseQuery(rawQuery)
	if err != nil {
		return logQuery{}, err
	}
	lq := logQuery{filter: parseFilter(q), limit: defaultLogLimit}
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 || n > maxLogLimit {
			return logQuery{}, server.InvalidRequest(fmt.Sprintf("limit must be an integer from 1 to %d", maxLogLimit))
		}
		lq.limit = n
	}
	if q.Has("wait") {
		n, err := strconv.Atoi(q.Get("wait"))
		if err != nil || n < 0 || n > maxLogWait {
			return logQuery{}, server.InvalidRequest(fmt.Sprintf("wait must be a whole number of seconds from 0 to %d", maxLogWait))
		}
		lq.wait = time.Duration(n) * time.Second
	}
	for _, name := range []string{"after", "cursor"} {
		if !q.Has(name) {
			continue
		}
		pos, err := parsePosition(name, q.Get(name))
		if err != nil {
			return logQuery{}, err
		}
		if lq.hasAfter && pos != lq.after {
			return logQuery{}, server.InvalidRequest("after and cursor name different positions")
		}
		lq.after, lq.hasAfter = pos, true
	}
	return lq, nil
}

// logPage is the answer of the log.
type logPage struct {
	// Items is never nil, so that an empty page is [].
	Items      []Record `json:"items"`
	NextCursor *string  `json:"next_cursor"`
	NextAfter  int64    `json:"next_after"`
	OldestSeq  int64    `json:"oldest_seq"`
	LastSeq    int64    `json:"last_seq"`
}

// serveLog answers GET /events/log: the visible, matching events after the
// position, at most limit of them. When there is none and wait is above 0,
// it holds the request until one is committed, the wait ends on the
// component's clock, the hub begins to stop or the client goes away.
func (c *Component) serveLog(w http.ResponseWriter, r *http.Request) error {
	q, err := parseLogQuery(r.URL.RawQuery)
	if err != nil {
		return err
	}
	ctx, caller := r.Context(), CallerOf(r)
	pos := q.after
	if !q.hasAfter {
		b, err := c.Bounds(ctx)
		if err != nil {
			return err
		}
		pos = max(b.OldestSeq-1, 0)
	}
	page, err := c.collect(ctx, caller, q.filter, pos, q.limit)
	if err != nil || len(page.Items) > 0 || q.wait == 0 {
		return writeLogPage(w, page, err)
	}

	timer := c.deps.Clock.NewTimer(q.wait)
	defer timer.Stop()
	for {
		// A stop releases a held request at once, before anything else
		// that is ready: the empty page tells the client where to resume.
		select {
		case <-c.deps.Shutdown:
			return writeLogPage(w, page, nil)
		default:
		}
		select {
		case <-c.deps.Shutdown:
			return writeLogPage(w, page, nil)
		case <-ctx.Done():
			// Nobody is left to answer.
			return nil
		case <-timer.C():
			// One last read, so that next_after is the position the log
			// holds now and not where the wait began.
			page, err = c.collect(ctx, caller, q.filter, page.NextAfter, q.limit)
			return writeLogPage(w, page, err)
		case <-c.waitAfter(page.NextAfter):
		}
		if page, err = c.collect(ctx, caller, q.filter, page.NextAfter, q.limit); err != nil || len(page.Items) > 0 {
			return writeLogPage(w, page, err)
		}
	}
}

// collect reads the page of the log after pos for caller: the events that
// match f and that the caller may see, in seq order, at most limit of them.
// It reads the log in batches until the page is full or the log is
// examined to its end, and decides the visibility of the whole page with one
// viewer, so that access is read once per page. next_after is the seq of
// the last item of a full page, else the position examined, which is then
// the page's last_seq.
func (c *Component) collect(ctx context.Context, caller Caller, f filter, pos int64, limit int) (logPage, error) {
	v := c.newViewer(caller)
	page := logPage{Items: []Record{}}
	for {
		recs, examined, b, err := c.readBatch(ctx, pos, logBatch)
		if err != nil {
			return logPage{}, err
		}
		page.OldestSeq, page.LastSeq = b.OldestSeq, b.LastSeq
		for _, rec := range recs {
			if !f.match(rec) {
				continue
			}
			ok, err := v.visible(ctx, rec)
			if err != nil {
				return logPage{}, err
			}
			if !ok {
				continue
			}
			page.Items = append(page.Items, rec)
			if len(page.Items) == limit {
				cursor := strconv.FormatInt(rec.Seq, 10)
				page.NextAfter, page.NextCursor = rec.Seq, &cursor
				return page, nil
			}
		}
		if len(recs) < logBatch {
			page.NextAfter = examined
			return page, nil
		}
		pos = examined
	}
}

// readBatch is ReadAfter with the bounds of the same snapshot, so that a
// page's position and its bounds agree.
func (c *Component) readBatch(ctx context.Context, after int64, limit int) ([]Record, int64, Bounds, error) {
	if c.deps.Store == nil {
		return nil, 0, Bounds{}, errNotBound
	}
	var recs []Record
	var b Bounds
	err := c.snapshot(ctx, func(tx *sql.Tx) error {
		var err error
		if b, err = readBounds(ctx, tx); err != nil {
			return err
		}
		recs, err = queryRecords(ctx, tx, max(after, b.OldestSeq-1), b.LastSeq, limit)
		return err
	})
	if err != nil {
		return nil, 0, Bounds{}, fmt.Errorf("read events: %w", err)
	}
	examined := b.LastSeq
	if len(recs) == limit {
		examined = recs[len(recs)-1].Seq
	}
	return recs, examined, b, nil
}

// writeLogPage answers 200 with page, or returns err when there is one. The
// page is written without HTML escaping, so that its items are the bytes
// the stream sends for the same events.
func writeLogPage(w http.ResponseWriter, page logPage, err error) error {
	if err != nil {
		return err
	}
	body, err := encode(page)
	if err != nil {
		return fmt.Errorf("encode a page of the event log: %w", err)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(append(body, '\n'))
	return nil
}
