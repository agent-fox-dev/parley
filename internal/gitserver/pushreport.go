package gitserver

import (
	"fmt"

	"github.com/agent-fox-dev/parley/internal/server"
)

// The reasons a pushed branch has no change, as its warning line words
// them.
const (
	reasonDirectPush = "branch is protected and was pushed directly"
	reasonInternal   = "internal error"
	reasonOwnTarget  = "branch is its own target"
	reasonNoScope    = "token lacks scope changes:write"
)

// refusalReason words the change feature's refusal to open a change onto
// the branch target, "" when unknown. A reason ends with the refusal's
// error type, so that an agent can match on it; the refusals an agent can
// act on from its push are worded for it, any other is its own message.
func refusalReason(refusal *server.Error, target string) string {
	switch t := refusal.Type(); {
	case t == "target_not_found" && target != "":
		return fmt.Sprintf("target branch %s does not exist (%s)", target, t)
	case t == "nothing_to_land" && target != "":
		return fmt.Sprintf("no commits ahead of %s (%s)", target, t)
	case t == "merge_commits_unsupported":
		return "merge commits are not supported (" + t + ")"
	case t == "intent_required":
		return "intent is required; push with -o parley.intent=<text> or -o parley.draft (" + t + ")"
	}
	return refusal.Message() + " (" + refusal.Type() + ")"
}

// Line is the side-band line that tells the git client the outcome, without
// its newline, or "" for an outcome the client is not told of. A change is
// named by the slug of the workspace it belongs to, a fork's parent for a
// push to a fork, and never by a URL.
func (o Outcome) Line() string {
	switch o.Kind {
	case OutcomeVersion:
		return fmt.Sprintf("change %s#%d version %d", o.Workspace, o.Number, o.Version)
	case OutcomeOpened:
		if o.Draft {
			return fmt.Sprintf("change %s#%d version 1 (opened, draft)", o.Workspace, o.Number)
		}
		return fmt.Sprintf("change %s#%d version 1 (opened)", o.Workspace, o.Number)
	case OutcomeWarning:
		return "warning: no change for " + o.Branch + ": " + o.Reason
	}
	return ""
}
