package cli

import (
	"net/http"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// patchesRoute is the route of a workspace's patches, which add posts to and
// list reads.
const patchesRoute = "/workspaces/:slug/patches"

// patchAddFlags are the optional flags of parley patch add other than
// --position, each sent as one body member when given.
var patchAddFlags = []struct {
	flag, member, usage string
	// isSwitch marks a boolean flag; the others take a text.
	isSwitch bool
}{
	{"upstream-pr", "upstream_pr_url", "the URL of the upstream pull request the patch is proposed in", false},
	{"description", "description", "what the patch does", false},
	{"skip-branch-check", "skip_branch_check", "register the patch without checking that its branch exists", true},
	{"if-not-exists", "if_not_exists", "answer the existing patch of the branch instead of failing", true},
}

// newPatchCommand builds parley patch: the patches of a carry-patch
// workspace, the branches the hub applies, in order, on top of upstream.
func newPatchCommand() *cobra.Command {
	return group("patch", "Register and read the patches of a carry-patch workspace: the branches applied, in order, on top of upstream",
		declare(command{
			Name:        "add",
			Description: "Register a branch as a patch of a carry-patch workspace, at the end of the order unless --position says where",
			Method:      http.MethodPost,
			Path:        patchesRoute,
			Args:        []argSpec{slugArg},
			Auth:        authUser,
			Flags: func(fs *pflag.FlagSet) {
				fs.String("branch", "", "the branch of the patch")
				// It fails only for a flag the set does not hold, and the
				// line above adds it.
				_ = cobra.MarkFlagRequired(fs, "branch")
				fs.Int("position", 0, "the position of the patch in the order, from 1 (default: after the last patch)")
				for _, f := range patchAddFlags {
					if f.isSwitch {
						fs.Bool(f.flag, false, f.usage)
					} else {
						fs.String(f.flag, "", f.usage)
					}
				}
			},
			Run: addPatch,
		}),
		scmRead{
			command: command{
				Name:        "list",
				Description: "List the patches of a carry-patch workspace in their order",
				Path:        patchesRoute,
				Args:        []argSpec{slugArg},
			},
			query: []queryFlag{
				{name: "include-deleted", param: "include_deleted", kind: switchFlag, sends: "true", usage: "list the deleted patches too, after the others"},
			},
		}.build(),
		declare(command{
			Name:        "show",
			Description: "Show one patch of a carry-patch workspace",
			Method:      http.MethodGet,
			Path:        patchesRoute + "/:id",
			Args:        []argSpec{slugArg, {Name: "id", Required: true}},
			Auth:        authUser,
		}),
	)
}

// addPatch is the run step of parley patch add. Its body holds the branch
// and a member for each other flag given, an empty text included, and none
// for a flag not given, so that the hub's default applies.
func addPatch(c *call) error {
	fs := c.cmd.Flags()
	branch, _ := fs.GetString("branch")
	body := map[string]any{"branch_name": branch}
	if fs.Changed("position") {
		body["position"], _ = fs.GetInt("position")
	}
	for _, f := range patchAddFlags {
		if !fs.Changed(f.flag) {
			continue
		}
		if f.isSwitch {
			body[f.member], _ = fs.GetBool(f.flag)
		} else {
			body[f.member], _ = fs.GetString(f.flag)
		}
	}
	return sendWith(c, nil, body)
}
