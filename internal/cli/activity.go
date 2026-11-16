package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// overlapFailOn is --fail-on-overlap of a command whose answer, check,
// tells whether other work touches the same paths.
func overlapFailOn(description string, check func(result []byte) (bool, string)) []failOn {
	return []failOn{{Name: "overlap", Description: description, Check: check}}
}

// newActivityCommand builds parley activity: the in-flight changes, live
// claims and recently landed changes that touch a list of path globs.
func newActivityCommand() *cobra.Command {
	return withNumbers(scmRead{
		command: command{
			Name:        "activity",
			Description: "Show the draft and open changes, the active claims and the recently landed changes that touch files matching path globs",
			Path:        "/workspaces/:slug/activity",
			Args:        []argSpec{slugArg},
			Flags: func(fs *pflag.FlagSet) {
				// It fails only for a flag the set does not hold, and the
				// query flags are added before this.
				_ = cobra.MarkFlagRequired(fs, "paths")
			},
			FailOn: overlapFailOn("exit 3 when a change or a claim touches the paths", activityOverlaps),
		},
		query: []queryFlag{
			{name: "paths", param: "paths", usage: "the path globs, separated by commas; * within a segment, ** across segments"},
			{name: "target", param: "target", usage: "the target branch the changes and claims are on (default: the workspace's default branch)"},
			{name: "since", param: "since", usage: "list the changes landed after this commit id or RFC 3339 time (default: 24 hours ago)"},
			{name: "exclude-session", param: "exclude_session", usage: "leave out the changes and claims of the agent session with this id"},
			{name: "exclude-change", param: "exclude_change", usage: "leave out the change with this number"},
		},
	}.build(), false, "exclude-change")
}

// activityOverlaps is the condition of parley activity --fail-on-overlap:
// the answer's changes or claims hold an item. Landed changes do not count:
// they are no longer in flight.
func activityOverlaps(result []byte) (bool, string) {
	var answer struct {
		Changes []json.RawMessage `json:"changes"`
		Claims  []json.RawMessage `json:"claims"`
	}
	if json.Unmarshal(result, &answer) != nil || len(answer.Changes)+len(answer.Claims) == 0 {
		return false, ""
	}
	return true, fmt.Sprintf("%d changes and %d claims touch the paths", len(answer.Changes), len(answer.Claims))
}
