package sandbox

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/server"
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

// EventProposalDecided is written when a decision approves or rejects a
// proposal, in the transaction that stores the decision.
const EventProposalDecided = "parley.sandbox.proposal_decided"

// proposalIDPrefix begins every proposal id; 32 lowercase hexadecimal
// characters follow it.
const proposalIDPrefix = "sbp_"

// ErrProposalNotFound is returned when a sandbox has no proposal of the id.
var ErrProposalNotFound = errors.New("sandbox: no such proposal")

// The scopes of an approval: the rule is approved for the proposal's
// sandbox only, or also saved as a workspace sandbox rule.
const (
	ScopeSandbox   = "sandbox"
	ScopeWorkspace = "workspace"
)

// The refusals of a decision of a proposal.
var (
	errProposalDecided         = server.NewError("proposal_decided", http.StatusConflict, "the proposal is decided already")
	errFindingsNotAcknowledged = server.NewError("findings_not_acknowledged", http.StatusConflict,
		"the proposal has a finding of severity high: approve it with acknowledge_findings true")
)

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

// decision is a decision of an open proposal, as a request asks for it.
type decision struct {
	// State is ProposalApproved or ProposalRejected.
	State string
	// Scope is ScopeSandbox or ScopeWorkspace for an approval, "" for a
	// rejection.
	Scope string
	// Comment is the decider's comment; nil for none.
	Comment *string
	// Acknowledge is the request's acknowledge_findings: an approval of a
	// proposal with a finding of severity high needs it.
	Acknowledge bool
}

// decide decides, for the caller, the proposal proposalID of the sandbox
// sandboxID of the workspace ws as d says, and returns the proposal as
// stored then. It is the one way a proposal is decided, for both decisions
// and both scopes; the request's body is read before it is called.
//
// An approval for the workspace first runs the checks of the workspace
// that will hold the rule (see promotionHolder). Decisions of one sandbox
// then run one at a time: the sandbox's turn is held from before the first
// read of the proposal to the commit, since the runtime is asked before the
// commit and a decision that lost a race must not leave its rule there. The
// checks run in this order, each refusing with nothing decided: the
// sandbox is the workspace's (404 sandbox_not_found); the proposal is the
// sandbox's (404 not_found); it is open (409 proposal_decided); an approval
// of a proposal with a finding of severity high acknowledges it (409
// findings_not_acknowledged). An approval for the workspace then takes the
// holding workspace's turn, held to the commit so that two promotions
// cannot pass the limit together, and is refused when the workspace holds
// no identical rule and no room for another (429
// sandbox_rule_limit_reached; see heldRule). Then an approval of a sandbox
// that is provisioning or ready adds the rule to its policy at the runtime
// (see addRule); a sandbox in another state takes the rule with its next
// start. A provisioning holds the sandbox's decision turn from before it
// composes the policy until the runtime has created or started the sandbox
// with it, so that an approval either is stored before the composition
// reads the approved rules or adds its rule after the runtime took the
// composed policy. Then one transaction stores the decision, conditional
// on the proposal still being open (see storeDecision); once the runtime
// has the rule, that transaction no longer depends on the caller staying
// (see storeApproval). Last, the runtime is told of a rejection, which is
// stored whether the runtime hears of it or not (see tellRejected).
func (s *Service) decide(ctx context.Context, caller identity.Principal, ws access.Record, sandboxID, proposalID string, d decision) (Proposal, error) {
	approve := d.State == ProposalApproved
	var holding string
	if approve && d.Scope == ScopeWorkspace {
		var err error
		if holding, err = s.promotionHolder(ctx, ws); err != nil {
			return Proposal{}, err
		}
	}
	unlock := s.deciding.lock(sandboxID)
	defer unlock()
	rec, err := s.readSandbox(ctx, ws.Slug, sandboxID)
	if err != nil {
		return Proposal{}, err
	}
	p, err := GetProposal(ctx, s.deps.Store.Main(), rec.ID, proposalID)
	switch {
	case errors.Is(err, ErrProposalNotFound):
		return Proposal{}, server.NotFound("the sandbox has no proposal " + proposalID)
	case err != nil:
		return Proposal{}, err
	case p.State != ProposalOpen:
		return Proposal{}, errProposalDecided
	}
	if approve && !d.Acknowledge && slices.ContainsFunc(p.Findings, func(f runtime.Finding) bool { return f.Severity == "high" }) {
		return Proposal{}, errFindingsNotAcknowledged
	}
	if holding != "" {
		unlockHolding := s.promoting.lock(holding)
		defer unlockHolding()
		if _, err := heldRule(ctx, s.deps.Store.Main(), holding, p.Rule); err != nil {
			return Proposal{}, err
		}
	}
	if approve && (rec.State == StateProvisioning || rec.State == StateReady) {
		if err := s.addRule(ctx, rec, p); err != nil {
			return Proposal{}, err
		}
		return s.storeApproval(ctx, caller, rec, holding, p, d)
	}
	err = s.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		p, err = s.storeDecision(ctx, tx, caller, rec.ID, holding, p, d)
		return err
	})
	if err != nil {
		return Proposal{}, err
	}
	if !approve {
		s.tellRejected(ctx, rec, p)
	}
	return p, nil
}

// rejectTimeout bounds the call that tells the runtime of a rejection,
// which the caller's answer waits for.
const rejectTimeout = 10 * time.Second

// storeTimeout bounds the transaction that stores an approval whose rule
// the runtime has already taken.
const storeTimeout = 10 * time.Second

// storeApproval stores the approval d of the proposal p of the sandbox rec,
// whose rule the runtime has taken into the sandbox's policy, even when the
// caller has left meanwhile: the runtime has no operation that takes a rule
// out again, so the decision must be recorded once the rule is in force.
// A transaction that fails all the same is logged, with the sandbox's and
// the proposal's ids and the error's text scrubbed of the hub's credentials
// and of the sandbox's values, and returned.
func (s *Service) storeApproval(ctx context.Context, caller identity.Principal, rec Record, holding string, p Proposal, d decision) (Proposal, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer cancel()
	stored := p
	err := s.deps.Store.Tx(ctx, func(tx *sql.Tx) (err error) {
		stored, err = s.storeDecision(ctx, tx, caller, rec.ID, holding, p, d)
		return err
	})
	if err != nil {
		s.logger().Error("the runtime took an approved rule whose approval could not be stored", "sandbox_id", rec.ID, "proposal_id", p.ID,
			"error", s.scrub(s.ScrubSandbox(ctx, rec.ID, err.Error())))
		return Proposal{}, err
	}
	return stored, nil
}

// tellRejected tells the runtime that the stored proposal p of the
// sandbox rec is rejected, with the decision's comment as the reason, even
// when the caller has left meanwhile. The decision is the hub's and stands
// as stored: a runtime that cannot be told, or that answers an error, is
// logged, with the error's text scrubbed of the hub's credentials and of
// the sandbox's values.
func (s *Service) tellRejected(ctx context.Context, rec Record, p Proposal) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rejectTimeout)
	defer cancel()
	var err error = RuntimeError("sandboxes are off")
	if client := s.Client(); client != nil {
		var reason string
		if p.Comment != nil {
			reason = *p.Comment
		}
		err = client.RejectProposal(ctx, rec.ID, p.RuntimeID, reason)
	}
	if err != nil {
		s.logger().Warn("the runtime could not be told of a proposal's rejection", "sandbox_id", rec.ID, "proposal_id", p.ID,
			"runtime_proposal_id", p.RuntimeID, "error", s.scrub(s.ScrubSandbox(ctx, rec.ID, err.Error())))
	}
}

// addRule adds the rule of the proposal p to the policy of the sandbox
// rec at the runtime, under the proposal's id. The runtime's rejection is
// 409 conflict with its reason; any other failure, an unreachable runtime
// or one that no longer holds the sandbox, is 502 sandbox_runtime_error.
// Both texts are scrubbed of the hub's credentials and of the sandbox's
// values, and cut to 1,024 bytes.
func (s *Service) addRule(ctx context.Context, rec Record, p Proposal) error {
	client := s.Client()
	if client == nil {
		return RuntimeError("sandboxes are off")
	}
	err := client.AddPolicyRule(ctx, rec.ID, runtime.Rule{Name: p.ID, Body: p.Rule})
	if err == nil {
		return nil
	}
	if errors.Is(err, runtime.ErrInvalid) {
		reason := err.Error()
		var re *runtime.Error
		if errors.As(err, &re) && re.Reason != "" {
			reason = re.Reason
		}
		return server.Conflict(s.ScrubSandbox(ctx, rec.ID, s.scrub("the runtime rejected the rule: "+reason)))
	}
	return RuntimeError(s.ScrubSandbox(ctx, rec.ID, s.scrub(err.Error())))
}

// storeDecision writes, in tx, the decision d of the open proposal p of
// the sandbox id for the caller, and returns the proposal as stored then.
// An approval for the workspace names the rule of the workspace holding
// identical to the proposal's, or saves the proposal's rule there as a new
// one (see promotedRule); the count and the save are in this transaction,
// under the holding workspace's turn (see decide), and the new rule's
// EventRuleAdd follows the decision's event. A proposal that is no longer
// open is refused with 409 proposal_decided, and the caller rolls tx back.
func (s *Service) storeDecision(ctx context.Context, tx *sql.Tx, caller identity.Principal, id, holding string, p Proposal, d decision) (Proposal, error) {
	rec, err := Get(ctx, tx, id)
	if err != nil {
		return Proposal{}, err
	}
	now := s.deps.Now().UTC().Truncate(time.Second)
	pd := ProposalDecision{State: d.State, DecidedBy: deciderOf(caller), DecidedAt: now, Comment: d.Comment}
	var scope any
	if d.State == ProposalApproved {
		pd.Scope, scope = &d.Scope, d.Scope
	}
	var added *SandboxRule
	if holding != "" {
		ruleID, err := heldRule(ctx, tx, holding, p.Rule)
		if err != nil {
			return Proposal{}, err
		}
		if ruleID == "" {
			r := promotedRule(caller, rec, holding, p, now)
			added, ruleID = &r, r.ID
		}
		pd.WorkspaceRuleID = &ruleID
	}
	decided, err := DecideProposal(ctx, tx, id, p.ID, pd)
	switch {
	case err != nil:
		return Proposal{}, err
	case !decided:
		return Proposal{}, errProposalDecided
	}
	if d.State == ProposalApproved {
		if err := SaveApprovedRule(ctx, tx, ApprovedRule{SandboxID: id, ProposalID: p.ID, Rule: p.Rule, DecidedAt: now}); err != nil {
			return Proposal{}, err
		}
		if err := Update(ctx, tx, id, SetPolicyRevision(rec.PolicyRevision+1)); err != nil {
			return Proposal{}, err
		}
	}
	actor := actorFor(caller)
	_, err = s.deps.Events.Emit(ctx, tx, event.Input{
		Type: EventProposalDecided, ActorType: actor.Type, ActorID: actor.ID,
		ResourceType: "sandbox", ResourceID: id, Workspace: &rec.Workspace, SessionID: rec.SessionID,
		Metadata: map[string]any{
			"proposal_id": p.ID, "decision": d.State, "acknowledged_findings": d.Acknowledge,
			"scope": scope, "rule_id": pd.WorkspaceRuleID,
		},
	})
	if err != nil {
		return Proposal{}, fmt.Errorf("event of sandbox %s: %w", id, err)
	}
	if added != nil {
		if err := s.addRuleOf(ctx, tx, caller, rec, *added); err != nil {
			return Proposal{}, err
		}
	}
	return GetProposal(ctx, tx, id, p.ID)
}

// deciderOf is the decided_by of a decision by the caller p: its
// credential's actor type and the user it acts for, empty for the admin
// token, which acts for none.
func deciderOf(p identity.Principal) Creator {
	c := Creator{ActorType: actorOf(p)}
	if p.User != nil {
		c.ActorID, c.Username = p.User.ID, p.User.Username
	}
	return c
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
// sandbox's proposals, its reported denials, how far its policy log was
// taken and the rules approved for it.
func PurgeNetworkPolicy(ctx context.Context, tx *sql.Tx, sandboxID string) error {
	for _, table := range []string{"sandbox_proposals", "sandbox_denials", "sandbox_policy_log", "sandbox_approved_rules"} {
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
