package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/agent-fox-dev/parley/internal/buildinfo"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
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

// withProbes is opts with the command group "probe" added to the tree, which
// exists only in tests: "probe get <name>", "probe count" and the commands
// more builds.
func withProbes(opts options, more ...func() *cobra.Command) options {
	return withProbeGroup(opts, append([]func() *cobra.Command{probeGet, probeCount}, more...)...)
}

// withProbeGroup is opts with the command group "probe" holding exactly the
// commands children builds. The tree is built afresh on every run, so each
// builder is called once per run.
func withProbeGroup(opts options, children ...func() *cobra.Command) options {
	opts.extra = append(slices.Clone(opts.extra), func() *cobra.Command {
		built := make([]*cobra.Command, 0, len(children))
		for _, build := range children {
			built = append(built, build())
		}
		return group("probe", "Commands that exist only in tests", built...)
	})
	return opts
}

// probeGet builds "probe get <name>": GET /things/:name, auth user, with an
// int flag --limit whose default is 50.
func probeGet() *cobra.Command {
	return declare(command{
		Name:        "get",
		Description: "Get a thing",
		Method:      http.MethodGet,
		Path:        "/things/:name",
		Args:        []argSpec{{Name: "name", Required: true}},
		Auth:        authUser,
		Flags:       func(fs *pflag.FlagSet) { fs.Int("limit", 50, "the most things to get") },
	})
}

// probeCount builds "probe count": GET /things, auth user, with an int flag
// --limit.
func probeCount() *cobra.Command {
	return declare(command{
		Name:        "count",
		Description: "Count the things",
		Method:      http.MethodGet,
		Path:        "/things",
		Auth:        authUser,
		Flags:       func(fs *pflag.FlagSet) { fs.Int("limit", 0, "the most things to count") },
	})
}

// probeAdmin builds the group "probe admin" holding "purge [reason]":
// DELETE /things, auth admin, with a bool flag --confirm it requires.
func probeAdmin() *cobra.Command {
	purge := declare(command{
		Name:        "purge",
		Description: "Purge the things",
		Method:      http.MethodDelete,
		Path:        "/things",
		Args:        []argSpec{{Name: "reason"}},
		Auth:        authAdmin,
		Flags: func(fs *pflag.FlagSet) {
			fs.Bool("confirm", false, "confirm the purge")
			cobra.MarkFlagRequired(fs, "confirm")
		},
	})
	return group("admin", "Administer the things", purge)
}

// runProbes runs parley with the probe commands added and args.
func runProbes(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return runCLIWith(t, withProbes(defaultOptions()), args...)
}

// thingHub is a stub hub that answers GET /api/v1/things/x, what "probe get
// x" sends, with 200 and body.
func thingHub(t *testing.T, body string) *stubHub {
	t.Helper()
	return newStubHub(t).answer(http.MethodGet, "/api/v1/things/x", http.StatusOK, body)
}

// probeEnv points PARLEY_CONFIG_DIR at an empty directory and
// PARLEY_ENDPOINT_URL and PARLEY_API_KEY at hubURL and the credential k. A
// test that calls it must not run in parallel.
func probeEnv(t *testing.T, hubURL string) {
	t.Helper()
	t.Setenv("PARLEY_CONFIG_DIR", t.TempDir())
	t.Setenv("PARLEY_ENDPOINT_URL", hubURL)
	t.Setenv("PARLEY_API_KEY", "k")
}

// indented is the JSON document body as parley prints it: compacted, then
// indented by 2 spaces, with one newline after it.
func indented(t *testing.T, body string) string {
	t.Helper()
	var compact, out bytes.Buffer
	if err := json.Compact(&compact, []byte(body)); err != nil {
		t.Fatalf("not JSON (%v): %q", err, body)
	}
	json.Indent(&out, compact.Bytes(), "", "  ")
	return out.String() + "\n"
}

// writeConfigFile writes content to config.toml in dir, creating dir, and
// returns the file's path.
func writeConfigFile(t *testing.T, dir, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	writeFile(content)(t, path)
	return path
}

// probeReturning builds "probe outcome", a command of auth none whose run
// step returns err.
func probeReturning(err error) func() *cobra.Command {
	return func() *cobra.Command {
		return declare(command{
			Name:        "outcome",
			Description: "Return a given outcome",
			Auth:        authNone,
			Run:         func(*call) error { return err },
		})
	}
}

// oneJSONDocument decodes s, which must hold exactly one JSON document and
// nothing else (json.Unmarshal refuses anything after the first value), into v.
func oneJSONDocument(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatalf("not exactly one JSON document (%v): %q", err, s)
	}
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
	handlers map[string]http.HandlerFunc
	requests []stubRequest
}

type stubAnswer struct {
	status int
	body   string
}

type stubRequest struct {
	method, path, body string
	// rawPath is the path as it was sent, percent-encoding included, and
	// query the query string.
	rawPath, query string
	contentLength  int64
	header         http.Header
}

func newStubHub(t *testing.T) *stubHub {
	t.Helper()
	h := &stubHub{answers: map[string]stubAnswer{}, handlers: map[string]http.HandlerFunc{}}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.requests = append(h.requests, stubRequest{
			method: r.Method, path: r.URL.Path, body: string(body),
			rawPath: r.URL.EscapedPath(), query: r.URL.RawQuery, contentLength: r.ContentLength,
			header: r.Header.Clone(),
		})
		a, ok := h.answers[r.Method+" "+r.URL.Path]
		handler := h.handlers[r.Method+" "+r.URL.Path]
		h.mu.Unlock()
		if !ok {
			a = stubAnswer{http.StatusNotFound, `{"error":{"code":404,"message":"not found","error_type":"not_found"}}` + "\n"}
		}
		if handler != nil {
			handler(w, r)
			return
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

// handle has handler answer method and path, for an answer status and body
// cannot describe.
func (h *stubHub) handle(method, path string, handler http.HandlerFunc) *stubHub {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.handlers[method+" "+path] = handler
	return h
}

// answerAfter is a handler that answers 200 with an empty object after d,
// or returns as soon as the client gives up.
func answerAfter(d time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(d):
			w.Write([]byte("{}"))
		case <-r.Context().Done():
		}
	}
}

// closeWithoutAnswer is a handler that closes the connection without
// writing a byte of an answer.
func closeWithoutAnswer(w http.ResponseWriter, _ *http.Request) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err == nil {
		conn.Close()
	}
}

// closedPortURL is the URL of a loopback port nothing listens on.
func closedPortURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	return "http://" + ln.Addr().String()
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

// The browser login's fixtures: the authorize URL the stub hub lists for
// github, the key its callback issues and its callback answer.
const (
	testAuthorizeURL = "https://idp.example/authorize?client_id=gh-id&scope=user%3Aemail"
	testIssuedKey    = "parley_wxyz9876_ZYXWVUTSRQponmlkjihgfedcba987654"
	testCallback     = `{"user":` + testUser + `,"api_key":{"key":"` + testIssuedKey + `","key_id":"wxyz9876","expires_at":null}}`
)

// providersAnswer is GET /auth/providers listing the providers given as
// name and authorize URL pairs.
func providersAnswer(nameAndURL ...string) string {
	type item struct {
		Name         string `json:"name"`
		AuthorizeURL string `json:"authorize_url"`
	}
	items := []item{}
	for i := 0; i+1 < len(nameAndURL); i += 2 {
		items = append(items, item{nameAndURL[i], nameAndURL[i+1]})
	}
	body, _ := json.Marshal(map[string]any{"items": items, "next_cursor": nil})
	return string(body)
}

// browserHub is a stub hub that lists github with testAuthorizeURL and
// answers the callback with testCallback.
func browserHub(t *testing.T) *stubHub {
	t.Helper()
	return newStubHub(t).
		answer(http.MethodGet, "/api/v1/auth/providers", http.StatusOK, providersAnswer("github", testAuthorizeURL)).
		answer(http.MethodPost, "/api/v1/auth/callback", http.StatusOK, testCallback)
}

// fakeBrowser is the browser opener of a test. It records the URLs it is
// given and whether the redirect_uri's port took a connection then; with a
// query it plays the browser and sends GET redirect_uri?<query(state)>,
// recording the answer; with fail it opens nothing and returns fail.
type fakeBrowser struct {
	query func(state string) string
	fail  error

	urls        []string
	reachable   bool
	status      int
	contentType string
}

func (b *fakeBrowser) open(rawURL string) error {
	b.urls = append(b.urls, rawURL)
	redirect := b.redirectURI()
	if u, err := url.Parse(redirect); err == nil {
		if c, err := net.Dial("tcp", u.Host); err == nil {
			b.reachable = true
			c.Close()
		}
	}
	if b.fail != nil || b.query == nil {
		return b.fail
	}
	res, err := http.Get(redirect + "?" + b.query(b.state()))
	if err != nil {
		return err
	}
	res.Body.Close()
	b.status, b.contentType = res.StatusCode, res.Header.Get("Content-Type")
	return nil
}

// last is the last URL the browser was given, parsed.
func (b *fakeBrowser) last() *url.URL {
	if len(b.urls) == 0 {
		return &url.URL{}
	}
	u, _ := url.Parse(b.urls[len(b.urls)-1])
	return u
}

func (b *fakeBrowser) redirectURI() string { return b.last().Query().Get("redirect_uri") }
func (b *fakeBrowser) state() string       { return b.last().Query().Get("state") }

// approve is the query of a redirect the provider sends on approval: the
// code c1 with the state it was given.
func approve(state string) string { return "code=c1&state=" + state }

// runLogin runs parley login with b as the browser opener and wait as the
// time it waits for the redirect, with args after "login".
func runLogin(t *testing.T, b *fakeBrowser, wait time.Duration, args ...string) (int, string, string) {
	t.Helper()
	opts := defaultOptions()
	opts.openBrowser, opts.loginWait = b.open, wait
	return runCLIWith(t, opts, append([]string{"login"}, args...)...)
}

// wantClosed fails the test when the host of rawURL still takes a TCP
// connection.
func wantClosed(t *testing.T, rawURL string) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		t.Fatalf("no host in %q", rawURL)
	}
	if c, err := net.Dial("tcp", u.Host); err == nil {
		c.Close()
		t.Errorf("%s still takes connections", u.Host)
	}
}

// callbacks is how many requests the stub hub got for the callback.
func (h *stubHub) callbacks() int {
	n := 0
	for _, r := range h.recorded() {
		if r.path == "/api/v1/auth/callback" {
			n++
		}
	}
	return n
}

// syncBuffer is a bytes.Buffer a running login may write to while the test
// reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// writeFile is a setup step that writes content to path with mode 0600.
func writeFile(content string) func(t *testing.T, path string) {
	return func(t *testing.T, path string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// wantClientError fails the test unless err is a client_error whose message
// contains inMessage, and returns the message.
func wantClientError(t *testing.T, err error, inMessage string) string {
	t.Helper()
	var ce *cliError
	if !errors.As(err, &ce) || ce.errorType != clientErrorType {
		t.Fatalf("error = %#v, want a client_error", err)
	}
	if !strings.Contains(ce.message, inMessage) {
		t.Errorf("message = %q, want it to contain %q", ce.message, inMessage)
	}
	return ce.message
}

// inodeOf is the inode number of the file at path.
func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return uint64(fi.Sys().(*syscall.Stat_t).Ino)
}

// snapshot describes everything under dir, dir included: each entry's
// relative path with its mode and, for a file, its bytes.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	state := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		state[rel] = fi.Mode().String()
		if fi.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			state[rel] += " " + string(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}
