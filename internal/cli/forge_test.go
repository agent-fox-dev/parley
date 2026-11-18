package cli

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// sentRead is a command line of a forge read and the one request it must
// send: the path below /api/v1/workspaces/, as sent, and the query.
type sentRead struct {
	args  []string
	path  string
	query url.Values
}

// wantReads runs each command line of reads against a stub hub of its own
// that answers its path with 200 and hubAnswer, and checks that the command
// sent one GET of that path with exactly that query and printed the answer.
func wantReads(t *testing.T, reads []sentRead) {
	t.Helper()
	for _, r := range reads {
		t.Run(strings.Join(r.args, " "), func(t *testing.T) {
			t.Parallel()
			rawPath := "/api/v1/workspaces/" + r.path
			path, err := url.PathUnescape(rawPath)
			if err != nil {
				t.Fatal(err)
			}
			hub := newStubHub(t).answer(http.MethodGet, path, http.StatusOK, hubAnswer)

			code, stdout, stderr, sent := runOnHub(t, hub, r.args...)

			wantResult(t, code, stdout, stderr, hubAnswer)
			got := wantOnlyRequest(t, sent, http.MethodGet, rawPath)
			wantQuery(t, got.query, r.query)
		})
	}
}

// wantRefused runs each command line against a stub hub and checks that it
// failed as a usage error, printing nothing on stdout, before it sent any
// request.
func wantRefused(t *testing.T, lines [][]string) {
	t.Helper()
	for _, args := range lines {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t)

			code, stdout, stderr, sent := runOnHub(t, hub, args...)

			wantClientFailure(t, code, stdout, stderr, "usage_error")
			if len(sent) != 0 {
				t.Errorf("the hub got %v, want no request", sent)
			}
		})
	}
}

// badNumbers break the number rule: digits only, without a sign or a
// leading zero, from 1 to 2,147,483,647.
var badNumbers = []string{"0", "01", "+1", "-1", "1.5", "abc", "", "2147483648"}

// withBadNumbers is command followed by the workspace widgets and each of
// badNumbers.
func withBadNumbers(command ...string) [][]string {
	lines := make([][]string, 0, len(badNumbers))
	for _, n := range badNumbers {
		lines = append(lines, append(append([]string{}, command...), "widgets", n))
	}
	return lines
}

func TestForge_ReadsSendOneGetWithTheFlagsGivenAndPrintTheAnswer(t *testing.T) {
	t.Parallel()
	wantReads(t, []sentRead{
		{[]string{"forge", "status", "widgets"}, "widgets/forge", nil},
		{[]string{"forge", "status", "a/b"}, "a%2Fb/forge", nil},
		{[]string{"forge", "repository", "widgets"}, "widgets/forge/repository", nil},
		{[]string{"forge", "repository", "a/b"}, "a%2Fb/forge/repository", nil},
		{[]string{"forge", "repository", "widgets", "--target", "origin"}, "widgets/forge/repository", url.Values{"target": {"origin"}}},
	})
}

func TestForge_RefusesABadCommandLineBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	wantRefused(t, [][]string{
		{"forge", "status"},
		{"forge", "status", "widgets", "7"},
		{"forge", "repository"},
		{"forge", "repository", "widgets", "7"},
		{"forge", "status", "widgets", "--target", "origin"},
		{"forge", "repository", "widgets", "--limit", "5"},
	})
}

// webhookPath is the webhook route of the workspace demo, as sent.
const webhookPath = "/api/v1/workspaces/demo/forge/webhook"

// A generated secret, and the answers to a set that generated it and to one
// that was given it.
const (
	generatedSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	generatedAnswer = `{"configured": true, "url": "https://hub.example/hooks/forge/demo", "secret": "` + generatedSecret + `"}`
	givenAnswer     = `{"configured": true, "url": "https://hub.example/hooks/forge/demo"}`
)

func TestForgeWebhookSet_SendsNoBodyToGenerateAndNotesTheSecretIsShownOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args    []string
		rawPath string
	}{
		{[]string{"demo"}, webhookPath},
		{[]string{"demo", "--generate"}, webhookPath},
		{[]string{"a/b"}, "/api/v1/workspaces/a%2Fb/forge/webhook"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Parallel()
			path, err := url.PathUnescape(tc.rawPath)
			if err != nil {
				t.Fatal(err)
			}
			hub := newStubHub(t).answer(http.MethodPut, path, http.StatusOK, generatedAnswer)

			code, stdout, stderr, sent := runOnHub(t, hub, append([]string{"forge", "webhook", "set"}, tc.args...)...)

			got := wantOnlyRequest(t, sent, http.MethodPut, tc.rawPath)
			if got.body != "" || got.contentLength > 0 {
				t.Errorf("the PUT carried the body %q, want none", got.body)
			}
			if want := indented(t, generatedAnswer); code != 0 || stdout != want {
				t.Errorf("exit status %d, stdout %q; want 0 and %q", code, stdout, want)
			}
			if !strings.Contains(stderr, "will not be shown again") || strings.Contains(stderr, generatedSecret) {
				t.Errorf("stderr = %q, want a note that the secret will not be shown again, without the secret", stderr)
			}
		})
	}
}

func TestForgeWebhookSet_SendsTheGivenSecretWithoutTheNote(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, stdin string
		args        []string
		want        string
	}{
		{"flag", "", []string{"--secret", "s3cret-value-0123"}, "s3cret-value-0123"},
		{"stdin with a newline", "s3cret-value-0123\n", []string{"--from-stdin"}, "s3cret-value-0123"},
		{"stdin with a carriage return and a newline", "s3cret-value-0123\r\n", []string{"--from-stdin"}, "s3cret-value-0123"},
		{"stdin without a line break", "s3cret-value-0123", []string{"--from-stdin"}, "s3cret-value-0123"},
		{"stdin with two newlines", "abc\n\n", []string{"--from-stdin"}, "abc\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t).answer(http.MethodPut, webhookPath, http.StatusOK, givenAnswer)

			code, stdout, stderr := runWithStdin(t, tc.stdin, onHub(hub.URL, append([]string{"forge", "webhook", "set", "demo"}, tc.args...)...)...)

			wantResult(t, code, stdout, stderr, givenAnswer)
			got := wantOnlyRequest(t, hub.recorded(), http.MethodPut, webhookPath)
			var body map[string]string
			if err := json.Unmarshal([]byte(got.body), &body); err != nil || len(body) != 1 || body["secret"] != tc.want {
				t.Errorf("the PUT carried %q, want {\"secret\": %q}", got.body, tc.want)
			}
		})
	}
}

func TestForgeWebhookSet_RefusesMoreThanOneSourceOrNoSlugBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	set := []string{"forge", "webhook", "set"}
	wantRefused(t, [][]string{
		append(set, "demo", "--secret", "s3cret-value-0123", "--generate"),
		append(set, "demo", "--secret", "s3cret-value-0123", "--from-stdin"),
		append(set, "demo", "--from-stdin", "--generate"),
		append(set, "demo", "--secret", "s3cret-value-0123", "--from-stdin", "--generate"),
		set,
	})
}

func TestForgeWebhookUnset_SendsTheDeleteAndConfirmsOrPrintsTheHubsError(t *testing.T) {
	t.Parallel()
	refusal := `{"error": {"code": 400, "message": "a task fork has no origin", "error_type": "fork_has_no_origin"}}`
	for _, tc := range []struct {
		name   string
		status int
		answer string
	}{
		{"unset", http.StatusNoContent, ""},
		{"refused", http.StatusBadRequest, refusal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t).answer(http.MethodDelete, webhookPath, tc.status, tc.answer)

			code, stdout, stderr, sent := runOnHub(t, hub, "forge", "webhook", "unset", "demo")

			wantOnlyRequest(t, sent, http.MethodDelete, webhookPath)
			if tc.status == http.StatusNoContent {
				wantResult(t, code, stdout, stderr, `{"status": "unset", "workspace": "demo"}`)
			} else {
				wantHubFailure(t, code, stdout, stderr, refusal)
			}
		})
	}
}

func TestForgeDeliveries_SendsAQueryParameterForEachFlagGiven(t *testing.T) {
	t.Parallel()
	wantReads(t, []sentRead{
		{[]string{"forge", "deliveries", "demo"}, "demo/forge/deliveries", nil},
		{[]string{"forge", "deliveries", "demo", "--since", "2026-10-21T09:00:00Z", "--limit", "10", "--cursor", "abc"}, "demo/forge/deliveries",
			url.Values{"since": {"2026-10-21T09:00:00Z"}, "limit": {"10"}, "cursor": {"abc"}}},
	})
}
