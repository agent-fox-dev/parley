package webhook

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/agent-fox-dev/parley/internal/secret"
)

// convertBatch is the number of subscriptions ConvertSecrets reads at a
// time.
const convertBatch = 100

// ConvertSecrets is the converter the hub registers with the secret
// component's re-encryption (a secret.Converter): with a current and a
// previous key in keys it seals again under the current key every signing
// secret still sealed under another key that the previous key opens, under
// the same binding. It changes no other column of a subscription, its
// updated_at included, and writes no event: the secret the owner holds is
// the same. A secret written in the meantime, by a rotation, keeps the
// rotation's value; a ciphertext that opens under neither key stays as it
// is. Without both keys it converts nothing.
//
// It returns how many secrets it converted and how many are still not under
// the current key, every one of them when there is no current key, and an
// error only when the store fails.
func (c *Component) ConvertSecrets(ctx context.Context, keys secret.Keys) (converted, remaining int, err error) {
	convert := keys.Current != nil && keys.Previous != nil &&
		secret.KeyID(keys.Current) != secret.KeyID(keys.Previous)
	after := ""
	for {
		batch, err := c.sealedAfter(ctx, after)
		if err != nil {
			return converted, remaining, err
		}
		for _, s := range batch {
			after = s.id
			if !secret.NeedsReseal(keys, s.sealed) {
				continue
			}
			if !convert {
				remaining++
				continue
			}
			result, err := c.reseal(ctx, keys, s)
			if err != nil {
				return converted, remaining, err
			}
			switch result {
			case resealed:
				converted++
			case unreadable:
				remaining++
			}
		}
		if len(batch) < convertBatch {
			return converted, remaining, nil
		}
	}
}

// sealedSecret is a subscription's sealed signing secret.
type sealedSecret struct {
	id     string
	sealed []byte
}

// sealedAfter reads the sealed signing secrets of up to convertBatch
// subscriptions whose id is after after, in the order of their ids.
func (c *Component) sealedAfter(ctx context.Context, after string) ([]sealedSecret, error) {
	rows, err := c.deps.Store.Main().QueryContext(ctx,
		`SELECT id, secret_sealed FROM webhook_subscriptions WHERE id > ? ORDER BY id LIMIT ?`, after, convertBatch)
	if err != nil {
		return nil, fmt.Errorf("read the event webhook secrets to re-encrypt: %w", err)
	}
	defer rows.Close()
	var out []sealedSecret
	for rows.Next() {
		var s sealedSecret
		if err := rows.Scan(&s.id, &s.sealed); err != nil {
			return nil, fmt.Errorf("read the event webhook secrets to re-encrypt: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the event webhook secrets to re-encrypt: %w", err)
	}
	return out, nil
}

// resealResult is what reseal did with a secret.
type resealResult int

const (
	// resealed is a secret now sealed under the current key.
	resealed resealResult = iota
	// unreadable is a secret neither key opens, left as it is.
	unreadable
	// replaced is a secret a rotation or a delete changed since it was
	// read, which is under the current key or gone.
	replaced
)

// reseal seals the secret of s again under the current key of keys,
// writing it only while the row still holds the ciphertext s was read with.
func (c *Component) reseal(ctx context.Context, keys secret.Keys, s sealedSecret) (resealResult, error) {
	plain, err := secret.OpenValue(keys, binding(s.id), s.sealed)
	if err != nil {
		return unreadable, nil
	}
	sealed, err := secret.SealValue(keys, binding(s.id), plain)
	if err != nil {
		return 0, fmt.Errorf("re-encrypt the secret of event webhook %s: %w", s.id, err)
	}
	var n int64
	err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE webhook_subscriptions SET secret_sealed = ? WHERE id = ? AND secret_sealed = ?`, sealed, s.id, s.sealed)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("re-encrypt the secret of event webhook %s: %w", s.id, err)
	}
	if n == 0 {
		return replaced, nil
	}
	return resealed, nil
}
