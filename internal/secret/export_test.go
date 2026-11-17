package secret

import "context"

// FailVarReadsOf makes every later read of the variables of scope that a
// resolution makes fail with err, as a store that cannot be read would; the
// other scopes are read from the store as before.
func (c *Component) FailVarReadsOf(scope Scope, err error) {
	read := varReader(func(ctx context.Context, q rowsQuerier, s Scope, id string) ([]Variable, error) {
		if s == scope {
			return nil, err
		}
		return scopeVars(ctx, q, s, id)
	})
	c.readVars.Store(&read)
}
