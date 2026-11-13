package hub

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/store"
)

// idempotencyStep creates the table of the idempotency records in the
// transactional store, so that a create recorded before a stop is replayed
// after the next start. written_at is in nanoseconds since the Unix epoch.
var idempotencyStep = store.Step{
	Name:   "idempotency_records",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE idempotency_records (
	key          TEXT    NOT NULL,
	caller       TEXT    NOT NULL,
	route        TEXT    NOT NULL,
	request_hash BLOB    NOT NULL,
	body         BLOB,
	header       TEXT,
	written_at   INTEGER NOT NULL,
	PRIMARY KEY (key, caller, route)
)`)
		return err
	},
}

// composeIdempotency adds the idempotency records' table. The server's
// option that takes the records is filled by ServerOptions.
func composeIdempotency(c *Composition) {
	c.AddSchemaStep(idempotencyStep)
}

// idempotencyStore keeps the server's idempotency records in the
// transactional store. The server decides from WrittenAt whether a record is
// still valid and calls DeleteExpired with its own clock.
type idempotencyStore struct {
	st *store.Store
}

func newIdempotencyStore(st *store.Store) server.IdempotencyStore {
	return idempotencyStore{st: st}
}

// Find returns the record of key, caller and route.
func (s idempotencyStore) Find(ctx context.Context, key, caller, route string) (server.IdempotencyRecord, bool, error) {
	rec := server.IdempotencyRecord{Key: key, Caller: caller, Route: route}
	var hash []byte
	var header sql.NullString
	var writtenAt int64
	err := s.st.Main().QueryRowContext(ctx,
		`SELECT request_hash, body, header, written_at FROM idempotency_records
		 WHERE key = ? AND caller = ? AND route = ?`, key, caller, route).
		Scan(&hash, &rec.Body, &header, &writtenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return server.IdempotencyRecord{}, false, nil
	}
	if err != nil {
		return server.IdempotencyRecord{}, false, fmt.Errorf("find idempotency record: %w", err)
	}
	if len(hash) != sha256.Size {
		return server.IdempotencyRecord{}, false, fmt.Errorf("idempotency record holds a request hash of %d bytes", len(hash))
	}
	copy(rec.RequestHash[:], hash)
	if header.Valid {
		var h http.Header
		if err := json.Unmarshal([]byte(header.String), &h); err != nil {
			return server.IdempotencyRecord{}, false, fmt.Errorf("idempotency record headers: %w", err)
		}
		rec.Header = h
	}
	rec.WrittenAt = time.Unix(0, writtenAt).UTC()
	return rec, true, nil
}

// Write stores rec, replacing any record with the same key, caller and route.
func (s idempotencyStore) Write(ctx context.Context, rec server.IdempotencyRecord) error {
	header, err := json.Marshal(rec.Header)
	if err != nil {
		return fmt.Errorf("idempotency record headers: %w", err)
	}
	return s.st.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO idempotency_records (key, caller, route, request_hash, body, header, written_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (key, caller, route) DO UPDATE SET
			   request_hash = excluded.request_hash, body = excluded.body,
			   header = excluded.header, written_at = excluded.written_at`,
			rec.Key, rec.Caller, rec.Route, rec.RequestHash[:], rec.Body, string(header), rec.WrittenAt.UnixNano())
		if err != nil {
			return fmt.Errorf("write idempotency record: %w", err)
		}
		return nil
	})
}

// DeleteExpired removes every record written at or before cutoff.
func (s idempotencyStore) DeleteExpired(ctx context.Context, cutoff time.Time) error {
	return s.st.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM idempotency_records WHERE written_at <= ?`, cutoff.UnixNano()); err != nil {
			return fmt.Errorf("delete expired idempotency records: %w", err)
		}
		return nil
	})
}
