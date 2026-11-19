package e2e

import (
	"context"
	"io"
	"net/http"
	"slices"
	"testing"
	"time"

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
