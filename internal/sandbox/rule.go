package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

// ruleIDPrefix begins every workspace sandbox rule id; 32 lowercase
// hexadecimal characters follow it.
const ruleIDPrefix = "sbr_"

// NewRuleID returns a new workspace sandbox rule id: sbr_ and 32 lowercase
// hexadecimal characters from a cryptographic random source.
func NewRuleID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	return ruleIDPrefix + hex.EncodeToString(b[:])
}

// ErrRuleNotFound is returned when a workspace holds no rule of the id.
var ErrRuleNotFound = errors.New("sandbox: no such workspace sandbox rule")

// ErrDuplicateRule is returned by SaveRule when the rule's id is taken or
// its workspace holds an identical rule (CanonicalRule).
var ErrDuplicateRule = errors.New("sandbox: the workspace holds the rule")

// SandboxRule is a workspace sandbox rule: a rule approved for the
// repository workspace that holds it, which every sandbox created later on
// the workspace or its forks gets on top of its profile's policy.
type SandboxRule struct {
	ID string
	// Workspace is the slug of the repository workspace that holds it.
	Workspace string
	// Rule is the rule's body, an object in OpenShell's schema that the hub
	// does not interpret.
	Rule json.RawMessage
	// Summary and Findings are those of the proposal it was approved from.
	Summary  string
	Findings []runtime.Finding
	// FromProposal is the proposal it was promoted from; nil for none.
	FromProposal *RuleOrigin
	CreatedBy    Creator
	// CreatedAt is in whole seconds, UTC, once stored.
	CreatedAt time.Time
}

// RuleOrigin is the proposal a workspace sandbox rule was promoted from:
// its sandbox, its id and the sandbox's workspace.
type RuleOrigin struct {
	SandboxID  string `json:"sandbox_id"`
	ProposalID string `json:"proposal_id"`
	Workspace  string `json:"workspace"`
}

// ApprovedRule is a rule approved for one sandbox, by the proposal
// ProposalID, whatever the approval's scope: it is in the sandbox's policy
// from the decision on, after its workspace sandbox rules.
type ApprovedRule struct {
	SandboxID  string
	ProposalID string
	Rule       json.RawMessage
	// DecidedAt is in whole seconds, UTC, once stored.
	DecidedAt time.Time
}

// CanonicalRule is the form of a rule's body by which two rules are
// compared: the JSON value encoded again, with the keys of every object
// sorted and no whitespace, so that key order and spacing make no
// difference.
func CanonicalRule(body json.RawMessage) (string, error) {
	var v any
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	if err := d.Decode(&v); err != nil {
		return "", fmt.Errorf("the rule is not JSON: %w", err)
	}
	if d.More() {
		return "", errors.New("the rule is not one JSON value")
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("the rule is not JSON: %w", err)
	}
	return string(b), nil
}

// findingJSON is a finding as the rule store keeps it.
type findingJSON struct {
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

// SaveRule stores r in tx. It returns an error wrapping ErrDuplicateRule,
// and writes nothing, when r's id is taken or r's workspace holds a rule
// identical to it.
func SaveRule(ctx context.Context, tx *sql.Tx, r SandboxRule) error {
	canonical, err := CanonicalRule(r.Rule)
	if err != nil {
		return fmt.Errorf("save workspace sandbox rule %s: %w", r.ID, err)
	}
	findings := make([]findingJSON, len(r.Findings))
	for i, f := range r.Findings {
		findings[i] = findingJSON(f)
	}
	fj, _ := json.Marshal(findings)    // strings always encode
	cb, _ := json.Marshal(r.CreatedBy) // strings always encode
	var from any
	if r.FromProposal != nil {
		b, _ := json.Marshal(r.FromProposal) // strings always encode
		from = string(b)
	}
	res, err := tx.ExecContext(ctx, `
INSERT INTO sandbox_rules (id, workspace, rule, canonical, summary, findings, from_proposal, created_by, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT DO NOTHING`,
		r.ID, r.Workspace, string(r.Rule), canonical, r.Summary, string(fj), from, string(cb), r.CreatedAt.Unix())
	if err != nil {
		return fmt.Errorf("save workspace sandbox rule %s: %w", r.ID, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("save workspace sandbox rule %s: %w", r.ID, err)
	} else if n == 0 {
		return fmt.Errorf("save workspace sandbox rule %s: %w", r.ID, ErrDuplicateRule)
	}
	return nil
}

// ruleColumns are the columns the rule reads take, in the order scanRule
// takes them.
const ruleColumns = `id, workspace, rule, summary, findings, from_proposal, created_by, created_at`

// ListRules reads the rules the workspace holds through q, oldest first,
// rules saved in the same second in the order of their ids.
func ListRules(ctx context.Context, q access.Querier, workspace string) ([]SandboxRule, error) {
	// SQLite reads a negative limit as none.
	return rulesAfter(ctx, q, workspace, nil, -1)
}

// rulesAfter reads, through q, up to limit rules the workspace holds in the
// order of ListRules, those after the position after when it is not nil.
func rulesAfter(ctx context.Context, q access.Querier, workspace string, after *Position, limit int) ([]SandboxRule, error) {
	query := `SELECT ` + ruleColumns + ` FROM sandbox_rules WHERE workspace = ?`
	args := []any{workspace}
	if after != nil {
		at := after.CreatedAt.Unix()
		query += ` AND (created_at > ? OR (created_at = ? AND id > ?))`
		args = append(args, at, at, after.ID)
	}
	query += ` ORDER BY created_at, id LIMIT ?`
	args = append(args, limit)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list the workspace sandbox rules of %s: %w", workspace, err)
	}
	defer func() { _ = rows.Close() }()
	var out []SandboxRule
	for rows.Next() {
		r, err := scanRule(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("list the workspace sandbox rules of %s: %w", workspace, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the workspace sandbox rules of %s: %w", workspace, err)
	}
	return out, nil
}

// RuleByCanonical reads, through q, the rule of the workspace identical to
// body (CanonicalRule); ok is false when it holds none.
func RuleByCanonical(ctx context.Context, q access.Querier, workspace string, body json.RawMessage) (r SandboxRule, ok bool, err error) {
	canonical, err := CanonicalRule(body)
	if err != nil {
		return SandboxRule{}, false, err
	}
	r, err = scanRule(q.QueryRowContext(ctx, `SELECT `+ruleColumns+` FROM sandbox_rules WHERE workspace = ? AND canonical = ?`, workspace, canonical).Scan)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return SandboxRule{}, false, nil
	case err != nil:
		return SandboxRule{}, false, fmt.Errorf("read the workspace sandbox rules of %s: %w", workspace, err)
	}
	return r, true, nil
}

// CountRules counts the rules the workspace holds, through q.
func CountRules(ctx context.Context, q access.Querier, workspace string) (int, error) {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM sandbox_rules WHERE workspace = ?`, workspace).Scan(&n); err != nil {
		return 0, fmt.Errorf("count the workspace sandbox rules of %s: %w", workspace, err)
	}
	return n, nil
}

// DeleteRule deletes the rule id of the workspace in tx and returns it as
// it was. It returns ErrRuleNotFound when the workspace holds no such rule.
// The sandboxes that recorded the rule keep their copies.
func DeleteRule(ctx context.Context, tx *sql.Tx, workspace, id string) (SandboxRule, error) {
	r, err := scanRule(tx.QueryRowContext(ctx, `DELETE FROM sandbox_rules WHERE workspace = ? AND id = ? RETURNING `+ruleColumns, workspace, id).Scan)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return SandboxRule{}, ErrRuleNotFound
	case err != nil:
		return SandboxRule{}, fmt.Errorf("delete workspace sandbox rule %s: %w", id, err)
	}
	return r, nil
}

// scanRule reads the ruleColumns of a row with scan.
func scanRule(scan func(dest ...any) error) (SandboxRule, error) {
	var (
		r                  SandboxRule
		rule, findings, by string
		from               sql.NullString
		created            int64
	)
	if err := scan(&r.ID, &r.Workspace, &rule, &r.Summary, &findings, &from, &by, &created); err != nil {
		return SandboxRule{}, err
	}
	r.Rule, r.CreatedAt = json.RawMessage(rule), time.Unix(created, 0).UTC()
	var fs []findingJSON
	if err := json.Unmarshal([]byte(findings), &fs); err != nil {
		return SandboxRule{}, fmt.Errorf("the findings of rule %s: %w", r.ID, err)
	}
	for _, f := range fs {
		r.Findings = append(r.Findings, runtime.Finding(f))
	}
	if from.Valid {
		r.FromProposal = &RuleOrigin{}
		if err := json.Unmarshal([]byte(from.String), r.FromProposal); err != nil {
			return SandboxRule{}, fmt.Errorf("the origin of rule %s: %w", r.ID, err)
		}
	}
	if err := json.Unmarshal([]byte(by), &r.CreatedBy); err != nil {
		return SandboxRule{}, fmt.Errorf("the creator of rule %s: %w", r.ID, err)
	}
	return r, nil
}

// HoldingWorkspace is the slug of the workspace that holds the workspace
// sandbox rules of the workspace slug, read through q: the parent of a task
// fork, the workspace itself otherwise. It returns an error when no
// workspace has the slug.
func HoldingWorkspace(ctx context.Context, q access.Querier, slug string) (string, error) {
	rec, ok, err := access.RecordBySlug(ctx, q, slug)
	switch {
	case err != nil:
		return "", err
	case !ok:
		return "", fmt.Errorf("workspace %s has no record", slug)
	}
	return holdingSlug(rec), nil
}

// holdingSlug is the slug of the workspace that holds the workspace
// sandbox rules of the workspace rec: its parent when it is a task fork,
// itself otherwise.
func holdingSlug(rec access.Record) string {
	if rec.Kind == access.KindFork && rec.ParentSlug != "" {
		return rec.ParentSlug
	}
	return rec.Slug
}

// SaveApprovedRule stores a in tx.
func SaveApprovedRule(ctx context.Context, tx *sql.Tx, a ApprovedRule) error {
	if _, err := CanonicalRule(a.Rule); err != nil {
		return fmt.Errorf("save the rule approved by %s: %w", a.ProposalID, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sandbox_approved_rules (sandbox_id, proposal_id, rule, decided_at) VALUES (?, ?, ?, ?)`,
		a.SandboxID, a.ProposalID, string(a.Rule), a.DecidedAt.Unix()); err != nil {
		return fmt.Errorf("save the rule approved by %s: %w", a.ProposalID, err)
	}
	return nil
}

// ApprovedRules reads, through q, the rules approved for the sandbox id in
// the order they were decided.
func ApprovedRules(ctx context.Context, q access.Querier, id string) ([]ApprovedRule, error) {
	rows, err := q.QueryContext(ctx, `SELECT sandbox_id, proposal_id, rule, decided_at FROM sandbox_approved_rules WHERE sandbox_id = ? ORDER BY decided_at, seq`, id)
	if err != nil {
		return nil, fmt.Errorf("list the rules approved for sandbox %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var out []ApprovedRule
	for rows.Next() {
		var (
			a       ApprovedRule
			rule    string
			decided int64
		)
		if err := rows.Scan(&a.SandboxID, &a.ProposalID, &rule, &decided); err != nil {
			return nil, fmt.Errorf("list the rules approved for sandbox %s: %w", id, err)
		}
		a.Rule, a.DecidedAt = json.RawMessage(rule), time.Unix(decided, 0).UTC()
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the rules approved for sandbox %s: %w", id, err)
	}
	return out, nil
}

// RemoveWorkspaceRules deletes every workspace sandbox rule the workspace
// slug holds, through tx, and writes no event. It is a remover of owned data
// of the workspace teardown (ADR 07): it runs in the transaction that
// removes the workspace, so the rules go with it or not at all, and it
// revokes no token.
func RemoveWorkspaceRules(ctx context.Context, tx *sql.Tx, slug string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM sandbox_rules WHERE workspace = ?`, slug); err != nil {
		return fmt.Errorf("remove the workspace sandbox rules of %s: %w", slug, err)
	}
	return nil
}
