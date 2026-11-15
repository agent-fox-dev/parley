package gitserver

import "fmt"

// The reasons a pushed branch has no change, as its warning line words
// them.
const (
	reasonDirectPush = "branch is protected and was pushed directly"
	reasonInternal   = "internal error"
)

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
