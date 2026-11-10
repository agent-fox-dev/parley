package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// jobHub stands in for the hub of the wait checks: POST /api/v1/jobs
// answers {id, state} with the start state, and GET /api/v1/jobs/j1 answers
// the polled states in order, repeating the last. poll, when set, answers a
// poll (numbered from 1) in place of the next state.
type jobHub struct {
	*httptest.Server
	start  string
	states []string
	poll   func(n int, w http.ResponseWriter, r *http.Request) bool

	mu     sync.Mutex
	log    []string
	served []string
	at     []time.Time
}

func newJobHub(t *testing.T, start string, states ...string) *jobHub {
	t.Helper()
	h := &jobHub{start: start, states: states}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.log = append(h.log, r.Method+" "+r.URL.Path)
		polls := 0
		for _, l := range h.log {
			if strings.HasPrefix(l, "GET ") {
				polls++
			}
		}
		if r.Method == http.MethodGet {
			h.at = append(h.at, time.Now())
		}
		h.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/jobs":
			h.serve(w, h.start)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/jobs/j1":
			if h.poll != nil && h.poll(polls, w, r) {
				return
			}
			state := h.start
			if len(h.states) > 0 {
				state = h.states[min(polls, len(h.states))-1]
			}
			h.serve(w, state)
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *jobHub) serve(w http.ResponseWriter, state string) {
	body := fmt.Sprintf(`{"id": "j1", "state": %q, "attempt": %d}`, state, time.Now().UnixNano())
	h.mu.Lock()
	h.served = append(h.served, body)
	h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func (h *jobHub) requests() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.log...)
}

func (h *jobHub) count(method string) int {
	n := 0
	for _, l := range h.requests() {
		if strings.HasPrefix(l, method+" ") {
			n++
		}
	}
	return n
}

func (h *jobHub) lastServed() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.served[len(h.served)-1]
}

// jobState classifies a job record: done succeeds, dead and cancelled fail,
// every other state (queued, running, failed: waiting to retry) is not
// terminal.
func jobState(record []byte) (Outcome, string) {
	var r struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(record, &r)
	switch r.State {
	case "done":
		return Succeeded, r.State
	case "dead", "cancelled":
		return Unsuccessful, r.State
	}
	return NotTerminal, r.State
}

// probeRun is probe run: POST /jobs, polled at GET /jobs/:id.
func probeRun() *cobra.Command {
	return Group("probe", "test commands", Declare(Spec{
		Use: "run", Description: "runs a job", Method: "POST", Path: "/jobs", Auth: AuthUser,
		Wait: &Wait{
			Poll: func(record []byte) (string, error) {
				var r struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(record, &r); err != nil || r.ID == "" {
					return "", ClientError("the record has no id")
				}
				return "/jobs/" + r.ID, nil
			},
			Classify: jobState,
		},
	}))
}

// waitEnv points the settings at the hub and sets the poll interval hook.
func waitEnv(t *testing.T, hub *jobHub, interval time.Duration) {
	t.Helper()
	t.Setenv("PARLEY_CONFIG_DIR", filepath.Join(t.TempDir(), "cfg"))
	t.Setenv("PARLEY_ENDPOINT_URL", hub.URL)
	t.Setenv("PARLEY_API_KEY", "parley_ABCDabcd_"+strings.Repeat("Q", 32))
	if interval > 0 {
		old := pollIntervalHook
		pollIntervalHook = interval
		t.Cleanup(func() { pollIntervalHook = old })
	}
}

func runJob(t *testing.T, args ...string) cliResult {
	t.Helper()
	return runProbe(t, []*cobra.Command{probeRun()}, append([]string{"probe", "run"}, args...)...)
}

func TestWait_FlagsAndTheirDurations(t *testing.T) {
	hub := newJobHub(t, "queued")
	waitEnv(t, hub, 0)
	d := helpDoc(t, runProbe(t, []*cobra.Command{probeRun()}, "help", "probe", "run", "--json"))
	if w := flagOf(t, d, "wait"); w["type"] != "bool" || w["default"] != "false" {
		t.Errorf("wait = %v", w)
	}
	if f := flagOf(t, d, "timeout"); f["default"] != "5m" || f["type"] != "duration" {
		t.Errorf("timeout = %v", f)
	}
	if f := flagOf(t, d, "poll-interval"); f["default"] != "5s" || f["type"] != "duration" {
		t.Errorf("poll-interval = %v", f)
	}
	for _, flag := range []string{"--timeout", "--poll-interval"} {
		for _, v := range []string{"90s", "5m", "2h", "1d"} {
			if r := runJob(t, flag, v); r.code != 0 {
				t.Errorf("%s %s: exit %d, %s", flag, v, r.code, r.stderr)
			}
		}
	}
	for text, want := range map[string]time.Duration{"1d": 24 * time.Hour, "90s": 90 * time.Second, "5m": 5 * time.Minute, "2h": 2 * time.Hour} {
		if got, err := parseDuration(text); err != nil || got != want {
			t.Errorf("parseDuration(%q) = %v, %v", text, got, err)
		}
	}
}

func TestWait_ARefusedDurationIsAUsageErrorBeforeAnyRequest(t *testing.T) {
	hub := newJobHub(t, "queued")
	waitEnv(t, hub, 0)
	for _, flag := range []string{"--timeout", "--poll-interval"} {
		for _, v := range []string{"0s", "0", "-5s", "5", "1.5m", "1m30s", "abc", "", "5 m", "5w"} {
			for _, wait := range []bool{false, true} {
				args := []string{flag, v}
				if wait {
					args = append(args, "--wait")
				}
				r := runJob(t, args...)
				if r.code != 2 || r.stdout != "" {
					t.Errorf("%v: exit %d, stdout %q", args, r.code, r.stdout)
					continue
				}
				e := r.envelope(t)
				if e["error_type"] != "usage_error" || !strings.Contains(e["message"].(string), strings.TrimPrefix(flag, "--")) {
					t.Errorf("%v: envelope %v", args, e)
				}
			}
		}
	}
	if n := len(hub.requests()); n != 0 {
		t.Errorf("hub received %d requests", n)
	}
}

func TestWait_ASuccessfulEndPrintsTheFinalRecord(t *testing.T) {
	cases := []struct {
		start  string
		states []string
		polls  int
	}{
		{"queued", []string{"running", "running", "done"}, 3},
		{"queued", []string{"failed", "running", "failed", "done"}, 4},
		{"done", nil, 0},
		{"running", []string{"done"}, 1},
	}
	for _, c := range cases {
		hub := newJobHub(t, c.start, c.states...)
		waitEnv(t, hub, 20*time.Millisecond)
		r := runJob(t, "--wait", "--timeout", "5s")
		if r.code != 0 {
			t.Errorf("%v: exit %d, %s", c, r.code, r.stderr)
			continue
		}
		dec := json.NewDecoder(strings.NewReader(r.stdout))
		var doc, extra map[string]any
		if err := dec.Decode(&doc); err != nil || dec.Decode(&extra) == nil {
			t.Errorf("%v: stdout is not one document: %q", c, r.stdout)
		}
		var last map[string]any
		_ = json.Unmarshal([]byte(hub.lastServed()), &last)
		if doc["state"] != "done" || !reflect.DeepEqual(doc, last) {
			t.Errorf("%v: printed %v, last served %v", c, doc, last)
		}
		if got := hub.count("GET"); got != c.polls {
			t.Errorf("%v: %d polls, want %d", c, got, c.polls)
		}
		seen := append([]string{c.start}, c.states...)
		changes := 0
		for i := 1; i < len(seen); i++ {
			if seen[i] != seen[i-1] {
				changes++
			}
		}
		lines := strings.Split(strings.TrimSuffix(r.stderr, "\n"), "\n")
		for _, l := range lines {
			if !strings.HasPrefix(l, "waiting: ") {
				t.Errorf("%v: stderr line %q", c, l)
			}
		}
		if len(lines) != changes+1 {
			t.Errorf("%v: %d progress lines, want %d: %q", c, len(lines), changes+1, r.stderr)
		}
		hub.mu.Lock()
		for i := 1; i < len(hub.at); i++ {
			if gap := hub.at[i].Sub(hub.at[i-1]); gap < 20*time.Millisecond {
				t.Errorf("%v: polls %v apart", c, gap)
			}
		}
		hub.mu.Unlock()
	}
}

func TestWait_AnUnsuccessfulEndIsWaitFailed(t *testing.T) {
	cases := []struct {
		start  string
		states []string
	}{
		{"queued", []string{"running", "dead"}},
		{"queued", []string{"cancelled"}},
		{"dead", nil},
	}
	for _, c := range cases {
		hub := newJobHub(t, c.start, c.states...)
		waitEnv(t, hub, 20*time.Millisecond)
		r := runJob(t, "--wait")
		final := c.start
		if len(c.states) > 0 {
			final = c.states[len(c.states)-1]
		}
		if r.code != 1 {
			t.Errorf("%v: exit %d, %s", c, r.code, r.stderr)
			continue
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil || doc["state"] != final {
			t.Errorf("%v: stdout %q", c, r.stdout)
		}
		e := r.lastEnvelope(t)
		if e["code"] != float64(0) || e["error_type"] != "wait_failed" || !strings.Contains(e["message"].(string), final) {
			t.Errorf("%v: envelope %v", c, e)
		}
		time.Sleep(60 * time.Millisecond)
		if got := hub.count("GET"); got != len(c.states) {
			t.Errorf("%v: %d polls, want %d", c, got, len(c.states))
		}
	}
}

func TestWait_PassingTheTimeoutIsWaitTimeout(t *testing.T) {
	cases := map[string]func(n int, w http.ResponseWriter, r *http.Request) bool{
		"running": nil,
		"failed":  nil,
		"stalls": func(n int, w http.ResponseWriter, r *http.Request) bool {
			if n < 2 {
				return false
			}
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			return true
		},
	}
	for name, poll := range cases {
		state := name
		if name == "stalls" {
			state = "running"
		}
		hub := newJobHub(t, "queued", state)
		hub.poll = poll
		waitEnv(t, hub, 50*time.Millisecond)
		begin := time.Now()
		r := runJob(t, "--wait", "--timeout", "1s")
		if took := time.Since(begin); took >= 2*time.Second {
			t.Errorf("%s: returned after %v", name, took)
		}
		if r.code != 1 || r.stdout != "" {
			t.Errorf("%s: exit %d, stdout %q", name, r.code, r.stdout)
			continue
		}
		if e := r.lastEnvelope(t); e["error_type"] != "wait_timeout" || e["code"] != float64(0) {
			t.Errorf("%s: envelope %v", name, e)
		}
		reqs := hub.requests()
		for _, l := range reqs[1:] {
			if !strings.HasPrefix(l, "GET ") {
				t.Errorf("%s: request %q after the start", name, l)
			}
		}
		time.Sleep(200 * time.Millisecond)
		if n := len(hub.requests()); n != len(reqs) {
			t.Errorf("%s: %d requests after the end", name, n-len(reqs))
		}
	}
}

func TestWait_TheHubsEnvelopeOnAPollEndsTheWait(t *testing.T) {
	envelopes := map[int]string{
		404: `{"error": {"code": 404, "message": "no job j1", "error_type": "not_found"}}`,
		429: `{"error": {"code": 429, "message": "slow down", "error_type": "rate_limited"}}`,
		500: `{"error": {"code": 500, "message": "boom", "error_type": "internal"}}`,
	}
	for status, body := range envelopes {
		hub := newJobHub(t, "queued", "running")
		hub.poll = func(n int, w http.ResponseWriter, _ *http.Request) bool {
			if n != 2 {
				return false
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return true
		}
		waitEnv(t, hub, 20*time.Millisecond)
		r := runJob(t, "--wait", "--timeout", "5s")
		if r.code != 1 || r.stdout != "" {
			t.Errorf("%d: exit %d, stdout %q", status, r.code, r.stdout)
			continue
		}
		var want struct {
			Error map[string]any `json:"error"`
		}
		_ = json.Unmarshal([]byte(body), &want)
		if got := r.lastEnvelope(t); !reflect.DeepEqual(got, want.Error) {
			t.Errorf("%d: envelope %v, want %v", status, got, want.Error)
		}
		time.Sleep(60 * time.Millisecond)
		if got := hub.count("GET"); got != 2 {
			t.Errorf("%d: %d polls", status, got)
		}
	}
}

func TestWait_AClientFailureOnAPollEndsTheWait(t *testing.T) {
	cases := map[string]func(h *jobHub) func(n int, w http.ResponseWriter, r *http.Request) bool{
		"closed": func(h *jobHub) func(int, http.ResponseWriter, *http.Request) bool {
			return func(n int, w http.ResponseWriter, _ *http.Request) bool {
				if n == 1 {
					h.serve(w, "running")
					go func() {
						time.Sleep(20 * time.Millisecond)
						_ = h.Listener.Close()
						h.CloseClientConnections()
					}()
					return true
				}
				return false
			}
		},
		"html": func(*jobHub) func(int, http.ResponseWriter, *http.Request) bool {
			return func(n int, w http.ResponseWriter, _ *http.Request) bool {
				if n != 2 {
					return false
				}
				_, _ = w.Write([]byte("<html>"))
				return true
			}
		},
		"502": func(*jobHub) func(int, http.ResponseWriter, *http.Request) bool {
			return func(n int, w http.ResponseWriter, _ *http.Request) bool {
				if n != 2 {
					return false
				}
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte("bad gateway"))
				return true
			}
		},
	}
	for name, mk := range cases {
		hub := newJobHub(t, "queued", "running")
		hub.poll = mk(hub)
		waitEnv(t, hub, 150*time.Millisecond)
		r := runJob(t, "--wait", "--timeout", "5s")
		if r.code != 2 || r.stdout != "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", name, r.code, r.stdout, r.stderr)
			continue
		}
		if e := r.lastEnvelope(t); e["error_type"] != "client_error" || e["code"] != float64(0) {
			t.Errorf("%s: envelope %v", name, e)
		}
		polls := hub.count("GET")
		time.Sleep(300 * time.Millisecond)
		if got := hub.count("GET"); got != polls || polls > 2 {
			t.Errorf("%s: %d polls, then %d", name, polls, got)
		}
	}
}

func TestWait_WithoutWaitTheStartRecordIsPrinted(t *testing.T) {
	for _, start := range []string{"queued", "done", "dead"} {
		hub := newJobHub(t, start, "done")
		waitEnv(t, hub, 20*time.Millisecond)
		r := runJob(t)
		var doc map[string]any
		if r.code != 0 || r.stderr != "" || json.Unmarshal([]byte(r.stdout), &doc) != nil || doc["state"] != start {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", start, r.code, r.stdout, r.stderr)
		}
		time.Sleep(60 * time.Millisecond)
		if hub.count("POST") != 1 || hub.count("GET") != 0 {
			t.Errorf("%s: requests %v", start, hub.requests())
		}
	}
}
