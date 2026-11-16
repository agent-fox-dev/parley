package cli

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// landRequestTimeout bounds the request of parley change land: the hub
// may hold the land for up to 60 seconds before it answers that the
// attempt still runs, so the default of 30 seconds would abandon a land
// the hub is about to answer.
const landRequestTimeout = 70 * time.Second

// landStates are the final statuses of a land attempt: landed ends a wait
// well, the others end it with wait_failed.
var (
	landSuccesses = []string{"landed"}
	landFailures  = []string{"conflict", "blocked", "failed"}
)

// newChangeLandCommand builds parley change land: one land of a change, or
// of its stack, which with --wait polls the attempt the hub answers until
// it is final.
func newChangeLandCommand() *cobra.Command {
	land := declare(command{
		Name:        "land",
		Description: "Land a change, or its whole stack, on its target branch, and with --wait wait until the land attempt is final",
		Method:      http.MethodPost,
		Path:        changePath + "/land",
		Args:        []argSpec{slugArg, numberArg},
		Auth:        authUser,
		Timeout:     landRequestTimeout,
		Flags: func(fs *pflag.FlagSet) {
			fs.Bool("stack", false, "land the change with every unlanded change it is stacked on, bottom first")
			fs.String("expected-version", "", "land only if the change's current version is this one")
			fs.String("override", "", "the reason to land past the landing policy, where the workspace allows it")
			fs.String("session", "", "the id of the agent session the land belongs to")
		},
		Wait: &waitFor{
			Start: func(c *call) (apiRequest, error) {
				return apiRequest{method: http.MethodPost, path: routePath(c.decl.Path, c.args), body: landBody(c.cmd.Flags())}, nil
			},
			Poll: func(c *call, record []byte) (apiRequest, error) {
				var attempt struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(record, &attempt); err != nil || attempt.ID == "" {
					return apiRequest{}, clientError("cannot read the id of the land attempt from the hub's answer")
				}
				return apiRequest{method: http.MethodGet, path: routePath("/workspaces/:slug/lands/:id", map[string]string{"slug": c.args["slug"], "id": attempt.ID})}, nil
			},
			Classify: byState("status", landSuccesses, landFailures),
		},
	})
	return checkFlags(withNumbers(land, true, "expected-version"), func(cmd *cobra.Command) error {
		if fs := cmd.Flags(); fs.Changed("override") {
			if reason, _ := fs.GetString("override"); strings.TrimSpace(reason) == "" {
				return usageError("%s takes --override <reason>, a reason that is not blank", cmd.CommandPath())
			}
		}
		return nil
	})
}

// landBody is the body of a land: a member for each flag given and none
// for a flag not given, so that the hub's default applies; without a flag
// it is {}. It carries no wait query, so the hub holds the call for its
// default time.
func landBody(fs *pflag.FlagSet) map[string]any {
	body := map[string]any{}
	if fs.Changed("stack") {
		body["stack"], _ = fs.GetBool("stack")
	}
	if fs.Changed("expected-version") {
		value, _ := fs.GetString("expected-version")
		// withNumbers accepted it, so it parses.
		body["expected_version"], _ = strconv.ParseInt(value, 10, 64)
	}
	if fs.Changed("override") {
		reason, _ := fs.GetString("override")
		body["override"] = map[string]string{"reason": reason}
	}
	if fs.Changed("session") {
		body["session_id"], _ = fs.GetString("session")
	}
	return body
}

// newChangeLandsCommand builds parley change lands: the land attempts of
// one change.
func newChangeLandsCommand() *cobra.Command {
	return withNumbers(scmRead{
		command: command{Name: "lands", Description: "List the land attempts of a change, newest first", Path: changePath + "/lands", Args: []argSpec{slugArg, numberArg}},
	}.build(), true)
}

// newLandCommand builds parley land: the land attempts of a workspace.
func newLandCommand() *cobra.Command {
	return group("land", "List and show the land attempts of a workspace",
		scmRead{
			command: command{Name: "list", Description: "List the land attempts of a workspace, newest first", Path: "/workspaces/:slug/lands", Args: []argSpec{slugArg}},
			query: []queryFlag{
				{name: "status", param: "status", kind: listFlag, usage: "only the attempts in this status: running, landed, conflict, blocked or failed; repeatable"},
				{name: "target", param: "target_branch", usage: "only the attempts that land on this branch"},
				{name: "since", param: "since", usage: "only the attempts created at or after this RFC 3339 time"},
				limitFlag, cursorFlag,
			},
		}.build(),
		scmRead{
			command: command{Name: "show", Description: "Show one land attempt", Path: "/workspaces/:slug/lands/:land-id", Args: []argSpec{slugArg, {Name: "land-id", Required: true}}},
		}.build(),
	)
}
