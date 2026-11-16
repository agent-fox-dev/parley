package cli

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// activityPath is the activity route of the workspace demo, as sent.
const activityPath = "/api/v1/workspaces/demo/activity"

// runOnHub runs parley with args against hub and returns what it printed
// and the requests this run sent. The runs of one test share its hub, so
// they run one at a time.
func runOnHub(t *testing.T, hub *stubHub, args ...string) (code int, stdout, stderr string, sent []stubRequest) {
	t.Helper()
	before := len(hub.recorded())
	code, stdout, stderr = runCLIWith(t, defaultOptions(), onHub(hub.URL, args...)...)
	return code, stdout, stderr, hub.recorded()[before:]
}

// wantFailOnOverlap checks the run of a command given --fail-on-overlap,
// or not, against a hub that answered body: the body on stdout in every
// case, then exit status 3 and condition_not_met on stderr when want is 3,
// and exit status 0 and nothing on stderr when it is 0.
func wantFailOnOverlap(t *testing.T, code int, stdout, stderr, body string, want int) {
	t.Helper()
	if want == 0 {
		wantResult(t, code, stdout, stderr, body)
		return
	}
	if code != want {
		t.Errorf("exit status = %d, want %d (stderr %q)", code, want, stderr)
	}
	if w := indented(t, body); stdout != w {
		t.Errorf("stdout = %q, want the answer %q", stdout, w)
	}
	if gotCode, message, errorType := clientEnvelope(t, stderr); gotCode != 0 || errorType != "condition_not_met" || message == "" {
		t.Errorf("envelope code %v, error_type %q, message %q; want 0, condition_not_met and a message", gotCode, errorType, message)
	}
}

func TestActivity_SendsAParameterForEachFlagGivenAndPrintsTheAnswer(t *testing.T) {
	t.Parallel()
	answer := `{"target_branch": "main", "target_sha": null, "changes": [], "claims": [], "landed": []}`
	tests := []struct {
		name    string
		args    []string
		want    url.Values
		status  int
		answers string
	}{
		{
			"the paths alone", []string{"activity", "demo", "--paths", "api/**,docs/**"}, url.Values{"paths": {"api/**,docs/**"}},
			http.StatusOK, answer,
		},
		{
			"every flag",
			[]string{"activity", "demo", "--paths", "api/**,docs/**", "--target", "release", "--since", changeSHA,
				"--exclude-session", "s1", "--exclude-change", "4"},
			url.Values{
				"paths": {"api/**,docs/**"}, "target": {"release"}, "since": {changeSHA},
				"exclude_session": {"s1"}, "exclude_change": {"4"},
			},
			http.StatusOK, answer,
		},
		{
			"a refusal of the hub", []string{"activity", "demo", "--paths", "api/**"}, url.Values{"paths": {"api/**"}},
			http.StatusNotFound, envelope(http.StatusNotFound, "not_found"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t).answer(http.MethodGet, activityPath, tt.status, tt.answers)

			code, stdout, stderr, sent := runOnHub(t, hub, tt.args...)

			got := wantOnlyRequest(t, sent, http.MethodGet, activityPath)
			wantQuery(t, got.query, tt.want)
			if got.body != "" {
				t.Errorf("body = %q, want none", got.body)
			}
			if tt.status == http.StatusOK {
				wantResult(t, code, stdout, stderr, tt.answers)
				return
			}
			if code != 1 || stdout != "" {
				t.Errorf("exit status %d, stdout %q; want 1 and nothing", code, stdout)
			}
			if want := indented(t, tt.answers); stderr != want {
				t.Errorf("stderr = %q, want the hub's envelope %q", stderr, want)
			}
		})
	}
}

func TestActivity_FailsOnOverlapWhenAChangeOrAClaimTouchesThePaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, body string
		flag       bool
		want       int
	}{
		{"a change", `{"changes": [{"change": {"number": 2}}], "claims": [], "landed": []}`, true, 3},
		{"a claim", `{"changes": [], "claims": [{"id": "c"}], "landed": []}`, true, 3},
		{"only a landed change", `{"changes": [], "claims": [], "landed": [{"number": 1}]}`, true, 0},
		{"a change without the flag", `{"changes": [{"change": {"number": 2}}], "claims": [], "landed": []}`, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t).answer(http.MethodGet, activityPath, http.StatusOK, tt.body)
			args := []string{"activity", "demo", "--paths", "api/**"}
			if tt.flag {
				args = append(args, "--fail-on-overlap")
			}

			code, stdout, stderr, sent := runOnHub(t, hub, args...)

			wantOnlyRequest(t, sent, http.MethodGet, activityPath)
			wantFailOnOverlap(t, code, stdout, stderr, tt.body, tt.want)
		})
	}
}

func TestActivity_RefusesABadCommandLineBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	hub := newStubHub(t)
	tests := []struct {
		args []string
		// inMessage is the part of the message that names the refusal.
		inMessage string
	}{
		{[]string{"activity", "demo"}, "parley activity needs the flag --paths"},
		{[]string{"activity"}, "parley activity needs the argument <slug>"},
		{[]string{"activity", "demo", "extra", "--paths", "a"}, `got the extra argument "extra"`},
		{[]string{"activity", "demo", "--paths", "a", "--exclude-change", "x"}, `--exclude-change <n>, a positive integer, got "x"`},
		{[]string{"activity", "demo", "--paths", "a", "--exclude-change", "0"}, `--exclude-change <n>, a positive integer, got "0"`},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			code, stdout, stderr, _ := runOnHub(t, hub, tt.args...)

			if message := wantClientFailure(t, code, stdout, stderr, "usage_error"); !strings.Contains(message, tt.inMessage) {
				t.Errorf("message = %q, want it to contain %q", message, tt.inMessage)
			}
		})
	}
	if sent := hub.recorded(); len(sent) != 0 {
		t.Errorf("the hub got %v, want no request", sent)
	}
}
