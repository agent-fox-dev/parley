package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/agent-fox-dev/parley/internal/server"
)

// LogFollowInterval is the longest a followed log stream waits, on the
// hub's clock, before it reads the store again: a line the collector
// stores without a signal reaching the stream is sent within it.
const LogFollowInterval = time.Second

// logFollowBatch is the most lines a followed log stream reads from the
// store at once, so that a long log is not held in memory whole.
const logFollowBatch = 500

// logsRequest is the query of a log read, as its validation read it.
type logsRequest struct {
	// sel selects the lines by source, since and tail.
	sel LogSelection
	// follow streams the lines, and then each new one, as server-sent
	// events.
	follow bool
	// page is the page a read without follow answers.
	page server.Page
}

// validateLogs reads the query of a log read into u: source process or
// policy, since an RFC 3339 timestamp, tail an integer of at least 1,
// follow true or false, and limit and cursor through the list helper,
// which a followed read, which sends every selected line, does not take.
func validateLogs(r *http.Request, u *useRequest) error {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return server.InvalidRequest("the query string is malformed")
	}
	var req logsRequest
	if q.Has("source") {
		switch v := q.Get("source"); v {
		case LogSourceProcess, LogSourcePolicy:
			req.sel.Source = v
		default:
			return server.InvalidRequest("source: must be process or policy")
		}
	}
	if q.Has("since") {
		since, err := time.Parse(time.RFC3339, q.Get("since"))
		if err != nil {
			return server.InvalidRequest("since: must be an RFC 3339 timestamp")
		}
		req.sel.Since = since
	}
	if q.Has("tail") {
		n, err := strconv.Atoi(q.Get("tail"))
		if err != nil || n < 1 {
			return server.InvalidRequest("tail: must be an integer of at least 1")
		}
		req.sel.Tail = n
	}
	if q.Has("follow") {
		switch q.Get("follow") {
		case "true":
			req.follow = true
		case "false":
		default:
			return server.InvalidRequest("follow: must be true or false")
		}
	}
	if req.follow {
		if q.Has("cursor") || q.Has("limit") {
			return server.InvalidRequest("cursor and limit: must not be sent with follow=true, which sends every selected line")
		}
	} else if req.page, err = server.ReadPage(r); err != nil {
		return err
	}
	u.logs = req
	return nil
}

// serveLogs answers GET …/logs from the hub's store, never from the
// runtime, so that the logs of a sandbox are there in every state and
// while the runtime is unreachable: a page of the selected lines, oldest
// first, or with follow a stream of them and then of each new one.
func (s *Service) serveLogs(w http.ResponseWriter, r *http.Request, u useRequest) error {
	if u.logs.follow {
		return s.streamLogs(w, r, u)
	}
	sel, page := u.logs.sel, u.logs.page
	if ok, err := page.After(&sel.After); err != nil {
		return err
	} else if ok {
		// The tail placed where the first page starts; the cursor
		// continues from there, so that a line stored meanwhile is not
		// skipped.
		sel.Tail = 0
	}
	sel.Limit = page.Limit + 1
	lines, err := LogLines(r.Context(), s.deps.Store.Main(), u.rec.ID, sel)
	if err != nil {
		return err
	}
	return server.WritePage(w, page, lines, func(l LogLine) any { return l.Seq })
}

// streamLogs streams the selected lines of the sandbox, oldest first, as
// frames named log, and then each line stored after them that the
// selection keeps. It reads the store past the last line it sent when the
// collector says it stored lines, when a sandbox changes state, and at
// least every LogFollowInterval on the hub's clock. It ends when the
// caller leaves; and when the sandbox is deleted or the hub begins to
// stop, once it sent the lines stored until then.
func (s *Service) streamLogs(w http.ResponseWriter, r *http.Request, u useRequest) error {
	ctx := r.Context()
	h := w.Header()
	h.Set("Content-Type", eventStreamType)
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		return nil
	}
	sel := u.logs.sel
	stopping := false
	for {
		// Taken before the reads, so that a line or a state stored after
		// them wakes the stream.
		stored, changed := s.LogLinesStored(), s.changes.next()
		rec, err := Get(ctx, s.deps.Store.Main(), u.rec.ID)
		gone := errors.Is(err, ErrNotFound)
		if err != nil && !gone {
			return followError(ctx, err)
		}
		if err := s.sendLogLines(ctx, w, rc, u.rec.ID, &sel); err != nil {
			return followError(ctx, err)
		}
		if gone || rec.State == StateDeleted || stopping {
			return nil
		}
		t := s.newTimer(LogFollowInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-s.deps.Shutdown:
			stopping = true
		case <-stored:
		case <-changed:
		case <-t.C():
		}
		t.Stop()
	}
}

// followError is what a followed log stream returns for err: nothing once
// the caller left, since the error is then the caller's leaving; else err,
// which only reaches the hub's log, as the stream's headers were sent.
func followError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// sendLogLines sends, as frames named log, the lines of the sandbox id
// that sel selects, and moves sel past the last one. The tail places only
// the first read: what follows it is every line stored after.
func (s *Service) sendLogLines(ctx context.Context, w http.ResponseWriter, rc *http.ResponseController, id string, sel *LogSelection) error {
	for {
		batch := *sel
		batch.Limit = logFollowBatch
		lines, err := LogLines(ctx, s.deps.Store.Main(), id, batch)
		if err != nil {
			return err
		}
		sel.Tail = 0
		for _, l := range lines {
			b, err := json.Marshal(l)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "event: log\ndata: %s\n\n", b); err != nil {
				return err
			}
			sel.After = l.Seq
		}
		if len(lines) > 0 {
			if err := rc.Flush(); err != nil {
				return err
			}
		}
		if len(lines) < logFollowBatch {
			return nil
		}
	}
}
