package secret

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/store"
)

// VarSchemaStep creates the variable table in the transactional store. A
// variable is a readable value: the table holds it in the clear, so that it
// is stored and answered with no encryption key configured. A row holds the
// key as its owner spelled it, the key with A to Z lowered, which every
// lookup, the order of a list and the uniqueness within a scope use, so that
// the store itself refuses a second entry for a key ignoring case, and its
// timestamps in nanoseconds since the Unix epoch in UTC, whole seconds.
var VarSchemaStep = store.Step{
	Name:   "variables",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
CREATE TABLE variables (
	id         INTEGER PRIMARY KEY,
	scope      TEXT    NOT NULL CHECK (scope IN ('user', 'org', 'workspace')),
	scope_id   TEXT    NOT NULL,
	key        TEXT    NOT NULL,
	key_lower  TEXT    NOT NULL,
	value      TEXT    NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	UNIQUE (scope, scope_id, key_lower)
)`)
		return err
	},
}

// Variable is a stored variable: unlike a secret, its value is told.
type Variable struct {
	// Key is the key as it was first stored.
	Key       string
	Value     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Position is the variable's place in the order of a list, which a list
// takes to continue after it.
func (v Variable) Position() string { return asciiLower(v.Key) }

// CreateVars creates every variable inside tx or none, with one creation
// time, and returns them in the order given. Variables are held to the
// limits of secrets and need no encryption key. It returns an error
// matching ErrInvalid for an entry outside the limits or two keys equal
// ignoring ASCII case, ErrLimitReached when the scope would hold more than
// MaxEntries variables, counting keys it already holds, and then an error
// matching ErrConflict whose message names the first key, as given, that
// the scope already holds ignoring ASCII case. Secrets are not counted.
func (c *Component) CreateVars(ctx context.Context, tx *sql.Tx, scope Scope, scopeID string, entries []Entry) ([]Variable, error) {
	if err := checkEntries(entries); err != nil {
		return nil, err
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM variables WHERE scope = ? AND scope_id = ?`,
		scope, scopeID).Scan(&n); err != nil {
		return nil, fmt.Errorf("count variables: %w", err)
	}
	if n+len(entries) > MaxEntries {
		return nil, ErrLimitReached
	}
	for _, e := range entries {
		if _, err := findVar(ctx, tx, scope, scopeID, e.Key); err == nil {
			return nil, conflict(e.Key)
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	now := c.now()
	vars := make([]Variable, len(entries))
	for i, e := range entries {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO variables (scope, scope_id, key, key_lower, value, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
			scope, scopeID, e.Key, asciiLower(e.Key), e.Value, now.UnixNano(), now.UnixNano()); err != nil {
			return nil, fmt.Errorf("store variable: %w", err)
		}
		vars[i] = Variable{Key: e.Key, Value: e.Value, CreatedAt: now, UpdatedAt: now}
	}
	return vars, nil
}

// ListVars returns at most limit variables of a scope, with their values,
// in the order of their keys with A to Z lowered, beginning after the
// variable whose Position is after, or at the first when after is empty.
func (c *Component) ListVars(ctx context.Context, scope Scope, scopeID, after string, limit int) ([]Variable, error) {
	if c.deps.Store == nil {
		return nil, errNotBound
	}
	return listVars(ctx, c.deps.Store.Main(), scope, scopeID, after, limit)
}

// rowsQuerier is what a read of several variables goes through: a
// transaction or the store's database.
type rowsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// listVars reads through q at most limit variables of a scope, every one
// when limit is negative, in the order of their keys with A to Z lowered,
// beginning after the variable whose Position is after.
func listVars(ctx context.Context, q rowsQuerier, scope Scope, scopeID, after string, limit int) ([]Variable, error) {
	rows, err := q.QueryContext(ctx, `
SELECT key, value, created_at, updated_at FROM variables
WHERE scope = ? AND scope_id = ? AND key_lower > ?
ORDER BY key_lower LIMIT ?`, scope, scopeID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list variables: %w", err)
	}
	defer rows.Close()
	vars := []Variable{}
	for rows.Next() {
		var v Variable
		var created, updated int64
		if err := rows.Scan(&v.Key, &v.Value, &created, &updated); err != nil {
			return nil, fmt.Errorf("list variables: %w", err)
		}
		v.CreatedAt, v.UpdatedAt = fromNanos(created), fromNanos(updated)
		vars = append(vars, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list variables: %w", err)
	}
	return vars, nil
}

// UpdateVar replaces, inside tx, the value of the variable whose key
// matches name ignoring ASCII case, and returns the variable: it keeps the
// key as first stored and its creation time, and is updated now. It
// returns an error matching ErrInvalid for a value over the limit, and then
// an error matching ErrNotFound, whose message names name, when the scope
// holds no such variable.
func (c *Component) UpdateVar(ctx context.Context, tx *sql.Tx, scope Scope, scopeID, name, value string) (Variable, error) {
	if err := CheckValue(name, value); err != nil {
		return Variable{}, err
	}
	v, err := findVar(ctx, tx, scope, scopeID, name)
	if err != nil {
		return Variable{}, err
	}
	v.Value, v.UpdatedAt = value, c.now()
	if _, err := tx.ExecContext(ctx,
		`UPDATE variables SET value = ?, updated_at = ? WHERE scope = ? AND scope_id = ? AND key_lower = ?`,
		value, v.UpdatedAt.UnixNano(), scope, scopeID, asciiLower(name)); err != nil {
		return Variable{}, fmt.Errorf("update variable: %w", err)
	}
	return v, nil
}

// DeleteVar removes, inside tx, the variable whose key matches name
// ignoring ASCII case, and returns what it was. It returns an error
// matching ErrNotFound, whose message names name, when the scope holds no
// such variable.
func (c *Component) DeleteVar(ctx context.Context, tx *sql.Tx, scope Scope, scopeID, name string) (Variable, error) {
	var v Variable
	var created, updated int64
	err := tx.QueryRowContext(ctx,
		`DELETE FROM variables WHERE scope = ? AND scope_id = ? AND key_lower = ? RETURNING key, value, created_at, updated_at`,
		scope, scopeID, asciiLower(name)).Scan(&v.Key, &v.Value, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Variable{}, notFound(name)
	}
	if err != nil {
		return Variable{}, fmt.Errorf("delete variable: %w", err)
	}
	v.CreatedAt, v.UpdatedAt = fromNanos(created), fromNanos(updated)
	return v, nil
}

// findVar returns the variable matching name ignoring ASCII case, or an
// error matching ErrNotFound.
func findVar(ctx context.Context, q querier, scope Scope, scopeID, name string) (Variable, error) {
	var v Variable
	var created, updated int64
	err := q.QueryRowContext(ctx,
		`SELECT key, value, created_at, updated_at FROM variables WHERE scope = ? AND scope_id = ? AND key_lower = ?`,
		scope, scopeID, asciiLower(name)).Scan(&v.Key, &v.Value, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Variable{}, notFound(name)
	}
	if err != nil {
		return Variable{}, fmt.Errorf("read variable: %w", err)
	}
	v.CreatedAt, v.UpdatedAt = fromNanos(created), fromNanos(updated)
	return v, nil
}
