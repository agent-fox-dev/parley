package sandbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
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
	// slug, as the secret component's ResolveSecrets does, with the chain's
	// workspace records read through q when it is not nil, as its
	// ResolveSecretsIn does.
	ResolveSecrets func(ctx context.Context, q access.Querier, slug string, names []string) (secret.SecretResolution, error)
	// Mint mints a PAT bound to a workspace in the caller's transaction, with
	// its parley.token.bind event, as the access component's MintBound does:
	// the one way a hub credential is minted.
	Mint func(ctx context.Context, tx *sql.Tx, in access.MintInput) (identity.PAT, string, error)
	// Revoke revokes the PAT tokenID at now in the caller's transaction, as
	// identity.RevokeTokenByID does.
	Revoke func(ctx context.Context, tx *sql.Tx, tokenID string, now time.Time) error
	// SetTokenExpiry sets the expiry of the PAT tokenID to at in the
	// caller's transaction, as identity.SetTokenExpiryByID does: a change of
	// a sandbox's ttl moves its hub credential's expiry with it.
	SetTokenExpiry func(ctx context.Context, tx *sql.Tx, tokenID string, at time.Time) error
	// MaxCheckpointSize is the largest git bundle in bytes a checkpoint may
	// bring to the hub, [git].max_push_size, 0 for no limit: a checkpoint
	// carries the commits of unpushed branches into the workspace's
	// repository as a push would, so it is held to the push's limit.
	MaxCheckpointSize int64
	// ExternalURL is [server].external_url, the address a sandbox reaches the
	// hub at; without one no sandbox gets a hub credential.
	ExternalURL string
	// OrgSlug returns the slug of the organization orgID, which the hub's
	// git URL of a workspace names.
	OrgSlug func(ctx context.Context, orgID string) (string, error)
	// User returns the user record of the user id, whose name and e-mail a
	// checkout commits with.
	User func(ctx context.Context, id string) (identity.User, error)
	// Logger receives what the provisioner cannot report otherwise; nil
	// logs nothing.
	Logger *slog.Logger
	// NewTimer returns a timer of the hub's clock, on which the provisioner
	// passes and bounds a provisioning; nil uses the real clock.
	NewTimer func(time.Duration) Timer
	// Readable returns the workspaces the caller can read, cut down to those
	// its binding covers, as the access component's Readable does: the list
	// of a repository's sandboxes with include_forks shows the forks' of
	// that set.
	Readable func(ctx context.Context, c access.Caller) (access.ReadableSet, error)
	// Shutdown is closed when the hub begins to stop: a read waiting for a
	// sandbox answers then.
	Shutdown <-chan struct{}
	// Limits returns the four sandbox limits a create is held to; nil uses
	// those of the loaded settings. A test that changes the limits while
	// the hub serves sets it.
	Limits func() Limits
	// HoldProvisioning keeps the provisioner from running: the sandboxes a
	// create stores stay in provisioning with their marker, and the runtime
	// is not called for them. A test of what precedes the provisioning sets
	// it.
	HoldProvisioning bool
}

// logger is Deps.Logger, or a logger that writes nothing.
func (s *Service) logger() *slog.Logger {
	if s.deps.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.deps.Logger
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
// An applied transition reports SandboxesMetric from the counts in its
// transaction (ReportStates). It also has a report for once it holds (see
// TransitionTx): it observes SecondsMetric for a move from ready to an
// ended state and notifies the reads that wait for a sandbox. In its own
// transaction Transition makes that report after the commit; with the
// caller's transaction it makes it at once, before the caller commits, so a
// caller that can act after its commit calls TransitionTx instead.
func (s *Service) Transition(ctx context.Context, tx *sql.Tx, id, from, to, reason string, ev *event.Input) (applied bool, err error) {
	var report func()
	if tx != nil {
		applied, report, err = s.TransitionTx(ctx, tx, id, from, to, reason, ev)
	} else {
		err = s.deps.Store.Tx(ctx, func(tx *sql.Tx) (err error) {
			applied, report, err = s.TransitionTx(ctx, tx, id, from, to, reason, ev)
			return err
		})
	}
	if err != nil {
		return false, err
	}
	report()
	return applied, nil
}

// TransitionTx is Transition in tx, the caller's open transaction, which the
// caller commits. It returns, with applied, the report of the transition,
// which the caller calls once tx has committed and drops when tx rolls back,
// so that nothing is reported of a transition that did not hold: an applied
// transition from ready to an ended state observes the seconds since the
// sandbox became ready in SecondsMetric, and every applied transition
// notifies the reads that wait for a sandbox. Every package leaves ready
// through Transition or TransitionTx; the runtime's pause (ready to
// provisioning and back) keeps the time it first became ready. report is
// never nil; it does nothing for a transition that did not apply.
func (s *Service) TransitionTx(ctx context.Context, tx *sql.Tx, id, from, to, reason string, ev *event.Input) (applied bool, report func(), err error) {
	if !slices.Contains(transitions[from], to) {
		return false, func() {}, fmt.Errorf("%w: %s to %s", ErrInvalidTransition, from, to)
	}
	applied, seconds, err := s.transition(ctx, tx, id, from, to, reason, ev)
	if err != nil || !applied {
		return false, func() {}, err
	}
	return true, func() {
		if seconds != nil {
			_ = s.deps.Recorder.Observe(SecondsMetric, nil, *seconds)
		}
		s.changes.notify()
	}, nil
}

// notifier tells the reads that wait for a sandbox that a transition
// applied. Each waiter takes the channel of the next notification before it
// reads the record, so that a transition after its read is not missed; a
// notification closes that channel and starts the next one. The zero value
// is ready to use.
type notifier struct {
	mu sync.Mutex
	ch chan struct{}
}

// next returns the channel the next notification closes.
func (n *notifier) next() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ch == nil {
		n.ch = make(chan struct{})
	}
	return n.ch
}

// notify closes the channel of the waiters that took it.
func (n *notifier) notify() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ch != nil {
		close(n.ch)
		n.ch = nil
	}
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
	s.reportStates(ctx, tx)
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
