package sandbox

import (
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
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// The states of a proposal: open until a decision approves or rejects it,
// or until its sandbox is deleted, which withdraws it.
const (
	ProposalOpen      = "open"
	ProposalApproved  = "approved"
	ProposalRejected  = "rejected"
	ProposalWithdrawn = "withdrawn"
)

// EventProposal is written when the hub stores a new proposal of a sandbox.
const EventProposal = "parley.sandbox.proposal"

// proposalIDPrefix begins every proposal id; 32 lowercase hexadecimal
// characters follow it.
const proposalIDPrefix = "sbp_"

// ErrProposalNotFound is returned when a sandbox has no proposal of the id.
var ErrProposalNotFound = errors.New("sandbox: no such proposal")

// Proposal is a rule the runtime proposed adding to the policy of a
// sandbox, as the hub stores it: open, or decided, or withdrawn with its
// sandbox.
type Proposal struct {
	// ID is the hub's id, sbp_ and 32 lowercase hexadecimal characters.
	ID        string
	SandboxID string
	// RuntimeID is the runtime's id of the proposal, by which a sandbox's
	// proposal is recognised when the runtime lists it again.
	RuntimeID string
	// Rule, Summary and Findings are as the runtime gave them; Rule is the
	// rule's body, which the hub does not interpret.
	Rule     json.RawMessage
	Summary  string
	Findings []runtime.Finding
	State    string
	// Scope, WorkspaceRuleID, DecidedBy, DecidedAt and Comment are those
	// of the decision; nil while the proposal is open or withdrawn, and
	// for what a decision leaves unset.
	Scope           *string
	WorkspaceRuleID *string
	DecidedBy       *Creator
	DecidedAt       *time.Time
	Comment         *string
	// CreatedAt is in whole seconds, UTC, once stored.
	CreatedAt time.Time
}

// ProposalDecision is what a decision writes to an open proposal.
type ProposalDecision struct {
	// State is ProposalApproved or ProposalRejected.
	State string
	// Scope is the approval's scope; nil for a rejection.
	Scope *string
	// WorkspaceRuleID is the workspace sandbox rule an approval for the
	// workspace saved or found; nil otherwise.
	WorkspaceRuleID *string
	DecidedBy       Creator
	DecidedAt       time.Time
	// Comment is the decider's comment; nil for none.
	Comment *string
}

// NewProposalID returns a new proposal id: sbp_ and 32 lowercase
// hexadecimal characters from a cryptographic random source.
func NewProposalID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	return proposalIDPrefix + hex.EncodeToString(b[:])
}

// severityRank orders the severities of findings; a severity it does not
// hold ranks below low.
var severityRank = map[string]int{"low": 1, "medium": 2, "high": 3}

// MaxSeverity returns the severity of the highest-ranked finding, in the
// order low < medium < high, a severity outside these three ranking below
// low; of findings of equal rank the first counts. It returns nil without
// findings.
func MaxSeverity(findings []runtime.Finding) *string {
	if len(findings) == 0 {
		return nil
	}
	top := findings[0]
	for _, f := range findings[1:] {
		if severityRank[f.Severity] > severityRank[top.Severity] {
			top = f
		}
	}
	return &top.Severity
}

// InsertProposal stores p in tx, unless its sandbox's proposal of the same
// runtime id is stored already; inserted says which. The rule must be JSON.
func InsertProposal(ctx context.Context, tx *sql.Tx, p Proposal) (inserted bool, err error) {
	if !json.Valid(p.Rule) {
		return false, fmt.Errorf("store proposal %s of sandbox %s: the rule is not JSON", p.RuntimeID, p.SandboxID)
	}
	findings := make([]findingJSON, len(p.Findings))
	for i, f := range p.Findings {
		findings[i] = findingJSON(f)
	}
	fj, _ := json.Marshal(findings) // strings always encode
	var by, decided any
	if p.DecidedBy != nil {
		b, _ := json.Marshal(p.DecidedBy) // strings always encode
		by = string(b)
	}
	if p.DecidedAt != nil {
		decided = p.DecidedAt.Unix()
	}
	res, err := tx.ExecContext(ctx, `
INSERT INTO sandbox_proposals (id, sandbox_id, runtime_id, rule, summary, findings, state, scope, workspace_rule_id, decided_by, decided_at, comment, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (sandbox_id, runtime_id) DO NOTHING`,
		p.ID, p.SandboxID, p.RuntimeID, string(p.Rule), p.Summary, string(fj), p.State, p.Scope, p.WorkspaceRuleID, by, decided, p.Comment, p.CreatedAt.Unix())
	if err != nil {
		return false, fmt.Errorf("store proposal %s of sandbox %s: %w", p.RuntimeID, p.SandboxID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store proposal %s of sandbox %s: %w", p.RuntimeID, p.SandboxID, err)
	}
	return n > 0, nil
}

// proposalStored reports, through q, whether the proposal of the runtime id
// runtimeID of the sandbox sandboxID is stored, in any state.
func proposalStored(ctx context.Context, q access.Querier, sandboxID, runtimeID string) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM sandbox_proposals WHERE sandbox_id = ? AND runtime_id = ?`, sandboxID, runtimeID).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read proposal %s of sandbox %s: %w", runtimeID, sandboxID, err)
	}
	return true, nil
}

// proposalColumns are the columns the proposal reads take, in the order
// scanProposal takes them.
const proposalColumns = `id, sandbox_id, runtime_id, rule, summary, findings, state, scope, workspace_rule_id, decided_by, decided_at, comment, created_at`

// GetProposal reads, through q, the proposal id of the sandbox sandboxID.
// It returns ErrProposalNotFound when the sandbox has no such proposal,
// whether another sandbox has it or not.
func GetProposal(ctx context.Context, q access.Querier, sandboxID, id string) (Proposal, error) {
	p, err := scanProposal(q.QueryRowContext(ctx, `SELECT `+proposalColumns+` FROM sandbox_proposals WHERE sandbox_id = ? AND id = ?`, sandboxID, id).Scan)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Proposal{}, ErrProposalNotFound
	case err != nil:
		return Proposal{}, fmt.Errorf("read proposal %s of sandbox %s: %w", id, sandboxID, err)
	}
	return p, nil
}

// ListProposals reads, through q, at most limit proposals of the sandbox
// sandboxID in the state, by created_at and then id, oldest first, from
// after the position after on (nil for the start).
func ListProposals(ctx context.Context, q access.Querier, sandboxID, state string, after *Position, limit int) ([]Proposal, error) {
	query := `SELECT ` + proposalColumns + ` FROM sandbox_proposals WHERE sandbox_id = ? AND state = ?`
	args := []any{sandboxID, state}
	if after != nil {
		at := after.CreatedAt.Unix()
		query += ` AND (created_at > ? OR (created_at = ? AND id > ?))`
		args = append(args, at, at, after.ID)
	}
	query += ` ORDER BY created_at, id LIMIT ?`
	args = append(args, limit)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list the proposals of sandbox %s: %w", sandboxID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Proposal
	for rows.Next() {
		p, err := scanProposal(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("list the proposals of sandbox %s: %w", sandboxID, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the proposals of sandbox %s: %w", sandboxID, err)
	}
	return out, nil
}

// DecideProposal writes the decision d to the proposal id of the sandbox
// sandboxID in tx, only while the proposal is open; decided is false, and
// nothing is written, when it is not, or when the sandbox has no such
// proposal.
func DecideProposal(ctx context.Context, tx *sql.Tx, sandboxID, id string, d ProposalDecision) (decided bool, err error) {
	by, _ := json.Marshal(d.DecidedBy) // strings always encode
	res, err := tx.ExecContext(ctx, `
UPDATE sandbox_proposals SET state = ?, scope = ?, workspace_rule_id = ?, decided_by = ?, decided_at = ?, comment = ?
WHERE sandbox_id = ? AND id = ? AND state = ?`,
		d.State, d.Scope, d.WorkspaceRuleID, string(by), d.DecidedAt.Unix(), d.Comment, sandboxID, id, ProposalOpen)
	if err != nil {
		return false, fmt.Errorf("decide proposal %s of sandbox %s: %w", id, sandboxID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("decide proposal %s of sandbox %s: %w", id, sandboxID, err)
	}
	return n > 0, nil
}

// WithdrawProposals is the delete hook of the proposals: in the transaction
// that marks the sandbox deleted, its open proposals become withdrawn, with
// no decision and no event; decided proposals keep their state.
func WithdrawProposals(ctx context.Context, tx *sql.Tx, sandboxID string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE sandbox_proposals SET state = ? WHERE sandbox_id = ? AND state = ?`,
		ProposalWithdrawn, sandboxID, ProposalOpen); err != nil {
		return fmt.Errorf("withdraw the proposals of sandbox %s: %w", sandboxID, err)
	}
	return nil
}

// PurgeNetworkPolicy is the purge hook of the network-policy feature: in
// the transaction that removes the sandbox's record, it deletes the
// sandbox's proposals, its reported denials and the rules approved for it.
func PurgeNetworkPolicy(ctx context.Context, tx *sql.Tx, sandboxID string) error {
	for _, table := range []string{"sandbox_proposals", "sandbox_denials", "sandbox_approved_rules"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE sandbox_id = ?`, sandboxID); err != nil {
			return fmt.Errorf("delete the %s rows of sandbox %s: %w", table, sandboxID, err)
		}
	}
	return nil
}

// proposalJSON is the proposal object every answer shows: its twelve
// members, each always present and null where the proposal leaves it
// unset, findings [] without findings, and the timestamps in RFC 3339, UTC,
// with whole seconds, as in the sandbox object.
type proposalJSON struct {
	ID              string          `json:"id"`
	SandboxID       string          `json:"sandbox_id"`
	Rule            json.RawMessage `json:"rule"`
	Summary         string          `json:"summary"`
	Findings        []findingJSON   `json:"findings"`
	State           string          `json:"state"`
	Scope           *string         `json:"scope"`
	WorkspaceRuleID *string         `json:"workspace_rule_id"`
	DecidedBy       *Creator        `json:"decided_by"`
	DecidedAt       *timefmt.Time   `json:"decided_at"`
	Comment         *string         `json:"comment"`
	CreatedAt       timefmt.Time    `json:"created_at"`
}

// proposalObject returns the proposal object of p.
func proposalObject(p Proposal) proposalJSON {
	o := proposalJSON{
		ID: p.ID, SandboxID: p.SandboxID, Rule: p.Rule, Summary: p.Summary,
		Findings: make([]findingJSON, len(p.Findings)), State: p.State, Scope: p.Scope,
		WorkspaceRuleID: p.WorkspaceRuleID, DecidedBy: p.DecidedBy, Comment: p.Comment,
		CreatedAt: timefmt.Time(p.CreatedAt),
	}
	for i, f := range p.Findings {
		o.Findings[i] = findingJSON(f)
	}
	if p.DecidedAt != nil {
		at := timefmt.Time(*p.DecidedAt)
		o.DecidedAt = &at
	}
	return o
}

// scanProposal reads the proposalColumns of a row with scan.
func scanProposal(scan func(dest ...any) error) (Proposal, error) {
	var (
		p                       Proposal
		rule, findings          string
		scope, ruleID, by, note sql.NullString
		decided                 sql.NullInt64
		created                 int64
	)
	if err := scan(&p.ID, &p.SandboxID, &p.RuntimeID, &rule, &p.Summary, &findings, &p.State, &scope, &ruleID, &by, &decided, &note, &created); err != nil {
		return Proposal{}, err
	}
	p.Rule, p.CreatedAt = json.RawMessage(rule), time.Unix(created, 0).UTC()
	var fs []findingJSON
	if err := json.Unmarshal([]byte(findings), &fs); err != nil {
		return Proposal{}, fmt.Errorf("the findings of proposal %s: %w", p.ID, err)
	}
	for _, f := range fs {
		p.Findings = append(p.Findings, runtime.Finding(f))
	}
	if scope.Valid {
		p.Scope = &scope.String
	}
	if ruleID.Valid {
		p.WorkspaceRuleID = &ruleID.String
	}
	if note.Valid {
		p.Comment = &note.String
	}
	if decided.Valid {
		at := time.Unix(decided.Int64, 0).UTC()
		p.DecidedAt = &at
	}
	if by.Valid {
		p.DecidedBy = &Creator{}
		if err := json.Unmarshal([]byte(by.String), p.DecidedBy); err != nil {
			return Proposal{}, fmt.Errorf("the decider of proposal %s: %w", p.ID, err)
		}
	}
	return p, nil
}
