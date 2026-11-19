package cli

import (
	"github.com/spf13/cobra"
)

// newAuditCommand builds parley audit: the query over the hub's events and
// the agents' run events.
func newAuditCommand() *cobra.Command {
	return group("audit", "Query who did what on the hub and in the agents' runs",
		scmRead{
			command: command{
				Name:        "query",
				Description: "List hub events and agent run events, newest first, one page at a time",
				Path:        "/audit",
			},
			// Every value is sent as given, so that the hub's own error says
			// what is wrong with one it refuses.
			query: []queryFlag{
				{name: "source", param: "source", usage: "only the hub events (hub) or only the agent run events (agent); both when not given"},
				{name: "workspace", param: "workspace", usage: "only the items of this workspace slug"},
				{name: "types", param: "types", usage: "only the items of these comma-separated types; an entry ending in * matches every type that begins with the text before it"},
				{name: "session", param: "session_id", usage: "only the items of this agent session id"},
				{name: "run-id", param: "run_id", usage: "only the run events of this agent run"},
				{name: "actor-id", param: "actor_id", usage: "only the hub events of this actor"},
				{name: "resource-type", param: "resource_type", usage: "only the hub events about this type of resource"},
				{name: "resource-id", param: "resource_id", usage: "only the hub events about this resource"},
				{name: "action", param: "action", usage: "only the hub events of this action"},
				{name: "since", param: "since", usage: "only the items at or after this RFC 3339 time"},
				{name: "until", param: "until", usage: "only the items before this RFC 3339 time"},
				limitFlag, cursorFlag,
			},
		}.build(),
	)
}
