package sandbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/store"
)

// ErrNotFound is returned, wrapped, for an id no stored sandbox has, or one
// of another workspace than the one asked for.
var ErrNotFound = errors.New("sandbox not found")

// SchemaStep creates the sandbox records: one row per sandbox, with every
// field of the sandbox object but runtime_reachable, and what the sandbox
// packages keep with it and never show. Lists, maps and objects are JSON;
// times are Unix seconds, NULL where the object shows null. A record names
// its workspace without a foreign key, because a deleted sandbox outlives
// its workspace until it is purged.
var SchemaStep = store.Step{
	Name:   "sandboxes",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
CREATE TABLE sandboxes (
	id                   TEXT NOT NULL PRIMARY KEY,
	workspace            TEXT NOT NULL,
	session_id           TEXT,
	profile              TEXT NOT NULL,
	purpose              TEXT NOT NULL,
	state                TEXT NOT NULL,
	state_reason         TEXT NOT NULL,
	command              TEXT,
	env                  TEXT NOT NULL,
	exit_code            INTEGER,
	checkout             TEXT,
	image                TEXT NOT NULL,
	cpu                  TEXT NOT NULL,
	memory               TEXT NOT NULL,
	gpu                  INTEGER NOT NULL,
	credentials          TEXT NOT NULL,
	exposed_secrets      TEXT NOT NULL,
	workspace_rules      TEXT NOT NULL,
	hub_access           TEXT,
	policy_revision      INTEGER NOT NULL,
	expires_at           INTEGER NOT NULL,
	idle_timeout_seconds INTEGER NOT NULL,
	last_activity_at     INTEGER NOT NULL,
	checkpoint           TEXT,
	restored_from        TEXT,
	labels               TEXT NOT NULL,
	created_by_type      TEXT NOT NULL,
	created_by_id        TEXT NOT NULL,
	created_by_username  TEXT NOT NULL,
	created_at           INTEGER NOT NULL,
	updated_at           INTEGER NOT NULL,
	ended_at             INTEGER,
	snapshot             TEXT NOT NULL,
	hub_token_id         TEXT NOT NULL,
	user_id              TEXT NOT NULL,
	org_id               TEXT NOT NULL,
	family_root          TEXT NOT NULL,
	marker               TEXT NOT NULL,
	runtime_created      INTEGER NOT NULL,
	runtime_provisioning INTEGER NOT NULL,
	ready_at             INTEGER
);
CREATE INDEX sandboxes_workspace ON sandboxes (workspace, created_at, id);
CREATE INDEX sandboxes_created ON sandboxes (created_at, id);
CREATE INDEX sandboxes_state ON sandboxes (state);
CREATE INDEX sandboxes_marker ON sandboxes (marker) WHERE marker <> '';`)
		return err
	},
}

// recordColumns are the columns Insert writes and scanRecord reads, in
// their order.
const recordColumns = `id, workspace, session_id, profile, purpose, state, state_reason, command, env, exit_code,
	checkout, image, cpu, memory, gpu, credentials, exposed_secrets, workspace_rules, hub_access, policy_revision,
	expires_at, idle_timeout_seconds, last_activity_at, checkpoint, restored_from, labels, created_by_type,
	created_by_id, created_by_username, created_at, updated_at, ended_at, snapshot, hub_token_id, user_id, org_id,
	family_root, marker, runtime_created, runtime_provisioning, ready_at`

// Insert stores rec in tx, the creating transaction. A nil list or map is
// stored empty.
func Insert(ctx context.Context, tx *sql.Tx, rec Record) error {
	var enc encoder
	values := []any{
		rec.ID, rec.Workspace, rec.SessionID, rec.Profile, rec.Purpose, rec.State, rec.StateReason,
		enc.nullable(rec.Command, rec.Command == nil), enc.json(orEmptyMap(rec.Env)), rec.ExitCode,
		enc.nullable(rec.Checkout, rec.Checkout == nil), rec.Image, rec.CPU, rec.Memory, rec.GPU,
		enc.json(orEmpty(rec.Credentials)), enc.json(orEmpty(rec.ExposedSecrets)),
		enc.json(orNoRules(rec.WorkspaceRules)), enc.nullable(rec.HubAccess, rec.HubAccess == nil), rec.PolicyRevision,
		rec.ExpiresAt.Unix(), int64(rec.IdleTimeout / time.Second), rec.LastActivityAt.Unix(),
		rawOrNull(rec.Checkpoint), rec.RestoredFrom, enc.json(orEmptyMap(rec.Labels)),
		rec.CreatedBy.ActorType, rec.CreatedBy.ActorID, rec.CreatedBy.Username,
		rec.CreatedAt.Unix(), rec.UpdatedAt.Unix(), unixOrNull(rec.EndedAt), enc.json(rec.Snapshot),
		rec.HubTokenID, rec.UserID, rec.OrgID, rec.FamilyRoot, rec.Marker, rec.RuntimeCreated,
		rec.RuntimeProvisioning, unixOrNull(rec.ReadyAt),
	}
	if enc.err != nil {
		return fmt.Errorf("store sandbox %s: %w", rec.ID, enc.err)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(values)), ", ")
	if _, err := tx.ExecContext(ctx, `INSERT INTO sandboxes (`+recordColumns+`) VALUES (`+placeholders+`)`, values...); err != nil {
		return fmt.Errorf("store sandbox %s: %w", rec.ID, err)
	}
	return nil
}

// Get reads the sandbox id through q, the store or the caller's
// transaction. An id no sandbox has is an error matching ErrNotFound.
func Get(ctx context.Context, q access.Querier, id string) (Record, error) {
	return getWhere(ctx, q, id, `id = ?`, id)
}

// GetIn reads the sandbox id of the workspace through q. An id no sandbox
// of that workspace has is an error matching ErrNotFound, whether another
// workspace has it or none.
func GetIn(ctx context.Context, q access.Querier, workspace, id string) (Record, error) {
	return getWhere(ctx, q, id, `id = ? AND workspace = ?`, id, workspace)
}

func getWhere(ctx context.Context, q access.Querier, id, where string, args ...any) (Record, error) {
	row := q.QueryRowContext(ctx, `SELECT `+recordColumns+` FROM sandboxes WHERE `+where, args...)
	rec, err := scanRecord(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return Record{}, fmt.Errorf("read sandbox %s: %w", id, err)
	}
	return rec, nil
}

// ListByState reads through q the sandboxes in one of states, by created_at
// and then by id.
func ListByState(ctx context.Context, q access.Querier, states ...string) ([]Record, error) {
	if len(states) == 0 {
		return nil, nil
	}
	args := make([]any, len(states))
	for i, s := range states {
		args[i] = s
	}
	in := strings.TrimSuffix(strings.Repeat("?, ", len(states)), ", ")
	rows, err := q.QueryContext(ctx, `SELECT `+recordColumns+` FROM sandboxes WHERE state IN (`+in+`) ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list sandboxes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Record
	for rows.Next() {
		rec, err := scanRecord(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("list sandboxes: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sandboxes: %w", err)
	}
	return out, nil
}

// Remove deletes the record of the sandbox id in tx. An id no sandbox has is
// an error matching ErrNotFound.
func Remove(ctx context.Context, tx *sql.Tx, id string) error {
	res, err := tx.ExecContext(ctx, `DELETE FROM sandboxes WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("remove sandbox %s: %w", id, err)
	}
	return requireRow(res, id)
}

// Field is one stored field of a sandbox, with the value an Update gives
// it; the Set functions make them.
type Field struct {
	column string
	value  any
	err    error
}

// Update gives the sandbox id the values of fields in tx, and changes no
// other field: updated_at moves only with SetUpdatedAt. An id no sandbox has
// is an error matching ErrNotFound.
func Update(ctx context.Context, tx *sql.Tx, id string, fields ...Field) error {
	if len(fields) == 0 {
		return nil
	}
	sets := make([]string, len(fields))
	args := make([]any, 0, len(fields)+1)
	for i, f := range fields {
		if f.err != nil {
			return fmt.Errorf("update sandbox %s: %s: %w", id, f.column, f.err)
		}
		sets[i] = f.column + " = ?"
		args = append(args, f.value)
	}
	res, err := tx.ExecContext(ctx, `UPDATE sandboxes SET `+strings.Join(sets, ", ")+` WHERE id = ?`, append(args, id)...)
	if err != nil {
		return fmt.Errorf("update sandbox %s: %w", id, err)
	}
	return requireRow(res, id)
}

// jsonField is a field stored as JSON, NULL when null is true.
func jsonField(column string, v any, null bool) Field {
	var enc encoder
	value := enc.nullable(v, null)
	return Field{column: column, value: value, err: enc.err}
}

// SetCheckout sets checkout; nil for none.
func SetCheckout(c *Checkout) Field { return jsonField("checkout", c, c == nil) }

// SetCheckpoint sets the latest checkpoint, a JSON object; nil for none.
func SetCheckpoint(cp json.RawMessage) Field {
	return Field{column: "checkpoint", value: rawOrNull(cp)}
}

// SetExitCode sets exit_code; nil for none.
func SetExitCode(code *int) Field { return Field{column: "exit_code", value: code} }

// SetExpiresAt sets expires_at.
func SetExpiresAt(t time.Time) Field { return Field{column: "expires_at", value: t.Unix()} }

// SetPurpose sets purpose.
func SetPurpose(p string) Field { return Field{column: "purpose", value: p} }

// SetLabels sets labels; nil is stored empty.
func SetLabels(l map[string]string) Field { return jsonField("labels", orEmptyMap(l), false) }

// SetHubAccess sets hub_access; nil for no hub credential.
func SetHubAccess(ha *HubAccess) Field { return jsonField("hub_access", ha, ha == nil) }

// SetCredentials sets the attached credential names; nil is stored empty.
func SetCredentials(names []string) Field { return jsonField("credentials", orEmpty(names), false) }

// SetExposedSecrets sets the exposed secret names; nil is stored empty.
func SetExposedSecrets(names []string) Field {
	return jsonField("exposed_secrets", orEmpty(names), false)
}

// SetPolicyRevision sets policy_revision.
func SetPolicyRevision(n int) Field { return Field{column: "policy_revision", value: n} }

// SetLastActivityAt sets last_activity_at.
func SetLastActivityAt(t time.Time) Field { return Field{column: "last_activity_at", value: t.Unix()} }

// SetHubTokenID sets the id of the hub credential's token; "" for none.
func SetHubTokenID(id string) Field { return Field{column: "hub_token_id", value: id} }

// SetMarker sets the provisioning marker: MarkerCreate, MarkerStart or ""
// for none.
func SetMarker(m string) Field { return Field{column: "marker", value: m} }

// SetRuntimeCreated sets the mark that the sandbox was created at the
// runtime.
func SetRuntimeCreated(b bool) Field { return Field{column: "runtime_created", value: b} }

// SetRuntimeProvisioning sets the mark that the runtime, not a request, put
// the sandbox in provisioning.
func SetRuntimeProvisioning(b bool) Field { return Field{column: "runtime_provisioning", value: b} }

// SetUpdatedAt sets updated_at.
func SetUpdatedAt(t time.Time) Field { return Field{column: "updated_at", value: t.Unix()} }

// requireRow turns a statement that touched no row into ErrNotFound.
func requireRow(res sql.Result, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sandbox %s: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return nil
}

// scanRecord reads one row of recordColumns.
func scanRecord(scan func(dest ...any) error) (Record, error) {
	var (
		rec                                       Record
		command, checkout, hubAccess, checkpoint  sql.NullString
		env, creds, exposed, rules, labels, snap  string
		expires, idle, activity, created, updated int64
		ended, ready                              sql.NullInt64
	)
	err := scan(&rec.ID, &rec.Workspace, &rec.SessionID, &rec.Profile, &rec.Purpose, &rec.State, &rec.StateReason,
		&command, &env, &rec.ExitCode, &checkout, &rec.Image, &rec.CPU, &rec.Memory, &rec.GPU, &creds, &exposed,
		&rules, &hubAccess, &rec.PolicyRevision, &expires, &idle, &activity, &checkpoint, &rec.RestoredFrom,
		&labels, &rec.CreatedBy.ActorType, &rec.CreatedBy.ActorID, &rec.CreatedBy.Username, &created, &updated,
		&ended, &snap, &rec.HubTokenID, &rec.UserID, &rec.OrgID, &rec.FamilyRoot, &rec.Marker,
		&rec.RuntimeCreated, &rec.RuntimeProvisioning, &ready)
	if err != nil {
		return Record{}, err
	}
	var dec jsonDecoder
	if command.Valid {
		dec.json(command.String, &rec.Command)
	}
	if checkout.Valid {
		dec.json(checkout.String, &rec.Checkout)
	}
	if hubAccess.Valid {
		dec.json(hubAccess.String, &rec.HubAccess)
	}
	if checkpoint.Valid {
		rec.Checkpoint = json.RawMessage(checkpoint.String)
	}
	dec.json(env, &rec.Env)
	dec.json(creds, &rec.Credentials)
	dec.json(exposed, &rec.ExposedSecrets)
	dec.json(rules, &rec.WorkspaceRules)
	dec.json(labels, &rec.Labels)
	dec.json(snap, &rec.Snapshot)
	if dec.err != nil {
		return Record{}, fmt.Errorf("sandbox %s: %w", rec.ID, dec.err)
	}
	rec.ExpiresAt, rec.LastActivityAt = unix(expires), unix(activity)
	rec.CreatedAt, rec.UpdatedAt = unix(created), unix(updated)
	rec.IdleTimeout = time.Duration(idle) * time.Second
	rec.EndedAt, rec.ReadyAt = unixPtr(ended), unixPtr(ready)
	return rec, nil
}

// encoder encodes values as JSON text and keeps the first error.
type encoder struct{ err error }

func (e *encoder) json(v any) string {
	b, err := json.Marshal(v)
	if err != nil && e.err == nil {
		e.err = err
	}
	return string(b)
}

// nullable is the JSON text of v, or NULL when null is true.
func (e *encoder) nullable(v any, null bool) any {
	if null {
		return nil
	}
	return e.json(v)
}

// jsonDecoder decodes JSON text and keeps the first error.
type jsonDecoder struct{ err error }

func (d *jsonDecoder) json(text string, v any) {
	if err := json.Unmarshal([]byte(text), v); err != nil && d.err == nil {
		d.err = err
	}
}

// orNoRules is rules, or an empty list for nil.
func orNoRules(rules []WorkspaceRule) []WorkspaceRule {
	if rules == nil {
		return []WorkspaceRule{}
	}
	return rules
}

func orEmptyMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func rawOrNull(raw json.RawMessage) any {
	if raw == nil {
		return nil
	}
	return string(raw)
}

func unixOrNull(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Unix()
}

func unix(s int64) time.Time { return time.Unix(s, 0).UTC() }

func unixPtr(s sql.NullInt64) *time.Time {
	if !s.Valid {
		return nil
	}
	t := unix(s.Int64)
	return &t
}
