package sandbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/store"
)

// ErrInvalidTransition is returned, wrapped, by Transition for a pair of
// states the transition table does not hold.
var ErrInvalidTransition = errors.New("invalid sandbox state transition")

// SecondsMetric is the histogram of the seconds a sandbox spent from ready
// until it stopped, completed, failed or was deleted.
const SecondsMetric = "parley_sandbox_seconds"

// transitions is the transition table: the states each state may move to.
// Every state but deleted may move to deleted, and nothing leaves deleted.
var transitions = map[string][]string{
	StateProvisioning: {StateReady, StateFailed, StateDeleted},
	StateReady:        {StateCompleted, StateFailed, StateStopped, StateProvisioning, StateDeleted},
	StateStopped:      {StateProvisioning, StateDeleted},
	StateCompleted:    {StateProvisioning, StateDeleted},
	StateFailed:       {StateProvisioning, StateDeleted},
}

// endedStates are the states in which a sandbox has ended: a transition to
// one sets ended_at.
var endedStates = []string{StateFailed, StateCompleted, StateStopped, StateDeleted}

// Deps are what the service works the stored sandboxes with. The hub binds
// them in a start hook, once the stores are open; a test changes one through
// the composition before they are bound.
type Deps struct {
	// Store is the transactional store the records live in.
	Store *store.Store
	// Now is the hub's clock.
	Now func() time.Time
	// Events is the event component a transition writes its event through.
	Events *event.Component
	// Recorder receives the sandbox metrics; nil records nothing.
	Recorder Recorder
	// Repos opens the repository of the workspace slug, in which a create
	// resolves the ref it checks out.
	Repos func(ctx context.Context, slug string) (*gitrepo.Repo, error)
	// ResolveSecrets resolves secret names along the chain of the workspace
	// slug, as the secret component's ResolveSecrets does.
	ResolveSecrets func(ctx context.Context, slug string, names []string) (secret.SecretResolution, error)
	// Mint mints a PAT bound to a workspace in the caller's transaction, with
	// its parley.token.bind event, as the access component's MintBound does:
	// the one way a hub credential is minted.
	Mint func(ctx context.Context, tx *sql.Tx, in access.MintInput) (identity.PAT, string, error)
	// Revoke revokes the PAT tokenID at now in the caller's transaction, as
	// identity.RevokeTokenByID does.
	Revoke func(ctx context.Context, tx *sql.Tx, tokenID string, now time.Time) error
	// ExternalURL is [server].external_url, the address a sandbox reaches the
	// hub at; without one no sandbox gets a hub credential.
	ExternalURL string
}

// Bind sets the dependencies the stored sandboxes are worked with. It is
// called once, before the hub serves.
func (s *Service) Bind(d Deps) {
	if d.Recorder == nil {
		d.Recorder = NopRecorder{}
	}
	s.deps = d
}

// Transition moves the sandbox id from the state from to the state to, with
// reason as its state_reason ("" for none), when from is the state the
// record has: it writes the state, the reason, updated_at and, for a target
// in which the sandbox has ended, ended_at, and the event ev when it is not
// nil, all in one transaction. A target that has not ended clears ended_at.
// The event is completed with the sandbox as its resource, its workspace and
// its session; the caller gives the type, the actor and the metadata.
//
// tx is the caller's transaction, in which everything is written; nil runs
// Transition in a transaction of its own. Since the store admits one write
// transaction at a time, two racing calls that expect the same state apply
// once.
//
// applied is false, with a nil error, when the record is in another state
// than from: nothing is written then. A pair the transition table does not
// hold is an error matching ErrInvalidTransition, and an id no sandbox has
// one matching ErrNotFound; nothing is written either.
//
// Every package leaves ready through here, so Transition reports
// SecondsMetric: an applied transition from ready to an ended state observes
// the seconds since the sandbox became ready. The runtime's pause (ready to
// provisioning and back) keeps the time it first became ready. With the
// caller's transaction the observation is made when the update applies,
// before the caller commits.
func (s *Service) Transition(ctx context.Context, tx *sql.Tx, id, from, to, reason string, ev *event.Input) (applied bool, err error) {
	if !slices.Contains(transitions[from], to) {
		return false, fmt.Errorf("%w: %s to %s", ErrInvalidTransition, from, to)
	}
	var seconds *float64
	run := func(tx *sql.Tx) error {
		applied, seconds, err = s.transition(ctx, tx, id, from, to, reason, ev)
		return err
	}
	if tx != nil {
		err = run(tx)
	} else {
		err = s.deps.Store.Tx(ctx, run)
	}
	if err != nil {
		return false, err
	}
	if seconds != nil {
		_ = s.deps.Recorder.Observe(SecondsMetric, nil, *seconds)
	}
	return applied, nil
}

// transition is Transition in tx. seconds is what to observe in
// SecondsMetric once the transition holds; nil for nothing.
func (s *Service) transition(ctx context.Context, tx *sql.Tx, id, from, to, reason string, ev *event.Input) (applied bool, seconds *float64, err error) {
	var (
		workspace string
		session   *string
		readyAt   sql.NullInt64
	)
	err = tx.QueryRowContext(ctx, `SELECT workspace, session_id, ready_at FROM sandboxes WHERE id = ?`, id).
		Scan(&workspace, &session, &readyAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return false, nil, fmt.Errorf("read sandbox %s: %w", id, err)
	}
	now := s.deps.Now()
	var ended any
	if slices.Contains(endedStates, to) {
		ended = now.Unix()
	}
	// ready_at is when the sandbox became ready: set on reaching ready unless
	// a pause kept it, and cleared once the sandbox has ended.
	ready := any(nil)
	switch {
	case to == StateReady && readyAt.Valid:
		ready = readyAt.Int64
	case to == StateReady:
		ready = now.Unix()
	case ended == nil && readyAt.Valid:
		ready = readyAt.Int64
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE sandboxes SET state = ?, state_reason = ?, updated_at = ?, ended_at = ?, ready_at = ? WHERE id = ? AND state = ?`,
		to, reason, now.Unix(), ended, ready, id, from)
	if err != nil {
		return false, nil, fmt.Errorf("move sandbox %s to %s: %w", id, to, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, nil, err
	}
	if ev != nil {
		in := *ev
		in.ResourceType, in.ResourceID, in.Workspace, in.SessionID = "sandbox", id, &workspace, session
		if _, err := s.deps.Events.Emit(ctx, tx, in); err != nil {
			return false, nil, fmt.Errorf("event of sandbox %s: %w", id, err)
		}
	}
	if from == StateReady && ended != nil && readyAt.Valid {
		v := float64(now.Unix() - readyAt.Int64)
		seconds = &v
	}
	return true, seconds, nil
}
