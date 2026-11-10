package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRun_NoArgumentsPrintsHelpWithoutARequest(t *testing.T) {
	var requests atomic.Int32
	hub := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer hub.Close()
	t.Setenv("PARLEY_ENDPOINT_URL", hub.URL)
	t.Setenv("PARLEY_API_KEY", "")

	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("Run returned %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "version") {
		t.Errorf("help %q does not list the version command", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("%d requests were sent, want 0", n)
	}
}
