package cli

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// rebuildsRoute is the route of a workspace's rebuild jobs.
const rebuildsRoute = "/workspaces/:slug/rebuilds"

// rebuildArg is the argument that names one rebuild job by its id.
var rebuildArg = argSpec{Name: "id", Required: true}

// rebuildSubmitFlags are the flags of parley rebuild submit, each sent as
// one body member when given, with the only values each takes.
var rebuildSubmitFlags = []struct {
	flag, member, usage string
	oneOf               []string
}{
	{"strategy", "strategy", "rebase or merge (default: the workspace's rebuild_strategy setting)", []string{"rebase", "merge"}},
	{"fail-mode", "fail_mode", "fail_fast or continue (default: the workspace's rebuild_fail_mode setting)", []string{"fail_fast", "continue"}},
}

// rebuildStatuses are the statuses of a rebuild job.
var rebuildStatuses = []string{"queued", "running", "failed", "completed", "dead_letter", "cancelled"}

// newRebuildCommand builds parley rebuild: the rebuild jobs that recreate a
// carry-patch workspace's integration branch from upstream and its patches.
func newRebuildCommand() *cobra.Command {
	return group("rebuild", "Queue, list, read, cancel, requeue and roll back the rebuild jobs of a carry-patch workspace, and preview a rebuild",
		checkFlags(declare(command{
			Name:        "submit",
			Description: "Queue a rebuild of a carry-patch workspace's integration branch; with --wait, wait until it ends",
			Method:      http.MethodPost,
			Path:        "/workspaces/:slug/rebuild",
			Args:        []argSpec{slugArg},
			Auth:        authUser,
			Flags: func(fs *pflag.FlagSet) {
				for _, f := range rebuildSubmitFlags {
					fs.String(f.flag, "", f.usage)
				}
			},
			Wait: rebuildWait(submitRebuild),
		}), checkRebuildSubmit),
		scmRead{
			command: command{
				Name:        "list",
				Description: "List the rebuild jobs of a carry-patch workspace, the newest first",
				Path:        rebuildsRoute,
				Args:        []argSpec{slugArg},
			},
			query: []queryFlag{{
				name: "status", param: "status", oneOf: rebuildStatuses,
				usage: "only the jobs in this status: " + strings.Join(rebuildStatuses, ", "),
			}},
		}.build(),
		declare(command{
			Name:        "status",
			Description: "Show one rebuild job of a carry-patch workspace",
			Method:      http.MethodGet,
			Path:        rebuildsRoute + "/:id",
			Args:        []argSpec{slugArg, rebuildArg},
			Auth:        authUser,
		}),
		declare(command{
			Name:        "cancel",
			Description: "Cancel a queued rebuild job",
			Method:      http.MethodDelete,
			Path:        rebuildsRoute + "/:id",
			Args:        []argSpec{slugArg, rebuildArg},
			Auth:        authUser,
		}),
		declare(command{
			Name:        "requeue",
			Description: "Queue a dead-lettered rebuild job again; with --wait, wait until it ends",
			Method:      http.MethodPost,
			Path:        rebuildsRoute + "/:id/requeue",
			Args:        []argSpec{slugArg, rebuildArg},
			Auth:        authUser,
			Wait:        rebuildWait(nil),
		}),
		declare(command{
			Name:        "rollback",
			Description: "Move a carry-patch workspace's integration branch back to the head it had before a completed rebuild",
			Method:      http.MethodPost,
			Path:        rebuildsRoute + "/:id/rollback",
			Args:        []argSpec{slugArg, rebuildArg},
			Auth:        authUser,
		}),
		declare(command{
			Name:        "preview",
			Description: "Predict which patches of a carry-patch workspace a rebuild would apply, skip or stop at with a conflict, against the upstream as last fetched, changing nothing",
			Method:      http.MethodGet,
			Path:        "/workspaces/:slug/rebuild-preview",
			Args:        []argSpec{slugArg},
			Auth:        authUser,
			FailOn: []failOn{{
				Name:        "conflict",
				Description: "exit 3 when a patch would conflict, naming the conflicting branches on stderr",
				Check:       previewConflicts,
			}},
		}),
	)
}

// previewConflicts holds when a patch of a rebuild preview would conflict,
// and names the branch of every such patch, in the preview's order.
func previewConflicts(result []byte) (bool, string) {
	var preview struct {
		PatchResults []struct {
			BranchName string `json:"branch_name"`
			Status     string `json:"status"`
		} `json:"patch_results"`
	}
	// The document is the hub's answer, printed already; one this cannot
	// read names no conflict.
	_ = json.Unmarshal(result, &preview)
	var names []string
	for _, p := range preview.PatchResults {
		if p.Status == "would_conflict" {
			names = append(names, p.BranchName)
		}
	}
	if len(names) == 0 {
		return false, ""
	}
	return true, "would conflict with the upstream: " + strings.Join(names, ", ")
}

// rebuildWait is the wait of a command that starts a rebuild job, with
// start building its request (the command's Method and Path when nil): it
// reads the job until it is completed, dead_letter or cancelled. A failed
// job waits to retry, so it does not end the wait.
func rebuildWait(start func(c *call) (apiRequest, error)) *waitFor {
	return &waitFor{
		Start: start,
		Poll: func(c *call, record []byte) (apiRequest, error) {
			var job struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(record, &job); err != nil || job.ID == "" {
				return apiRequest{}, clientError("the hub's answer names no rebuild job")
			}
			path := "/workspaces/" + pathSegment(c.args[slugArg.Name]) + "/rebuilds/" + pathSegment(job.ID)
			return apiRequest{method: http.MethodGet, path: path}, nil
		},
		Classify: byState("status", []string{"completed"}, []string{"dead_letter", "cancelled"}),
	}
}

// submitRebuild is the request of parley rebuild submit. Its body holds a
// member for each flag given and none for a flag not given, so that the
// workspace's setting applies.
func submitRebuild(c *call) (apiRequest, error) {
	body := map[string]string{}
	for _, f := range rebuildSubmitFlags {
		if c.cmd.Flags().Changed(f.flag) {
			body[f.member], _ = c.cmd.Flags().GetString(f.flag)
		}
	}
	return apiRequest{method: c.decl.Method, path: routePath(c.decl.Path, c.args), body: body}, nil
}

// checkRebuildSubmit refuses, as a usage error, a value of --strategy or
// --fail-mode that the flag does not take, before any request.
func checkRebuildSubmit(cmd *cobra.Command) error {
	for _, f := range rebuildSubmitFlags {
		if !cmd.Flags().Changed(f.flag) {
			continue
		}
		if value, _ := cmd.Flags().GetString(f.flag); !slices.Contains(f.oneOf, value) {
			return usageError("%s takes --%s %s, got %q", cmd.CommandPath(), f.flag, strings.Join(f.oneOf, " or "), value)
		}
	}
	return nil
}
