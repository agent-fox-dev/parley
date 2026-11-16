package cli

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// landWorkspace is the path of the workspace api, as sent; landID is the
// id of its land attempt.
const (
	landWorkspace = "/api/v1/workspaces/api"
	landID        = "5b0e6f3c-2f55-4c1e-9a52-6a3f0e0d2a11"
	landPath      = landWorkspace + "/changes/12/land"
	landPollPath  = landWorkspace + "/lands/" + landID
)

// landAttempt is the land attempt as the hub serves it the n-th time (0
// for the answer to the land), in status, so that two records of one
// status differ.
func landAttempt(status string, n int) string {
	return fmt.Sprintf(`{"id":%q,"status":%q,"changes":[12],"read":%d}`, landID, status, n)
}

// landHub is a stub hub that answers the land of change 12 of api with
// start, and the n-th GET of its attempt with polls[n-1], the last of
// polls answering every later one too.
func landHub(t *testing.T, start stubAnswer, polls ...stubAnswer) *stubHub {
	t.Helper()
	h := newStubHub(t).answer(http.MethodPost, landPath, start.status, start.body)
	if len(polls) == 0 {
		return h
	}
	var mu sync.Mutex
	n := 0
	return h.handle(http.MethodGet, landPollPath, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		a := polls[min(n, len(polls))-1]
		mu.Unlock()
		answerWith(a.status, a.body)(w, r)
	})
}

// runLand runs parley with args against hub, polling every 10 ms, and
// returns what it printed and the requests the hub got.
func runLand(t *testing.T, hub *stubHub, args ...string) (code int, stdout, stderr string, sent []stubRequest) {
	t.Helper()
	opts := defaultOptions()
	opts.pollInterval = 10 * time.Millisecond
	code, stdout, stderr = runCLIWith(t, opts, onHub(hub.URL, args...)...)
	return code, stdout, stderr, hub.recorded()
}

func TestChangeLand_SendsOneLandWithAMemberForEachFlagGiven(t *testing.T) {
	t.Parallel()
	all := []string{"--stack", "--expected-version", "3", "--override", "hotfix now", "--session", "s1"}
	tests := []struct {
		name, slug string
		args       []string
		body       string
		// answer is the status of the hub's answer: 200 a landed attempt,
		// 202 a running one.
		answer int
	}{
		{"no flag", "api", nil, `{}`, http.StatusOK},
		{"stack", "api", []string{"--stack"}, `{"stack":true}`, http.StatusOK},
		{"expected version", "api", []string{"--expected-version", "3"}, `{"expected_version":3}`, http.StatusOK},
		{"override", "api", []string{"--override", "hotfix now"}, `{"override":{"reason":"hotfix now"}}`, http.StatusOK},
		{"session", "api", []string{"--session", "s1"}, `{"session_id":"s1"}`, http.StatusOK},
		{"every flag", "api", all, `{"stack":true,"expected_version":3,"override":{"reason":"hotfix now"},"session_id":"s1"}`, http.StatusOK},
		{"still running", "api", nil, `{}`, http.StatusAccepted},
		{"a slug as one segment", "a/b", nil, `{}`, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			attempt := landAttempt("landed", 0)
			if tt.answer == http.StatusAccepted {
				attempt = landAttempt("running", 0)
			}
			hub := newStubHub(t).answer(http.MethodPost, "/api/v1/workspaces/"+tt.slug+"/changes/12/land", tt.answer, attempt)

			code, stdout, stderr, sent := runLand(t, hub, append([]string{"change", "land", tt.slug, "12"}, tt.args...)...)

			wantResult(t, code, stdout, stderr, attempt)
			got := wantOnlyRequest(t, sent, http.MethodPost, "/api/v1/workspaces/"+url.PathEscape(tt.slug)+"/changes/12/land")
			wantJSONBody(t, got.body, tt.body)
			if got.query != "" {
				t.Errorf("query = %q, want none", got.query)
			}
		})
	}
}

func TestChangeLand_OutwaitsTheDefaultRequestTimeout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		// path is the route the hub answers late.
		path string
		code int
	}{
		{"land", []string{"change", "land", "api", "12"}, landPath, 0},
		{"another command", []string{"land", "show", "api", landID}, landPollPath, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			late := newStubHub(t).handle(http.MethodPost, tt.path, answerAfter(300*time.Millisecond)).
				handle(http.MethodGet, tt.path, answerAfter(300*time.Millisecond))
			opts := defaultOptions()
			opts.requestTimeout = 100 * time.Millisecond

			code, stdout, stderr := runCLIWith(t, opts, onHub(late.URL, tt.args...)...)

			if tt.code == 0 {
				wantResult(t, code, stdout, stderr, `{}`)
				return
			}
			wantClientFailure(t, code, stdout, stderr, "client_error")
		})
	}
}

func TestChangeLand_PrintsTheHubsRefusalAsReceived(t *testing.T) {
	t.Parallel()
	details := `{"id":"` + landID + `","status":"conflict","conflict":{"files":[{"path":"a.go","kind":"content"}]}}`
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"conflict", http.StatusConflict, `{"error":{"code":409,"message":"conflict","error_type":"land_conflict","details":` + details + `}}`},
		{"blocked", http.StatusConflict, `{"error":{"code":409,"message":"blocked","error_type":"policy_blocked","details":{"id":"` + landID + `","status":"blocked"}}}`},
		{"not open", http.StatusConflict, `{"error":{"code":409,"message":"not open","error_type":"change_not_open"}}`},
		{"failed", http.StatusServiceUnavailable, `{"error":{"code":503,"message":"failed","error_type":"land_failed","details":{"id":"` + landID + `","status":"failed","error":"internal"}}}`},
		{"forbidden", http.StatusForbidden, `{"error":{"code":403,"message":"no","error_type":"forbidden"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := landHub(t, stubAnswer{tt.status, tt.body})

			code, stdout, stderr, sent := runLand(t, hub, "change", "land", "api", "12")

			if code != 1 || stdout != "" {
				t.Errorf("exit status %d, stdout %q; want 1 and nothing", code, stdout)
			}
			if want := indented(t, tt.body); stderr != want {
				t.Errorf("stderr = %q, want the hub's envelope %q", stderr, want)
			}
			wantOnlyRequest(t, sent, http.MethodPost, landPath)
		})
	}
}

func TestChangeLand_WaitsUntilTheAttemptIsFinal(t *testing.T) {
	t.Parallel()
	notFound := `{"error":{"code":404,"message":"no attempt","error_type":"land_not_found"}}`
	conflict := `{"error":{"code":409,"message":"conflict","error_type":"land_conflict","details":` + landAttempt("conflict", 0) + `}}`
	tests := []struct {
		name  string
		start stubAnswer
		polls []stubAnswer
		flags []string
		// stdout is the document printed, "" for none; errorType the
		// envelope stderr ends with, "" for none.
		stdout, errorType string
		polled            int
	}{
		{"landed", stubAnswer{202, landAttempt("running", 0)}, []stubAnswer{{200, landAttempt("running", 1)}, {200, landAttempt("landed", 2)}},
			nil, landAttempt("landed", 2), "", 2},
		{"conflict", stubAnswer{202, landAttempt("running", 0)}, []stubAnswer{{200, landAttempt("conflict", 1)}}, nil, landAttempt("conflict", 1), "wait_failed", 1},
		{"blocked", stubAnswer{202, landAttempt("running", 0)}, []stubAnswer{{200, landAttempt("blocked", 1)}}, nil, landAttempt("blocked", 1), "wait_failed", 1},
		{"failed", stubAnswer{202, landAttempt("running", 0)}, []stubAnswer{{200, landAttempt("failed", 1)}}, nil, landAttempt("failed", 1), "wait_failed", 1},
		{"timeout", stubAnswer{202, landAttempt("running", 0)}, []stubAnswer{{200, landAttempt("running", 1)}}, []string{"--timeout", "1s"}, "", "wait_timeout", -1},
		{"attempt gone", stubAnswer{202, landAttempt("running", 0)}, []stubAnswer{{404, notFound}}, nil, "", "land_not_found", 1},
		{"landed at once", stubAnswer{200, landAttempt("landed", 0)}, nil, nil, landAttempt("landed", 0), "", 0},
		{"refused at once", stubAnswer{409, conflict}, nil, nil, "", "land_conflict", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := landHub(t, tt.start, tt.polls...)

			code, stdout, stderr, sent := runLand(t, hub, append([]string{"change", "land", "api", "12", "--wait"}, tt.flags...)...)

			want := ""
			if tt.stdout != "" {
				want = indented(t, tt.stdout)
			}
			if stdout != want {
				t.Errorf("stdout = %q, want %q", stdout, want)
			}
			if wantCode := map[bool]int{true: 0, false: 1}[tt.errorType == ""]; code != wantCode {
				t.Errorf("exit status = %d, want %d; stderr %q", code, wantCode, stderr)
			}
			if tt.errorType != "" {
				if _, _, got := clientEnvelope(t, lastDocument(stderr)); got != tt.errorType {
					t.Errorf("stderr = %q, want an envelope of %s", stderr, tt.errorType)
				}
			}
			if len(sent) == 0 {
				t.Fatal("the hub got no request")
			}
			if sent[0].method != http.MethodPost || sent[0].rawPath != landPath || sent[0].query != "" {
				t.Errorf("first request %s %s?%s, want POST %s without a query", sent[0].method, sent[0].rawPath, sent[0].query, landPath)
			}
			for _, r := range sent[1:] {
				if r.method != http.MethodGet || r.rawPath != landPollPath {
					t.Errorf("the hub got %s %s, want only GET %s after the land", r.method, r.rawPath, landPollPath)
				}
			}
			// -1: polls until the timeout, at least two.
			switch polled := len(sent) - 1; {
			case tt.polled < 0 && polled < 2, tt.polled >= 0 && polled != tt.polled:
				t.Errorf("the hub got %d polls, want %d", polled, tt.polled)
			}
		})
	}
}

func TestLandReads_SendTheOneGETOfTheCommand(t *testing.T) {
	t.Parallel()
	notFound := `{"error":{"code":404,"message":"no attempt","error_type":"land_not_found"}}`
	hub := newStubHub(t).
		answer(http.MethodGet, landWorkspace+"/changes/12/lands", http.StatusOK, hubAnswer).
		answer(http.MethodGet, landWorkspace+"/lands", http.StatusOK, hubAnswer).
		answer(http.MethodGet, landPollPath, http.StatusOK, hubAnswer).
		answer(http.MethodGet, landWorkspace+"/lands/gone", http.StatusNotFound, notFound)
	since := "2026-01-02T03:04:05Z"
	tests := []struct {
		args    []string
		rawPath string
		query   url.Values
		// refusal is the hub's envelope on stderr, "" for hubAnswer on
		// stdout.
		refusal string
	}{
		{[]string{"change", "lands", "api", "12"}, landWorkspace + "/changes/12/lands", nil, ""},
		{[]string{"land", "list", "api"}, landWorkspace + "/lands", nil, ""},
		{
			[]string{"land", "list", "api", "--status", "landed", "--status", "failed", "--target", "release/1", "--since", since, "--limit", "10", "--cursor", "c1"},
			landWorkspace + "/lands",
			url.Values{"status": {"landed", "failed"}, "target_branch": {"release/1"}, "since": {since}, "limit": {"10"}, "cursor": {"c1"}},
			"",
		},
		{[]string{"land", "show", "api", landID}, landPollPath, nil, ""},
		{[]string{"land", "show", "api", "gone"}, landWorkspace + "/lands/gone", nil, notFound},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			before := len(hub.recorded())

			code, stdout, stderr := runCLIWith(t, defaultOptions(), onHub(hub.URL, tt.args...)...)

			got := wantOnlyRequest(t, hub.recorded()[before:], http.MethodGet, tt.rawPath)
			wantQuery(t, got.query, tt.query)
			if tt.refusal == "" {
				wantResult(t, code, stdout, stderr, hubAnswer)
				return
			}
			if code != 1 || stdout != "" || stderr != indented(t, tt.refusal) {
				t.Errorf("exit status %d, stdout %q, stderr %q; want 1, nothing and the hub's envelope", code, stdout, stderr)
			}
		})
	}

	t.Run("land alone", func(t *testing.T) {
		before := len(hub.recorded())

		code, stdout, stderr := runCLIWith(t, defaultOptions(), onHub(hub.URL, "land")...)

		if code != 0 || stderr != "" || !strings.Contains(stdout, "list") || !strings.Contains(stdout, "show") {
			t.Errorf("exit status %d, stdout %q, stderr %q; want 0 and the help naming list and show", code, stdout, stderr)
		}
		if n := len(hub.recorded()) - before; n != 0 {
			t.Errorf("the hub got %d requests, want none", n)
		}
	})
}

func TestLandCommands_RefuseAMalformedCommandLineBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	hub := newStubHub(t)
	tests := []struct {
		args []string
		// inMessage is the part of the message that names the refusal.
		inMessage string
	}{
		{[]string{"change", "land", "api"}, "parley change land needs the argument <number>"},
		{[]string{"change", "land", "api", "twelve"}, `parley change land takes a <number> that is a positive integer, got "twelve"`},
		{[]string{"change", "land", "api", "0"}, `parley change land takes a <number> that is a positive integer, got "0"`},
		{[]string{"change", "land", "api", "12", "--expected-version", "0"}, `--expected-version <n>, a positive integer, got "0"`},
		{[]string{"change", "land", "api", "12", "--expected-version", "x"}, `--expected-version <n>, a positive integer, got "x"`},
		{[]string{"change", "land", "api", "12", "--override", ""}, "parley change land takes --override <reason>"},
		{[]string{"change", "land", "api", "12", "--wait", "--timeout", "0s"}, `"0s" for "--timeout" flag`},
		{[]string{"change", "land", "api", "12", "--wait", "--poll-interval", "-1s"}, `"-1s" for "--poll-interval" flag`},
		{[]string{"change", "lands", "api"}, "parley change lands needs the argument <number>"},
		{[]string{"change", "lands", "api", "0"}, `parley change lands takes a <number> that is a positive integer, got "0"`},
		{[]string{"land", "list"}, "parley land list needs the argument <slug>"},
		{[]string{"land", "list", "api", "--limit", "x"}, `"x" for "--limit" flag`},
		{[]string{"land", "show", "api"}, "parley land show needs the argument <land-id>"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			code, stdout, stderr := runCLIWith(t, defaultOptions(), onHub(hub.URL, tt.args...)...)

			if message := wantClientFailure(t, code, stdout, stderr, "usage_error"); !strings.Contains(message, tt.inMessage) {
				t.Errorf("message = %q, want it to contain %q", message, tt.inMessage)
			}
		})
	}
	if sent := hub.recorded(); len(sent) != 0 {
		t.Errorf("the hub got %v, want no request", sent)
	}
}
