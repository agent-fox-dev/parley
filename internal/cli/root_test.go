package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
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

// runProbe runs the CLI with extra commands the test declares.
func runProbe(t *testing.T, extra []*cobra.Command, args ...string) cliResult {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), args, Options{Stdin: strings.NewReader("")}, &out, &errb, extra...)
	return cliResult{code, out.String(), errb.String()}
}

func TestRun_EveryOutcomeEndsWithItsExitStatus(t *testing.T) {
	outcomes := []struct {
		err  error
		want int
	}{
		{nil, 0},
		{&HubError{Status: 404, Body: []byte(`{"error":{"code":404,"message":"m","error_type":"not_found"}}`)}, 1},
		{WaitFailed("x"), 1},
		{WaitTimeout("x"), 1},
		{UsageError("x"), 2},
		{ClientError("x"), 2},
		{errors.New("other"), 2},
		{ConditionNotMet("x"), 3},
	}
	for _, o := range outcomes {
		probe := Group("probe", "test commands", Declare(Spec{Use: "outcome", Description: "returns an outcome", Auth: AuthNone,
			Run: func(*Call) error { return o.err }}))
		if r := runProbe(t, []*cobra.Command{probe}, "probe", "outcome"); r.code != o.want {
			t.Errorf("%v: exit %d, want %d", o.err, r.code, o.want)
		}
	}
}

func TestRun_AUsageErrorStopsBeforeAnything(t *testing.T) {
	dir := cliEnv(t)
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte("not = [toml"), 0o600)
	t.Setenv("PARLEY_ENDPOINT_URL", "http://127.0.0.1:1")
	t.Setenv("PARLEY_API_KEY", "k")
	var runs int
	probe := Group("probe", "test commands",
		Declare(Spec{Use: "get", Description: "gets a thing", Method: "GET", Path: "/things/:name", Args: []Arg{{Name: "name", Required: true}}, Auth: AuthUser,
			Run: func(*Call) error { runs++; return nil }}),
		Declare(Spec{Use: "count", Description: "counts things", Method: "GET", Path: "/things", Auth: AuthUser,
			Flags: func(fs *pflag.FlagSet) { fs.Int("limit", 50, "how many") },
			Run:   func(*Call) error { runs++; return nil }}),
	)
	tests := []struct {
		args    []string
		mention string
	}{
		{[]string{"nosuch"}, "nosuch"},
		{[]string{"probe", "nosuch"}, "nosuch"},
		{[]string{"probe", "get", "x", "--bogus"}, "bogus"},
		{[]string{"probe", "get"}, ""},
		{[]string{"probe", "get", "x", "y"}, ""},
		{[]string{"probe", "count", "--limit", "abc"}, ""},
		{[]string{"version", "extra"}, ""},
	}
	for _, tt := range tests {
		r := runProbe(t, []*cobra.Command{probe}, tt.args...)
		if r.code != 2 || r.stdout != "" {
			t.Errorf("%v: exit %d stdout %q", tt.args, r.code, r.stdout)
			continue
		}
		var doc map[string]map[string]any
		if err := json.Unmarshal([]byte(r.stderr), &doc); err != nil {
			t.Errorf("%v: stderr %q is not one JSON document", tt.args, r.stderr)
			continue
		}
		e := doc["error"]
		if e["code"] != float64(0) || e["error_type"] != "usage_error" || !strings.Contains(e["message"].(string), tt.mention) {
			t.Errorf("%v: %v", tt.args, e)
		}
	}
	if runs != 0 {
		t.Errorf("a run step ran %d times", runs)
	}
}
