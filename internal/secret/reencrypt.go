package secret

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
)

// reencryptBatch is the number of secrets the re-encryption reads at a time,
// so that a large store is converted without holding every row in memory.
const reencryptBatch = 100

// Converter re-seals the values a part of the hub keeps outside the secret
// table, such as the signing secret of an event webhook stored with
// SealValue, so that one maintenance task finishes a key rotation for every
// sealed value. It is given the operator's keys and converts the values
// under keys.Previous to keys.Current, converting nothing when either is
// absent; it returns how many values it converted and how many are still not
// under the current key (all of its values when there is no current key).
type Converter func(ctx context.Context, keys Keys) (converted, remaining int, err error)

// RegisterConverter adds fn to the converters the re-encryption runs after
// the secrets, in the order they were registered. A part registers its
// converter when it is composed, before the hub's maintenance loop starts.
func (c *Component) RegisterConverter(fn Converter) {
	c.converters = append(c.converters, fn)
}

// Reencrypt is the maintenance task that finishes a key rotation. With a
// current and a previous key configured it seals again under the current key
// every secret sealed under the previous one, with the binding of its entry,
// leaving its key and timestamps as they are and writing no event; a value
// that fails to open under the previous key stays as it is. Each value is
// written only while it still holds the sealed bytes the task read, so a
// value a request updated or deleted in the meantime keeps the request's
// result. Then it runs every registered converter and writes one info line,
// `secret re-encryption`, with converted, the values it and the converters
// re-sealed, and remaining, the values still not under the current key: the
// secrets whose key id is not the current key's, every secret when no
// current key is configured, plus what the converters report. The operator
// drops the previous key once remaining is 0.
//
// A store failure stops the task before the converters run and before the
// line is written, and returns the error, which the maintenance loop logs;
// the values converted until then stay converted. A converter's error
// returns after the line is written, with the counts the converters reported.
func (c *Component) Reencrypt(ctx context.Context, logger *slog.Logger) error {
	if c.deps.Store == nil {
		return errNotBound
	}
	return c.reencrypt(ctx, logger, table{c})
}

// reencrypt is Reencrypt reading and writing the secret table through rows.
func (c *Component) reencrypt(ctx context.Context, logger *slog.Logger, rows rowStore) error {
	keys := c.deps.Keys
	converted, err := c.resealSecrets(ctx, keys, rows)
	if err != nil {
		return err
	}
	remaining, err := c.notUnderCurrent(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, fn := range c.converters {
		n, left, err := fn(ctx, keys)
		converted += n
		remaining += left
		if err != nil {
			errs = append(errs, err)
		}
	}
	logger.Info("secret re-encryption", "converted", converted, "remaining", remaining)
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("re-encrypt sealed values: %w", err)
	}
	return nil
}

// resealSecrets converts the secrets under the previous key to the current
// key, batch by batch in the order of their ids, and returns how many it
// converted. It converts nothing unless both keys are configured and differ.
func (c *Component) resealSecrets(ctx context.Context, keys Keys, rows rowStore) (int, error) {
	if keys.Current == nil || keys.Previous == nil {
		return 0, nil
	}
	previous := KeyID(keys.Previous)
	if previous == KeyID(keys.Current) {
		return 0, nil
	}
	converted := 0
	var after int64
	for {
		batch, err := rows.underKey(ctx, previous, after, reencryptBatch)
		if err != nil {
			return converted, err
		}
		for _, r := range batch {
			after = r.id
			binding := entryBinding(r.scope, r.scopeID, r.key)
			value, err := Open(keys, binding, r.sealed)
			if err != nil {
				continue
			}
			s, err := Seal(keys, binding, value)
			if err != nil {
				return converted, err
			}
			written, err := rows.reseal(ctx, r, s)
			if err != nil {
				return converted, err
			}
			if written {
				converted++
			}
		}
		if len(batch) < reencryptBatch {
			return converted, nil
		}
	}
}

// notUnderCurrent counts the secrets whose key id is not the current key's,
// every secret when no current key is configured.
func (c *Component) notUnderCurrent(ctx context.Context) (int, error) {
	query, args := `SELECT COUNT(*) FROM secrets`, []any{}
	if c.deps.Keys.Current != nil {
		query, args = query+` WHERE key_id <> ?`, []any{KeyID(c.deps.Keys.Current)}
	}
	var n int
	if err := c.deps.Store.Main().QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count the secrets not under the current key: %w", err)
	}
	return n, nil
}

// sealedRow is a stored secret as the re-encryption reads it.
type sealedRow struct {
	id                  int64
	scope, scopeID, key string
	sealed              Sealed
}

// rowStore is what the re-encryption reads and writes the secret table
// through, so that the tests can step in between a read and a write and make
// a write fail.
type rowStore interface {
	// underKey returns at most limit secrets sealed under keyID whose id is
	// greater than afterID, in the order of their ids.
	underKey(ctx context.Context, keyID string, afterID int64, limit int) ([]sealedRow, error)
	// reseal replaces the sealed value of r with s while the row still holds
	// the value r was read with, and reports whether it did.
	reseal(ctx context.Context, r sealedRow, s Sealed) (bool, error)
}

// table is the secret table of the component's store.
type table struct{ c *Component }

func (t table) underKey(ctx context.Context, keyID string, afterID int64, limit int) ([]sealedRow, error) {
	rs, err := t.c.deps.Store.Main().QueryContext(ctx, `
SELECT id, scope, scope_id, key, sealed FROM secrets
WHERE key_id = ? AND id > ? ORDER BY id LIMIT ?`, keyID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("read the secrets to re-encrypt: %w", err)
	}
	defer rs.Close()
	var out []sealedRow
	for rs.Next() {
		r := sealedRow{sealed: Sealed{KeyID: keyID}}
		if err := rs.Scan(&r.id, &r.scope, &r.scopeID, &r.key, &r.sealed.Bytes); err != nil {
			return nil, fmt.Errorf("read the secrets to re-encrypt: %w", err)
		}
		out = append(out, r)
	}
	if err := rs.Err(); err != nil {
		return nil, fmt.Errorf("read the secrets to re-encrypt: %w", err)
	}
	return out, nil
}

func (t table) reseal(ctx context.Context, r sealedRow, s Sealed) (bool, error) {
	var n int64
	err := t.c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE secrets SET sealed = ?, key_id = ? WHERE id = ? AND key_id = ? AND sealed = ?`,
			s.Bytes, s.KeyID, r.id, r.sealed.KeyID, r.sealed.Bytes)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return false, fmt.Errorf("re-encrypt secret: %w", err)
	}
	return n == 1, nil
}
