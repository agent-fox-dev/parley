package hub

import (
	"context"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime/openshell"
)

const (
	// sandboxCheckHook is the name of the start hook that runs the sandbox
	// runtime's start-up check.
	sandboxCheckHook = "sandbox_runtime_check"
	// sandboxRecordsHook is the name of the start hook that binds the
	// stored sandboxes to the store.
	sandboxRecordsHook = "sandbox_records"
	// sandboxProvisionerHook is the name of the start hook that has the
	// provisioner take up the provisionings an earlier process left, and
	// sandboxProvisionerWork that of the provisioner's background work.
	sandboxProvisionerHook = "sandbox_provisioner"
	sandboxProvisionerWork = "sandbox_provisioner"
	// sandboxProvisionerGrace is how long the stop waits for the
	// provisionings it cancels: each returns once its runtime call does.
	sandboxProvisionerGrace = 5 * time.Second
)

// composeSandbox adds the sandbox feature: it makes the feature the owner of
// the [sandbox] section, checks the section right after the load (the
// check needs [server].external_url and the config dir, which a section
// validator does not see), and, with sandboxes on, builds the runtime
// client in a start hook and runs the start-up check through it before the
// hub listens. With sandboxes off the hook builds nothing and contacts no
// runtime. GET /version reports the runtime the check connected to, and GET
// /sandbox-profiles lists the configured profiles, and GET /sandboxes lists,
// to the admin token, the sandboxes of the source a later part registers
// (Service.RegisterSource). The sandbox routes,
// declared through the service, scrub the runtime errors they answer with
// the hub's scrubber.
//
// The sandbox records are a table of their own, created whether sandboxes
// are on or not, so that turning them on later finds it. A start hook binds
// the service to the store, the hub's clock, the event component, through
// which a create and a transition write their events, the metrics recorder
// the workspaces' repositories, in which a create resolves its checkout,
// the secret component's resolution, through which a create resolves the
// profile's credentials and exposed secrets, the access component's
// MintBound and identity.RevokeTokenByID, through which a sandbox's hub
// credential is minted and revoked, [server].external_url, the org lookup
// that names a workspace's git remote and the hub's logger; the other
// sandbox packages register their delete and purge hooks on the service.
// POST /workspaces/:slug/sandboxes creates a sandbox through the service's
// create.
//
// With sandboxes on, the provisioner runs as background work: it brings each
// created sandbox up at the runtime until it is ready or failed, woken by a
// create and every second on the hub's clock, and a start hook after the
// runtime check reports the stored sandboxes' states and wakes it, so that
// it takes up what a stopped process left. The stop cancels it without
// changing a record.
//
// The parts composed after this one reach the feature through
// Composition.Sandbox; a test replaces the runtime client with
// SetSandboxRuntime, and a dependency of the stored sandboxes with
// AdjustSandbox, and parleyd passes the metrics registry with
// SetSandboxRecorder.
func composeSandbox(c *Composition) {
	svc := sandbox.NewService()
	c.sandbox = svc
	c.sandboxRuntime = openShellRuntime
	if err := sandbox.Register(c.registry); err != nil {
		c.errs = append(c.errs, err)
	}
	c.AddSchemaStep(sandbox.SchemaStep)
	c.AddStartHook(sandboxRecordsHook, func(_ context.Context, rt *Runtime) error {
		git := gitFor(c.env)
		d := sandbox.Deps{
			Store: rt.Store, Now: rt.Now, Events: c.Events(), Recorder: c.sandboxRecorder,
			Repos: func(ctx context.Context, slug string) (*gitrepo.Repo, error) {
				return git.Open(ctx, c.workspaces.RepoPath(slug))
			},
			ResolveSecrets: c.Secrets().ResolveSecrets,
			Mint:           c.access.MintBound, Revoke: identity.RevokeTokenByID, ExternalURL: svc.ExternalURL(),
			OrgSlug: func(ctx context.Context, id string) (string, error) {
				org, err := identity.OrgByID(ctx, rt.Store.Main(), id)
				return org.Slug, err
			},
			Logger: rt.Logger,
		}
		for _, adjust := range c.sandboxAdjusters {
			adjust(&d)
		}
		svc.Bind(d)
		return nil
	})
	c.addConfigCheck(svc.Configure)
	c.AddRoute(svc.ProfilesRoute())
	c.AddRoute(svc.SandboxesRoute())
	c.AddRoute(svc.CreateRoute())
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
	c.AddStartHook(sandboxProvisionerHook, func(ctx context.Context, _ *Runtime) error {
		if !svc.Enabled() {
			return nil
		}
		return svc.ResumeProvisioning(ctx)
	})
	c.AddBackgroundWork(sandboxProvisionerWork, sandboxProvisionerGrace, func(ctx context.Context, _ *Runtime) {
		svc.RunProvisioner(ctx)
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

// AdjustSandbox adds fn to the functions that change the dependencies of
// the stored sandboxes after the start hook has built them and before it
// binds them, in the order they were added. Tests replace a collaborator
// with it.
func (c *Composition) AdjustSandbox(fn func(*sandbox.Deps)) {
	c.sandboxAdjusters = append(c.sandboxAdjusters, fn)
}

// SetSandboxRecorder sets the recorder the sandbox feature and its runtime
// client report their metrics to; nil records nothing.
func (c *Composition) SetSandboxRecorder(rec sandbox.Recorder) { c.sandboxRecorder = rec }
