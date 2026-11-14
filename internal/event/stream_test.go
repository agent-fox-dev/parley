package event

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// frame is one frame of an event stream: its lines, without the empty line
// that ends it, and the values of its id, event and data fields.
type frame struct {
	lines []string
	id    string
	event string
	data  string
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
	t      testing.TB
	status int
	header http.Header
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

// openStream sends GET /api/v1/events?query with credential and the given
// headers on a real connection, and returns once the answer's headers have
// arrived. Its frames are read as they come.
func (w *world) openStream(t testing.TB, credential, query string, header map[string]string) *eventStream {
	t.Helper()
	srv := w.serve()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/events?"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	for name, value := range header {
		req.Header.Set(name, value)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		cancel()
		t.Fatalf("GET /events?%s: %v", query, err)
	}
	frames := make(chan frame, 1024)
	go func() {
		defer close(frames)
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		var f frame
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				frames <- f
				f = frame{}
				continue
			}
			f.lines = append(f.lines, line)
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
	}()
	s := &eventStream{t: t, status: res.StatusCode, header: res.Header, frames: frames}
	s.close = func() {
		cancel()
		res.Body.Close()
	}
	t.Cleanup(s.close)
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

// quiet fails the test when a frame has arrived or arrives within d of real
// time.
func (s *eventStream) quiet(d time.Duration) {
	s.t.Helper()
	select {
	case f, ok := <-s.frames:
		if ok {
			s.t.Fatalf("frame %q, want none yet", f.lines)
		}
		s.t.Fatal("the stream ended, want it open")
	default:
	}
	select {
	case f, ok := <-s.frames:
		if ok {
			s.t.Fatalf("frame %q, want none yet", f.lines)
		}
		s.t.Fatal("the stream ended, want it open")
	case <-time.After(d):
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
	for {
		got := w.events.Counters().OpenStreams
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("open_streams = %d after 2 s, want %d", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStream_OpensWithHeartbeat(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for range 3 {
		w.emit(on("parley.change.open", "api"))
	}

	s := w.openStream(t, w.admin, "", nil)
	if s.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", s.status)
	}
	if got := s.header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := s.header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
	f := s.next(2 * time.Second)
	if len(f.lines) != 3 || f.lines[0] != "id: 3" || f.lines[1] != "event: heartbeat" || !strings.HasPrefix(f.lines[2], "data: ") {
		t.Fatalf("first frame = %q, want id: 3, event: heartbeat and one data line", f.lines)
	}
	var hb heartbeatData
	if err := json.Unmarshal([]byte(f.data), &hb); err != nil {
		t.Fatalf("heartbeat data %q: %v", f.data, err)
	}
	if want := (heartbeatData{Time: "2026-11-14T09:30:15Z", Seq: 3}); hb != want {
		t.Errorf("heartbeat data = %+v, want %+v", hb, want)
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
	for range 300 {
		w.emit(on("parley.change.open", "api"))
	}

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
	s := w.openStream(t, w.admin, "after=100", nil)
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
		seq := f.seq(t)
		got = append(got, seq)
		data[seq] = f.data
	}
	if err := <-written; err != nil {
		t.Fatalf("the writer: %v", err)
	}
	if want := seqRange(101, 500); !slices.Equal(got, want) {
		t.Fatalf("hub_event ids = %v, want 101..500 once each in order", got)
	}
	for _, rec := range w.readAll(100) {
		raw, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if data[rec.Seq] != string(raw) {
			t.Errorf("frame %d data = %s, want the stored record %s", rec.Seq, data[rec.Seq], raw)
		}
	}
}

func TestStream_LiveOnlyWithoutPosition(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for range 3 {
		w.emit(on("parley.change.open", "api"))
	}

	s := w.openStream(t, w.admin, "", nil)
	if hb := s.heartbeat(2 * time.Second); hb.id != "3" {
		t.Fatalf("first heartbeat id = %s, want 3", hb.id)
	}
	committed := time.Now()
	w.emit(on("parley.change.open", "api"))
	f := s.next(2 * time.Second)
	if elapsed := time.Since(committed); elapsed > time.Second {
		t.Errorf("the event arrived %v after its commit, want within 1 s", elapsed)
	}
	if f.event != "hub_event" || f.id != "4" {
		t.Errorf("frame after the commit = %q, want the hub_event 4", f.lines)
	}
}

func TestStream_HeartbeatAdvancesPosition(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	start := w.clock.Now()

	quiet := w.openStream(t, w.admin, "types=parley.nothing.*", nil)
	all := w.openStream(t, w.admin, "types=parley.change.*", nil)
	for name, s := range map[string]*eventStream{"filtered": quiet, "matching": all} {
		if hb := s.heartbeat(2 * time.Second); hb.id != "0" {
			t.Fatalf("%s stream: first heartbeat id = %s, want 0", name, hb.id)
		}
	}
	w.clock.WaitTimers(t, 2)

	for range 10 {
		w.emit(on("parley.change.open", "api"))
	}
	// Every matching event up to a heartbeat's seq comes before it.
	for want := int64(1); want <= 10; want++ {
		if f := all.next(2 * time.Second); f.event != "hub_event" || f.seq(t) != want {
			t.Fatalf("matching stream: frame %q, want the hub_event %d", f.lines, want)
		}
	}
	w.clock.Advance(29 * time.Second)
	quiet.quiet(200 * time.Millisecond)
	all.quiet(time.Millisecond)
	w.clock.Advance(time.Second)
	for name, s := range map[string]*eventStream{"filtered": quiet, "matching": all} {
		hb := s.heartbeat(2 * time.Second)
		var data heartbeatData
		if err := json.Unmarshal([]byte(hb.data), &data); err != nil || hb.id != "10" || data.Seq != 10 ||
			data.Time != "2026-11-14T09:30:45Z" {
			t.Errorf("%s stream: heartbeat %q, want id 10 with seq 10 at 30 s", name, hb.lines)
		}
	}

	w.clock.WaitTimers(t, 2)
	w.clock.Set(start.Add(3 * time.Hour))
	for _, s := range []*eventStream{quiet, all} {
		if hb := s.heartbeat(2 * time.Second); hb.id != "10" {
			t.Fatalf("heartbeat after the jump: id %s, want 10", hb.id)
		}
	}
	w.clock.WaitTimers(t, 2)
	for range 5 {
		w.emit(on("parley.change.open", "api"))
	}
	for want := int64(11); want <= 15; want++ {
		if f := all.next(2 * time.Second); f.event != "hub_event" || f.seq(t) != want {
			t.Fatalf("matching stream: frame %q, want the hub_event %d", f.lines, want)
		}
	}
	w.clock.Advance(30 * time.Second)
	if hb := quiet.heartbeat(2 * time.Second); hb.id != "15" {
		t.Fatalf("filtered stream: heartbeat id %s, want 15", hb.id)
	}

	if oldest, err := w.events.Expire(context.Background(), start.Add(time.Hour)); err != nil || oldest != 11 {
		t.Fatalf("Expire = %d, %v, want 11", oldest, err)
	}
	again := w.openStream(t, w.admin, "types=parley.nothing.*", map[string]string{"Last-Event-ID": "15"})
	if again.status != http.StatusOK {
		t.Fatalf("reconnect with Last-Event-ID: 15: status = %d, want 200", again.status)
	}
	if hb := again.heartbeat(2 * time.Second); hb.id != "15" {
		t.Errorf("reconnect: first heartbeat id = %s, want 15", hb.id)
	}
}
