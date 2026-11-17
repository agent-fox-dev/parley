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

// RecordSecretReads makes every later read a secret resolution makes report
// its scope, scope id and name to record before it reads the store as
// before.
func (c *Component) RecordSecretReads(record func(scope Scope, scopeID, name string)) {
	read := secretReader(func(ctx context.Context, s Scope, id, name string) (string, bool, error) {
		record(s, id, name)
		return c.Get(ctx, s, id, name)
	})
	c.readSecret.Store(&read)
}
