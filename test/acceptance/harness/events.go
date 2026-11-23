package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Event is one hub_event frame of an event stream.
type Event struct {
	Seq int64
	// Type is the record's type; Record is the frame's data decoded.
	Type   string
	Record map[string]any
	// At is when the frame arrived.
	At time.Time
}

// Stream is an open GET /api/v1/events connection that records every
// hub_event frame, skipping heartbeats.
type Stream struct {
	t      testing.TB
	res    *http.Response
	mu     sync.Mutex
	events []Event
	ended  bool
	taken  int
	notify chan struct{}
}

// streamClient opens event streams. It has no overall timeout, since a
// stream lasts until it is closed, but a hub that does not begin to answer
// fails the scenario rather than hanging it.
var streamClient = &http.Client{Transport: &http.Transport{
	Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 30 * time.Second,
}}

// Events opens the event stream with credential, the query (such as
// "types=parley.claim.*&after=0", "" for none) and the header given as
// name, value pairs, such as Last-Event-ID, and fails the test unless it
// is answered 200 with an event stream. It is closed when the test ends.
func (h *Hub) Events(credential, query string, header ...string) *Stream {
	h.t.Helper()
	path := "/api/v1/events"
	if query != "" {
		path += "?" + query
	}
	req, err := http.NewRequest(http.MethodGet, h.URL(path), nil)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := streamClient.Do(req)
	if err != nil {
		h.t.Fatalf("GET %s: %v", path, err)
	}
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		body, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		h.t.Fatalf("GET %s = %d %s, want 200 with an event stream", path, res.StatusCode, body)
	}
	s := &Stream{t: h.t, res: res, notify: make(chan struct{}, 1)}
	go s.read()
	h.t.Cleanup(s.Close)
	return s
}

func (s *Stream) read() {
	sc := bufio.NewScanner(s.res.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	var id int64
	var kind, data string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if kind == "hub_event" {
				e := Event{Seq: id, At: time.Now()}
				_ = json.Unmarshal([]byte(data), &e.Record)
				e.Type = Str(e.Record["type"])
				s.mu.Lock()
				s.events = append(s.events, e)
				s.mu.Unlock()
				s.wake()
			}
			id, kind, data = 0, "", ""
		case strings.HasPrefix(line, "id: "):
			id, _ = strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
		case strings.HasPrefix(line, "event: "):
			kind = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		}
	}
	s.mu.Lock()
	s.ended = true
	s.mu.Unlock()
	s.wake()
}

func (s *Stream) wake() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// Next returns the next n events after those Next returned before, and
// fails the test when they have not all arrived within timeout.
func (s *Stream) Next(n int, timeout time.Duration) []Event {
	s.t.Helper()
	deadline := time.After(timeout)
	for {
		s.mu.Lock()
		if len(s.events) >= s.taken+n {
			got := append([]Event(nil), s.events[s.taken:s.taken+n]...)
			s.taken += n
			s.mu.Unlock()
			return got
		}
		have, ended := len(s.events)-s.taken, s.ended
		s.mu.Unlock()
		if ended {
			s.t.Fatalf("the event stream ended after %d of %d events", have, n)
		}
		select {
		case <-s.notify:
		case <-deadline:
			s.t.Fatalf("%d of %d events arrived within %v", have, n, timeout)
		}
	}
}

// All is every event received so far.
func (s *Stream) All() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.events...)
}

// MaxSeq is the highest seq received, 0 before any event.
func (s *Stream) MaxSeq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var top int64
	for _, e := range s.events {
		top = max(top, e.Seq)
	}
	return top
}

// Close drops the connection, as a consumer that goes away does.
func (s *Stream) Close() { _ = s.res.Body.Close() }

// EventsRefused opens the event stream as Events does and returns the
// answer when the hub refuses it; a stream answered 200 is closed at once
// and returned with that status and no body.
func (h *Hub) EventsRefused(credential, query string, header ...string) Response {
	path := "/api/v1/events"
	if query != "" {
		path += "?" + query
	}
	req, err := http.NewRequest(http.MethodGet, h.URL(path), nil)
	if err != nil {
		return Response{Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := streamClient.Do(req)
	if err != nil {
		return Response{Err: err}
	}
	defer res.Body.Close()
	r := Response{Status: res.StatusCode, Header: res.Header}
	if res.StatusCode == http.StatusOK {
		return r
	}
	r.Raw, r.Err = io.ReadAll(res.Body)
	_ = json.Unmarshal(r.Raw, &r.Body)
	return r
}

// Tail runs parley events tail with credential and args, such as --after
// 12, until it has printed n lines or timeout has passed, and returns the
// lines it printed by then: the command itself never ends.
func (h *Hub) Tail(credential string, n int, timeout time.Duration, args ...string) []string {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, built.parley, append([]string{"events", "tail"}, args...)...)
	// The environment CLI gives the parley it runs.
	cmd.Dir = h.cliHome
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + h.cliHome, "PARLEY_CONFIG_DIR=" + h.cliConfig,
		"PARLEY_ENDPOINT_URL=" + h.URL(""), "PARLEY_API_KEY=" + credential,
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.StdoutPipe()
	if err != nil {
		h.t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		h.t.Fatalf("start parley events tail: %v", err)
	}
	var lines []string
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for len(lines) < n && sc.Scan() {
		lines = append(lines, sc.Text())
	}
	cancel()
	_ = cmd.Wait()
	if len(lines) < n {
		h.t.Logf("parley events tail printed %d of %d lines within %v; its standard error:\n%s", len(lines), n, timeout, stderr.String())
	}
	return lines
}
