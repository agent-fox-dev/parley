package cli

import (
	"net/http"
	"slices"
	"strings"

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

// patchUpdateFlags are the flags of parley patch update other than
// --position, each sent as one body member when given. update needs at least
// one of them or --position.
var patchUpdateFlags = []struct{ flag, member, usage string }{
	{"status", "status", "the patch's new status: active, disabled, conflict or merged_upstream"},
	{"description", "description", "what the patch does; an empty text clears it"},
	{"upstream-pr", "upstream_pr_url", "the URL of the upstream pull request the patch is proposed in; an empty text clears it"},
}

// patchArg is the argument that names one patch by its id.
var patchArg = argSpec{Name: "id", Required: true}

// newPatchCommand builds parley patch: the patches of a carry-patch
// workspace, the branches the hub applies, in order, on top of upstream.
func newPatchCommand() *cobra.Command {
	return group("patch", "Register, read, update, remove, restore and reorder the patches of a carry-patch workspace: the branches applied, in order, on top of upstream",
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
			Args:        []argSpec{slugArg, patchArg},
			Auth:        authUser,
		}),
		newPatchUpdateCommand(),
		declare(command{
			Name:        "remove",
			Description: "Remove a patch from the order; its branch stays, and restore brings it back",
			Method:      http.MethodDelete,
			Path:        patchesRoute + "/:id",
			Args:        []argSpec{slugArg, patchArg},
			Auth:        authUser,
			Confirm: func(c *call) any {
				return struct {
					Status  string `json:"status"`
					PatchID string `json:"patch_id"`
				}{"removed", c.args[patchArg.Name]}
			},
		}),
		declare(command{
			Name:        "restore",
			Description: "Bring a removed patch back as the last patch of the order",
			Method:      http.MethodPost,
			Path:        patchesRoute + "/:id/restore",
			Args:        []argSpec{slugArg, patchArg},
			Auth:        authUser,
		}),
		declare(command{
			Name:        "reorder",
			Description: "Put every patch of a carry-patch workspace in the order of the ids given",
			Method:      http.MethodPost,
			Path:        patchesRoute + "/reorder",
			Args:        []argSpec{slugArg, {Name: "id", Required: true, Repeats: true}},
			Auth:        authUser,
			Run:         reorderPatches,
		}),
	)
}

// newPatchUpdateCommand builds parley patch update: a PATCH of a patch whose
// body holds a member for each flag given and none for a flag not given, so
// that an empty --description is sent and a member not named is left as it
// is.
func newPatchUpdateCommand() *cobra.Command {
	update := declare(command{
		Name:        "update",
		Description: "Change the status, position, description or upstream pull request of a patch",
		Method:      http.MethodPatch,
		Path:        patchesRoute + "/:id",
		Args:        []argSpec{slugArg, patchArg},
		Auth:        authUser,
		Flags: func(fs *pflag.FlagSet) {
			fs.Int("position", 0, "the patch's new position in the order, from 1")
			for _, f := range patchUpdateFlags {
				fs.String(f.flag, "", f.usage)
			}
		},
		Run: updatePatch,
	})
	return checkFlags(update, func(cmd *cobra.Command) error {
		changes := []string{"status", "position", "description", "upstream-pr"}
		if slices.ContainsFunc(changes, cmd.Flags().Changed) {
			return nil
		}
		return usageError("%s needs at least one of --%s", cmd.CommandPath(), strings.Join(changes, ", --"))
	})
}

// updatePatch is the run step of parley patch update. The status is sent as
// typed: the hub knows the statuses and answers one it does not.
func updatePatch(c *call) error {
	fs := c.cmd.Flags()
	body := map[string]any{}
	if fs.Changed("position") {
		body["position"], _ = fs.GetInt("position")
	}
	for _, f := range patchUpdateFlags {
		if fs.Changed(f.flag) {
			body[f.member], _ = fs.GetString(f.flag)
		}
	}
	return sendWith(c, nil, body)
}

// reorderPatches is the run step of parley patch reorder. It takes the ids
// from the command line, not from the joined argument, so that each stays
// one id in the order given.
func reorderPatches(c *call) error {
	ids := c.cmd.Flags().Args()[1:]
	return sendWith(c, nil, map[string][]string{"patch_ids": ids})
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
