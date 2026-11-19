package hub

import (
	"context"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitserver"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime/openshell"
	"github.com/agent-fox-dev/parley/internal/secret"
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
	// sandboxLogCollectorWork is the name of the log collector's background
	// work, and sandboxLogCollectorGrace how long the stop waits for it:
	// each follower returns once its runtime stream ends with its context.
	sandboxLogCollectorWork  = "sandbox_log_collector"
	sandboxLogCollectorGrace = 5 * time.Second
	// sandboxPolicyCollectorWork is the name of the policy collector's
	// background work, and sandboxPolicyCollectorGrace how long the stop
	// waits for it: each follower returns once its runtime stream ends with
	// its context.
	sandboxPolicyCollectorWork  = "sandbox_policy_collector"
	sandboxPolicyCollectorGrace = 5 * time.Second
	// sandboxLifecycleWork is the name of the lifecycle loop's background
	// work, and sandboxLifecycleGrace how long the stop waits for it: a
	// pass returns before its next runtime call once its context ends.
	sandboxLifecycleWork  = "sandbox_lifecycle"
	sandboxLifecycleGrace = 15 * time.Second
	// sandboxReconcileHook is the name of the start hook that runs the
	// first pass of the reconciliation with the runtime, and
	// sandboxReconcileWork that of the background work that runs the
	// others; sandboxReconcileGrace is how long the stop waits for it: a
	// pass returns before its next runtime call once its context ends.
	sandboxReconcileHook  = "sandbox_reconcile"
	sandboxReconcileWork  = "sandbox_reconcile"
	sandboxReconcileGrace = 15 * time.Second
	// sandboxLogRetentionTask is the name of the maintenance task that
	// deletes the stored log lines past their retention.
	sandboxLogRetentionTask = "sandbox_log_retention"
	// sandboxRecordCleanupTask is the name of the maintenance task that
	// removes the records of sandboxes deleted more than a day ago.
	sandboxRecordCleanupTask = "sandbox_record_cleanup"
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
// MintBound, identity.RevokeTokenByID and identity.SetTokenExpiryByID,
// through which a sandbox's hub credential is minted, revoked and given a
// new expiry, [server].external_url, the org lookup
// that names a workspace's git remote, the user lookup that gives a
// checkout its commit identity and the hub's logger; the other
// sandbox packages register their delete and purge hooks on the service.
// POST /workspaces/:slug/sandboxes creates a sandbox through the service's
// create, and so does POST /workspaces/:slug/forks with a sandbox member,
// in the fork's transaction: with sandboxes on, the same start hook
// registers the service as the fork component's sandbox component, and
// with sandboxes off it registers nothing, so that such a fork create is
// refused 404 sandboxes_disabled; GET /workspaces/:slug/sandboxes/:id reads
// one, waiting on the service's transitions until the hub begins to stop at
// the latest, and GET
// /workspaces/:slug/sandboxes lists a workspace's, with its forks' that the
// access component's readable set holds; GET /sandboxes lists the stored
// sandboxes of every workspace through the service's source. The routes
// that use a sandbox, POST exec, PUT and GET files, GET logs and GET attach
// under /workspaces/:slug/sandboxes/:id, exist only through the service's
// gate (Service.UseRoutes). POST
// /workspaces/:slug/sandboxes/:id/checkpoint saves a ready sandbox's
// working directory to its workspace through the service's checkpoint,
// POST /workspaces/:slug/sandboxes/:id/stop stops a ready sandbox through
// the service's stop, DELETE /workspaces/:slug/sandboxes/:id deletes a
// sandbox through the service's delete, which runs the delete hooks, and
// PATCH /workspaces/:slug/sandboxes/:id sets a sandbox's ttl, purpose and
// labels through the service's extend.
//
// A workspace's deletion has the service delete the sandboxes of the
// workspace and its forks first, by the system, with reason
// workspace_deleted and no checkpoint, and its removal transaction, and that
// of a fork, remove their sandbox records, running the purge hooks, and a
// fork's checkpoint refs; an archive has the service stop the ready
// sandboxes of the workspace and its forks with reason archived. These are
// registered on the workspaces' collaborator set while the hub composes,
// whether sandboxes are on or not. With sandboxes on, the start hook also
// registers the service as the fork component's sandbox lifecycle, through
// which a fork's removal deletes its sandboxes and its early expiry waits
// while one is provisioning or ready. The maintenance task
// sandbox_record_cleanup removes the records of sandboxes deleted more than
// 24 hours ago, running the purge hooks.
//
// The output of each ready sandbox's main process and its policy log are
// stored in a table of their own: with sandboxes on, the log collector runs
// as background work, the maintenance task sandbox_log_retention deletes
// the lines past [sandbox].log_retention and those of removed records, and
// a purge hook deletes a sandbox's lines with its record.
//
// With sandboxes on, the provisioner runs as background work: it brings each
// created sandbox up at the runtime until it is ready or failed, woken by a
// create and every second on the hub's clock, and a start hook after the
// runtime check reports the stored sandboxes' states and wakes it, so that
// it takes up what a stopped process left. The stop cancels it without
// changing a record. The service's restore is the restorer of the
// provisionings: it seeds the working directory of a sandbox created from a
// checkpoint, and that of a started sandbox whose working directory the
// runtime lost. The lifecycle loop runs as background work too: every
// minute on the hub's clock it stops the sandboxes idle for longer than
// their idle timeout and deletes those whose expires_at has come, which it
// also does at a timer set to the earliest expires_at.
//
// The records are reconciled with the runtime once at start, in a start
// hook after the runtime check that never keeps the hub from starting, and
// then every [sandbox].reconcile_interval on the hub's clock as background
// work: the end of a ready sandbox's main process is recorded, a sandbox
// the runtime lost is marked failed, the runtime's pause is followed, what
// the runtime holds of no live sandbox is deleted, and the idle stops and
// expiries that came due are done.
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
	c.AddSchemaStep(sandbox.LogSchemaStep)
	c.AddSchemaStep(sandbox.RuleSchemaStep)
	c.AddSchemaStep(sandbox.ApprovedRuleSchemaStep)
	c.AddSchemaStep(sandbox.DenialSchemaStep)
	c.AddSchemaStep(sandbox.ProposalSchemaStep)
	registerSandboxCollaborators(c, svc)
	c.AddStartHook(sandboxRecordsHook, func(_ context.Context, rt *Runtime) error {
		git := gitFor(c.env)
		d := sandbox.Deps{
			Store: rt.Store, Now: rt.Now, Events: c.Events(), Recorder: c.sandboxRecorder,
			Repos: func(ctx context.Context, slug string) (*gitrepo.Repo, error) {
				return git.Open(ctx, c.workspaces.RepoPath(slug))
			},
			ResolveSecrets: func(ctx context.Context, q access.Querier, slug string, names []string) (secret.SecretResolution, error) {
				if q == nil {
					return c.Secrets().ResolveSecrets(ctx, slug, names)
				}
				return c.Secrets().ResolveSecretsIn(ctx, q, slug, names)
			},
			Mint: c.access.MintBound, Revoke: identity.RevokeTokenByID, SetTokenExpiry: identity.SetTokenExpiryByID,
			ExternalURL: svc.ExternalURL(), MaxCheckpointSize: gitserver.MaxPushSizeFrom(rt.Config),
			OrgSlug: func(ctx context.Context, id string) (string, error) {
				org, err := identity.OrgByID(ctx, rt.Store.Main(), id)
				return org.Slug, err
			},
			User: func(ctx context.Context, id string) (identity.User, error) {
				return identity.UserByID(ctx, rt.Store.Main(), id)
			},
			Logger:   rt.Logger,
			Readable: c.access.Readable, Shutdown: c.ShuttingDown(),
		}
		for _, adjust := range c.sandboxAdjusters {
			adjust(&d)
		}
		svc.Bind(d)
		// Without the component the fork create refuses a sandbox 404
		// sandboxes_disabled, as a hub with sandboxes off must.
		if svc.Enabled() {
			c.Forks().RegisterSandbox(svc)
			c.Forks().RegisterSandboxes(svc)
		}
		return nil
	})
	c.addConfigCheck(svc.Configure)
	c.AddRoute(svc.ProfilesRoute())
	c.AddRoute(svc.SandboxesRoute())
	c.AddRoute(svc.CreateRoute())
	c.AddRoute(svc.GetRoute())
	c.AddRoute(svc.ListRoute())
	c.AddRoute(svc.CheckpointRoute())
	c.AddRoute(svc.StopRoute())
	c.AddRoute(svc.StartRoute())
	c.AddRoute(svc.DeleteRoute())
	c.AddRoute(svc.ExtendRoute())
	for _, rt := range svc.UseRoutes() {
		c.AddRoute(rt)
	}
	// Registered while the hub composes, so that a test's source, registered
	// after, replaces it; the list is refused while sandboxes are off.
	svc.RegisterSource(svc.StoredSource())
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
	composeSandboxWork(c, svc)
}

// registerSandboxCollaborators registers what the sandbox service takes
// part in while the hub composes: the purge hook of the stored logs, the
// delete hook that withdraws a deleted sandbox's open proposals, the purge
// hook that removes a sandbox's proposals, reported denials and approved
// rules with its record, the service as its own restorer of checkpoints, the network-policy feature's
// workspace rule provider and policy composer (RegisterNetworkPolicy),
// through which a create records the workspace sandbox rules and every
// provisioning composes the policy value, and the service on the
// workspaces' collaborator set (ADR 07), through which a workspace's
// archive and deletion reach its sandboxes whether sandboxes are on or not:
// with them off the records are still deleted and removed, and no runtime
// is called.
func registerSandboxCollaborators(c *Composition, svc *sandbox.Service) {
	svc.RegisterPurgeHook(sandbox.PurgeLogLines)
	svc.RegisterDeleteHook(sandbox.WithdrawProposals)
	svc.RegisterPurgeHook(sandbox.PurgeNetworkPolicy)
	svc.RegisterRestorer(svc)
	svc.RegisterNetworkPolicy()
	td := c.workspaces.Teardown()
	td.RegisterSandboxRemover(svc)
	td.RegisterArchiveObserver(svc)
	td.RegisterRemover(svc.RemoveWorkspaceRecords)
}

// composeSandboxWork adds the sandbox feature's work besides the requests,
// after the runtime check's start hook: the start hooks that resume the
// provisionings an earlier process left and run the first reconcile pass,
// the background work of the provisioner, the log collector, the policy
// collector, the idle and expiry passes and the reconciliation, and the
// maintenance tasks of log retention and record cleanup. With sandboxes off
// each returns at once.
func composeSandboxWork(c *Composition, svc *sandbox.Service) {
	c.AddStartHook(sandboxProvisionerHook, func(ctx context.Context, _ *Runtime) error {
		if !svc.Enabled() {
			return nil
		}
		return svc.ResumeProvisioning(ctx)
	})
	c.AddStartHook(sandboxReconcileHook, func(ctx context.Context, _ *Runtime) error {
		if !svc.Enabled() || svc.Client() == nil {
			return nil
		}
		return svc.ReconcileAtStart(ctx)
	})
	c.AddBackgroundWork(sandboxProvisionerWork, sandboxProvisionerGrace, func(ctx context.Context, _ *Runtime) {
		svc.RunProvisioner(ctx)
	})
	c.AddBackgroundWork(sandboxLogCollectorWork, sandboxLogCollectorGrace, func(ctx context.Context, _ *Runtime) {
		svc.CollectLogs(ctx)
	})
	c.AddBackgroundWork(sandboxPolicyCollectorWork, sandboxPolicyCollectorGrace, func(ctx context.Context, _ *Runtime) {
		svc.CollectPolicy(ctx, c.sandboxPolicyCollector)
	})
	c.AddBackgroundWork(sandboxLifecycleWork, sandboxLifecycleGrace, func(ctx context.Context, _ *Runtime) {
		svc.RunLifecycle(ctx)
	})
	c.AddBackgroundWork(sandboxReconcileWork, sandboxReconcileGrace, func(ctx context.Context, _ *Runtime) {
		svc.RunReconcile(ctx)
	})
	c.AddMaintenanceTask(sandboxLogRetentionTask, func(ctx context.Context, _ *Runtime) error {
		return svc.RetainLogs(ctx)
	})
	c.AddMaintenanceTask(sandboxRecordCleanupTask, func(ctx context.Context, _ *Runtime) error {
		return svc.CleanupRecords(ctx)
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

// SetSandboxPolicyCollector sets the options of the policy collector the
// hub runs with sandboxes on: shorter intervals for a run that cannot wait
// for the defaults, or Hold for a test that drives a collector's passes
// itself.
func (c *Composition) SetSandboxPolicyCollector(o sandbox.PolicyCollectorOptions) {
	c.sandboxPolicyCollector = o
}

// SetSandboxRecorder sets the recorder the sandbox feature and its runtime
// client report their metrics to; nil records nothing.
func (c *Composition) SetSandboxRecorder(rec sandbox.Recorder) { c.sandboxRecorder = rec }
