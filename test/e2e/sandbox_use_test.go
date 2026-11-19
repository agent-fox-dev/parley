package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime/runtimetest"
	"github.com/agent-fox-dev/parley/internal/secret"
)

// npmToken is the value of the secret NPM_TOKEN the sandbox of a sandbox
// use hub exposes.
const npmToken = "npm_LONGSECRET1"

// sandboxUseRun is the run of the session of a sandbox use hub.
const sandboxUseRun = "20260102_030405_5a4d0e"

// sandboxUseHub is a sandbox create hub with the fork fork of demo, created
// in the session of the run sandboxUseRun, and the ready sandbox id in it,
// whose main process runs until the test ends and whose finished operations
// observed records. Once the sandbox is ready, every command the runtime
// runs is a real process in the sandbox's directory on the host.
type sandboxUseHub struct {
	*sandboxCreateHub
	fork, id, session string
	observed          *operations
	terms             *terminalSizes
}

// startSandboxUseHub starts a sandbox use hub: its profile agent exposes
// the secret NPM_TOKEN and gives a sandbox a hub credential bound to its
// fork, at most one exec runs at once in a sandbox, and its sandbox is
// created with the fork demo--t1, with the main process agent, and waited
// for until it is ready. The fork is created in a session of the run
// sandboxUseRun, whose sandbox the sandbox is.
func startSandboxUseHub(t *testing.T) *sandboxUseHub {
	t.Helper()
	ops, terms := &operations{}, &terminalSizes{}
	h := &sandboxUseHub{observed: ops, terms: terms, fork: "demo--t1"}
	h.sandboxCreateHub = startSandboxCreateHubWith(t, sandboxHubConfig{
		profiles:    "[[sandbox.profiles]]\nname = \"agent\"\ndefault = true\nttl = \"4h\"\nhub_access = \"fork\"\nexpose_secrets = [\"NPM_TOKEN\"]\n",
		sandboxKeys: "max_concurrent_execs = 1\n",
		fake:        []runtimetest.Option{runtimetest.WithHostDirs()},
		wrap: func(c runtime.Client) runtime.Client {
			terms.Client = c
			return terms
		},
		compose: []func(*hub.Composition){func(c *hub.Composition) { c.Sandbox().RegisterObserver(ops.observe) }},
	})
	h.putSecret(t, secret.ScopeOrg, h.org, "NPM_TOKEN", npmToken)
	// The provisioning's commands are scripted, so that its checkout needs
	// no reachable hub; the main process runs until the runtime stops.
	h.fake.SetExec(func(ctx context.Context, _ string, req runtime.ExecRequest, stdout, _ io.Writer) int {
		switch {
		case slices.Equal(req.Command, []string{"agent"}):
			<-ctx.Done()
		case slices.Equal(req.Command, []string{"git", "rev-parse", "HEAD"}):
			_, _ = io.WriteString(stdout, sandboxHead+"\n")
		}
		return 0
	})
	h.session, _ = h.object(t, http.MethodPost, "/api/v1/sessions",
		map[string]any{"workspace_slug": "demo", "run_id": sandboxUseRun}, http.StatusCreated)["id"].(string)
	created, _ := h.object(t, http.MethodPost, "/api/v1/workspaces/demo/forks",
		map[string]any{"name": "t1", "session_id": h.session, "sandbox": map[string]any{"command": []string{"agent"}}},
		http.StatusCreated)["sandbox"].(map[string]any)
	h.id, _ = created["id"].(string)
	if s := h.object(t, http.MethodGet, h.sandboxPath("?wait_for=ready&wait=30"), nil, http.StatusOK); s["state"] != sandbox.StateReady {
		t.Fatalf("the sandbox of %s = %v; want it ready", h.fork, s)
	}
	h.fake.SetExec(nil)
	return h
}

// sandboxPath is the API path of the hub's sandbox followed by rest.
func (h *sandboxUseHub) sandboxPath(rest string) string {
	return "/api/v1/workspaces/" + h.fork + "/sandboxes/" + h.id + rest
}

// send sends method path with the hub's key and body as contentType, and
// returns the answer's status and body.
func (h *sandboxUseHub) send(t *testing.T, method, path, contentType string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, h.URL(path), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.key)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: reading the answer: %v", method, path, err)
	}
	return resp.StatusCode, got
}

// sandbox returns the hub's sandbox object.
func (h *sandboxUseHub) sandbox(t *testing.T) map[string]any {
	t.Helper()
	return h.object(t, http.MethodGet, h.sandboxPath(""), nil, http.StatusOK)
}

// lastActivity returns the last_activity_at of the hub's sandbox.
func (h *sandboxUseHub) lastActivity(t *testing.T) time.Time {
	t.Helper()
	s := h.sandbox(t)
	at, err := time.Parse(time.RFC3339, s["last_activity_at"].(string))
	if err != nil {
		t.Fatalf("last_activity_at of %v: %v", s, err)
	}
	return at
}

// mainRunning reports whether the main process of the hub's sandbox runs.
func (h *sandboxUseHub) mainRunning(t *testing.T) bool {
	t.Helper()
	st, err := h.fake.Client("parley", nil).GetSandbox(context.Background(), h.id)
	if err != nil {
		t.Fatal(err)
	}
	return !st.MainExited
}

// openTerminals counts the terminals of the hub's sandbox the runtime holds
// open.
func (h *sandboxUseHub) openTerminals() int {
	n := 0
	for _, s := range h.fake.Streams() {
		if s.Op == runtime.OpExecInteractive && s.Sandbox == h.id {
			n++
		}
	}
	return n
}

// operations keeps the operation records an observer is given, in order.
type operations struct {
	mu   sync.Mutex
	list []sandbox.Operation
}

func (o *operations) observe(_ context.Context, op sandbox.Operation) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.list = append(o.list, op)
	return nil
}

// wait waits up to 5 s for n records and returns those given by then.
func (o *operations) wait(n int) []sandbox.Operation {
	deadline := time.Now().Add(5 * time.Second)
	for {
		o.mu.Lock()
		list := slices.Clone(o.list)
		o.mu.Unlock()
		if len(list) >= n || time.Now().After(deadline) {
			return list
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// kinds returns the kind of each record of ops, in order.
func kinds(ops []sandbox.Operation) []string {
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		out = append(out, op.Kind)
	}
	return out
}

// terminalSizes is the hub's runtime client, which records the size of
// every terminal it opens.
type terminalSizes struct {
	runtime.Client
	mu    sync.Mutex
	sizes []runtime.TerminalSize
}

func (c *terminalSizes) ExecInteractive(ctx context.Context, name string, req runtime.ExecRequest, size runtime.TerminalSize) (runtime.Terminal, error) {
	c.mu.Lock()
	c.sizes = append(c.sizes, size)
	c.mu.Unlock()
	return c.Client.ExecInteractive(ctx, name, req, size)
}

func (c *terminalSizes) opened() []runtime.TerminalSize {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.sizes)
}

// eventually waits up to d for cond, and fails the test naming what it
// waited for when it does not hold by then.
func eventually(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited %s for %s", d, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSandboxUse_AnAgentUploadsAScriptRunsItAndDownloadsItsResult(t *testing.T) {
	parallel(t)
	h := startSandboxUseHub(t)
	before := h.lastActivity(t)

	status, body := h.send(t, http.MethodPut, h.sandboxPath("/files?path=run.sh&mode=755"), "application/octet-stream",
		[]byte("echo hello; echo data > result.txt\n"))
	if status != http.StatusOK {
		t.Fatalf("upload = %d %s; want 200", status, body)
	}
	r := h.object(t, http.MethodPost, h.sandboxPath("/exec"), map[string]any{"command": []string{"sh", "run.sh"}}, http.StatusOK)
	if r["exit_code"] != 0.0 || r["stdout"] != "hello\n" {
		t.Errorf("exec = %v; want exit_code 0 and stdout %q", r, "hello\n")
	}
	downloadStart := time.Now().UTC().Truncate(time.Second)
	status, body = h.send(t, http.MethodGet, h.sandboxPath("/files?path=result.txt"), "", nil)
	downloadEnd := time.Now().UTC()
	if status != http.StatusOK || string(body) != "data\n" {
		t.Errorf("download = %d %q; want 200 and what the script wrote, %q", status, body, "data\n")
	}

	ops := h.observed.wait(3)
	if got, want := kinds(ops), []string{sandbox.OperationUpload, sandbox.OperationExec, sandbox.OperationDownload}; !slices.Equal(got, want) {
		t.Fatalf("the observer got %v; want %v", got, want)
	}
	if ops[1].Result != sandbox.ExecResultOK {
		t.Errorf("the exec's record has the result %q; want %q", ops[1].Result, sandbox.ExecResultOK)
	}
	// The hub's own observer, the recorder of sandbox use, was told of each
	// before the test's, and stored it as a run event of the session's run.
	rows, err := h.Store.Analytics().QueryContext(context.Background(),
		`SELECT event_type, session_id FROM run_events WHERE workspace = ? AND run_id = ? ORDER BY event_type`, h.fork, sandboxUseRun)
	if err != nil {
		t.Fatal(err)
	}
	var recorded []string
	for rows.Next() {
		var eventType, session string
		if err := rows.Scan(&eventType, &session); err != nil {
			t.Fatal(err)
		}
		recorded = append(recorded, eventType+" of "+session)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	of := " of " + h.session
	if want := []string{"sandbox.download" + of, "sandbox.exec" + of, "sandbox.upload" + of}; !slices.Equal(recorded, want) {
		t.Errorf("run events of %s and the run = %v; want %v", h.fork, recorded, want)
	}
	at := h.lastActivity(t)
	if at.Before(before) || at.Before(downloadStart) || at.After(downloadEnd) {
		t.Errorf("last_activity_at = %s; want the time of the download, from %s to %s, and not before %s", at, downloadStart, downloadEnd, before)
	}
}

func TestSandboxUse_ADeveloperRunsACommandWithTheCLIAndGetsItsExitCode(t *testing.T) {
	parallel(t)
	h := startSandboxUseHub(t)

	got := h.parley(t, "sandbox", "exec", h.fork, h.id, "--", "sh", "-c", "echo out; echo err >&2; exit 3")

	if got.code != 3 || got.stdout != "out\n" || got.stderr != "err\n" {
		t.Errorf("parley sandbox exec = %d, stdout %q, stderr %q; want 3, %q, %q", got.code, got.stdout, got.stderr, "out\n", "err\n")
	}
	// The sandbox runs one exec at most: another is accepted once the
	// streamed one no longer counts.
	eventually(t, 2*time.Second, "an exec to be accepted after the streamed one", func() bool {
		status, _ := h.Call(t, http.MethodPost, h.sandboxPath("/exec"), h.key, map[string]any{"command": []string{"true"}})
		return status == http.StatusOK
	})
}

func TestSandboxUse_ADeveloperReadsTheRedactedLogsOfADetachedAgent(t *testing.T) {
	parallel(t)
	h := startSandboxUseHub(t)
	before := time.Now().UTC().Truncate(time.Second)

	h.fake.EmitOutput("parley", h.id, runtime.Stdout, "key "+npmToken)
	h.fake.EmitPolicyLog("parley", h.id, runtime.LogEntry{Action: "deny", Program: "/usr/bin/curl", Host: "example.com", Port: 443})
	// The collector looks for ready sandboxes every 5 seconds.
	eventually(t, 15*time.Second, "both lines to be stored", func() bool {
		var page struct {
			Items []any `json:"items"`
		}
		status, raw := h.Call(t, http.MethodGet, h.sandboxPath("/logs"), h.key, nil)
		return status == http.StatusOK && json.Unmarshal(raw, &page) == nil && len(page.Items) == 2
	})
	got := h.parley(t, "sandbox", "logs", h.fork, h.id)
	after := time.Now().UTC()

	if got.code != 0 {
		t.Fatalf("parley sandbox logs = %d, stderr %q; want 0", got.code, got.stderr)
	}
	// The two logs are followed side by side, so either line may be stored
	// first.
	bySource := map[string]map[string]any{}
	lines := strings.Split(strings.TrimSuffix(got.stdout, "\n"), "\n")
	for _, l := range lines {
		var line map[string]any
		if err := json.Unmarshal([]byte(l), &line); err != nil {
			t.Fatalf("a line of stdout %q is not a JSON object: %v", l, err)
		}
		source, _ := line["source"].(string)
		bySource[source] = line
	}
	process, policy := bySource["process"], bySource["policy"]
	if len(lines) != 2 || process["line"] != "key [redacted]" || policy["action"] != "deny" ||
		policy["host"] != "example.com" || policy["port"] != 443.0 {
		t.Errorf("printed %q; want a process line redacted and a policy line denying example.com:443", got.stdout)
	}
	if strings.Contains(got.stdout, npmToken) {
		t.Errorf("stdout %q holds the exposed value", got.stdout)
	}
	if at := h.lastActivity(t); at.Before(before) || at.After(after) {
		t.Errorf("last_activity_at = %s; want the time the process line was stored, from %s to %s", at, before, after)
	}
}

func TestSandboxUse_ADeveloperAttachesATerminalAndLeavesWithoutEndingTheSandbox(t *testing.T) {
	parallel(t)
	h := startSandboxUseHub(t)

	cfg, err := websocket.NewConfig("ws"+strings.TrimPrefix(h.URL(h.sandboxPath("/attach?cols=100&rows=30")), "http"), "http://localhost/")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Header = http.Header{"Authorization": {"Bearer " + h.key}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, err := cfg.DialContext(ctx)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := websocket.Message.Send(ws, []byte("echo hi\n")); err != nil {
		t.Fatal(err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var echoed []byte
	if err := websocket.Message.Receive(ws, &echoed); err != nil || string(echoed) != "echo hi\n" {
		t.Errorf("received %q, %v; want the bytes sent back, %q", echoed, err, "echo hi\n")
	}
	_ = ws.Close()

	eventually(t, 2*time.Second, "the terminal to close", func() bool { return h.openTerminals() == 0 })
	if sizes := h.terms.opened(); len(sizes) != 1 || sizes[0] != (runtime.TerminalSize{Cols: 100, Rows: 30}) {
		t.Errorf("the runtime opened terminals of %v; want one of 100 by 30", sizes)
	}
	if !h.mainRunning(t) {
		t.Error("the main process ended; want it running")
	}
	if s := h.sandbox(t); s["state"] != sandbox.StateReady {
		t.Errorf("the sandbox is %v; want it ready", s["state"])
	}
	if got := kinds(h.observed.wait(1)); !slices.Equal(got, []string{sandbox.OperationAttach}) {
		t.Errorf("the observer got %v; want one attach", got)
	}
}
