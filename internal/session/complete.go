package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
)

// msgNotCreator is the message of the refusal to change a session the
// caller may read but did not open.
const msgNotCreator = "only the creator of the session or the admin token may change it"

// canChange reports whether caller may change the session s, by completing
// it or reporting its usage: the admin token always, and a user when the
// session's user id is theirs, with any of their credentials. A session the
// admin token opened has no user id, so only the admin token changes it.
func canChange(caller access.Caller, s row) bool {
	return caller.Admin || caller.UserID != "" && caller.UserID == s.UserID
}

// ending is what ends an active session: its terminal status, the time it
// ended, and the members a complete sets (nil for null).
type ending struct {
	Status                   string
	EndedAt                  time.Time
	ErrorMessage             *string
	DurationMS               *int64
	CacheCreationInputTokens int64
}

// endActive ends the session id with e in tx, only while it is active: of
// any number of racing ends exactly one takes effect, and an end of a
// session already ended changes nothing. changed reports whether this one
// took effect.
func endActive(ctx context.Context, tx *sql.Tx, id string, e ending) (changed bool, err error) {
	res, err := tx.ExecContext(ctx, `
UPDATE sessions SET status = ?, ended_at = ?, error_message = ?, duration_ms = ?, cache_creation_input_tokens = ?
WHERE id = ? AND status = 'active'`,
		e.Status, formatOptional(&e.EndedAt), e.ErrorMessage, e.DurationMS, e.CacheCreationInputTokens, id)
	if err != nil {
		return false, fmt.Errorf("end session %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("end session %s: %w", id, err)
	}
	return n > 0, nil
}

// completeRoute serves POST /sessions/:id/complete: it ends an active
// session with the outcome the creator or the admin token sends, once.
// It decides in this order: the path id, read access (an unreadable
// session is not found), the creator rule, the body, then the transition.
// A session already ended is answered as it is stored, so a retried
// complete answers what the first one did.
func (c *Component) completeRoute(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	sc, ok := server.CallerOf(ctx)
	if !ok {
		return errNoPrincipal
	}
	caller := access.CallerFrom(sc)
	s, err := c.readableSession(ctx, caller, server.Param(r, "id"))
	if err != nil {
		return err
	}
	if !canChange(caller, s) {
		return server.Forbidden(msgNotCreator)
	}
	var body map[string]json.RawMessage
	if err := server.DecodeJSON(r, &body); err != nil {
		return err
	}
	e, err := completion(body)
	if err != nil {
		return err
	}
	e.EndedAt = c.deps.Now().UTC().Truncate(time.Second)
	if e.DurationMS == nil {
		d := e.EndedAt.Sub(s.StartedAt).Milliseconds()
		e.DurationMS = &d
	}
	var stored row
	err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := endActive(ctx, tx, s.ID, e); err != nil {
			return err
		}
		var found bool
		var err error
		if stored, found, err = readRow(ctx, tx, s.ID); err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("session %s is gone after its complete", s.ID)
		}
		return nil
	})
	if err != nil {
		return err
	}
	o, err := object(ctx, c.deps.Store.Main(), stored)
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, o)
	return nil
}

// completion reads the body of a complete: status, one of completed,
// failed and timeout; error_message, a string or absent; duration_ms, an
// integer of at least 0 or absent (nil); and cache_creation_input_tokens,
// an integer of at least 0, 0 when absent.
func completion(body map[string]json.RawMessage) (ending, error) {
	var e ending
	status, err := optionalString(body, "status")
	if err != nil || status == nil {
		return ending{}, server.InvalidRequest("status is required and must be completed, failed or timeout")
	}
	switch *status {
	case StatusCompleted, StatusFailed, StatusTimeout:
		e.Status = *status
	default:
		return ending{}, server.InvalidRequest("status must be completed, failed or timeout")
	}
	if e.ErrorMessage, err = optionalString(body, "error_message"); err != nil {
		return ending{}, err
	}
	if e.DurationMS, err = optionalCount(body, "duration_ms"); err != nil {
		return ending{}, err
	}
	cache, err := optionalCount(body, "cache_creation_input_tokens")
	if err != nil {
		return ending{}, err
	}
	if cache != nil {
		e.CacheCreationInputTokens = *cache
	}
	return e, nil
}

// optionalCount reads the member name of body as an integer of at least 0:
// nil when it is absent or null, and 400 naming the member when it is a
// string, a fraction, negative or beyond 64 bits. Only the plain integer
// form is accepted.
func optionalCount(body map[string]json.RawMessage, name string) (*int64, error) {
	raw := body[name]
	if isNull(raw) {
		return nil, nil
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || n < 0 {
		return nil, server.InvalidRequest(name + " must be an integer of at least 0")
	}
	return &n, nil
}
