package secret

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/scrub"
	"github.com/agent-fox-dev/parley/internal/store"
)

// Scope is the kind of owner a secret belongs to; with the owner's id it
// names the one set of secrets an operation reads or changes.
type Scope string

// The secret scopes.
const (
	// ScopeUser holds a user's own secrets; its id is the user's id.
	ScopeUser Scope = "user"
	// ScopeOrg holds an organization's secrets; its id is the org's id.
	ScopeOrg Scope = "org"
	// ScopeWorkspace holds a workspace's secrets; its id is the workspace's
	// slug.
	ScopeWorkspace Scope = "workspace"
)

// The limits every secret is held to, by the routes and by hub code alike.
const (
	// MaxEntries is the most secrets one scope holds.
	MaxEntries = 100
	// MaxKeyLen is the longest key, in characters.
	MaxKeyLen = 255
	// MaxValueSize is the largest value, in bytes.
	MaxValueSize = 262144
)

// SchemaStep creates the secret table in the transactional store. A row
// holds the key as its owner spelled it, the key with A to Z lowered, which
// every lookup, the order of a list and the uniqueness within a scope use,
// the sealed value and the id of the key that sealed it, and its timestamps
// in nanoseconds since the Unix epoch in UTC, whole seconds. The value is
// never stored in the clear.
var SchemaStep = store.Step{
	Name:   "secrets",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
CREATE TABLE secrets (
	id         INTEGER PRIMARY KEY,
	scope      TEXT    NOT NULL CHECK (scope IN ('user', 'org', 'workspace')),
	scope_id   TEXT    NOT NULL,
	key        TEXT    NOT NULL,
	key_lower  TEXT    NOT NULL,
	sealed     BLOB    NOT NULL,
	key_id     TEXT    NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	UNIQUE (scope, scope_id, key_lower)
);
CREATE INDEX secrets_key_id ON secrets (key_id);`)
		return err
	},
}

// Emitter writes a hub event inside the transaction of the change it
// reports; *event.Component is one. The routes write their secret events
// through it.
type Emitter interface {
	Emit(ctx context.Context, tx *sql.Tx, in event.Input) (event.Record, error)
}

// Deps are what the component is built from.
type Deps struct {
	// Store is the transactional store the secrets live in.
	Store *store.Store
	// Keys are the operator's keys; without a current key nothing is stored.
	Keys Keys
	// Now is the hub's clock; nil is time.Now.
	Now func() time.Time
	// Scrubber is the hub's credential scrubber, which learns every value
	// Get returns so that no later log line or message carries it; nil
	// registers nothing.
	Scrubber *scrub.Scrubber
	// Events writes the events of the secret routes.
	Events Emitter
}

// Component is the one place the hub reads, stores and removes secrets, so
// that the routes and hub code hold them to the same rules. The hub creates
// it when it is composed and binds it with Bind once the stores are open;
// New does both at once. Every operation that changes secrets runs inside a
// transaction of the store the caller owns, writes no event and leaves the
// caller to commit; the store runs those transactions one at a time, which
// makes a count, a uniqueness check and an insert of one call atomic.
type Component struct {
	deps Deps
}

// New returns a component bound to d.
func New(d Deps) *Component {
	c := &Component{}
	c.Bind(d)
	return c
}

// Bind sets the component's dependencies.
func (c *Component) Bind(d Deps) {
	if d.Now == nil {
		d.Now = time.Now
	}
	c.deps = d
}

// Available reports whether a current key is configured, so that secrets can
// be stored.
func (c *Component) Available() bool { return c.deps.Keys.Current != nil }

// Entry is one secret to create.
type Entry struct {
	Key   string
	Value string
}

// Item is what may be told about a stored secret: never its value.
type Item struct {
	// Key is the key as it was first stored.
	Key       string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Position is the item's place in the order of a list, which ListPage takes
// to continue after it.
func (i Item) Position() string { return asciiLower(i.Key) }

// errNotBound is returned by a read made before Bind.
var errNotBound = errors.New("the secret component is not bound to a store")

// querier is a transaction or the store's database.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// CheckKey returns an error matching ErrInvalid, whose message names the key,
// unless key has 1 to MaxKeyLen characters, each an ASCII letter, a digit or
// '_', and does not begin with a digit.
func CheckKey(key string) error {
	if key == "" || len(key) > MaxKeyLen {
		return invalid("key %q must have 1 to %d characters", key, MaxKeyLen)
	}
	for i := 0; i < len(key); i++ {
		b := key[i]
		switch {
		case 'A' <= b && b <= 'Z', 'a' <= b && b <= 'z', b == '_':
		case '0' <= b && b <= '9':
			if i == 0 {
				return invalid("key %q must not begin with a digit", key)
			}
		default:
			return invalid("key %q may hold only ASCII letters, digits and _", key)
		}
	}
	return nil
}

// CheckValue returns an error matching ErrInvalid, whose message names the
// key, when value is longer than MaxValueSize bytes.
func CheckValue(key, value string) error {
	if len(value) > MaxValueSize {
		return invalid("the value of key %q is longer than %d bytes", key, MaxValueSize)
	}
	return nil
}

func invalid(format string, args ...any) error {
	return &keyError{kind: ErrInvalid, msg: fmt.Sprintf(format, args...)}
}

// Get returns the value of the secret of exactly that scope whose key equals
// name ignoring ASCII case, read from the store on every call: found is
// false, with no error, when the scope holds no such key. A value that
// cannot be opened returns an error matching ErrUnreadable, never found
// false, so the hub does not act as if the secret were missing. A non-empty
// value is registered with the scrubber before it is returned.
func (c *Component) Get(ctx context.Context, scope Scope, scopeID, name string) (value string, found bool, err error) {
	if c.deps.Store == nil {
		return "", false, errNotBound
	}
	var key, keyID string
	var sealed []byte
	err = c.deps.Store.Main().QueryRowContext(ctx,
		`SELECT key, key_id, sealed FROM secrets WHERE scope = ? AND scope_id = ? AND key_lower = ?`,
		scope, scopeID, asciiLower(name)).Scan(&key, &keyID, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read secret: %w", err)
	}
	value, err = Open(c.deps.Keys, entryBinding(string(scope), scopeID, key), Sealed{KeyID: keyID, Bytes: sealed})
	if err != nil {
		return "", true, fmt.Errorf("secret %q %w", key, ErrUnreadable)
	}
	if c.deps.Scrubber != nil {
		c.deps.Scrubber.Add(value)
	}
	return value, true, nil
}

// Put stores a secret inside tx: it creates the entry, or overwrites the
// value of the entry whose key matches name ignoring ASCII case, which keeps
// its key as first spelled and its creation time. It returns an error
// matching ErrInvalid for a key or a value outside the limits, ErrUnavailable
// without a current key, and ErrLimitReached when a new entry would exceed
// MaxEntries; it writes nothing then.
func (c *Component) Put(ctx context.Context, tx *sql.Tx, scope Scope, scopeID, name, value string) error {
	if err := CheckKey(name); err != nil {
		return err
	}
	if err := CheckValue(name, value); err != nil {
		return err
	}
	if !c.Available() {
		return ErrUnavailable
	}
	if _, err := c.overwrite(ctx, tx, scope, scopeID, name, value); !errors.Is(err, ErrNotFound) {
		return err
	}
	n, err := count(ctx, tx, scope, scopeID)
	if err != nil {
		return err
	}
	if n >= MaxEntries {
		return ErrLimitReached
	}
	return c.insert(ctx, tx, scope, scopeID, Entry{Key: name, Value: value}, c.now())
}

// Delete removes the secret of exactly that scope whose key equals name
// ignoring ASCII case inside tx, without opening its value, so it works
// without a key and on a value no key opens. It reports whether one existed.
func (c *Component) Delete(ctx context.Context, tx *sql.Tx, scope Scope, scopeID, name string) (bool, error) {
	_, err := c.DeleteEntry(ctx, tx, scope, scopeID, name)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// RemoveScope deletes every secret of one scope inside tx, as the deletion
// of its owner does, and nothing else.
func (c *Component) RemoveScope(ctx context.Context, tx *sql.Tx, scope Scope, scopeID string) error {
	return removeScope(ctx, tx, scope, scopeID)
}

// RemoveWorkspace deletes every secret of the workspace slug inside tx. It
// is the remover of the workspace's owned data that runs when a workspace is
// deleted or a task fork expires; it needs no key and no component.
func RemoveWorkspace(ctx context.Context, tx *sql.Tx, slug string) error {
	return removeScope(ctx, tx, ScopeWorkspace, slug)
}

func removeScope(ctx context.Context, tx *sql.Tx, scope Scope, scopeID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM secrets WHERE scope = ? AND scope_id = ?`, scope, scopeID); err != nil {
		return fmt.Errorf("remove the secrets of %s %s: %w", scope, scopeID, err)
	}
	return nil
}

// CreateEntries creates every entry inside tx or none, with one creation
// time, and returns them in the order given. It returns an error matching
// ErrInvalid for an entry outside the limits or two keys equal ignoring
// ASCII case, ErrUnavailable without a current key, ErrLimitReached when the
// scope would hold more than MaxEntries secrets, counting keys it already
// holds, and then an error matching ErrConflict whose message names the
// first key, as given, that the scope already holds ignoring ASCII case.
func (c *Component) CreateEntries(ctx context.Context, tx *sql.Tx, scope Scope, scopeID string, entries []Entry) ([]Item, error) {
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if err := CheckKey(e.Key); err != nil {
			return nil, err
		}
		if err := CheckValue(e.Key, e.Value); err != nil {
			return nil, err
		}
		lower := asciiLower(e.Key)
		if seen[lower] {
			return nil, invalid("key %q is given more than once", e.Key)
		}
		seen[lower] = true
	}
	if !c.Available() {
		return nil, ErrUnavailable
	}
	n, err := count(ctx, tx, scope, scopeID)
	if err != nil {
		return nil, err
	}
	if n+len(entries) > MaxEntries {
		return nil, ErrLimitReached
	}
	for _, e := range entries {
		if _, err := find(ctx, tx, scope, scopeID, e.Key); err == nil {
			return nil, &keyError{kind: ErrConflict, msg: fmt.Sprintf("key %q already exists", e.Key)}
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	now := c.now()
	items := make([]Item, len(entries))
	for i, e := range entries {
		if err := c.insert(ctx, tx, scope, scopeID, e, now); err != nil {
			return nil, err
		}
		items[i] = Item{Key: e.Key, CreatedAt: now, UpdatedAt: now}
	}
	return items, nil
}

// UpdateEntry seals a new value for the entry whose key matches name ignoring
// ASCII case inside tx, without opening the old one, and returns the entry.
// It returns an error matching ErrInvalid for a value over the limit,
// ErrUnavailable without a current key, and then an error matching
// ErrNotFound, whose message names name, when the scope holds no such key.
func (c *Component) UpdateEntry(ctx context.Context, tx *sql.Tx, scope Scope, scopeID, name, value string) (Item, error) {
	if err := CheckValue(name, value); err != nil {
		return Item{}, err
	}
	if !c.Available() {
		return Item{}, ErrUnavailable
	}
	return c.overwrite(ctx, tx, scope, scopeID, name, value)
}

// DeleteEntry removes the entry whose key matches name ignoring ASCII case
// inside tx, without opening its value, and returns what it was. It returns
// an error matching ErrNotFound, whose message names name, when the scope
// holds no such key.
func (c *Component) DeleteEntry(ctx context.Context, tx *sql.Tx, scope Scope, scopeID, name string) (Item, error) {
	var it Item
	var created, updated int64
	err := tx.QueryRowContext(ctx,
		`DELETE FROM secrets WHERE scope = ? AND scope_id = ? AND key_lower = ? RETURNING key, created_at, updated_at`,
		scope, scopeID, asciiLower(name)).Scan(&it.Key, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, notFound(name)
	}
	if err != nil {
		return Item{}, fmt.Errorf("delete secret: %w", err)
	}
	it.CreatedAt, it.UpdatedAt = fromNanos(created), fromNanos(updated)
	return it, nil
}

// ListPage returns at most limit entries of a scope in the order of their
// keys with A to Z lowered, beginning after the item whose Position is after,
// or at the first when after is empty. It reads no value.
func (c *Component) ListPage(ctx context.Context, scope Scope, scopeID, after string, limit int) ([]Item, error) {
	if c.deps.Store == nil {
		return nil, errNotBound
	}
	rows, err := c.deps.Store.Main().QueryContext(ctx, `
SELECT key, created_at, updated_at FROM secrets
WHERE scope = ? AND scope_id = ? AND key_lower > ?
ORDER BY key_lower LIMIT ?`, scope, scopeID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	defer rows.Close()
	items := []Item{}
	for rows.Next() {
		var it Item
		var created, updated int64
		if err := rows.Scan(&it.Key, &created, &updated); err != nil {
			return nil, fmt.Errorf("list secrets: %w", err)
		}
		it.CreatedAt, it.UpdatedAt = fromNanos(created), fromNanos(updated)
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	return items, nil
}

// Count returns the number of secrets a scope holds.
func (c *Component) Count(ctx context.Context, scope Scope, scopeID string) (int, error) {
	if c.deps.Store == nil {
		return 0, errNotBound
	}
	return count(ctx, c.deps.Store.Main(), scope, scopeID)
}

// overwrite seals value for the existing entry matching name and returns the
// entry, or an error matching ErrNotFound.
func (c *Component) overwrite(ctx context.Context, tx *sql.Tx, scope Scope, scopeID, name, value string) (Item, error) {
	it, err := find(ctx, tx, scope, scopeID, name)
	if err != nil {
		return Item{}, err
	}
	s, err := Seal(c.deps.Keys, entryBinding(string(scope), scopeID, it.Key), value)
	if err != nil {
		return Item{}, err
	}
	it.UpdatedAt = c.now()
	if _, err := tx.ExecContext(ctx,
		`UPDATE secrets SET sealed = ?, key_id = ?, updated_at = ? WHERE scope = ? AND scope_id = ? AND key_lower = ?`,
		s.Bytes, s.KeyID, it.UpdatedAt.UnixNano(), scope, scopeID, asciiLower(it.Key)); err != nil {
		return Item{}, fmt.Errorf("update secret: %w", err)
	}
	return it, nil
}

// insert seals and stores one new entry created at now.
func (c *Component) insert(ctx context.Context, tx *sql.Tx, scope Scope, scopeID string, e Entry, now time.Time) error {
	s, err := Seal(c.deps.Keys, entryBinding(string(scope), scopeID, e.Key), e.Value)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO secrets (scope, scope_id, key, key_lower, sealed, key_id, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		scope, scopeID, e.Key, asciiLower(e.Key), s.Bytes, s.KeyID, now.UnixNano(), now.UnixNano()); err != nil {
		return fmt.Errorf("store secret: %w", err)
	}
	return nil
}

// now is the clock's time in UTC cut to whole seconds, the precision every
// timestamp of a secret is answered with, so a stored time reads back equal.
func (c *Component) now() time.Time { return c.deps.Now().UTC().Truncate(time.Second) }

// find returns the entry matching name, or an error matching ErrNotFound.
func find(ctx context.Context, q querier, scope Scope, scopeID, name string) (Item, error) {
	var it Item
	var created, updated int64
	err := q.QueryRowContext(ctx,
		`SELECT key, created_at, updated_at FROM secrets WHERE scope = ? AND scope_id = ? AND key_lower = ?`,
		scope, scopeID, asciiLower(name)).Scan(&it.Key, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, notFound(name)
	}
	if err != nil {
		return Item{}, fmt.Errorf("read secret: %w", err)
	}
	it.CreatedAt, it.UpdatedAt = fromNanos(created), fromNanos(updated)
	return it, nil
}

// count returns the number of secrets of a scope.
func count(ctx context.Context, q querier, scope Scope, scopeID string) (int, error) {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM secrets WHERE scope = ? AND scope_id = ?`,
		scope, scopeID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count secrets: %w", err)
	}
	return n, nil
}

func notFound(name string) error {
	return &keyError{kind: ErrNotFound, msg: fmt.Sprintf("key %q not found", name)}
}

func fromNanos(ns int64) time.Time { return time.Unix(0, ns).UTC() }
