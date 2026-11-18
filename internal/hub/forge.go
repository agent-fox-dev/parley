package hub

import (
	"context"
	"time"

	"github.com/agent-fox-dev/parley/internal/forge"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// forgeHook is the name of the start hook that binds the forge component,
// and deliveryPruningTask that of the maintenance task that deletes the
// webhook deliveries past their retention, under which package maintenance
// orders it.
const (
	forgeHook           = "forge"
	deliveryPruningTask = "forge_delivery_pruning"
)

// composeForge adds the forge component, the hub's client of the forge
// that hosts a workspace's remote repository. Its start hook binds it to
// the workspace records in the store, the workspace settings, the secret
// component, through which a forge credential is read and registered with
// the scrubber, and the access rule, which decides a task fork's caller's
// access to the fork's parent; and to the options of its client: the hub's
// environment, from which the proxy variables are read (the process
// environment never is, docs/adr/06-load-the-secret-keys-before-the-stores-open.md);
// the client takes the hub's clock. It hands it the event component, which
// records the forge writes, and the hub's scrubber, logger and external URL
// too; and the store, the secret component as the writer of the webhook
// secret, and the event component as the log the webhook routes record
// their events in, inside the same store transaction, as the recording of
// a webhook delivery does with the delivery and its events. The part adds
// the schema step of the delivery table, and mounts the webhook ingress
// outside the REST API: a forge sends no bearer credential, must not be
// rate limited as a caller, and is held to the ingress's own body limit.
// Each maintenance pass deletes the deliveries received more than
// forge.DeliveryRetention before it; a store error is returned to the loop,
// which logs it, and the next pass deletes them. A workspace's deliveries
// are removed in the transaction that deletes it (docs/adr/07-register-the-teardown-collaborators-on-the-workspace-service.md),
// and the newest one's time is the diagnostic route's webhook
// last_delivery_at.
//
// The component exists from the composition on (Forge), so that the forge
// routes, which the part registers, are built with it; a later part and tests change its dependencies
// with AdjustForge.
func composeForge(c *Composition) {
	comp := &forge.Component{}
	c.forge = comp
	c.AddSchemaStep(forge.DeliverySchemaStep)
	for _, rt := range comp.Routes() {
		c.AddRoute(rt)
	}
	c.Handle(forge.IngressPattern, comp.Ingress())
	c.AddMaintenanceTask(deliveryPruningTask, func(ctx context.Context, rt *Runtime) error {
		return forge.PruneDeliveries(ctx, rt.Store, rt.Now().Add(-forge.DeliveryRetention))
	})
	c.workspaces.Teardown().RegisterRemover(forge.RemoveDeliveries)
	c.AddStartHook(forgeHook, func(_ context.Context, rt *Runtime) error {
		db := rt.Store.Main()
		d := forge.Deps{
			Workspaces: func(ctx context.Context, slug string) (workspace.Workspace, bool, error) {
				return workspace.BySlug(ctx, db, slug)
			},
			Settings:     c.workspaces.Settings(),
			Secrets:      c.secrets,
			Access:       c.access,
			Client:       forge.ClientOptions{Env: c.env},
			Events:       c.Events(),
			Scrubber:     rt.Scrubber,
			Logger:       rt.Logger,
			Now:          rt.Now,
			ExternalURL:  rt.Config.Server.ExternalURL,
			Store:        rt.Store,
			SecretWriter: c.secrets,
			Log:          c.Events(),
		}
		for _, adjust := range c.forgeAdjusters {
			adjust(&d)
		}
		comp.Bind(d)
		comp.SetLastDeliveryProvider(func(ctx context.Context, slug string) (time.Time, bool, error) {
			return forge.LastDeliveryAt(ctx, db, slug)
		})
		return nil
	})
}

// Forge returns the hub's forge component, which serves once its start
// hook has run.
func (c *Composition) Forge() *forge.Component { return c.forge }

// AdjustForge adds fn to the functions that change the forge component's
// dependencies after the start hook has built them and before it binds
// them, in the order they were added. Tests replace a collaborator with it,
// such as the client's transport or the secret reader.
func (c *Composition) AdjustForge(fn func(*forge.Deps)) {
	c.forgeAdjusters = append(c.forgeAdjusters, fn)
}
