package fork

import (
	"encoding/json"
	"errors"

	"github.com/agent-fox-dev/parley/internal/workspace"
)

// TTLField is the handler of ttl in an update of a fork, a
// workspace.ForkFieldHandler: a string ResolveTTL accepts sets the fork's
// expires_at to the time of the request plus ttl, later or earlier than
// before; null, a number or a boolean is refused, and so is a string
// ResolveTTL refuses, with its answer. It is a method because it needs the
// hub's clock and the [forks] limits, which the handler's signature does
// not pass; the component reads them when it is called.
func (c *Component) TTLField(_ workspace.Workspace, raw json.RawMessage) (workspace.ForkFieldWrite, error) {
	var ttl string
	if isNull(raw) || json.Unmarshal(raw, &ttl) != nil {
		return nil, errors.New("must be a string")
	}
	at, err := ResolveTTL(ttl, c.deps.Now(), c.deps.Limits)
	if err != nil {
		return nil, err
	}
	// In nanoseconds, the unit of every time column of the workspace record.
	return writeColumn("expires_at", at.UnixNano()), nil
}
