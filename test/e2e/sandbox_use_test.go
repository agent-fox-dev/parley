package e2e

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/sandbox"
)

// sandboxUseHub is a sandbox create hub with the fork fork of demo and the
// ready sandbox id in it, whose finished operations observed records.
type sandboxUseHub struct {
	*sandboxCreateHub
	fork, id string
	observed *operations
}

// startSandboxUseHub starts a sandbox use hub: its profile agent gives a
// sandbox a hub credential bound to its fork, and its sandbox is created
// with the fork demo--t1 and waited for until it is ready.
func startSandboxUseHub(t *testing.T) *sandboxUseHub {
	t.Helper()
	ops := &operations{}
	h := &sandboxUseHub{observed: ops, fork: "demo--t1"}
	h.sandboxCreateHub = startSandboxCreateHub(t, "[[sandbox.profiles]]\nname = \"agent\"\ndefault = true\nttl = \"4h\"\nhub_access = \"fork\"\n",
		func(c *hub.Composition) { c.Sandbox().RegisterObserver(ops.observe) })
	created, _ := h.object(t, http.MethodPost, "/api/v1/workspaces/demo/forks", map[string]any{"name": "t1", "sandbox": map[string]any{}}, http.StatusCreated)["sandbox"].(map[string]any)
	h.id, _ = created["id"].(string)
	if s := h.object(t, http.MethodGet, h.sandboxPath("?wait_for=ready&wait=30"), nil, http.StatusOK); s["state"] != sandbox.StateReady {
		t.Fatalf("the sandbox of %s = %v; want it ready", h.fork, s)
	}
	return h
}

// sandboxPath is the API path of the hub's sandbox followed by rest.
func (h *sandboxUseHub) sandboxPath(rest string) string {
	return "/api/v1/workspaces/" + h.fork + "/sandboxes/" + h.id + rest
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

func TestSandboxUse_TellsTheObserversOfAnExec(t *testing.T) {
	parallel(t)
	h := startSandboxUseHub(t)

	r := h.object(t, http.MethodPost, h.sandboxPath("/exec"), map[string]any{"command": []string{"true"}}, http.StatusOK)

	if r["exit_code"] != 0.0 {
		t.Errorf("exec = %v; want exit_code 0", r)
	}
	ops := h.observed.wait(1)
	if len(ops) != 1 || ops[0].Kind != sandbox.OperationExec || ops[0].SandboxID != h.id || ops[0].Workspace != h.fork || ops[0].Result != sandbox.ExecResultOK {
		t.Errorf("the observer got %+v; want one exec of %s in %s with the result ok", ops, h.id, h.fork)
	}
}
