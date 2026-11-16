package cli

import (
	"net/http"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// claimPath is the route of one claim, which renew patches and release
// deletes.
const claimPath = "/workspaces/:slug/claims/:id"

// claimIDArg is the <id> argument: the id of a claim.
var claimIDArg = argSpec{Name: "id", Required: true}

// defaultRenewTTL is the lifetime a renewal without --ttl gives a claim
// from now on.
const defaultRenewTTL = "1h"

// claimCreateFlags are the optional string flags of parley claim create,
// each sent as one body member when given.
var claimCreateFlags = []struct {
	flag, member, usage string
}{
	{"target", "target_branch", "the branch the claimed work will land on (default: the workspace's default branch)"},
	{"ttl", "ttl", "how long the claim lasts: a whole number followed by s, m, h or d (default: the hub's, 1h unless configured)"},
	{"session", "session_id", "the id of the agent session the claim belongs to"},
}

// newClaimCommand builds parley claim: the advisory, expiring statements
// that an agent intends to work on some paths.
func newClaimCommand() *cobra.Command {
	return group("claim", "Create, list, renew and release claims: advisory, expiring statements that you intend to work on some paths",
		newClaimCreateCommand(),
		scmRead{
			command: command{
				Name:        "list",
				Description: "List the claims of a workspace, the active ones unless --state says otherwise, newest first",
				Path:        "/workspaces/:slug/claims",
				Args:        []argSpec{slugArg},
			},
			query: []queryFlag{
				{name: "state", param: "state", usage: "only the claims in this state: active, released, expired or fulfilled (default: active)"},
				{name: "session", param: "session_id", usage: "only the claims of the agent session with this id"},
				{name: "path", param: "path", usage: "only the claims whose globs can match a file this glob matches"},
				limitFlag, cursorFlag,
			},
		}.build(),
		declare(command{
			Name:        "renew",
			Description: "Extend the lifetime of an active claim, counted from now",
			Method:      http.MethodPatch,
			Path:        claimPath,
			Args:        []argSpec{slugArg, claimIDArg},
			Auth:        authUser,
			Flags: func(fs *pflag.FlagSet) {
				fs.String("ttl", defaultRenewTTL, "how long the claim lasts from now: a whole number followed by s, m, h or d")
			},
			Run: func(c *call) error {
				ttl, _ := c.cmd.Flags().GetString("ttl")
				return sendWith(c, nil, map[string]string{"ttl": ttl})
			},
		}),
		declare(command{
			Name:        "release",
			Description: "Release a claim, ending it before it expires",
			Method:      http.MethodDelete,
			Path:        claimPath,
			Args:        []argSpec{slugArg, claimIDArg},
			Auth:        authUser,
		}),
	)
}

// newClaimCreateCommand builds parley claim create: a claim on path globs
// with an intent, answered with the claim and the current activity on its
// paths.
func newClaimCreateCommand() *cobra.Command {
	return declare(command{
		Name:        "create",
		Description: "Claim path globs of a workspace for the work you intend, and show what else touches them",
		Method:      http.MethodPost,
		Path:        "/workspaces/:slug/claims",
		Args:        []argSpec{slugArg},
		Auth:        authUser,
		Flags: func(fs *pflag.FlagSet) {
			fs.String("paths", "", "the path globs claimed, separated by commas; * within a segment, ** across segments")
			fs.String("intent", "", "what you intend to do on the paths")
			// Each fails only for a flag the set does not hold, and the
			// lines above add them.
			_ = cobra.MarkFlagRequired(fs, "paths")
			_ = cobra.MarkFlagRequired(fs, "intent")
			for _, f := range claimCreateFlags {
				fs.String(f.flag, "", f.usage)
			}
		},
		Run: createClaim,
	})
}

// createClaim is the run step of parley claim create. Its body holds the
// globs of --paths as a list, the intent and a member for each other flag
// given, and none for a flag not given, so that the hub's default applies.
func createClaim(c *call) error {
	fs := c.cmd.Flags()
	paths, _ := fs.GetString("paths")
	intent, _ := fs.GetString("intent")
	body := map[string]any{"paths": strings.Split(paths, ","), "intent": intent}
	for _, f := range claimCreateFlags {
		if fs.Changed(f.flag) {
			body[f.member], _ = fs.GetString(f.flag)
		}
	}
	return sendWith(c, nil, body)
}
