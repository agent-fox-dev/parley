package hub

import (
	"context"
	"errors"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/land"
	"github.com/agent-fox-dev/parley/internal/store"
)

// autoLandHook is the name of the start hook that binds the automatic
// lander and runs its start-up, after the land component's landRecoverHook;
// autoLandWork the name of the background work that runs it, and
// autoLandGrace how long the stop waits for it, which lets a start it is
// making commit.
const (
	autoLandHook  = "auto-land"
	autoLandWork  = "auto_land"
	autoLandGrace = 5 * time.Second
)

// composeLandRules adds the rules that surround landing once the change
// component, the land component and the policy's evaluation exist, each
// registered on the instance of the collaborator it plugs into
// (docs/adr/08-register-the-push-collaborators-on-the-ref-writer.md).
//
// It sets the push rule engine's version-files lookup to the land
// component's (VersionFiles), which composeGitServer binds to the engine
// in its start hook: an update that adds no commit of its own, a git push
// or an API ref move onto commits the repository already has, which moves
// the source branch of a draft or open change or opens a change, is held
// to a path-scoped token's paths by the files of the version it makes,
// read from the repository against the merge base with the change's
// target, or the target an open defaults to. The lookup reads the change
// records, the workspace records and the repositories the land component
// is bound to, so it answers once the land component's start hook has
// run, before the hub serves its first push.
//
// It registers the land component's path guard twice. As the change
// component's version guard, asked by a caller's restack once the replay
// is computed, with or without resolutions, and before the source branch
// moves: a version with a file outside the caller's token paths is
// refused 403 path_not_allowed, and nothing moves. The restacks the hub
// makes on its own are not asked. And as a request guard of the land
// call, asked after the caller is admitted and the body read and before
// the land's preconditions: a change the attempt would cover, the changes
// below it for a stack, with a file outside the paths is refused the same
// way, and no attempt is recorded. Neither looks at the policy: a token
// whose paths cover the change gets the answer any credential of its user
// would.
//
// It registers the override's two rules. The override guard, a request
// guard of the land call asked after the path guard, refuses an override,
// whatever the landing status of the changes, with 403
// override_not_allowed and no attempt recorded: from a PAT bound to a
// workspace or one of its forks, from anyone but the workspace's owner and
// the admin token, and from everyone while the workspace's
// land.allow_override is off. The override rule, asked by the land for a
// gate answer that is not ready on a request the guard let through, lets a
// pending change pass, one held back by approvals or checks, and never a
// blocked one; the land records the override only when it let a change
// pass. The policy's gate knows nothing of overrides.
//
// It adds the automatic lander, with the schema of its rows, as the change
// component's auto_land guard: a path-scoped credential may turn a
// change's auto_land on only when every file of its current version is
// within its paths, refused 403 path_not_allowed with nothing of the
// request applied otherwise; turning the flag on for a change whose flag
// was off makes a start of its land due, in the request's transaction, so
// that a change landable already is landed too; turning it off is never
// refused. The lander is bound in a start hook that runs after the land
// component recovered its running attempts, to the store, the land
// component, the change component, the event log, the user records, the
// access rule, the workspace records with their effective land settings,
// the repositories and the hub's clock, and it then runs the start
// procedure for every open change whose flag is on. As background work,
// off the request path and stopped with the hub, it follows the
// parley.change.ready announcements in the event log from its stored
// position and starts the lands that are due, every second: a land of the
// change alone, trigger auto_land, as the user who set the flag, if they
// may land it and its files are within the paths their credential had;
// otherwise the flag is cleared and a parley.change.land_fail by system
// tells why.
//
// The lander is the land component's final observer too, told of every
// attempt that ended, and it follows up those with trigger auto_land: an
// attempt that landed, ended conflict or was blocked counts the change's
// retries from zero and makes no start due, so that the change waits,
// with its flag on, to be announced landable again; one that failed makes
// a retry due 2 s later, the delay doubling with each retry made since the
// last attempt that did not fail, at most 5 min, and the eleventh failure
// in a row clears the flag instead. A retry that comes due for a change
// that is no longer open, landable or flagged is dropped and its count
// reset. The loop that follows the event log waits for the earliest due
// start when it comes sooner than the next second, and stops with the hub,
// leaving the due times stored; the next start of the hub makes those that
// came due at once, with the count stored. A change whose land is held
// back while the change below it, which its depends_on names, is not
// landed keeps its flag and is landed on its own once that change has
// landed, whoever landed it: the lander, told of the land, has the change
// announced landable again when it already sits on the landed commit,
// and otherwise starts it from the announcement of the version the hub's
// restack records. Tests change its dependencies with AdjustAutoLand.
func composeLandRules(c *Composition) {
	c.versionFiles = c.Land().VersionFiles()
	paths := c.Land().PathGuard()
	c.Changes().RegisterVersionGuard(paths)
	c.Land().AddRequestGuard(paths)
	c.Land().AddRequestGuard(c.Land().OverrideGuard())
	c.Land().RegisterOverrideRule(land.PendingOverride{})
	lander := &land.AutoLander{}
	c.autoLander = lander
	c.AddSchemaStep(land.AutoLandStep)
	c.Changes().RegisterAutoLandGuard(lander)
	c.Land().AddFinalObserver(lander.Finished)
	c.AddStartHook(autoLandHook, func(ctx context.Context, rt *Runtime) error {
		git := gitFor(c.env)
		d := land.AutoLandDeps{
			Store: rt.Store, Lands: c.Land(), Changes: c.Changes(), Events: c.Events(), Users: landUsers{rt.Store},
			Access: c.access, Workspaces: landWorkspaces{c.workspaces.Settings()},
			Repos: func(ctx context.Context, slug string) (*gitrepo.Repo, error) {
				return git.Open(ctx, c.workspaces.RepoPath(slug))
			},
			Now: rt.Now, Logger: rt.Logger,
		}
		for _, adjust := range c.autoLandAdjusters {
			adjust(&d)
		}
		lander.Bind(d)
		return lander.Recover(ctx)
	})
	c.AddBackgroundWork(autoLandWork, autoLandGrace, func(ctx context.Context, _ *Runtime) { lander.Run(ctx) })
}

// landUsers reads the user records for the automatic lander: a user who
// was deleted or is blocked may not act.
type landUsers struct{ store *store.Store }

func (u landUsers) User(ctx context.Context, id string) (string, bool, error) {
	user, err := identity.UserByID(ctx, u.store.Main(), id)
	switch {
	case errors.Is(err, identity.ErrNotFound):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	return user.Username, user.Status != identity.StatusBlocked, nil
}

// AutoLander returns the hub's automatic lander, which runs once its start
// hook has run.
func (c *Composition) AutoLander() *land.AutoLander { return c.autoLander }

// AdjustAutoLand adds fn to the functions that change the automatic
// lander's dependencies after the start hook has built them and before it
// binds them, in the order they were added.
func (c *Composition) AdjustAutoLand(fn func(*land.AutoLandDeps)) {
	c.autoLandAdjusters = append(c.autoLandAdjusters, fn)
}
