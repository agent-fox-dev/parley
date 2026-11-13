package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// The test sets environment variables, so it must not run in parallel.
func TestRun_PrintsHelpWithoutArguments(t *testing.T) {
	var requests atomic.Int64
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
	}))
	defer hub.Close()
	t.Setenv("PARLEY_ENDPOINT_URL", hub.URL)
	unsetEnv(t, "PARLEY_API_KEY")

	code, stdout, stderr := runCLI(t)

	if code != 0 {
		t.Errorf("Run returned %d, want 0", code)
	}
	if !strings.Contains(stdout, "version") {
		t.Errorf("stdout does not list the version command:\n%s", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want it empty", stderr)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("the hub got %d requests, want 0", n)
	}
}
