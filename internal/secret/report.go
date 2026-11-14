package secret

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// noKeyWarning is the warn line of a start without a current key: the hub
// runs, lists and deletes secrets, and refuses only to store one.
const noKeyWarning = "secrets cannot be stored: no encryption key configured"

// StartReport is what the hub tells the operator about secret encryption when
// it starts: booleans and a count, never key material.
type StartReport struct {
	// KeyConfigured is whether a current key is configured.
	KeyConfigured bool
	// PreviousKeyPresent is whether a previous key is configured.
	PreviousKeyPresent bool
	// Undecryptable is the number of stored values whose key id is neither
	// configured key's: the values a missing key has made unreadable.
	Undecryptable int
}

// StartReport reads the start report from the store, comparing each stored
// value's key id with the configured keys' ids and decrypting nothing.
func (c *Component) StartReport(ctx context.Context) (StartReport, error) {
	if c.deps.Store == nil {
		return StartReport{}, errNotBound
	}
	keys := c.deps.Keys
	r := StartReport{KeyConfigured: keys.Current != nil, PreviousKeyPresent: keys.Previous != nil}
	var ids []any
	for _, k := range []Key{keys.Current, keys.Previous} {
		if k != nil {
			ids = append(ids, KeyID(k))
		}
	}
	query := `SELECT COUNT(*) FROM secrets`
	if len(ids) > 0 {
		query += ` WHERE key_id NOT IN (?` + strings.Repeat(`, ?`, len(ids)-1) + `)`
	}
	if err := c.deps.Store.Main().QueryRowContext(ctx, query, ids...).Scan(&r.Undecryptable); err != nil {
		return StartReport{}, fmt.Errorf("count the secrets no configured key opens: %w", err)
	}
	return r, nil
}

// Log writes the report to logger: one info line `secret encryption` with
// key_configured, previous_key_present and undecryptable_secrets, and,
// without a current key, the warn line that secrets cannot be stored.
func (r StartReport) Log(logger *slog.Logger) {
	logger.Info("secret encryption",
		"key_configured", r.KeyConfigured,
		"previous_key_present", r.PreviousKeyPresent,
		"undecryptable_secrets", r.Undecryptable)
	if !r.KeyConfigured {
		logger.Warn(noKeyWarning)
	}
}
