package hub

import (
	"context"
	"fmt"

	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime/openshell"
)

// sandboxCheckHook is the name of the start hook that runs the sandbox
// runtime's start-up check.
const sandboxCheckHook = "sandbox_runtime_check"

// composeSandbox adds the sandbox feature: it makes the feature the owner of
// the [sandbox] section, checks the section right after the load (the
// check needs [server].external_url and the config dir, which a section
// validator does not see), and, with sandboxes on, builds the runtime
// client in a start hook and runs the start-up check through it before the
// hub listens. With sandboxes off the hook builds nothing and contacts no
// runtime. GET /version reports the runtime the check connected to. The
// sandbox routes, declared through the service, scrub the runtime errors
// they answer with the hub's scrubber.
//
// The parts composed after this one reach the feature through
// Composition.Sandbox; a test replaces the runtime client with
// SetSandboxRuntime and parleyd passes the metrics registry with
// SetSandboxRecorder.
func composeSandbox(c *Composition) {
	svc := sandbox.NewService()
	c.sandbox = svc
	c.sandboxRuntime = openShellRuntime
	if err := sandbox.Register(c.registry); err != nil {
		c.errs = append(c.errs, err)
	}
	c.addConfigCheck(svc.Configure)
	c.addVersionMember(func() map[string]any {
		if v, ok := svc.Version(); ok {
			return map[string]any{"sandbox_runtime": v}
		}
		return nil
	})
	c.AddStartHook(sandboxCheckHook, func(ctx context.Context, rt *Runtime) error {
		// The sandbox routes scrub a runtime error with the hub's scrubber,
		// which learns the gateway's credentials below.
		svc.SetScrubber(rt.Scrubber)
		if !svc.Enabled() {
			return nil
		}
		s := svc.Settings()
		// Whatever client the factory builds, its credentials stay out of
		// every line the hub writes.
		rt.Scrubber.Add(s.Token())
		rt.Scrubber.Add(s.OIDCClientSecret())
		rec := c.sandboxRecorder
		if rec == nil {
			rec = sandbox.NopRecorder{}
		}
		client, err := c.sandboxRuntime(s, sandbox.RuntimeDeps{Scrubber: rt.Scrubber, Recorder: rec})
		if err != nil {
			return fmt.Errorf("sandbox.gateway_url: cannot build the runtime client: %w", err)
		}
		// The budget is counted on the real clock: the check's deadline is a
		// context's, whatever clock the hub was given.
		return svc.Start(ctx, client, sandbox.CheckOptions{Recorder: rec})
	})
}

// openShellRuntime is the default runtime factory: the client over the
// OpenShell Go SDK.
func openShellRuntime(s *sandbox.Settings, deps sandbox.RuntimeDeps) (runtime.Client, error) {
	return openshell.New(s, openshell.Deps{Scrubber: deps.Scrubber, Recorder: deps.Recorder})
}

// Sandbox returns the sandbox feature. A part composed after the sandbox
// part keeps it and reads it once the hub serves: its settings are loaded
// before the stores are opened and, with sandboxes on, its runtime client is
// set by the start-up check before the hub listens.
func (c *Composition) Sandbox() *sandbox.Service { return c.sandbox }

// SetSandboxRuntime replaces the factory that builds the runtime client with
// sandboxes on; nil restores the default, the OpenShell implementation.
func (c *Composition) SetSandboxRuntime(f sandbox.RuntimeFactory) {
	if f == nil {
		f = openShellRuntime
	}
	c.sandboxRuntime = f
}

// SetSandboxRecorder sets the recorder the sandbox feature and its runtime
// client report their metrics to; nil records nothing.
func (c *Composition) SetSandboxRecorder(rec sandbox.Recorder) { c.sandboxRecorder = rec }
