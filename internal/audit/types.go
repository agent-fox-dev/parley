package audit

import (
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
)

// actorTypes are the actor types of a hub event, the values the actor_type
// filter takes.
var actorTypes = []string{event.ActorAdminToken, event.ActorAPIKey, event.ActorPAT, event.ActorForge, event.ActorSystem}

// filters are the filters of an audit query, each nil (or empty) when the
// query does not give it. Every filter given narrows the list: they combine
// with AND, and the entries of types with OR.
type filters struct {
	// source is the one source the list holds, sourceHub or sourceAgent;
	// "" for both.
	source string
	// workspace, types, sessionID, since and until apply to both sources.
	// sessionID is in lower case, the form ids are stored in.
	workspace, sessionID *string
	types                []string
	since, until         *time.Time

	// The fields of a hub event only.
	actorID, actorType, resourceType, resourceID, action *string
	// The fields of a run event only.
	runID, nodeID, severity *string
}

// named is a filter by its parameter's name.
type named struct {
	name  string
	value *string
}

// hubOnly are the filters on the fields only a hub event has.
func (f filters) hubOnly() []named {
	return []named{
		{"actor_id", f.actorID}, {"actor_type", f.actorType}, {"resource_type", f.resourceType},
		{"resource_id", f.resourceID}, {"action", f.action},
	}
}

// agentOnly are the filters on the fields only a run event has.
func (f filters) agentOnly() []named {
	return []named{{"run_id", f.runID}, {"node_id", f.nodeID}, {"severity", f.severity}}
}

// firstGiven is the name of the first filter of fs the query gives, "" when
// it gives none.
func firstGiven(fs []named) string {
	for _, f := range fs {
		if f.value != nil {
			return f.name
		}
	}
	return ""
}
