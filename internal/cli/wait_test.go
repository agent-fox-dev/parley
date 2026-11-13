package cli

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// The tests in this file that run a command set environment variables, so
// they do not run in parallel, except the subtests of the timeout test,
// which give the settings as flags.

// waitOptions is the default options with "probe run" in the tree and
// polls every interval in place of --poll-interval when interval is not
// zero.
func waitOptions(interval time.Duration, more ...func() *cobra.Command) options {
	opts := withProbeGroup(defaultOptions(), append([]func() *cobra.Command{probeRun}, more...)...)
	opts.pollInterval = interval
	return opts
}

// lastDocument is the last JSON document on stderr: everything from the
// last line that begins with {.
func lastDocument(stderr string) string {
	i := strings.LastIndex("\n"+stderr, "\n{")
	if i < 0 {
		return ""
	}
	return stderr[i:]
}

// wantProgress fails the test unless the lines of stderr before its last
// JSON document are exactly want.
func wantProgress(t *testing.T, stderr string, want ...string) {
	t.Helper()
	before := strings.TrimSuffix(stderr, lastDocument(stderr))
	got := nonEmptyLines(before)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("progress = %q, want %q", got, want)
	}
}

// progressOf is the progress lines of a wait that observes states in
// order: one when the wait begins and one per change of state.
func progressOf(states ...string) []string {
	var lines []string
	for i, s := range states {
		if i == 0 || s != states[i-1] {
			lines = append(lines, "waiting: "+s)
		}
	}
	return lines
}

func TestParseDuration_ReadsAPositiveWholeNumberAndOneUnit(t *testing.T) {
	t.Parallel()
	accepted := []struct {
		value string
		want  time.Duration
	}{
		{"1s", time.Second},
		{"90s", 90 * time.Second},
		{"5m", 5 * time.Minute},
		{"2h", 2 * time.Hour},
		{"1d", 24 * time.Hour},
		{"05s", 5 * time.Second},
	}
	for _, tt := range accepted {
		got, err := parseDuration(tt.value)
		if err != nil || got != tt.want {
			t.Errorf("parseDuration(%q) = %v, %v; want %v", tt.value, got, err, tt.want)
		}
	}
	refused := []string{
		"0s", "00m", "0", "-5s", "+5s", "5", "1.5m", "1m30s", "abc", "", "5 m",
		" 5s", "5s ", "5w", "5S", "s", "1_0s", "0x10s", "106752d",
	}
	for _, value := range refused {
		if got, err := parseDuration(value); err == nil {
			t.Errorf("parseDuration(%q) = %v, want an error", value, got)
		}
	}
}

func TestWait_GivesACommandThatWaitsItsFlagsAndTheirDefaults(t *testing.T) {
	hub := jobHub(t, "queued", jobStates("done")...)
	probeEnv(t, hub.URL)

	code, stdout, stderr := runCLIWith(t, waitOptions(0), "help", "probe", "run", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("exit status %d, stderr %q; want 0 and nothing", code, stderr)
	}
	var d commandHelp
	oneJSONDocument(t, stdout, &d)
	for _, want := range []flagHelp{
		{Name: "wait", Type: "bool", Default: "false"},
		{Name: "timeout", Type: "duration", Default: "5m"},
		{Name: "poll-interval", Type: "duration", Default: "5s"},
	} {
		got := flagNamed(d, want.Name)
		if got.Name != want.Name || got.Type != want.Type || got.Default != want.Default || got.Required || got.Description == "" {
			t.Errorf("flag %s = %+v, want type %s, default %q, not required, described", want.Name, got, want.Type, want.Default)
		}
	}

	for _, flag := range []string{"--timeout", "--poll-interval"} {
		for _, value := range []string{"90s", "5m", "2h", "1d"} {
			t.Run(flag+" "+value, func(t *testing.T) {
				hub := jobHub(t, "queued", jobStates("done")...)
				probeEnv(t, hub.URL)

				code, stdout, stderr := runCLIWith(t, waitOptions(0), "probe", "run", flag, value)

				if code != 0 || stderr != "" {
					t.Fatalf("exit status %d, stderr %q; want 0 and nothing", code, stderr)
				}
				if want := indented(t, jobRecord("queued", 0)); stdout != want {
					t.Errorf("stdout = %q, want %q", stdout, want)
				}
			})
		}
	}
}

func TestWait_RefusesADurationItCannotReadBeforeAnyRequest(t *testing.T) {
	values := []string{"0s", "0", "-5s", "5", "1.5m", "1m30s", "abc", "", "5 m", "5w"}
	for _, flag := range []string{"--timeout", "--poll-interval"} {
		for _, value := range values {
			for _, wait := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s %q wait %v", flag, value, wait), func(t *testing.T) {
					hub := newStubHub(t)
					probeEnv(t, hub.URL)
					args := []string{"probe", "run", flag, value}
					if wait {
						args = append(args, "--wait")
					}

					code, stdout, stderr := runCLIWith(t, waitOptions(time.Millisecond), args...)

					message := wantClientFailure(t, code, stdout, stderr, "usage_error")
					if !strings.Contains(message, flag) {
						t.Errorf("message = %q, want it to name %s", message, flag)
					}
					if n := len(hub.recorded()); n != 0 {
						t.Errorf("the hub got %d requests, want none", n)
					}
				})
			}
		}
	}
}

func TestWait_PrintsTheFinalRecordOnceTheOperationSucceeds(t *testing.T) {
	t.Parallel()
	const interval = 20 * time.Millisecond
	tests := []struct {
		name   string
		start  string
		states []string
	}{
		{"queued, running twice, done", "queued", []string{"running", "running", "done"}},
		{"waiting to retry on the way", "queued", []string{"failed", "running", "failed", "done"}},
		{"done at the start", "done", nil},
		{"running, then done", "running", []string{"done"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := jobHub(t, tt.start, jobStates(tt.states...)...)

			code, stdout, stderr := runCLIWith(t, waitOptions(interval), onHub(hub.URL, "probe", "run", "--wait", "--timeout", "5s")...)

			if code != 0 {
				t.Fatalf("exit status = %d, want 0; stderr %q", code, stderr)
			}
			last := jobRecord(tt.start, 0)
			if n := len(tt.states); n > 0 {
				last = jobRecord(tt.states[n-1], n)
			}
			var doc map[string]any
			oneJSONDocument(t, stdout, &doc)
			if want := indented(t, last); stdout != want {
				t.Errorf("stdout = %q, want the last record the hub served, %q", stdout, want)
			}
			if got := hub.count(http.MethodGet); got != len(tt.states) {
				t.Errorf("the hub got %d polls, want %d", got, len(tt.states))
			}
			if lastDocument(stderr) != "" {
				t.Errorf("stderr holds a JSON document: %q", stderr)
			}
			wantProgress(t, stderr, progressOf(append([]string{tt.start}, tt.states...)...)...)
			requests := hub.recorded()
			for i := 1; i < len(requests); i++ {
				if gap := requests[i].at.Sub(requests[i-1].at); gap < interval {
					t.Errorf("request %d came %v after the one before, want at least %v", i+1, gap, interval)
				}
			}
		})
	}
}

func TestWait_PrintsTheFinalRecordAndFailsWhenTheOperationEndsUnsuccessfully(t *testing.T) {
	t.Parallel()
	tests := []struct {
		start  string
		states []string
	}{
		{"queued", []string{"running", "dead"}},
		{"queued", []string{"cancelled"}},
		{"dead", nil},
	}
	for _, tt := range tests {
		t.Run(strings.Join(append([]string{tt.start}, tt.states...), " "), func(t *testing.T) {
			t.Parallel()
			hub := jobHub(t, tt.start, jobStates(tt.states...)...)

			code, stdout, stderr := runCLIWith(t, waitOptions(20*time.Millisecond), onHub(hub.URL, "probe", "run", "--wait")...)

			if code != 1 {
				t.Errorf("exit status = %d, want 1", code)
			}
			final, last := tt.start, jobRecord(tt.start, 0)
			if n := len(tt.states); n > 0 {
				final, last = tt.states[n-1], jobRecord(tt.states[n-1], n)
			}
			if want := indented(t, last); stdout != want {
				t.Errorf("stdout = %q, want the final record %q", stdout, want)
			}
			gotCode, message, errorType := clientEnvelope(t, lastDocument(stderr))
			if gotCode != 0 || errorType != "wait_failed" || !strings.Contains(message, final) {
				t.Errorf("envelope code %v, error_type %q, message %q; want 0, wait_failed and a message naming %q", gotCode, errorType, message, final)
			}
			wantProgress(t, stderr, progressOf(append([]string{tt.start}, tt.states...)...)...)
			if got := hub.count(http.MethodGet); got != len(tt.states) {
				t.Errorf("the hub got %d polls, want %d", got, len(tt.states))
			}
		})
	}
}

func TestWait_TimesOutWithoutPrintingOrChangingTheOperation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		polls []http.HandlerFunc
	}{
		{"every poll answers running", jobStates("running")},
		{"every poll answers waiting to retry", jobStates("failed")},
		{"the second poll stalls", append(jobStates("running"), answerAfter(5*time.Second))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Each case takes the one second of its timeout, so the cases
			// run side by side.
			t.Parallel()
			hub := jobHub(t, "queued", tt.polls...)

			start := time.Now()
			code, stdout, stderr := runCLIWith(t, waitOptions(50*time.Millisecond),
				onHub(hub.URL, "probe", "run", "--wait", "--timeout", "1s")...)
			took := time.Since(start)

			if code != 1 {
				t.Errorf("exit status = %d, want 1", code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want it empty", stdout)
			}
			gotCode, _, errorType := clientEnvelope(t, lastDocument(stderr))
			if gotCode != 0 || errorType != "wait_timeout" {
				t.Errorf("envelope code %v, error_type %q, want 0 and wait_timeout", gotCode, errorType)
			}
			if took < time.Second || took >= 2*time.Second {
				t.Errorf("the command took %v, want from 1 s to less than 2 s", took)
			}
			requests := hub.recorded()
			if len(requests) < 2 || requests[0].method != http.MethodPost {
				t.Fatalf("the hub got %d requests, want the start and at least one poll", len(requests))
			}
			for _, r := range requests[1:] {
				if r.method != http.MethodGet {
					t.Errorf("after the start the hub got %s %s, want only GET", r.method, r.path)
				}
			}
			time.Sleep(200 * time.Millisecond)
			if n := len(hub.recorded()); n != len(requests) {
				t.Errorf("the hub got %d requests after the command ended", n-len(requests))
			}
		})
	}
}

func TestWait_EndsWithTheHubsErrorWhenAPollIsAnsweredWithOne(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		status   int
		envelope string
	}{
		{"not found", 404, `{"error":{"code":404,"message":"no job j1","error_type":"not_found"}}`},
		{"rate limited", 429, `{"error":{"code":429,"message":"slow down","error_type":"rate_limited","retry_after":2}}`},
		{"internal", 500, `{"error":{"message":"it broke","code":500,"error_type":"internal"},"trace":"t-1"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := jobHub(t, "queued", append(jobStates("running"), answerWith(tt.status, tt.envelope))...)

			code, stdout, stderr := runCLIWith(t, waitOptions(20*time.Millisecond), onHub(hub.URL, "probe", "run", "--wait")...)

			if code != 1 {
				t.Errorf("exit status = %d, want 1", code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want it empty", stdout)
			}
			if got, want := lastDocument(stderr), indented(t, tt.envelope); got != want {
				t.Errorf("last document on stderr = %q, want the hub's envelope %q", got, want)
			}
			wantProgress(t, stderr, "waiting: queued", "waiting: running")
			if got := hub.count(http.MethodGet); got != 2 {
				t.Errorf("the hub got %d polls, want 2", got)
			}
		})
	}
}

func TestWait_EndsWithAClientErrorWhenAPollFailsOnTheClientSide(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// second answers the second poll; nil closes the hub's listener
		// once the first poll is answered.
		second         http.HandlerFunc
		requestTimeout time.Duration
		inMessage      string
		polls          int
	}{
		{"the hub's listener is closed", nil, 0, "cannot reach the hub", 1},
		{"an answer that is not JSON", answerWith(http.StatusOK, "<html>"), 0, "not JSON", 2},
		{"502 without an envelope", answerWith(http.StatusBadGateway, "Bad Gateway"), 0, "502", 2},
		{"no answer within the request timeout", answerAfter(5 * time.Second), 100 * time.Millisecond, "no complete answer", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var hub *stubHub
			polls := jobStates("running")
			if tt.second == nil {
				first := polls[0]
				polls[0] = func(w http.ResponseWriter, r *http.Request) {
					first(w, r)
					hub.Listener.Close()
				}
			} else {
				polls = append(polls, tt.second)
			}
			hub = jobHub(t, "queued", polls...)
			opts := waitOptions(20 * time.Millisecond)
			if tt.requestTimeout > 0 {
				opts.requestTimeout = tt.requestTimeout
			}

			code, stdout, stderr := runCLIWith(t, opts, onHub(hub.URL, "probe", "run", "--wait", "--timeout", "5s")...)

			message := wantClientFailure(t, code, stdout, lastDocument(stderr), "client_error")
			if !strings.Contains(message, tt.inMessage) {
				t.Errorf("message = %q, want it to contain %q", message, tt.inMessage)
			}
			time.Sleep(100 * time.Millisecond)
			if got := hub.count(http.MethodGet); got != tt.polls {
				t.Errorf("the hub got %d polls, want %d", got, tt.polls)
			}
		})
	}
}

func TestWait_PrintsTheStartAnswerAndPollsNothingWithoutWait(t *testing.T) {
	for _, start := range []string{"queued", "done", "dead"} {
		t.Run(start, func(t *testing.T) {
			hub := jobHub(t, start, jobStates("done")...)
			probeEnv(t, hub.URL)

			code, stdout, stderr := runCLIWith(t, waitOptions(time.Millisecond), "probe", "run")

			if code != 0 || stderr != "" {
				t.Fatalf("exit status %d, stderr %q; want 0 and nothing", code, stderr)
			}
			if want := indented(t, jobRecord(start, 0)); stdout != want {
				t.Errorf("stdout = %q, want the start record %q", stdout, want)
			}
			if post, get := hub.count(http.MethodPost), hub.count(http.MethodGet); post != 1 || get != 0 {
				t.Errorf("the hub got %d POST and %d GET, want 1 and 0", post, get)
			}
		})
	}
}

// probeSync builds "probe sync", "probe run" printing in place of the
// final record the document {"started": <start record>, "job": <final
// record>}.
func probeSync() *cobra.Command {
	decl := probeRunCommand()
	decl.Name, decl.Description = "sync", "Run a job and print it with its start"
	decl.Wait.Document = func(_ *call, start, final []byte) ([]byte, error) {
		return []byte(`{"started":` + string(start) + `,"job":` + string(final) + `}`), nil
	}
	return declare(decl)
}

func TestWait_PrintsTheDocumentACommandBuildsFromTheFinalRecord(t *testing.T) {
	t.Parallel()
	tests := []struct {
		final     string
		code      int
		errorType string
	}{
		{"done", 0, ""},
		{"dead", 1, "wait_failed"},
	}
	for _, tt := range tests {
		t.Run(tt.final, func(t *testing.T) {
			t.Parallel()
			hub := jobHub(t, "queued", jobStates("running", tt.final)...)

			code, stdout, stderr := runCLIWith(t, waitOptions(20*time.Millisecond, probeSync), onHub(hub.URL, "probe", "sync", "--wait")...)

			if code != tt.code {
				t.Errorf("exit status = %d, want %d; stderr %q", code, tt.code, stderr)
			}
			want := indented(t, `{"started":`+jobRecord("queued", 0)+`,"job":`+jobRecord(tt.final, 2)+`}`)
			if stdout != want {
				t.Errorf("stdout = %q, want %q", stdout, want)
			}
			if tt.errorType != "" {
				if _, _, errorType := clientEnvelope(t, lastDocument(stderr)); errorType != tt.errorType {
					t.Errorf("error_type = %q, want %q", errorType, tt.errorType)
				}
			}
			wantProgress(t, stderr, progressOf("queued", "running", tt.final)...)
		})
	}
}
