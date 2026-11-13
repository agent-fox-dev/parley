package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/agent-fox-dev/parley/internal/buildinfo"
)

// setBuildValues sets the build values of internal/buildinfo for the test and
// restores them after it. A test that calls it must not run in parallel.
func setBuildValues(t *testing.T, version, commit string) {
	t.Helper()
	oldVersion, oldCommit := buildinfo.Version, buildinfo.Commit
	t.Cleanup(func() { buildinfo.Version, buildinfo.Commit = oldVersion, oldCommit })
	buildinfo.Version, buildinfo.Commit = version, commit
}

// unsetEnv removes key from the environment for the test and restores it
// after it. A test that calls it must not run in parallel.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	os.Unsetenv(key)
}

// runCLI calls Run with args and returns the exit status, stdout and stderr.
func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// runCLIWith is runCLI with opts in place of the defaults.
func runCLIWith(t *testing.T, opts options, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr, opts)
	return code, stdout.String(), stderr.String()
}

// nonEmptyLines splits s into its lines, without the empty ones.
func nonEmptyLines(s string) []string {
	var lines []string
	for line := range strings.SplitSeq(s, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// stubHub is a hub that answers each method and path from a script the test
// sets, answers 404 with an error envelope to anything else, and records
// every request.
type stubHub struct {
	*httptest.Server

	mu       sync.Mutex
	answers  map[string]stubAnswer
	requests []stubRequest
}

type stubAnswer struct {
	status int
	body   string
}

type stubRequest struct {
	method, path string
	header       http.Header
}

func newStubHub(t *testing.T) *stubHub {
	t.Helper()
	h := &stubHub{answers: map[string]stubAnswer{}}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.requests = append(h.requests, stubRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone()})
		a, ok := h.answers[r.Method+" "+r.URL.Path]
		h.mu.Unlock()
		if !ok {
			a = stubAnswer{http.StatusNotFound, `{"error":{"code":404,"message":"not found","error_type":"not_found"}}` + "\n"}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(a.status)
		w.Write([]byte(a.body))
	}))
	t.Cleanup(h.Close)
	return h
}

// answer scripts the hub's answer to method and path.
func (h *stubHub) answer(method, path string, status int, body string) *stubHub {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.answers[method+" "+path] = stubAnswer{status, body}
	return h
}

func (h *stubHub) recorded() []stubRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.requests)
}

// userHub is a stub hub that answers GET /api/v1/user with 200 and user.
func userHub(t *testing.T, user string) *stubHub {
	t.Helper()
	return newStubHub(t).answer(http.MethodGet, "/api/v1/user", http.StatusOK, user)
}

// testUser is a User as the hub answers it, compact.
const testUser = `{"id":"u-1","username":"alice","email":"a@x.io","full_name":"Alice","role":"user","status":"active","provider":"github","provider_id":"123","created_at":"2026-11-13T10:00:00Z","updated_at":"2026-11-13T10:00:00Z"}`

// Credentials of the three formats the hub knows.
const (
	testAPIKey = "parley_abcd1234_0123456789abcdefghijABCDEFGHIJ01"
	testPAT    = "parley_pat_abcd1234_0123456789abcdefghijklmnopqrstuv"
)

// loginEnv points PARLEY_CONFIG_DIR at a directory that does not exist yet,
// unsets PARLEY_ENDPOINT_URL and PARLEY_API_KEY, and returns the directory.
// A test that calls it must not run in parallel.
func loginEnv(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cfg")
	t.Setenv("PARLEY_CONFIG_DIR", dir)
	unsetEnv(t, "PARLEY_ENDPOINT_URL")
	unsetEnv(t, "PARLEY_API_KEY")
	return dir
}

// runWithStdin is runCLI with stdin as the standard input.
func runWithStdin(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	opts := defaultOptions()
	opts.stdin = strings.NewReader(stdin)
	return runCLIWith(t, opts, args...)
}

// clientEnvelope decodes stderr as the error envelope parley writes and fails
// the test unless it is one.
func clientEnvelope(t *testing.T, stderr string) (code float64, message, errorType string) {
	t.Helper()
	var doc struct {
		Error *struct {
			Code      *float64 `json:"code"`
			Message   string   `json:"message"`
			ErrorType string   `json:"error_type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr), &doc); err != nil || doc.Error == nil || doc.Error.Code == nil {
		t.Fatalf("stderr is not an error envelope (%v): %q", err, stderr)
	}
	return *doc.Error.Code, doc.Error.Message, doc.Error.ErrorType
}

// wantClientFailure checks a failure parley reports itself: exit status 2,
// an envelope with code 0 and errorType on stderr, and nothing on stdout. It
// returns the envelope's message.
func wantClientFailure(t *testing.T, code int, stdout, stderr, errorType string) string {
	t.Helper()
	if code != 2 {
		t.Errorf("exit status = %d, want 2", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want it empty", stdout)
	}
	gotCode, message, gotType := clientEnvelope(t, stderr)
	if gotCode != 0 || gotType != errorType {
		t.Errorf("envelope code %v, error_type %q, want 0 and %q", gotCode, gotType, errorType)
	}
	return message
}

// readTOML parses the TOML file at path.
func readTOML(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if err := toml.Unmarshal(data, &m); err != nil {
		t.Fatalf("%s is not TOML: %v", path, err)
	}
	return m
}

// modeOf is the permission bits of the file at path.
func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// namesIn lists the names in directory dir.
func namesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// wantAbsent fails the test unless nothing exists at path.
func wantAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s exists (or cannot be checked: %v), want it absent", path, err)
	}
}
