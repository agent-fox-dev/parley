package event

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// The limits of a stream, on the component's clock.
const (
	// heartbeatInterval is how often a stream sends a heartbeat.
	heartbeatInterval = 30 * time.Second
	// maxWaiting is how many events may wait for a client that reads more
	// slowly than they come; one more closes its stream.
	maxWaiting = 256
	// idleWriteLimit is how long a write to the client may take before its
	// stream is closed.
	idleWriteLimit = 60 * time.Second
	// streamRetryAfter is the Retry-After, in seconds, of a stream refused
	// because every place is taken.
	streamRetryAfter = "5"
)

// streamLimit is the number of streams that may be open at once: the
// configured MaxStreams, or no limit when none is set.
func (c *Component) streamLimit() int {
	if c.deps.MaxStreams <= 0 {
		return math.MaxInt
	}
	return c.deps.MaxStreams
}

// serveStream answers GET /events: a Server-Sent Events stream of the
// visible, matching events after the resume position, or after last_seq at
// open without one, followed live on the same connection, with a heartbeat
// at once and then every 30 seconds. Every refusal is answered before the
// stream's headers are written, so that it is the JSON error envelope and
// no stream starts: the filters, the position against the log's bounds and
// the workspace filter against the caller's access, checked in that order
// by the rules the log applies too; then a request that finds every stream
// place taken is refused with 503 and Retry-After: 5.
//
// The stream ends when the client goes away, when the hub begins to stop,
// when the credential no longer authenticates at a heartbeat, when 256
// events wait for the client and another is due, or when a write to the
// client has not completed for 60 seconds. It never skips an event while it
// is open: what the client did not receive, it reads again by resuming from
// the last id it got.
func (c *Component) serveStream(w http.ResponseWriter, r *http.Request) error {
	q, err := parseQuery(r.URL.RawQuery)
	if err != nil {
		return err
	}
	f, err := parseFilter(q)
	if err != nil {
		return err
	}
	pos, posName, err := streamPosition(r, q)
	if err != nil {
		return err
	}
	ctx, caller := r.Context(), CallerOf(r)
	b, err := c.Bounds(ctx)
	if err != nil {
		return err
	}
	if posName == "" {
		pos = b.LastSeq
	} else if err := checkPosition(posName, pos, b); err != nil {
		return err
	}
	if err := c.checkWorkspace(ctx, caller, f.Workspace); err != nil {
		return err
	}
	if !c.takePlace(c.streamLimit()) {
		w.Header().Set("Retry-After", streamRetryAfter)
		return server.Unavailable("too many event streams are open")
	}
	defer c.givePlace()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	s := &stream{c: c, caller: caller, filter: f, pos: pos, out: newOutbox(c.deps.Clock, w)}
	go s.out.run()
	defer s.out.stop()

	clock := c.deps.Clock
	s.heartbeat()
	nextBeat := clock.Now().Add(heartbeatInterval)
	timer := clock.NewTimer(heartbeatInterval)
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
		case err := <-s.out.failed:
			return s.ended(ctx, err)
		case <-timer.C():
			now := clock.Now()
			if since, busy := s.out.writingSince(); busy && now.Sub(since) >= idleWriteLimit {
				// The client has accepted no data for the limit.
				return nil
			}
			if now.Before(nextBeat) {
				timer = clock.NewTimer(s.out.nextWake(now, nextBeat))
				continue
			}
			// The credential is checked before anything more is sent, so no
			// event follows the check that found it ended.
			if err := s.authenticate(r); err != nil {
				return s.ended(ctx, err)
			}
			// The heartbeat tells the client how far the log has been
			// examined for it, so it reads the log up to now first.
			if err := s.deliver(ctx); err != nil {
				return s.ended(ctx, err)
			}
			s.heartbeat()
			nextBeat = now.Add(heartbeatInterval)
			timer = clock.NewTimer(s.out.nextWake(now, nextBeat))
		case <-c.waitAfter(s.pos):
		}
	}
}

// stream is one open event stream.
type stream struct {
	c      *Component
	caller Caller
	filter filter
	// pos is the position up to which the log has been examined for the
	// client: every visible, matching event at or below it has been queued
	// for the client.
	pos int64
	out *outbox
}

// deliver queues the visible, matching events after the stream's position,
// in batches until the log has nothing further, and moves the position to
// where the log was examined, also past the events it did not send. Each
// batch is decided by one viewer, so the caller's access is read once per
// batch and a change of it shows at the next one. An error ends the stream;
// so does an event that finds 256 events waiting, which is never dropped.
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
			if !s.out.push(outFrame{id: rec.Seq, event: "hub_event", data: data}) {
				return errTooSlow
			}
		}
		s.pos = examined
		if len(recs) < logBatch {
			return nil
		}
	}
}

// heartbeat queues a heartbeat with the current time and the stream's
// position, which is also the frame's id, so that a client that reconnects
// with it resumes where the log was examined for it.
func (s *stream) heartbeat() {
	// Two plain members cannot fail to encode.
	data, _ := encode(struct {
		Time string `json:"time"`
		Seq  int64  `json:"seq"`
	}{timefmt.Format(s.c.deps.Clock.Now()), s.pos})
	s.out.push(outFrame{id: s.pos, event: "heartbeat", data: data})
}

// authenticate checks the stream's credential again, as the request's was
// checked when the stream opened: a credential revoked, expired or no longer
// accepted since (its user blocked) ends the stream with errCredentialEnded.
// A failure to check also ends it.
func (s *stream) authenticate(r *http.Request) error {
	if s.c.deps.Authenticate == nil {
		return nil
	}
	_, err := s.c.deps.Authenticate(r)
	var refused *server.Error
	if errors.As(err, &refused) {
		return fmt.Errorf("%w: %w", errCredentialEnded, err)
	}
	return err
}

// The ends of a stream that are not failures of the hub.
var (
	// errClientGone wraps a failure to write to the client of a stream.
	errClientGone = errors.New("the client of the stream went away")
	// errTooSlow ends a stream whose client lets 256 events wait.
	errTooSlow = errors.New("the client of the stream reads too slowly")
	// errCredentialEnded ends a stream whose credential no longer
	// authenticates.
	errCredentialEnded = errors.New("the credential of the stream no longer authenticates")
)

// ended is what serveStream returns when err ended the stream: nothing when
// the stream ended for a reason of the client's, which is how a stream
// normally ends, else err, which the server logs, since the answer has
// begun and no status can be sent.
func (s *stream) ended(ctx context.Context, err error) error {
	if errors.Is(err, errClientGone) || errors.Is(err, errTooSlow) || errors.Is(err, errCredentialEnded) ||
		ctx.Err() != nil {
		return nil
	}
	return err
}

// outFrame is one frame of a stream: its id, its event name and one line of
// data.
type outFrame struct {
	id    int64
	event string
	data  []byte
}

// outbox holds the frames of one stream between the handler, which reads
// the log, and the goroutine that writes them to the client, so that a
// client that reads slowly holds up neither the handler's heartbeats nor
// its checks. At most maxWaiting events wait in it.
type outbox struct {
	clock Clock
	w     io.Writer
	rc    *http.ResponseController
	// ready has a token when a frame was queued; failed receives the error
	// that ended the writer; done is closed when the writer has returned.
	ready  chan struct{}
	failed chan error
	done   chan struct{}

	mu     sync.Mutex
	frames []outFrame
	// events is the number of hub_event frames in frames.
	events int
	// closed stops the writer before its next frame.
	closed bool
	// since is when the write under way began, on the component's clock;
	// zero while none is.
	since time.Time
}

func newOutbox(clock Clock, w http.ResponseWriter) *outbox {
	return &outbox{clock: clock, w: w, rc: http.NewResponseController(w),
		ready: make(chan struct{}, 1), failed: make(chan error, 1), done: make(chan struct{})}
}

// push queues f, and reports false, queuing nothing, when f is an event and
// maxWaiting events are already waiting.
func (o *outbox) push(f outFrame) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if f.event == "hub_event" {
		if o.events >= maxWaiting {
			return false
		}
		o.events++
	}
	o.frames = append(o.frames, f)
	select {
	case o.ready <- struct{}{}:
	default:
	}
	return true
}

// next takes the first frame and marks a write under way, unless the outbox
// is closed or empty.
func (o *outbox) next() (outFrame, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed || len(o.frames) == 0 {
		return outFrame{}, false
	}
	f := o.frames[0]
	o.frames[0] = outFrame{}
	o.frames = o.frames[1:]
	if f.event == "hub_event" {
		o.events--
	}
	o.since = o.clock.Now()
	return f, true
}

// empty reports whether no frame waits, when the writer flushes.
func (o *outbox) empty() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.frames) == 0
}

// written marks the write under way, with its flush, as done.
func (o *outbox) written() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.since = time.Time{}
}

// writingSince reports when the write under way began, if one is.
func (o *outbox) writingSince() (time.Time, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.since, !o.since.IsZero()
}

// nextWake is how long the handler waits from now: until the next heartbeat
// at beat, or sooner when the write under way reaches the idle limit before.
func (o *outbox) nextWake(now, beat time.Time) time.Duration {
	d := beat.Sub(now)
	if since, busy := o.writingSince(); busy {
		d = min(d, since.Add(idleWriteLimit).Sub(now))
	}
	return d
}

// run writes the queued frames to the client in order, flushing whenever
// the outbox is empty, until stop or a failed write.
func (o *outbox) run() {
	defer close(o.done)
	for {
		f, ok := o.next()
		if !ok {
			o.mu.Lock()
			closed := o.closed
			o.mu.Unlock()
			if closed {
				return
			}
			<-o.ready
			continue
		}
		_, err := fmt.Fprintf(o.w, "id: %d\nevent: %s\ndata: %s\n\n", f.id, f.event, f.data)
		if err == nil && o.empty() {
			err = o.rc.Flush()
		}
		o.written()
		if err != nil {
			o.failed <- fmt.Errorf("%w: %w", errClientGone, err)
			return
		}
	}
}

// stop ends the writer and waits for it, so that nothing writes to the
// response once the handler returns. A write that is blocked on a client
// that does not read is ended with a write deadline in the past, so the
// handler never waits for the client. Where the response cannot take a
// deadline, the writer is left to end after its write, without another.
func (o *outbox) stop() {
	o.mu.Lock()
	o.closed = true
	busy := !o.since.IsZero()
	o.mu.Unlock()
	select {
	case o.ready <- struct{}{}:
	default:
	}
	if busy {
		if err := o.rc.SetWriteDeadline(time.Now()); err != nil {
			return
		}
	}
	<-o.done
}
