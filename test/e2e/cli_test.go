package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/agent-fox-dev/parley/internal/cli"
	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/land"
)

// serveHub serves a hub with no identity provider on a free loopback port
// and returns its URL.
func serveHub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeConfig(t, dir, "[server]\nbind = \"127.0.0.1\"\nport = 0\n")
	env := config.MapEnv(map[string]string{"XDG_CONFIG_HOME": dir, "XDG_DATA_HOME": dir})
	return hubtest.Serve(t, hub.Options{Env: env, WorkDir: dir}).URL("")
}

// A test that sets the process environment, through clientEnv or t.Setenv,
// or signals the process, runs alone; every other test of the package runs
// in parallel through parallel. The helpers that run parley against a
// test's hub pass --endpoint-url and --api-key, so such a run reads neither
// the environment nor a configuration file.

// parallelSlots bounds how many tests of the package run at once. Each runs
// a hub and git processes; unbounded, the package takes every core from the
// packages go test runs beside it, whose timing tests then fail.
var parallelSlots = make(chan struct{}, 3)

// parallel runs t in parallel with the package's other parallel tests, at
// most cap(parallelSlots) at a time, its cleanups included.
func parallel(t *testing.T) {
	t.Parallel()
	parallelSlots <- struct{}{}
	t.Cleanup(func() { <-parallelSlots })
}

// clientEnv points PARLEY_CONFIG_DIR at configDir and unsets
// PARLEY_ENDPOINT_URL and PARLEY_API_KEY.
func clientEnv(t *testing.T, configDir string) {
	t.Helper()
	t.Setenv("PARLEY_CONFIG_DIR", configDir)
	for _, key := range []string{"PARLEY_ENDPOINT_URL", "PARLEY_API_KEY"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
}

// unknownAPIKey is a well-formed API key no hub issued.
func unknownAPIKey() string {
	const alphanumerics = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	draw := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = alphanumerics[rand.IntN(len(alphanumerics))]
		}
		return string(b)
	}
	return "parley_" + draw(8) + "_" + draw(32)
}

// proxied is a request the recording proxy passed to the hub, with the
// status the hub answered.
type proxied struct {
	path, authorization string
	status              int
}

// recordingProxy serves a reverse proxy to target on a free loopback port and
// records every request it passes on.
type recordingProxy struct {
	*httptest.Server

	mu       sync.Mutex
	requests []proxied
}

func newRecordingProxy(t *testing.T, target string) *recordingProxy {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	p := &recordingProxy{}
	rp := httputil.NewSingleHostReverseProxy(u)
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		rp.ServeHTTP(rec, r)
		p.mu.Lock()
		p.requests = append(p.requests, proxied{path: r.URL.Path, authorization: r.Header.Get("Authorization"), status: rec.status})
		p.mu.Unlock()
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *recordingProxy) recorded() []proxied {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]proxied(nil), p.requests...)
}

// statusRecorder is a ResponseWriter that keeps the status written to it.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func TestCLI_LoginPrintsTheHubsRefusalOfAnUnknownKeyAsReceived(t *testing.T) {
	proxy := newRecordingProxy(t, serveHub(t))
	configDir := filepath.Join(t.TempDir(), "parley")
	clientEnv(t, configDir)
	key := unknownAPIKey()

	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), []string{"login", "--api-key", key, "--endpoint-url", proxy.URL}, &stdout, &stderr)

	if code != 1 {
		t.Errorf("parley login returned %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want it empty", stdout.String())
	}
	var api []proxied
	for _, r := range proxy.recorded() {
		if strings.HasPrefix(r.path, "/api/v1/") {
			api = append(api, r)
		}
	}
	if len(api) != 1 || api[0].authorization != "Bearer "+key {
		t.Fatalf("the hub got %+v under /api/v1, want one request with Authorization Bearer <the key>", api)
	}
	var envelope struct {
		Error struct {
			Code      int    `json:"code"`
			ErrorType string `json:"error_type"`
		} `json:"error"`
	}
	dec := json.NewDecoder(&stderr)
	if err := dec.Decode(&envelope); err != nil {
		t.Fatalf("stderr is not an error envelope: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		t.Errorf("stderr holds more than one JSON document")
	}
	if e := envelope.Error; e.Code != api[0].status || e.Code < 400 || (e.ErrorType != "unauthenticated" && e.ErrorType != "not_found") {
		t.Errorf("envelope code %d, error_type %q, want the hub's status %d (400 or above) and unauthenticated or not_found", e.Code, e.ErrorType, api[0].status)
	}
	if _, err := os.Lstat(configDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s exists (or cannot be checked: %v), want it absent", configDir, err)
	}
}

// mcpProcess is parley mcp started as a harness starts it, with the MCP
// session of the SDK's client on its standard input and output.
type mcpProcess struct {
	// dir is the process's working directory.
	dir     string
	cmd     *exec.Cmd
	session *mcp.ClientSession
	stderr  *hubtest.LogBuffer
	// initialized is what the server answered to initialize.
	initialized *mcp.InitializeResult
}

// startMCP starts the built parley binary as `parley mcp args` against the
// hub at hubURL with the credential key and connects an MCP client to it.
// The session and the process end with the test.
func startMCP(t *testing.T, hubURL, key string, args ...string) *mcpProcess {
	t.Helper()
	p := &mcpProcess{stderr: &hubtest.LogBuffer{}}
	p.cmd = exec.Command(credentialHelper(t), append([]string{"mcp", "--endpoint-url", hubURL, "--api-key", key}, args...)...)
	p.cmd.Stderr = p.stderr
	p.dir = t.TempDir()
	p.cmd.Dir = p.dir
	client := mcp.NewClient(&mcp.Implementation{Name: "harness", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: p.cmd}, nil)
	if err != nil {
		t.Fatalf("connect to parley mcp %v: %v\nstderr: %s", args, err, p.stderr)
	}
	p.session, p.initialized = session, session.InitializeResult()
	t.Cleanup(func() { _ = session.Close() })
	return p
}

// toolNames are the names of the tools the session lists, in order.
func (p *mcpProcess) toolNames(t *testing.T) []string {
	t.Helper()
	list, err := p.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list the tools: %v", err)
	}
	names := make([]string, 0, len(list.Tools))
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// call calls the tool name with arguments and returns its structured
// content and whether the result is an error.
func (p *mcpProcess) call(t *testing.T, name string, arguments map[string]any) (map[string]any, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := p.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("call %s: %v\nstderr: %s", name, err, p.stderr)
	}
	// The server sends a result as the text of its first content: the
	// envelope {ok, data, error, detail}. The data is returned, with the
	// error and its detail of a failed result among its members.
	var envelope struct {
		Data   map[string]any `json:"data"`
		Error  string         `json:"error"`
		Detail string         `json:"detail"`
	}
	for _, c := range res.Content {
		if text, isText := c.(*mcp.TextContent); isText && json.Unmarshal([]byte(text.Text), &envelope) == nil {
			break
		}
	}
	data := envelope.Data
	if data == nil {
		data = map[string]any{}
	}
	if res.IsError {
		data["error"], data["detail"] = envelope.Error, envelope.Detail
	}
	return data, res.IsError
}

// listedByMCP runs `parley mcp args --list` with the built binary and
// returns the names it prints.
func listedByMCP(t *testing.T, args ...string) []string {
	t.Helper()
	out, err := exec.Command(credentialHelper(t), append([]string{"mcp", "--list"}, args...)...).Output()
	if err != nil {
		t.Fatalf("parley mcp --list %v: %v", args, err)
	}
	var tools []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &tools); err != nil {
		t.Fatalf("parley mcp --list printed no JSON array: %v\n%s", err, out)
	}
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}

func TestMCP_ServesTheCommandsAsToolsToAHarnessOnStandardInputAndOutput(t *testing.T) {
	parallel(t)
	h := hubtest.Start(t, nil)
	p := startMCP(t, h.URL(""), h.IssueKey(t, h.CreateUser(t, "dana")), "--groups", "workspace:read", "--no-code-mode")

	if info := p.initialized.ServerInfo; info == nil || info.Name != "parley" || info.Version != "dev" {
		t.Errorf("the server is %+v; want parley in the built version dev", info)
	}
	if in := p.initialized.Instructions; in == "" || strings.Count(in, ". ") > 2 {
		t.Errorf("the instructions are %q; want one to three sentences", in)
	}
	names := p.toolNames(t)
	if want := listedByMCP(t, "--groups", "workspace:read", "--no-code-mode"); !slices.Equal(names, want) || len(names) == 0 {
		t.Errorf("the session lists %v; want what --list prints, %v", names, want)
	}
	data, isError := p.call(t, "workspace_list", map[string]any{"limit": 5})
	if items, isList := data["items"].([]any); isError || !isList || len(items) != 0 {
		t.Errorf("workspace_list answered %v (error %v); want the hub's empty page", data, isError)
	}
	data, isError = p.call(t, "workspace_get", map[string]any{"slug": "nope"})
	if !isError || data["error"] != "not_found" {
		t.Errorf("workspace_get of an unknown workspace answered %v (error %v); want a failed result with the hub's error", data, isError)
	}

	if err := p.session.Close(); err != nil {
		t.Errorf("close the session: %v", err)
	}
	if state := p.cmd.ProcessState; state == nil || state.ExitCode() != 0 {
		t.Errorf("after the client closed its end the process is %v; want it exited with 0", state)
	}
	lines := strings.Split(strings.TrimSpace(p.stderr.String()), "\n")
	var calls int
	for _, line := range lines {
		var l map[string]any
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Errorf("the stderr line %q is not one JSON object", line)
		}
		if l["tool"] == "workspace_get" || l["tool"] == "workspace_list" {
			calls++
		}
	}
	if calls != 2 {
		t.Errorf("stderr has %d lines for the two calls:\n%s", calls, p.stderr)
	}
}

func TestMCP_EndsItsSessionAndItsCallsOnASignal(t *testing.T) {
	parallel(t)
	for _, sig := range []os.Signal{syscall.SIGTERM, os.Interrupt} {
		t.Run(sig.String(), func(t *testing.T) {
			arrived, cancelled := make(chan struct{}), make(chan struct{})
			hub := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				close(arrived)
				<-r.Context().Done()
				close(cancelled)
			}))
			defer hub.Close()
			p := startMCP(t, hub.URL, "k", "--groups", "workspace:read", "--no-code-mode")
			go func() {
				_, _ = p.session.CallTool(context.Background(), &mcp.CallToolParams{Name: "workspace_get", Arguments: map[string]any{"slug": "demo"}})
			}()
			select {
			case <-arrived:
			case <-time.After(10 * time.Second):
				t.Fatalf("the call did not reach the hub\nstderr: %s", p.stderr)
			}
			if err := p.cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			exited := make(chan struct{})
			go func() { _ = p.session.Wait(); close(exited) }()
			for name, done := range map[string]chan struct{}{"the hub to see the call cancelled": cancelled, "the session to end": exited} {
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Errorf("waited 5 s for %s after %s", name, sig)
				}
			}
		})
	}
}

func TestMCP_SaysAtStartHowManyToolsItServesAndWhatCodeModeCosts(t *testing.T) {
	parallel(t)
	h := hubtest.Start(t, nil)
	key := h.IssueKey(t, h.CreateUser(t, "dana"))
	for _, row := range []struct {
		name string
		args []string
	}{{"with code mode", []string{"--groups", "workspace:read"}}, {"without", []string{"--groups", "workspace:read", "--no-code-mode"}}} {
		t.Run(row.name, func(t *testing.T) {
			p := startMCP(t, h.URL(""), key, row.args...)
			list, err := p.session.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			// The session lists its tools by name, as the MCP server does;
			// --list prints them in the order they are served.
			wantBytes, hasCodeMode := 0, false
			for _, tool := range list.Tools {
				if tool.Name == "code_mode" {
					wantBytes, hasCodeMode = len(tool.Description), true
				}
			}
			if hasCodeMode != (row.name == "with code mode") {
				t.Errorf("the session serves code_mode: %v", hasCodeMode)
			}
			first, _, _ := strings.Cut(p.stderr.String(), "\n")
			var line struct {
				Tools *int `json:"tools"`
				Bytes *int `json:"code_mode_description_bytes"`
			}
			if err := json.Unmarshal([]byte(first), &line); err != nil || line.Tools == nil || line.Bytes == nil {
				t.Fatalf("the first line of stderr is %q; want one JSON object with tools and code_mode_description_bytes", first)
			}
			if *line.Tools != len(list.Tools) || *line.Bytes != wantBytes {
				t.Errorf("the start line says %d tools and %d bytes; the session lists %d tools and a code_mode description of %d bytes", *line.Tools, *line.Bytes, len(list.Tools), wantBytes)
			}
		})
	}
}

// toolLines are the lines the process wrote to stderr for its tool calls,
// as the tools' names, in order.
func (p *mcpProcess) toolLines(t *testing.T) []string {
	t.Helper()
	var tools []string
	for _, line := range strings.Split(strings.TrimSpace(p.stderr.String()), "\n") {
		var l struct {
			Tool string `json:"tool"`
		}
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Errorf("the stderr line %q is not one JSON object", line)
		}
		if l.Tool != "" {
			tools = append(tools, l.Tool)
		}
	}
	return tools
}

// script calls code_mode with the script src and returns the text of its
// result; it fails the test when the script failed.
func (p *mcpProcess) script(t *testing.T, src string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := p.session.CallTool(ctx, &mcp.CallToolParams{Name: "code_mode", Arguments: map[string]any{"script": src}})
	if err != nil {
		t.Fatalf("call code_mode: %v\nstderr: %s", err, p.stderr)
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, isText := c.(*mcp.TextContent); isText {
			text.WriteString(tc.Text)
		}
	}
	if res.IsError {
		t.Fatalf("the script failed: %s\nstderr: %s", text.String(), p.stderr)
	}
	return text.String()
}

// An agent whose harness started parley mcp for demo sees who else works
// there, forks, and in one script commits two files to the fork, opens the
// change and lands it, as the shell commands would.
func TestMCP_AnAgentLandsAChangeThroughToolCalls(t *testing.T) {
	parallel(t)
	h := startLifecycleHub(t)
	p := startMCP(t, h.URL(""), h.key, "--workspace", "demo")
	for name, content := range map[string]string{"one.txt": "one\n", "two.txt": "two\n"} {
		if err := os.WriteFile(filepath.Join(p.dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	since := time.Now().UTC().Add(-time.Second).Format(time.RFC3339)

	if _, isError := p.call(t, "activity", map[string]any{"paths": "**"}); isError {
		t.Fatalf("activity failed\nstderr: %s", p.stderr)
	}
	fork, isError := p.call(t, "fork_create", map[string]any{"name": "m1", "no_auto_change": true})
	slug := str(fork, "workspace", "slug")
	if isError || slug != "demo--m1" {
		t.Fatalf("fork_create answered %v (error %v); want the fork demo--m1", fork, isError)
	}
	out := p.script(t, `
def main():
    commit = scm_commit(slug="demo--m1", branch="feat", base="main", message="Add one and two", put=["one.txt=one.txt", "two.txt=two.txt"])
    if is_error(commit):
        return "commit: " + commit.error + " " + commit.detail
    change = change_open(source="feat", source_workspace="demo--m1", intent="add the two files")
    if is_error(change):
        return "open: " + change.error + " " + change.detail
    landed = change_land(number=str(change["number"]), wait=True)
    if is_error(landed):
        return "land: " + landed.error + " " + landed.detail
    return "landed " + landed["landed_sha"] + " as change " + str(change["number"])
`)
	head := gitrepotest.Refs(t, h.repo("demo"), "refs/heads/")["main"]
	if !strings.Contains(out, "landed "+head) || head == "" {
		t.Fatalf("the script returned %q; want the sha main is at now, %s", out, head)
	}
	_, number, _ := strings.Cut(strings.TrimSpace(out[strings.Index(out, "landed "):]), " as change ")
	number = strings.Trim(strings.Fields(number)[0], `"'.,`)
	if c := h.read(t, h.key, number); c["state"] != "landed" {
		t.Errorf("demo#%s is %v, want landed", number, c["state"])
	}
	files := gitrepotest.Git(t, h.repo("demo"), "ls-tree", "-r", "--name-only", "main")
	if !strings.Contains(files, "one.txt") || !strings.Contains(files, "two.txt") {
		t.Errorf("main holds\n%s\nwant the two files the script committed", files)
	}

	calls := p.toolLines(t)
	slices.Sort(calls)
	if want := []string{"activity", "change_land", "change_open", "code_mode", "fork_create", "scm_commit"}; !slices.Equal(calls, want) {
		t.Errorf("stderr has lines for %v, want one for each call: %v", calls, want)
	}
	if err := strings.Contains(p.stderr.String(), "add the two files"); err {
		t.Error("stderr holds an argument of a call")
	}
	// The hub recorded what the shell commands would have it record, for
	// the developer whose credential the session has.
	status, body := h.Call(t, http.MethodGet, "/api/v1/audit?source=hub&limit=100&since="+since, h.key, nil)
	var audit struct {
		Items []struct {
			Type string `json:"type"`
		} `json:"items"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &audit) != nil {
		t.Fatalf("GET /audit = %d %s", status, body)
	}
	var types []string
	for _, item := range audit.Items {
		types = append(types, item.Type)
	}
	for _, want := range []string{"parley.workspace.fork", "parley.change.open", land.EventLand} {
		if !slices.Contains(types, want) {
			t.Errorf("the audit holds %v; want an event %s", types, want)
		}
	}
}

// A read-only assistant sees only commands that read, and a selection that
// names an administrative group is refused before anything is served.
func TestMCP_AReadOnlyAssistantReadsAChangeAndAnAdminSelectionIsRefused(t *testing.T) {
	parallel(t)
	h := startLifecycleHub(t)
	agent := cloneAs(t, h.forkHub, h.agent, "demo--t1")
	agent("checkout", "--quiet", "-b", "feat")
	commitFile(t, agent, "b.txt", "b\n", "Add b")
	agent("push", "--quiet", "origin", "feat")
	n := h.open(t, h.agent, "demo--t1", "feat", map[string]any{"intent": "why"})

	p := startMCP(t, h.URL(""), h.key, "--read-only", "--workspace", "demo")
	names := p.toolNames(t)
	for _, name := range []string{"change_land", "scm_commit", "fork_create", "sandbox_exec"} {
		if slices.Contains(names, name) {
			t.Errorf("the read-only session serves %s", name)
		}
	}
	if !slices.Contains(names, "change_show") || !slices.Contains(names, "activity") {
		t.Errorf("the read-only session serves %v; want change_show and activity among them", names)
	}
	change, isError := p.call(t, "change_show", map[string]any{"number": n})
	if isError || change["state"] != "open" || change["intent"] != "why" {
		t.Errorf("change_show answered %v (error %v); want the open change", change, isError)
	}

	var stdout, stderr bytes.Buffer
	refused := exec.Command(credentialHelper(t), "mcp", "--endpoint-url", h.URL(""), "--api-key", h.key, "--groups", "admin")
	refused.Stdout, refused.Stderr = &stdout, &stderr
	err := refused.Run()
	var exit *exec.ExitError
	var envelope struct {
		Error struct {
			Message   string `json:"message"`
			ErrorType string `json:"error_type"`
		} `json:"error"`
	}
	if !errors.As(err, &exit) || exit.ExitCode() != 2 || stdout.Len() != 0 || json.Unmarshal(stderr.Bytes(), &envelope) != nil ||
		envelope.Error.ErrorType != "usage_error" || !strings.Contains(envelope.Error.Message, "admin") {
		t.Errorf("parley mcp --groups admin: %v, stdout %q, stderr %q; want exit status 2, nothing, and one usage_error naming admin", err, stdout.String(), stderr.String())
	}
}
