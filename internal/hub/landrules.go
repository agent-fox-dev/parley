package hub

import "github.com/agent-fox-dev/parley/internal/land"

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
func composeLandRules(c *Composition) {
	c.versionFiles = c.Land().VersionFiles()
	paths := c.Land().PathGuard()
	c.Changes().RegisterVersionGuard(paths)
	c.Land().AddRequestGuard(paths)
	c.Land().AddRequestGuard(c.Land().OverrideGuard())
	c.Land().RegisterOverrideRule(land.PendingOverride{})
}
