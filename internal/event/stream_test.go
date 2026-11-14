package event

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/identity"
)

// frame is one frame of an event stream: its lines, without the empty line
// that ends it, and the values of its id, event and data fields.
type frame struct {
	lines []string
	id    string
	event string
	data  string
}

// parseFrame is the frame of lines.
func parseFrame(lines []string) frame {
	f := frame{lines: lines}
	for _, line := range lines {
		name, value, _ := strings.Cut(line, ": ")
		switch name {
		case "id":
			f.id = value
		case "event":
			f.event = value
		case "data":
			f.data = value
		}
	}
	return f
}

// seq is the frame's id as a number.
func (f frame) seq(t testing.TB) int64 {
	t.Helper()
	n, err := strconv.ParseInt(f.id, 10, 64)
	if err != nil {
		t.Fatalf("frame id %q: %v", f.id, err)
	}
	return n
}

// eventStream is a stream the test opened on a real HTTP connection.
type eventStream struct {
	answer
	t      testing.TB
	frames <-chan frame
	// close ends the connection from the client's side.
	close func()
}

// serve serves the world's router on a loopback HTTP server, started on
// first use. At the end of the test the hub's stop ends the streams still
// open before the server closes, which waits for its requests.
func (w *world) serve() *httptest.Server {
	if w.http == nil {
		w.http = httptest.NewServer(w.srv.Handler())
		w.t.Cleanup(func() {
			w.shutDown()
			w.http.Close()
		})
	}
	return w.http
}

// openStream sends GET /api/v1/events?query with credential, and the
// Last-Event-ID lastEventID unless it is "", on a real connection, and
// returns once the answer's headers have arrived. Its frames are read as
// they come.
func (w *world) openStream(t testing.TB, credential, query, lastEventID string) *eventStream {
	t.Helper()
	srv := w.serve()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/events?"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		cancel()
		t.Fatalf("GET /events?%s: %v", query, err)
	}
	frames := make(chan frame, 1024)
	s := &eventStream{answer: answer{status: res.StatusCode, header: res.Header}, t: t, frames: frames}
	s.close = func() {
		cancel()
		res.Body.Close()
	}
	t.Cleanup(s.close)
	if res.StatusCode != http.StatusOK {
		var body bytes.Buffer
		_, _ = body.ReadFrom(res.Body)
		s.body = body.Bytes()
		s.close()
		close(frames)
		return s
	}
	go func() {
		defer close(frames)
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		var lines []string
		for sc.Scan() {
			if sc.Text() != "" {
				lines = append(lines, sc.Text())
				continue
			}
			frames <- parseFrame(lines)
			lines = nil
		}
	}()
	return s
}

// next returns the next frame, failing the test when none arrives within d
// of real time or the stream ends.
func (s *eventStream) next(d time.Duration) frame {
	s.t.Helper()
	select {
	case f, ok := <-s.frames:
		if !ok {
			s.t.Fatal("the stream ended, want another frame")
		}
		return f
	case <-time.After(d):
		s.t.Fatalf("no frame within %v", d)
		return frame{}
	}
}

// heartbeat returns the next frame and fails the test unless it is a
// heartbeat.
func (s *eventStream) heartbeat(d time.Duration) frame {
	s.t.Helper()
	f := s.next(d)
	if f.event != "heartbeat" {
		s.t.Fatalf("frame %q, want a heartbeat", f.lines)
	}
	return f
}

// quiet fails the test when a frame arrives, or the stream ends, within d
// of real time.
func (s *eventStream) quiet(d time.Duration) {
	s.t.Helper()
	select {
	case f, ok := <-s.frames:
		s.t.Fatalf("frame %q (stream open: %v), want none yet", f.lines, ok)
	case <-time.After(d):
	}
}

// ends fails the test unless the stream ends within d of real time without
// another frame.
func (s *eventStream) ends(d time.Duration) {
	s.t.Helper()
	select {
	case f, ok := <-s.frames:
		if ok {
			s.t.Fatalf("frame %q, want the stream to end without one", f.lines)
		}
	case <-time.After(d):
		s.t.Fatalf("the stream is still open %v later, want it ended", d)
	}
}

// heartbeatData is the data of a heartbeat frame.
type heartbeatData struct {
	Time string `json:"time"`
	Seq  int64  `json:"seq"`
}

// waitOpenStreams waits up to 2 s for the counters to report want open
// streams.
func (w *world) waitOpenStreams(t testing.TB, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for got := w.events.Counters().OpenStreams; got != want; got = w.events.Counters().OpenStreams {
		if time.Now().After(deadline) {
			t.Fatalf("open_streams = %d after 2 s, want %d", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStream_OpensWithHeartbeatThenGoesLive(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.emitMany(3)

	s := w.openStream(t, w.admin, "", "")
	if s.status != http.StatusOK || s.header.Get("Content-Type") != "text/event-stream" || s.header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("answer = %d with Content-Type %q and Cache-Control %q, want 200, text/event-stream and no-cache",
			s.status, s.header.Get("Content-Type"), s.header.Get("Cache-Control"))
	}
	f := s.next(2 * time.Second)
	if len(f.lines) != 3 || f.lines[0] != "id: 3" || f.lines[1] != "event: heartbeat" || !strings.HasPrefix(f.lines[2], "data: ") {
		t.Fatalf("first frame = %q, want id: 3, event: heartbeat and one data line", f.lines)
	}
	var hb heartbeatData
	if err := json.Unmarshal([]byte(f.data), &hb); err != nil || hb != (heartbeatData{Time: "2026-11-14T09:30:15Z", Seq: 3}) {
		t.Errorf("heartbeat data = %s, want time 2026-11-14T09:30:15Z and seq 3", f.data)
	}

	// Without a position the stream sends what is committed after it opened.
	committed := time.Now()
	w.emit(on("parley.change.open", "api"))
	if f := s.next(2 * time.Second); f.event != "hub_event" || f.id != "4" || time.Since(committed) > time.Second {
		t.Errorf("frame %q %v after the commit, want the hub_event 4 within 1 s", f.lines, time.Since(committed))
	}

	// The stream holds a place while it is open, and gives it back when the
	// client goes away.
	w.waitOpenStreams(t, 1)
	s.close()
	w.waitOpenStreams(t, 0)
}

func TestStream_ReplayThenLiveWithoutGap(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.emitMany(300)

	written := make(chan error, 1)
	go func() {
		for range 200 {
			if _, err := w.events.EmitNow(context.Background(), on("parley.change.open", "api")); err != nil {
				written <- err
				return
			}
		}
		written <- nil
	}()
	s := w.openStream(t, w.admin, "after=100", "")
	if s.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", s.status)
	}

	var got []int64
	data := map[int64]string{}
	for len(got) == 0 || got[len(got)-1] < 500 {
		f := s.next(10 * time.Second)
		if f.event != "hub_event" {
			continue
		}
		if len(f.lines) != 3 || !strings.HasPrefix(f.lines[0], "id: ") || f.lines[1] != "event: hub_event" || !strings.HasPrefix(f.lines[2], "data: ") {
			t.Fatalf("frame = %q, want the lines id, event: hub_event and data", f.lines)
		}
		got = append(got, f.seq(t))
		data[f.seq(t)] = f.data
	}
	if err := <-written; err != nil {
		t.Fatalf("the writer: %v", err)
	}
	if want := seqRange(101, 500); !slices.Equal(got, want) {
		t.Fatalf("hub_event ids = %v, want 101..500 once each in order", got)
	}
	for seq, stored := range w.logJSON(100) {
		if data[seq] != stored {
			t.Errorf("frame %d data = %s, want the stored record %s", seq, data[seq], stored)
		}
	}
}

func TestStream_HeartbeatAdvancesPosition(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	start := w.clock.Now()
	quiet := w.openStream(t, w.admin, "types=parley.nothing.*", "")
	all := w.openStream(t, w.admin, "types=parley.change.*", "")
	both := map[string]*eventStream{"filtered": quiet, "matching": all}
	// wantHeartbeat takes the next frame of both streams, a heartbeat at id.
	wantHeartbeat := func(id string) {
		t.Helper()
		for name, s := range both {
			if hb := s.heartbeat(2 * time.Second); hb.id != id {
				t.Fatalf("%s stream: heartbeat id = %s, want %s", name, hb.id, id)
			}
		}
	}
	// wantEvents takes the hub_events from to to of the matching stream.
	wantEvents := func(from, to int64) {
		t.Helper()
		for want := from; want <= to; want++ {
			if f := all.next(2 * time.Second); f.event != "hub_event" || f.seq(t) != want {
				t.Fatalf("matching stream: frame %q, want the hub_event %d", f.lines, want)
			}
		}
	}
	wantHeartbeat("0")
	w.clock.WaitTimers(t, 2)

	// Every matching event up to a heartbeat's seq comes before it.
	w.emitMany(10)
	wantEvents(1, 10)
	w.clock.Advance(29 * time.Second)
	quiet.quiet(200 * time.Millisecond)
	all.quiet(time.Millisecond)
	w.clock.Advance(time.Second)
	for name, s := range both {
		hb := s.heartbeat(2 * time.Second)
		var data heartbeatData
		if err := json.Unmarshal([]byte(hb.data), &data); err != nil || hb.id != "10" || data != (heartbeatData{Time: "2026-11-14T09:30:45Z", Seq: 10}) {
			t.Errorf("%s stream: heartbeat %q, want id 10 with seq 10 at 30 s", name, hb.lines)
		}
	}

	w.clock.WaitTimers(t, 2)
	w.clock.Set(start.Add(3 * time.Hour))
	wantHeartbeat("10")
	w.clock.WaitTimers(t, 2)
	w.emitMany(5)
	wantEvents(11, 15)
	w.clock.Advance(30 * time.Second)
	if hb := quiet.heartbeat(2 * time.Second); hb.id != "15" {
		t.Fatalf("filtered stream: heartbeat id %s, want 15", hb.id)
	}

	if oldest, err := w.events.Expire(context.Background(), start.Add(time.Hour)); err != nil || oldest != 11 {
		t.Fatalf("Expire = %d, %v, want 11", oldest, err)
	}
	again := w.openStream(t, w.admin, "types=parley.nothing.*", "15")
	if again.status != http.StatusOK {
		t.Fatalf("reconnect with Last-Event-ID: 15: status = %d, want 200", again.status)
	}
	if hb := again.heartbeat(2 * time.Second); hb.id != "15" {
		t.Errorf("reconnect: first heartbeat id = %s, want 15", hb.id)
	}
}

// stuckWriter is the response writer of a client that stops reading: it
// accepts its first accept writes, then blocks every later one until the
// server gives up on the connection with a write deadline, as a real
// connection does, or the test ends.
type stuckWriter struct {
	header http.Header
	accept int

	mu      sync.Mutex
	writes  int
	written bytes.Buffer
	// blocked is closed when the first write blocks; release ends every
	// blocked write.
	blocked     chan struct{}
	release     chan struct{}
	blockOnce   sync.Once
	releaseOnce sync.Once
}

func (sw *stuckWriter) Header() http.Header { return sw.header }

func (sw *stuckWriter) WriteHeader(int) {}

func (sw *stuckWriter) Write(p []byte) (int, error) {
	sw.mu.Lock()
	if sw.writes < sw.accept {
		sw.writes++
		defer sw.mu.Unlock()
		return sw.written.Write(p)
	}
	sw.mu.Unlock()
	sw.blockOnce.Do(func() { close(sw.blocked) })
	<-sw.release
	return 0, os.ErrDeadlineExceeded
}

func (sw *stuckWriter) Flush() {}

// SetWriteDeadline ends the blocked writes, as a deadline in the past does
// on a real connection.
func (sw *stuckWriter) SetWriteDeadline(time.Time) error {
	sw.unblock()
	return nil
}

func (sw *stuckWriter) unblock() { sw.releaseOnce.Do(func() { close(sw.release) }) }

// hubEventIDs are the ids of the hub_event frames the writer accepted, in
// their order.
func (sw *stuckWriter) hubEventIDs(t testing.TB) []int64 {
	t.Helper()
	sw.mu.Lock()
	defer sw.mu.Unlock()
	var ids []int64
	for block := range strings.SplitSeq(sw.written.String(), "\n\n") {
		if f := parseFrame(strings.Split(block, "\n")); f.event == "hub_event" {
			ids = append(ids, f.seq(t))
		}
	}
	return ids
}

// stuckStream serves GET /api/v1/events as the admin token to a client that
// stops reading after accept writes, through the world's router, and returns
// the writer and a channel closed when the handler has returned.
func (w *world) stuckStream(t testing.TB, accept int) (*stuckWriter, <-chan struct{}) {
	t.Helper()
	sw := &stuckWriter{header: http.Header{}, accept: accept, blocked: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+w.admin)
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.srv.Handler().ServeHTTP(sw, req)
	}()
	// Cleanups run last first: the stop releases the handler before the
	// store closes.
	t.Cleanup(func() {
		cancel()
		w.shutDown()
		sw.unblock()
		<-done
	})
	return sw, done
}

// happens fails the test unless ch is closed within d of real time.
func happens(t testing.TB, ch <-chan struct{}, d time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(d):
		t.Fatalf("%s: not within %v", what, d)
	}
}

// running fails the test when done is closed now or within d of real time.
func running(t testing.TB, done <-chan struct{}, d time.Duration, what string) {
	t.Helper()
	select {
	case <-done:
		t.Fatalf("%s returned, want it still open", what)
	case <-time.After(d):
	}
}

func TestStream_SlowConsumerIsClosedNotSkipped(t *testing.T) {
	t.Parallel()
	t.Run("256 events waiting", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		// The client takes the opening heartbeat and five events, then
		// stops reading while the sixth is written.
		sw, done := w.stuckStream(t, 6)
		w.clock.WaitTimers(t, 1)
		w.emitMany(6)
		happens(t, sw.blocked, 5*time.Second, "a blocked write")

		w.emitMany(256)
		running(t, done, 500*time.Millisecond, "the stream with 256 events waiting")
		if got := w.events.Counters().OpenStreams; got != 1 {
			t.Fatalf("open_streams = %d with 256 events waiting, want 1", got)
		}
		w.emitMany(1)
		happens(t, done, 2*time.Second, "the end of the stream with a 257th event due")
		w.emitMany(300 - 263)

		if got, want := sw.hubEventIDs(t), seqRange(1, 5); !slices.Equal(got, want) {
			t.Errorf("hub_event ids written = %v, want %v", got, want)
		}
		if got := w.events.Counters().OpenStreams; got != 0 {
			t.Errorf("open_streams = %d after the stream ended, want 0", got)
		}
		if got := seqs(w.readAll(5)); !slices.Equal(got, seqRange(6, 300)) {
			t.Errorf("the log after the last id written = %v, want 6..300", got)
		}
	})
	t.Run("no data accepted for 60 s", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		sw, done := w.stuckStream(t, 1)
		w.clock.WaitTimers(t, 1)
		w.emitMany(1)
		happens(t, sw.blocked, 5*time.Second, "a blocked write")

		w.clock.Advance(59 * time.Second)
		running(t, done, 300*time.Millisecond, "the stream after 59 s")
		w.clock.WaitTimers(t, 1)
		w.clock.Advance(time.Second)
		happens(t, done, 2*time.Second, "the end of the stream after 60 s without a write")
		if got := w.events.Counters().OpenStreams; got != 0 {
			t.Errorf("open_streams = %d, want 0", got)
		}
	})
}

func TestStream_ConnectionLimit(t *testing.T) {
	t.Parallel()
	w := newWorld(t, func(d *Deps) { d.MaxStreams = 2 })
	w.emitMany(1)

	// Held long-polls take no place.
	for range 3 {
		w.holdLog(w.admin, "wait=60&after=1")
	}
	w.clock.WaitTimers(t, 3)

	first := w.openStream(t, w.admin, "", "")
	second := w.openStream(t, w.admin, "", "")
	if first.status != http.StatusOK || second.status != http.StatusOK {
		t.Fatalf("two streams: status %d and %d, want 200 and 200", first.status, second.status)
	}
	third := w.openStream(t, w.admin, "", "")
	third.refused(t, "third stream", http.StatusServiceUnavailable, "unavailable")
	if got := third.header.Get("Retry-After"); got != "5" {
		t.Errorf("third stream: Retry-After = %q, want 5", got)
	}

	first.close()
	w.waitOpenStreams(t, 1)
	if again := w.openStream(t, w.admin, "", ""); again.status != http.StatusOK {
		t.Errorf("after a stream ended: status %d, want 200", again.status)
	}
}

func TestStream_ClosesWhenCredentialEnds(t *testing.T) {
	t.Parallel()
	// tokenTx runs change in a committed transaction.
	tokenTx := func(w *world, change func(ctx context.Context, tx *sql.Tx) error) {
		if err := w.store.Tx(context.Background(), func(tx *sql.Tx) error { return change(context.Background(), tx) }); err != nil {
			w.t.Fatalf("change the PAT: %v", err)
		}
	}
	for _, tc := range []struct {
		name  string
		apply func(w *world, tokenID string)
	}{
		{"the PAT is revoked", func(w *world, id string) {
			tokenTx(w, func(ctx context.Context, tx *sql.Tx) error {
				return identity.RevokeTokenByID(ctx, tx, id, w.clock.Now())
			})
		}},
		{"the PAT expires", func(w *world, id string) {
			tokenTx(w, func(ctx context.Context, tx *sql.Tx) error {
				return identity.SetTokenExpiryByID(ctx, tx, id, w.clock.Now().Add(10*time.Second))
			})
		}},
		{"the PAT's user is blocked", func(w *world, _ string) {
			w.adminCall(http.MethodPost, "/users/"+w.users["dev"]+"/block", nil, http.StatusOK)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			token, id := w.pat("dev", "", "audit:read")
			s := w.openStream(t, token, "", "")
			if s.status != http.StatusOK {
				t.Fatalf("status = %d, want 200", s.status)
			}
			s.heartbeat(2 * time.Second)
			w.clock.WaitTimers(t, 1)

			tc.apply(w, id)
			w.emit(on("parley.change.open", "api"))
			if f := s.next(2 * time.Second); f.event != "hub_event" {
				t.Fatalf("frame %q, want the event emitted before the check", f.lines)
			}
			w.clock.WaitTimers(t, 1)
			w.clock.Advance(30 * time.Second)
			s.ends(2 * time.Second)
			w.waitOpenStreams(t, 0)
		})
	}
}

func TestStream_ShutdownClosesStreams(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	readers := []*eventStream{w.openStream(t, w.admin, "", ""), w.openStream(t, w.admin, "", "")}
	for _, s := range readers {
		s.heartbeat(2 * time.Second)
	}
	// The third client never reads: its handler is blocked in a write.
	sw, done := w.stuckStream(t, 1)
	w.clock.WaitTimers(t, 3)
	w.emitMany(1)
	happens(t, sw.blocked, 5*time.Second, "a blocked write")
	for _, s := range readers {
		if f := s.next(2 * time.Second); f.event != "hub_event" {
			t.Fatalf("frame %q, want the hub_event", f.lines)
		}
	}

	w.shutDown()
	happens(t, done, time.Second, "the end of the stream blocked in a write")
	for _, s := range readers {
		s.ends(time.Second)
	}
	w.waitOpenStreams(t, 0)
}

func TestStream_AccessChangeWhileOpen(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	mem, out := w.openStream(t, w.key("mem"), "", ""), w.openStream(t, w.key("out"), "", "")
	mem.heartbeat(2 * time.Second)
	out.heartbeat(2 * time.Second)
	w.clock.WaitTimers(t, 2)

	e1 := w.emit(on("parley.change.open", "api"))
	if f := mem.next(2 * time.Second); f.event != "hub_event" || f.seq(t) != e1.Seq {
		t.Fatalf("mem: frame %q, want the hub_event %d", f.lines, e1.Seq)
	}
	out.quiet(200 * time.Millisecond)

	w.removeMember(w.acme, "mem")
	w.addMember(w.acme, "out")
	e2 := w.emit(on("parley.change.open", "api"))
	if f := out.next(2 * time.Second); f.event != "hub_event" || f.seq(t) != e2.Seq {
		t.Fatalf("out after joining acme: frame %q, want the hub_event %d", f.lines, e2.Seq)
	}
	mem.quiet(200 * time.Millisecond)
	w.clock.Advance(heartbeatInterval)
	if hb := mem.heartbeat(2 * time.Second); hb.seq(t) < e2.Seq {
		t.Errorf("mem after leaving acme: heartbeat id %s, want at least %d", hb.id, e2.Seq)
	}
}
