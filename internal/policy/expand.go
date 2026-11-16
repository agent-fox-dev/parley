package policy

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/change"
)

// ExpandApprovals is the member approvals that include adds to the change
// ch: the verdicts on its current version, newest first, as GET
// …/approvals?version=<current> lists them. The change component hands an
// expander no transaction, so it reads from the store as that route does.
// It has the signature of change.Expander.
func (c *Component) ExpandApprovals(ctx context.Context, ch change.Change) (any, error) {
	return c.listApprovals(ctx, c.deps.Store.Main(), ch, verdictFilter{Version: ch.Version})
}

// ExpandChecks is the member checks that include adds to the change ch:
// the results that apply to its current version, by name, as GET
// …/checks lists them, read from the store as ExpandApprovals reads. It
// has the signature of change.Expander.
func (c *Component) ExpandChecks(ctx context.Context, ch change.Change) (any, error) {
	return c.listChecks(ctx, c.deps.Store.Main(), ch, ch.Version)
}
