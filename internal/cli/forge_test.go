package cli

import (
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
