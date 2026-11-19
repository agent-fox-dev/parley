package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/cli"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime/runtimetest"
)

// lifecycleProfile is the profile agent of the lifecycle scenarios: four
// hours to live, stopped after 30 idle minutes, a hub credential bound to
// the sandbox's fork and a checkpoint before the sandbox loses its compute.
const lifecycleProfile = "[[sandbox.profiles]]\nname = \"agent\"\ndefault = true\nttl = \"4h\"\nidle_timeout = \"30m\"\n" +
	"hub_access = \"fork\"\ncheckpoint = true\n"

// startSandboxLifecycleHub starts a sandbox create hub with the profile
// agent over a fake runtime in host-directory mode, and the fork demo--t1.
// Its provisioning commands are scripted, so that a checkout needs no
// reachable hub, and a main process agent runs until the runtime ends it;
// once the test calls the returned function, every command in a sandbox is
// a real process in the sandbox's directory on the host.
func startSandboxLifecycleHub(t *testing.T) (h *sandboxCreateHub, realCommands func()) {
	t.Helper()
	h = startSandboxCreateHubWith(t, sandboxHubConfig{profiles: lifecycleProfile, fake: []runtimetest.Option{runtimetest.WithHostDirs()}})
	h.fake.SetExec(func(ctx context.Context, _ string, req runtime.ExecRequest, stdout, _ io.Writer) int {
		switch {
		case slices.Equal(req.Command, []string{"agent"}):
			<-ctx.Done()
		case slices.Equal(req.Command, []string{"git", "rev-parse", "HEAD"}):
			_, _ = io.WriteString(stdout, sandboxHead+"\n")
		}
		return 0
	})
	h.object(t, http.MethodPost, "/api/v1/workspaces/demo/forks", map[string]any{"name": "t1"}, http.StatusCreated)
	return h, func() { h.fake.SetExec(nil) }
}

// readySandbox creates a sandbox of demo--t1 whose main process is agent,
// waits until it is ready and returns its id.
func readySandbox(t *testing.T, h *sandboxCreateHub) string {
	t.Helper()
	const sandboxes = "/api/v1/workspaces/demo--t1/sandboxes"
	id, _ := h.object(t, http.MethodPost, sandboxes, map[string]any{"command": []string{"agent"}}, http.StatusCreated)["id"].(string)
	if s := h.object(t, http.MethodGet, sandboxes+"/"+id+"?wait_for=ready&wait=30", nil, http.StatusOK); s["state"] != sandbox.StateReady {
		t.Fatalf("the sandbox %s = %v; want it ready", id, s)
	}
	return id
}

// sandboxEventTypes returns the types of the hub events about the sandbox
// id, oldest first.
func sandboxEventTypes(t *testing.T, h *sandboxCreateHub, id string) []string {
	t.Helper()
	rows, err := h.Store.Main().Query(`SELECT type FROM hub_events WHERE resource_id = ? ORDER BY seq`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var types []string
	for rows.Next() {
		var typ string
		if err := rows.Scan(&typ); err != nil {
			t.Fatal(err)
		}
		types = append(types, typ)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return types
}

func TestSandboxLifecycle_AHubRestartLeavesSandboxesRunningAndRecordsWhatEndedMeanwhile(t *testing.T) {
	parallel(t)
	h, realCommands := startSandboxLifecycleHub(t)
	a, b := readySandbox(t, h), readySandbox(t, h)
	realCommands()
	runtimeClient := h.fake.Client("parley", nil)
	ctx := context.Background()

	calls := len(h.fake.Calls())
	h.Stop()
	if code := h.ExitCode(t, 30*time.Second); code != 0 {
		t.Fatalf("the hub stopped with %d; want 0", code)
	}
	for _, c := range h.fake.Calls()[calls:] {
		switch c.Op {
		case runtime.OpStopSandbox, runtime.OpDeleteSandbox, runtime.OpExec, runtime.OpExecStream, runtime.OpExecInteractive:
			t.Errorf("the runtime received %s %s while the hub stopped", c.Op, c.Target)
		}
	}
	// While the hub is down, A's main process ends and something leaves a
	// sandbox with the hub's label and no record at the runtime.
	h.fake.EndMain("parley", a, 0)
	orphan := sandbox.NewID()
	if _, err := runtimeClient.CreateSandbox(ctx, runtime.SandboxSpec{Name: orphan, Labels: map[string]string{sandbox.LabelSandbox: orphan}}); err != nil {
		t.Fatalf("create the orphan at the runtime: %v", err)
	}

	h.Restart(t)

	got := h.object(t, http.MethodGet, "/api/v1/workspaces/demo--t1/sandboxes/"+a, nil, http.StatusOK)
	checkpoint, _ := got["checkpoint"].(map[string]any)
	if got["state"] != sandbox.StateCompleted || got["exit_code"] != float64(0) || checkpoint["ok"] != true {
		t.Errorf("A after the restart = %v; want completed with exit_code 0 and a checkpoint that succeeded", got)
	}
	types, exits := sandboxEventTypes(t, h, a), 0
	for _, typ := range types {
		if typ == sandbox.EventExit {
			exits++
		}
	}
	if exits != 1 {
		t.Errorf("A's events = %v; want one %s", types, sandbox.EventExit)
	}
	if got := h.object(t, http.MethodGet, "/api/v1/workspaces/demo--t1/sandboxes/"+b, nil, http.StatusOK); got["state"] != sandbox.StateReady {
		t.Errorf("B after the restart = %v; want ready", got)
	}
	if st, err := runtimeClient.GetSandbox(ctx, b); err != nil || st.MainExited || st.State != runtime.StateRunning {
		t.Errorf("the runtime reports B as %+v (%v); want it running with its main process", st, err)
	}
	if types := sandboxEventTypes(t, h, b); len(types) == 0 || types[len(types)-1] != sandbox.EventReady {
		t.Errorf("B's events = %v; want none since its %s", types, sandbox.EventReady)
	}
	list, err := runtimeClient.ListSandboxes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, sb := range list {
		if sb.Name == orphan {
			t.Errorf("the runtime still holds the orphan %s", orphan)
		}
	}
}

// realGit makes every command of a sandbox of h a real process in the
// sandbox's directory on the host, git without the host's configuration,
// with two stand-ins for what needs the hub's git service, which the fake
// runtime cannot reach: the checkout's credential helper, a global setting,
// is left out, and its clone clones demo's repository from its directory.
// A main process agent runs until the runtime ends it.
func realGit(h *sandboxCreateHub) runtimetest.ExecFunc {
	demo := h.workspaces.RepoPath("demo")
	return func(ctx context.Context, name string, req runtime.ExecRequest, stdout, stderr io.Writer) int {
		argv := req.Command
		switch {
		case slices.Equal(argv, []string{"agent"}):
			<-ctx.Done()
			return 0
		case len(argv) > 2 && argv[0] == "git" && argv[1] == "config" && argv[2] == "--global":
			return 0
		case len(argv) > 1 && argv[0] == "git" && argv[1] == "clone":
			argv = []string{"git", "clone", "--quiet", "--no-checkout", demo, "."}
		}
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = filepath.Join(h.fake.HostDir("parley", name), filepath.FromSlash(req.WorkDir))
		cmd.Env = append(hostGitEnv(), "GIT_TERMINAL_PROMPT=0")
		for k, v := range req.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(req.Stdin), stdout, stderr
		var exit *exec.ExitError
		switch err := cmd.Run(); {
		case errors.As(err, &exit):
			return exit.ExitCode()
		case err != nil:
			_, _ = io.WriteString(stderr, err.Error())
			return 127
		}
		return 0
	}
}

// hostGitEnv is the environment git runs in on the host: the test's, without
// the host's global and system configuration.
func hostGitEnv() []string {
	return append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
}

// inSandbox runs git with args in the working directory of the sandbox id
// of h and returns what it printed, trimmed.
func inSandbox(t *testing.T, h *sandboxCreateHub, id string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = workDirOf(h, id)
	cmd.Env = hostGitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in the sandbox %s: %v: %s", strings.Join(args, " "), id, err, out)
	}
	return strings.TrimSpace(string(out))
}

// workDirOf is the host directory that holds the working directory of the
// sandbox id of h.
func workDirOf(h *sandboxCreateHub, id string) string {
	return filepath.Join(h.fake.HostDir("parley", id), filepath.FromSlash(sandbox.WorkDir))
}

// sandboxOf reads the sandbox id of demo--t1 and decodes it.
func sandboxOf(t *testing.T, h *sandboxCreateHub, id string) map[string]any {
	t.Helper()
	return h.object(t, http.MethodGet, "/api/v1/workspaces/demo--t1/sandboxes/"+id, nil, http.StatusOK)
}

// parleyAs runs the CLI in-process against the hub with the credential
// cred and decodes what it printed on stdout, which must be a JSON object.
func parleyAs(t *testing.T, h *sandboxCreateHub, cred string, args ...string) map[string]any {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), append([]string{"--endpoint-url", h.URL(""), "--api-key", cred}, args...), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("parley %s exited %d: %s%s", strings.Join(args, " "), code, stdout.String(), stderr.String())
	}
	var o map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &o); err != nil {
		t.Fatalf("parley %s printed %q: %v", strings.Join(args, " "), stdout.String(), err)
	}
	return o
}

// startRealGitLifecycleHub starts a sandbox lifecycle hub, on clock when it
// is not nil, whose sandboxes run real git (realGit) once their checkout is
// done, and returns it with a token bound to demo--t1 that carries
// sandboxes:read, sandboxes:write, git:read and git:write, which the hub
// credential of a sandbox it creates takes over.
func startRealGitLifecycleHub(t *testing.T, clock *hubtest.JobClock) (*sandboxCreateHub, string) {
	t.Helper()
	h := startSandboxCreateHubWith(t, sandboxHubConfig{profiles: lifecycleProfile, fake: []runtimetest.Option{runtimetest.WithHostDirs()}, clock: clock})
	h.object(t, http.MethodPost, "/api/v1/workspaces/demo/forks", map[string]any{"name": "t1"}, http.StatusCreated)
	h.fake.SetExec(realGit(h))
	created := parleyAs(t, h, h.key, "tokens", "create", "--name", "agent", "--workspace", "demo--t1",
		"--permissions", "sandboxes:read,sandboxes:write,git:read,git:write")
	token, _ := created["token"].(string)
	if token == "" {
		t.Fatalf("tokens create printed %v; want a token", created)
	}
	return h, token
}

// inOrder reports whether want appear in got in their order, others between
// them allowed.
func inOrder(got, want []string) bool {
	for _, g := range got {
		if len(want) > 0 && g == want[0] {
			want = want[1:]
		}
	}
	return len(want) == 0
}

func TestSandboxLifecycle_AnAgentStopsItsSandboxWithUnsavedWorkAndStartsItAgain(t *testing.T) {
	parallel(t)
	h, token := startRealGitLifecycleHub(t, nil)
	id, _ := parleyAs(t, h, token, "sandbox", "create", "demo--t1", "--wait", "--poll-interval", "1s")["id"].(string)
	if err := os.WriteFile(filepath.Join(workDirOf(h, id), "notes.txt"), []byte("not saved anywhere yet\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hub, ok := h.fake.Provider("parley", id+"-hub")
	if !ok {
		t.Fatalf("the runtime holds no hub credential of %s", id)
	}
	probe := "/api/v1/workspaces/demo--t1/sandboxes/" + id
	if status, body := h.Call(t, http.MethodGet, probe, hub.Value, nil); status == http.StatusUnauthorized {
		t.Fatalf("the hub credential before the stop = %d %s; want it to authenticate", status, body)
	}

	stopped := parleyAs(t, h, token, "sandbox", "stop", "demo--t1", id)
	checkpoint, _ := stopped["checkpoint"].(map[string]any)
	refs, _ := checkpoint["refs"].([]any)
	if stopped["state"] != sandbox.StateStopped || stopped["state_reason"] != sandbox.StopAPI || len(refs) != 1 {
		t.Fatalf("sandbox stop printed %v; want it stopped for api with one checkpoint ref", stopped)
	}
	if ref, _ := refs[0].(map[string]any); ref["branch"] != "t1" || ref["dirty"] != true {
		t.Errorf("the checkpoint's ref = %v; want t1, dirty", ref)
	}
	show := exec.Command("git", "--git-dir", h.workspaces.RepoPath("demo--t1"), "cat-file", "-e", "refs/parley/sandboxes/"+id+"/t1:notes.txt")
	show.Env = hostGitEnv()
	if out, err := show.CombinedOutput(); err != nil {
		t.Errorf("the fork's checkpoint ref holds no notes.txt: %v: %s", err, out)
	}

	started := parleyAs(t, h, token, "sandbox", "start", "demo--t1", id, "--wait", "--poll-interval", "1s")
	if started["state"] != sandbox.StateReady {
		t.Fatalf("sandbox start --wait printed %v; want it ready", started)
	}
	if _, err := os.Stat(filepath.Join(workDirOf(h, id), "notes.txt")); err != nil {
		t.Errorf("notes.txt after the start: %v", err)
	}
	if types := sandboxEventTypes(t, h, id); !inOrder(types, []string{sandbox.EventCheckpoint, sandbox.EventStop, sandbox.EventStart}) {
		t.Errorf("the sandbox's events = %v; want %s, %s and %s in that order", types, sandbox.EventCheckpoint, sandbox.EventStop, sandbox.EventStart)
	}
	if status, body := h.Call(t, http.MethodGet, probe, hub.Value, nil); status != http.StatusUnauthorized || errorType(body) != "unauthenticated" {
		t.Errorf("the hub credential of before the stop = %d %s; want 401 unauthenticated", status, body)
	}
}

func TestSandboxLifecycle_AnExpiredSandboxIsCheckpointedAndItsWorkRestoredIntoANewOne(t *testing.T) {
	parallel(t)
	start := time.Now().UTC().Truncate(time.Second)
	clock := hubtest.NewJobClock(func() time.Time { return start })
	h, token := startRealGitLifecycleHub(t, clock)
	// The sandbox is set up through the API, whose wait wakes on the
	// sandbox's change; the CLI's wait polls once a second at the most.
	status, raw := h.Call(t, http.MethodPost, "/api/v1/workspaces/demo--t1/sandboxes", token, map[string]any{"ttl": "1m"})
	var created map[string]any
	if status != http.StatusCreated || json.Unmarshal(raw, &created) != nil {
		t.Fatalf("create a sandbox of demo--t1 = %d %s; want 201", status, raw)
	}
	first, _ := created["id"].(string)
	if s := h.object(t, http.MethodGet, "/api/v1/workspaces/demo--t1/sandboxes/"+first+"?wait_for=ready&wait=30", nil, http.StatusOK); s["state"] != sandbox.StateReady {
		t.Fatalf("the sandbox %s = %v; want it ready", first, s)
	}
	inSandbox(t, h, first, "-c", "user.email=agent@example.test", "-c", "user.name=agent", "commit", "--allow-empty", "--quiet", "-m", "c1")
	c1 := inSandbox(t, h, first, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(workDirOf(h, first), "notes.txt"), []byte("not saved anywhere yet\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	clock.Advance(61 * time.Second)
	deadline := time.Now().Add(5 * time.Second)
	old := sandboxOf(t, h, first)
	for old["state"] != sandbox.StateDeleted && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		old = sandboxOf(t, h, first)
	}
	checkpoint, _ := old["checkpoint"].(map[string]any)
	if old["state"] != sandbox.StateDeleted || old["state_reason"] != sandbox.DeleteExpired || checkpoint["ok"] != true {
		t.Fatalf("the first sandbox after its expiry = %v; want deleted for expired with a checkpoint that succeeded", old)
	}
	if types := sandboxEventTypes(t, h, first); !inOrder(types, []string{sandbox.EventCheckpoint, sandbox.EventDelete}) {
		t.Errorf("the first sandbox's events = %v; want %s then %s", types, sandbox.EventCheckpoint, sandbox.EventDelete)
	}

	restored := parleyAs(t, h, token, "sandbox", "create", "demo--t1", "--restore-from", first, "--wait", "--poll-interval", "1s")
	second, _ := restored["id"].(string)
	if restored["state"] != sandbox.StateReady || restored["restored_from"] != first {
		t.Fatalf("sandbox create --restore-from printed %v; want it ready, restored from %s", restored, first)
	}
	if got := inSandbox(t, h, second, "rev-parse", "t1"); got != c1 {
		t.Errorf("t1 in the new sandbox = %s; want c1 %s", got, c1)
	}
	if got := inSandbox(t, h, second, "status", "--porcelain"); got != "?? notes.txt" {
		t.Errorf("git status in the new sandbox = %q; want %q", got, "?? notes.txt")
	}
}
