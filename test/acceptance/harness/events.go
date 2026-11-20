package harness

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Event is one hub_event frame of an event stream.
type Event struct {
	Seq int64
	// Type is the record's type; Record is the frame's data decoded and
	// Data the data as received.
	Type   string
	Record map[string]any
	Data   string
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
	// No client timeout: the stream lasts until it is closed.
	res, err := http.DefaultClient.Do(req)
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
				e := Event{Seq: id, Data: data, At: time.Now()}
				_ = json.Unmarshal([]byte(data), &e.Record)
				e.Type = str(e.Record["type"])
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
