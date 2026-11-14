package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// eventRecord is the hub event seq as the hub sends it in a frame's data: a
// record on one line, with a < the CLI must not escape.
func eventRecord(seq int) string {
	return fmt.Sprintf(`{"seq":%d,"id":"e-%d","type":"parley.change.open","time":"2026-11-14T09:30:20Z","workspace":"api","actor_type":"pat","actor_id":"u_1","resource_type":"change","resource_id":"42","action":"open","session_id":null,"metadata":{"title":"a <b>"}}`, seq, seq)
}

// eventFrame is the hub_event frame of the event seq.
func eventFrame(seq int) string {
	return fmt.Sprintf("id: %d\nevent: hub_event\ndata: %s\n\n", seq, eventRecord(seq))
}

// heartbeatFrame is a heartbeat at the position seq.
func heartbeatFrame(seq int) string {
	return fmt.Sprintf("id: %d\nevent: heartbeat\ndata: {\"time\":\"2026-11-14T09:30:15Z\",\"seq\":%d}\n\n", seq, seq)
}

// eventLines is what parley events tail prints for the events seqs: each
// record on a line of its own.
func eventLines(seqs ...int) string {
	var b strings.Builder
	for _, seq := range seqs {
		b.WriteString(eventRecord(seq) + "\n")
	}
	return b.String()
}

// sendFrames writes frames to an event stream, flushing each, and begins
// the stream's 200 answer when it has not begun yet.
func sendFrames(w http.ResponseWriter, frames ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, f := range frames {
		io.WriteString(w, f)
		http.NewResponseController(w).Flush()
	}
}

// streamThenClose answers a stream of frames and then ends it.
func streamThenClose(frames ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { sendFrames(w, frames...) }
}

// stopStream answers a stream of frames, then ends the run as an interrupt
// does and holds the stream until the client leaves.
func stopStream(stop context.CancelFunc, frames ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sendFrames(w, frames...)
		stop()
		<-r.Context().Done()
	}
}

// hubRefusals are the envelopes of the hub's permanent refusals of an event
// route, by status.
var hubRefusals = map[int]string{
	400: `{"error":{"code":400,"message":"wait must be a whole number of seconds from 0 to 60","error_type":"invalid_request"}}`,
	401: `{"error":{"code":401,"message":"authentication required","error_type":"unauthenticated"}}`,
	403: `{"error":{"code":403,"message":"this route needs the scope audit:read","error_type":"insufficient_scope"}}`,
	404: `{"error":{"code":404,"message":"workspace not found","error_type":"not_found"}}`,
	410: `{"error":{"code":410,"message":"event cursor expired","error_type":"cursor_expired","oldest_seq":31007}}`,
}

// rateLimited answers 429, as a hub that limits the caller's rate does.
var rateLimited = answerWith(http.StatusTooManyRequests, `{"error":{"code":429,"message":"rate limited","error_type":"rate_limited"}}`)

// unavailable answers 503 with Retry-After, as a hub with every stream
// place taken does.
func unavailable(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Retry-After", "5")
	answerWith(http.StatusServiceUnavailable, `{"error":{"code":503,"message":"too many event streams are open","error_type":"unavailable"}}`)(w, nil)
}

// eventsHub is a stub hub whose n-th GET /api/v1/events is answered by
// script[n-1]. A request past the script ends the run as an interrupt does,
// so a command that sends one more request than the test expects ends
// there, and the test sees the extra request.
func eventsHub(t *testing.T, stop context.CancelFunc, script ...http.HandlerFunc) *stubHub {
	t.Helper()
	var mu sync.Mutex
	n := 0
	return newStubHub(t).handle(http.MethodGet, "/api/v1/events", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		i := n - 1
		mu.Unlock()
		if i < len(script) {
			script[i](w, r)
			return
		}
		stopStream(stop)(w, r)
	})
}

// fakeWaits is the reconnect clock of a test: it records each wait and
// returns at once.
type fakeWaits struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (f *fakeWaits) wait(ctx context.Context, d time.Duration) error {
	f.mu.Lock()
	f.waits = append(f.waits, d)
	f.mu.Unlock()
	return ctx.Err()
}

func (f *fakeWaits) got() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.waits)
}

// stopAfterLines is a stdout that ends the run, as an interrupt does, once
// n lines were written to it.
type stopAfterLines struct {
	syncBuffer
	n    int
	stop context.CancelFunc
}

func (w *stopAfterLines) Write(p []byte) (int, error) {
	n, err := w.syncBuffer.Write(p)
	if strings.Count(w.String(), "\n") >= w.n {
		w.stop()
	}
	return n, err
}

// runTail runs parley events tail with args against hub under ctx, with
// opts and waits as its reconnect clock, writing stdout to stdout, and
// returns the exit status and stderr.
func runTail(t *testing.T, ctx context.Context, opts options, hub *stubHub, waits *fakeWaits, stdout io.Writer, args ...string) (int, string) {
	t.Helper()
	opts.reconnectWait = waits.wait
	var stderr syncBuffer
	code := run(ctx, onHub(hub.URL, append([]string{"events", "tail"}, args...)...), stdout, &stderr, opts)
	return code, stderr.String()
}

func TestEventsTail_PrintsOneLinePerEvent(t *testing.T) {
	t.Parallel()
	const session = "6f1c2a7e-3b4d-4c5e-8f9a-0b1c2d3e4f5a"
	tests := []struct {
		name string
		args []string
		want url.Values
	}{
		{"every filter and a position", []string{"--workspace", "api", "--types", "parley.change.*", "--session", session, "--after", "4"},
			url.Values{"workspace": {"api"}, "types": {"parley.change.*"}, "session_id": {session}, "after": {"4"}}},
		{"no position", []string{"--workspace", "api"}, url.Values{"workspace": {"api"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			opts := defaultOptions()
			// A tail bound by the request timeout would give up while the
			// hub is quiet, long before the second event.
			opts.requestTimeout = 20 * time.Millisecond
			hub := eventsHub(t, stop, func(w http.ResponseWriter, r *http.Request) {
				sendFrames(w, heartbeatFrame(4), eventFrame(5), heartbeatFrame(5))
				time.Sleep(10 * opts.requestTimeout)
				sendFrames(w, eventFrame(6))
				<-r.Context().Done()
			})
			stdout := &stopAfterLines{n: 2, stop: stop}
			waits := &fakeWaits{}

			code, stderr := runTail(t, ctx, opts, hub, waits, stdout, tt.args...)

			if code != 0 || stderr != "" {
				t.Errorf("exit status %d, stderr %q, want 0 and nothing", code, stderr)
			}
			if got, want := stdout.String(), eventLines(5, 6); got != want {
				t.Errorf("stdout = %q, want %q", got, want)
			}
			sent := hub.recorded()
			if len(sent) != 1 {
				t.Fatalf("the hub got %d requests, want 1 (reconnect waits %v)", len(sent), waits.got())
			}
			if got, _ := url.ParseQuery(sent[0].query); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("query = %q, want %q", sent[0].query, tt.want.Encode())
			}
			if accept := sent[0].header.Get("Accept"); accept != "text/event-stream" {
				t.Errorf("Accept = %q, want text/event-stream", accept)
			}
			if auth := sent[0].header.Get("Authorization"); auth != "Bearer k" {
				t.Errorf("Authorization = %q, want Bearer k", auth)
			}
		})
	}
}

func TestEventsTail_ReconnectsAndResumes(t *testing.T) {
	t.Parallel()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	hub := eventsHub(t, stop,
		streamThenClose(eventFrame(1), eventFrame(2), heartbeatFrame(7)),
		unavailable,
		closeWithoutAnswer,
		rateLimited,
		unavailable,
		closeWithoutAnswer,
		rateLimited,
		answerWith(http.StatusBadGateway, "bad gateway"),
		streamThenClose(eventFrame(8), eventFrame(9)),
		streamThenClose(eventFrame(9), eventFrame(10)),
	)
	var stdout syncBuffer
	waits := &fakeWaits{}

	code, stderr := runTail(t, ctx, defaultOptions(), hub, waits, &stdout)

	if code != 0 {
		t.Errorf("exit status = %d, want 0 (stderr %q)", code, stderr)
	}
	if got, want := stdout.String(), eventLines(1, 2, 8, 9, 10); got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	var afters []string
	for _, r := range hub.recorded() {
		q, _ := url.ParseQuery(r.query)
		afters = append(afters, q.Get("after"))
	}
	// The last request is the one the test ends the run with.
	wantAfters := []string{"", "7", "7", "7", "7", "7", "7", "7", "7", "9", "10"}
	if !slices.Equal(afters, wantAfters) {
		t.Errorf("after of each request = %q, want %q", afters, wantAfters)
	}
	s := time.Second
	wantWaits := []time.Duration{1 * s, 2 * s, 4 * s, 8 * s, 16 * s, 30 * s, 30 * s, 30 * s, 1 * s, 1 * s}
	if got := waits.got(); !slices.Equal(got, wantWaits) {
		t.Errorf("reconnect waits = %v, want %v", got, wantWaits)
	}
	lines := nonEmptyLines(stderr)
	if len(lines) != len(wantWaits) {
		t.Errorf("stderr has %d lines, want one per reconnect, %d: %q", len(lines), len(wantWaits), stderr)
	}
	for i, line := range lines {
		if want := fmt.Sprintf("reconnecting in %v: ", wantWaits[min(i, len(wantWaits)-1)]); !strings.HasPrefix(line, want) {
			t.Errorf("stderr line %d = %q, want it to begin with %q", i+1, line, want)
		}
	}
}

func TestEventsTail_StopsOnPermanentErrors(t *testing.T) {
	t.Parallel()
	for status, body := range hubRefusals {
		for _, reconnect := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d reconnect=%v", status, reconnect), func(t *testing.T) {
				t.Parallel()
				ctx, stop := context.WithCancel(context.Background())
				defer stop()
				script := []http.HandlerFunc{answerWith(status, body)}
				wantStdout := ""
				if reconnect {
					script = append([]http.HandlerFunc{streamThenClose(eventFrame(1))}, script...)
					wantStdout = eventLines(1)
				}
				hub := eventsHub(t, stop, script...)
				var stdout syncBuffer

				code, stderr := runTail(t, ctx, defaultOptions(), hub, &fakeWaits{}, &stdout)

				if code != 1 {
					t.Errorf("exit status = %d, want 1", code)
				}
				// Reconnect progress, if any, comes before the envelope.
				envelope := indented(t, body)
				progressLines, found := strings.CutSuffix(stderr, envelope)
				if !found {
					t.Errorf("stderr = %q, want it to end with the hub's envelope as received, %q", stderr, envelope)
				}
				for _, line := range nonEmptyLines(progressLines) {
					if !strings.HasPrefix(line, "reconnecting in ") {
						t.Errorf("stderr holds %q before the envelope, want only reconnect progress", line)
					}
				}
				if got := stdout.String(); got != wantStdout {
					t.Errorf("stdout = %q, want %q", got, wantStdout)
				}
				if n := len(hub.recorded()); n != len(script) {
					t.Errorf("the hub got %d requests, want %d", n, len(script))
				}
			})
		}
	}
}

func TestEvents_RejectsBadAfter(t *testing.T) {
	t.Parallel()
	for _, command := range []string{"tail", "list"} {
		for _, after := range []string{"-1", "x", "1.5"} {
			t.Run(command+" "+after, func(t *testing.T) {
				t.Parallel()
				hub := newStubHub(t)

				code, stdout, stderr := runCLIWith(t, defaultOptions(), onHub(hub.URL, "events", command, "--after", after)...)

				message := wantClientFailure(t, code, stdout, stderr, "usage_error")
				if !strings.Contains(message, `"--after"`) {
					t.Errorf("message = %q, want it to name --after", message)
				}
				if n := len(hub.recorded()); n != 0 {
					t.Errorf("the hub got %d requests, want none", n)
				}
			})
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// runList runs parley events list with args against hub and returns the exit
// status, stdout, stderr and the time each request had before its deadline,
// 0 for none.
func runList(t *testing.T, hub *stubHub, args ...string) (int, string, string, []time.Duration) {
	t.Helper()
	opts := defaultOptions()
	// The run sends its requests from the goroutine that calls it.
	var left []time.Duration
	opts.transport = func(next http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			var d time.Duration
			if at, ok := r.Context().Deadline(); ok {
				d = time.Until(at)
			}
			left = append(left, d)
			return next.RoundTrip(r)
		})
	}
	code, stdout, stderr := runCLIWith(t, opts, onHub(hub.URL, append([]string{"events", "list"}, args...)...)...)
	return code, stdout, stderr, left
}

func TestEventsList_CallsLogOnceAndPrintsTheAnswer(t *testing.T) {
	t.Parallel()
	const session = "6f1c2a7e-3b4d-4c5e-8f9a-0b1c2d3e4f5a"
	page := `{"items":[` + eventRecord(5) + `],"next_cursor":null,"next_after":5,"oldest_seq":1,"last_seq":5}`
	tests := []struct {
		name    string
		args    []string
		want    url.Values
		timeout time.Duration
		status  int
		body    string
	}{
		{"every flag", []string{"--workspace", "api", "--types", "parley.change.*", "--session", session, "--after", "4", "--limit", "10", "--wait", "45"},
			url.Values{"workspace": {"api"}, "types": {"parley.change.*"}, "session_id": {session}, "after": {"4"}, "limit": {"10"}, "wait": {"45"}}, 75 * time.Second, 200, page},
		{"no flag", nil, url.Values{}, 30 * time.Second, 200, page},
		{"limit and wait the hub judges", []string{"--limit", "0", "--wait", "61"}, url.Values{"limit": {"0"}, "wait": {"61"}}, 91 * time.Second, 400, hubRefusals[400]},
		{"a workspace the caller may not read", []string{"--workspace", "secret"}, url.Values{"workspace": {"secret"}}, 30 * time.Second, 404, hubRefusals[404]},
		{"an expired position", []string{"--after", "3"}, url.Values{"after": {"3"}}, 30 * time.Second, 410, hubRefusals[410]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t).answer(http.MethodGet, "/api/v1/events/log", tt.status, tt.body)

			code, stdout, stderr, left := runList(t, hub, tt.args...)

			// The hub's answer goes to stdout, or its refusal to stderr, as
			// received.
			wantCode, wantStdout, wantStderr := 0, indented(t, tt.body), ""
			if tt.status != http.StatusOK {
				wantCode, wantStdout, wantStderr = 1, "", wantStdout
			}
			if code != wantCode || stdout != wantStdout || stderr != wantStderr {
				t.Errorf("exit status %d, stdout %q, stderr %q, want %d, %q and %q", code, stdout, stderr, wantCode, wantStdout, wantStderr)
			}
			sent := hub.recorded()
			if len(sent) != 1 {
				t.Fatalf("the hub got %d requests, want 1", len(sent))
			}
			if got, _ := url.ParseQuery(sent[0].query); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("query = %q, want %q", sent[0].query, tt.want.Encode())
			}
			if auth := sent[0].header.Get("Authorization"); auth != "Bearer k" {
				t.Errorf("Authorization = %q, want Bearer k", auth)
			}
			if len(left) != 1 || left[0].Round(time.Second) != tt.timeout {
				t.Errorf("request timeouts = %v, want one of %v", left, tt.timeout)
			}
		})
	}
}
