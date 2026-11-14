package event

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// heartbeatInterval is how often a stream sends a heartbeat, on the
// component's clock.
const heartbeatInterval = 30 * time.Second

// streamLimit is the number of streams that may be open at once. Every
// open stream holds a place, which Counters reports as OpenStreams; no limit
// is configured, so a place is always free.
const streamLimit = math.MaxInt

// serveStream answers GET /events: a Server-Sent Events stream of the
// visible, matching events after the resume position, or after last_seq at
// open without one, followed live on the same connection, with a heartbeat
// at once and then every 30 seconds. It ends when the client goes away or
// the hub begins to stop.
func (c *Component) serveStream(w http.ResponseWriter, r *http.Request) error {
	q, err := parseQuery(r.URL.RawQuery)
	if err != nil {
		return err
	}
	f := parseFilter(q)
	pos, ok, err := streamPosition(r, q)
	if err != nil {
		return err
	}
	ctx := r.Context()
	if !ok {
		b, err := c.Bounds(ctx)
		if err != nil {
			return err
		}
		pos = b.LastSeq
	}
	if !c.takePlace(streamLimit) {
		return server.Unavailable("too many event streams are open")
	}
	defer c.givePlace()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	s := &stream{c: c, w: w, rc: http.NewResponseController(w), caller: CallerOf(r), filter: f, pos: pos}
	if err := s.heartbeat(); err != nil {
		return s.ended(ctx, err)
	}
	timer := c.deps.Clock.NewTimer(heartbeatInterval)
	defer func() { timer.Stop() }()
	for {
		// Replay and live delivery are this one read by position, so the
		// two phases can neither leave a gap nor repeat an event.
		if err := s.deliver(ctx); err != nil {
			return s.ended(ctx, err)
		}
		select {
		case <-c.deps.Shutdown:
			return nil
		case <-ctx.Done():
			return nil
		case <-timer.C():
			// The heartbeat tells the client how far the log has been
			// examined for it, so it reads the log up to now first.
			if err := s.deliver(ctx); err != nil {
				return s.ended(ctx, err)
			}
			if err := s.heartbeat(); err != nil {
				return s.ended(ctx, err)
			}
			timer = c.deps.Clock.NewTimer(heartbeatInterval)
		case <-c.waitAfter(s.pos):
		}
	}
}

// stream is one open event stream.
type stream struct {
	c      *Component
	w      io.Writer
	rc     *http.ResponseController
	caller Caller
	filter filter
	// pos is the position up to which the log has been examined for the
	// client: every visible, matching event at or below it has been sent.
	pos int64
}

// deliver sends the visible, matching events after the stream's position, in
// batches until the log has nothing further, and moves the position to
// where the log was examined, also past the events it did not send. Each
// batch is decided by one viewer, so the caller's access is read once per
// batch and a change of it shows at the next one. An error ends the stream.
func (s *stream) deliver(ctx context.Context) error {
	for {
		recs, examined, _, err := s.c.readBatch(ctx, s.pos, logBatch)
		if err != nil {
			return err
		}
		v := s.c.newViewer(s.caller)
		for _, rec := range recs {
			if !s.filter.match(rec) {
				continue
			}
			ok, err := v.visible(ctx, rec)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			data, err := encode(rec)
			if err != nil {
				return fmt.Errorf("encode event %d: %w", rec.Seq, err)
			}
			if err := s.frame(rec.Seq, "hub_event", data); err != nil {
				return err
			}
		}
		s.pos = examined
		if err := s.flush(); err != nil {
			return err
		}
		if len(recs) < logBatch {
			return nil
		}
	}
}

// heartbeat sends a heartbeat with the current time and the stream's
// position, which is also the frame's id, so that a client that reconnects
// with it resumes where the log was examined for it.
func (s *stream) heartbeat() error {
	data, err := encode(struct {
		Time string `json:"time"`
		Seq  int64  `json:"seq"`
	}{timefmt.Format(s.c.deps.Clock.Now()), s.pos})
	if err != nil {
		return err
	}
	if err := s.frame(s.pos, "heartbeat", data); err != nil {
		return err
	}
	return s.flush()
}

// flush sends what the stream has written to the client.
func (s *stream) flush() error {
	if err := s.rc.Flush(); err != nil {
		return fmt.Errorf("%w: %w", errClientGone, err)
	}
	return nil
}

// frame writes one frame: its id, its event name and one line of data,
// ended by an empty line.
func (s *stream) frame(id int64, event string, data []byte) error {
	if _, err := fmt.Fprintf(s.w, "id: %d\nevent: %s\ndata: %s\n\n", id, event, data); err != nil {
		return fmt.Errorf("%w: %w", errClientGone, err)
	}
	return nil
}

// errClientGone wraps a failure to write to the client of a stream.
var errClientGone = errors.New("the client of the stream went away")

// ended is what serveStream returns when err ended the stream: nothing when
// the client went away, which is how a stream normally ends, else err, which
// the server logs, since the answer has begun and no status can be sent.
func (s *stream) ended(ctx context.Context, err error) error {
	if errors.Is(err, errClientGone) || ctx.Err() != nil {
		return nil
	}
	return err
}
