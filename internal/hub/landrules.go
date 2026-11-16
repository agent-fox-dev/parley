package hub

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
func composeLandRules(c *Composition) {
	c.versionFiles = c.Land().VersionFiles()
}
