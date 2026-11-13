package cli

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRun_EndsWithTheExitStatusOfTheOutcome(t *testing.T) {
	t.Parallel()
	envelope := []byte(`{"error":{"code":404,"message":"workspace not found","error_type":"not_found"}}`)
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"success", nil, 0},
		{"hub error 404", &hubError{status: 404, body: envelope}, 1},
		{"hub error 500", &hubError{status: 500, body: envelope}, 1},
		{"wait_failed", waitFailed("the job failed"), 1},
		{"wait_timeout", waitTimeout("the job is still running"), 1},
		{"usage_error", usageError("bad argument"), 2},
		{"client_error", clientError("cannot reach the hub"), 2},
		{"an error of no named kind", errors.New("other"), 2},
		{"condition_not_met", conditionNotMet("n is odd"), 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			code, stdout, stderr := runCLIWith(t, withProbes(defaultOptions(), probeReturning(tt.err)), "probe", "outcome")

			if code != tt.want {
				t.Errorf("exit status = %d, want %d; stderr %q", code, tt.want, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want it empty", stdout)
			}
			if tt.err == nil && stderr != "" {
				t.Errorf("stderr = %q, want it empty", stderr)
			}
			if tt.err != nil && strings.TrimSpace(stderr) == "" {
				t.Errorf("stderr is empty, want the error")
			}
		})
	}
}

// The test sets environment variables, so it must not run in parallel.
func TestRun_RefusesWhatItCannotParseWithAUsageErrorBeforeAnyRequest(t *testing.T) {
	hub := newStubHub(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte("endpoint_url = = not TOML\n"), 0o600)
	t.Setenv("PARLEY_CONFIG_DIR", dir)
	t.Setenv("PARLEY_ENDPOINT_URL", hub.URL)
	t.Setenv("PARLEY_API_KEY", testAPIKey)

	tests := []struct {
		args []string
		// names is a text the message must hold, if any.
		names string
	}{
		{[]string{"nosuch"}, "nosuch"},
		{[]string{"--bogus"}, "bogus"},
		{[]string{"probe", "nosuch"}, "nosuch"},
		{[]string{"probe", "get", "x", "--bogus"}, "bogus"},
		{[]string{"probe", "get"}, "name"},
		{[]string{"probe", "get", "x", "y"}, "y"},
		{[]string{"probe", "count", "--limit", "abc"}, "limit"},
		{[]string{"version", "extra"}, "extra"},
		{[]string{"probe", "admin", "purge"}, "--confirm"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			code, stdout, stderr := runCLIWith(t, withProbes(defaultOptions(), probeAdmin), tt.args...)

			if code != 2 {
				t.Errorf("exit status = %d, want 2", code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want it empty", stdout)
			}
			var doc struct {
				Error struct {
					Code      *int   `json:"code"`
					Message   string `json:"message"`
					ErrorType string `json:"error_type"`
				} `json:"error"`
			}
			oneJSONDocument(t, stderr, &doc)
			if doc.Error.Code == nil || *doc.Error.Code != 0 || doc.Error.ErrorType != "usage_error" {
				t.Errorf("stderr = %q, want an envelope with code 0 and error_type usage_error", stderr)
			}
			if !strings.Contains(doc.Error.Message, tt.names) {
				t.Errorf("message %q does not name %q", doc.Error.Message, tt.names)
			}
			if n := len(hub.recorded()); n != 0 {
				t.Errorf("the hub got %d requests, want none", n)
			}
		})
	}
}

// The test sets environment variables, so it must not run in parallel.
func TestRun_AcceptsTheGlobalFlagsInAnyPosition(t *testing.T) {
	loginEnv(t)
	hub := thingHub(t, `{"name":"x"}`)
	first := []string{"--endpoint-url", hub.URL, "--api-key", "k", "probe", "get", "x"}
	tests := [][]string{
		first,
		{"probe", "--api-key", "k", "get", "x", "--endpoint-url", hub.URL},
		{"probe", "get", "x", "--endpoint-url=" + hub.URL, "--api-key=k"},
		append(slices.Clone(first), "--json"),
	}
	var firstStdout string
	for i, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			before := len(hub.recorded())

			code, stdout, stderr := runProbes(t, args...)

			if code != 0 {
				t.Fatalf("exit status = %d, want 0; stderr %q", code, stderr)
			}
			got := hub.recorded()[before:]
			if len(got) != 1 || got[0].method != http.MethodGet || got[0].path != "/api/v1/things/x" || got[0].header.Get("Authorization") != "Bearer k" {
				t.Errorf("the hub got %v, want one GET /api/v1/things/x with Authorization Bearer k", got)
			}
			if i == 0 {
				firstStdout = stdout
			} else if stdout != firstStdout {
				t.Errorf("stdout = %q, want %q as without the change of order", stdout, firstStdout)
			}
		})
	}
}
