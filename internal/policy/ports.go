package policy

import (
	"context"
	"database/sql"
	"sync"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

// EventWriter takes the policy events in the caller's transaction, which an
// error from it rolls back.
type EventWriter interface {
	Emit(ctx context.Context, tx *sql.Tx, in event.Input) (event.Record, error)
}

// Users is what the validator asks about the users a document names: whether
// a user record with exactly that username exists, whatever its status. A
// failure of the lookup is returned as an error, never as false, so that a
// document is not refused for an unknown user the store could not read.
type Users interface {
	UserExists(ctx context.Context, username string) (bool, error)
}

// Querier is what a read goes through: the store's *sql.DB or the caller's
// open *sql.Tx, so that an evaluation, and the evidence it asks for, see
// what the caller's transaction wrote.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Workspace is what the policy routes know of a workspace besides the
// access decision: its kind (repository or fork), its status, and its
// default branch, "" for none.
type Workspace struct {
	Kind, Status, DefaultBranch string
}

// WorkspaceReader reads a workspace record through q, the caller's
// transaction or the store; found is false for a slug no workspace has.
type WorkspaceReader interface {
	Workspace(ctx context.Context, q Querier, slug string) (w Workspace, found bool, err error)
}

// Changes is what the policy reads and writes of the change records, which
// the change component keeps: a change, the cause it cannot land for
// whatever the policy requires, its landing, and the announcement step
// that is the one writer of parley.change.ready. Every call goes through
// the caller's querier or transaction.
type Changes interface {
	ReadChange(ctx context.Context, q change.Querier, workspace string, number int64) (change.Change, error)
	LandingCause(ctx context.Context, q change.Querier, ch change.Change) (*string, error)
	SetLanding(ctx context.Context, tx *sql.Tx, workspace string, number int64, l change.Landing) error
	AnnounceLandable(ctx context.Context, tx *sql.Tx, workspace string, number int64) error
}

// Repos opens the repository of the workspace slug.
type Repos func(ctx context.Context, slug string) (*gitrepo.Repo, error)

// ChangeFacts is what the evaluator knows of a change besides its files.
type ChangeFacts struct {
	// Workspace is the slug the change lives under, its target workspace.
	Workspace string
	Number    int64
	// Version is the number of the change's current version, the one its
	// files and evidence are read for.
	Version      int64
	State        string
	TargetBranch string
	Mergeable    string
	// Cause is the structural reason the change cannot land whatever its
	// requirements say (nothing_to_land, target_not_found, merge_commits,
	// unrelated_history), nil when there is none. A draft is blocked for
	// being a draft whatever Cause holds.
	Cause *string
}

// ChangeFiles reads, through q, the files the current version of a change
// touches against its base, both paths of a rename included. A failure is
// the evaluation's failure: no status is derived from part of the files.
type ChangeFiles interface {
	Files(ctx context.Context, q Querier, workspace string, number int64) ([]string, error)
}

// EvidenceQuery is what the evidence source is asked about one requirement
// of a change: the approvals of its approvers and the results of its named
// checks on the current version (or carried over to it, unless FreshChecks).
type EvidenceQuery struct {
	Workspace   string
	Number      int64
	Version     int64
	Approvers   []string
	Checks      []string
	FreshChecks bool
}

// EvidenceAnswer is what counts toward one requirement: the approvals
// counted, whether a counted rejection is outstanding, and the status
// ("passed", "failed" or "pending") of each named check that has a result.
// A named check missing from Checks has none and is pending.
type EvidenceAnswer struct {
	Counted              int
	RejectionOutstanding bool
	Checks               map[string]string
}

// Evidence is where the evaluator reads approvals and check results, asked
// once per requirement, through the caller's querier. Counting and carrying
// over approvals and results are the evidence source's rules, not the
// evaluator's.
type Evidence interface {
	Evidence(ctx context.Context, q Querier, in EvidenceQuery) (EvidenceAnswer, error)
}

// NoEvidence is the evidence source of a hub where nothing records
// approvals or check results: no approval counted, no rejection, no check
// result. It is installed until another source is registered.
var NoEvidence Evidence = noEvidence{}

type noEvidence struct{}

func (noEvidence) Evidence(context.Context, Querier, EvidenceQuery) (EvidenceAnswer, error) {
	return EvidenceAnswer{}, nil
}

// EvidenceSlot holds the evidence source installed on a hub. Its zero value
// holds NoEvidence. It is registered on the instance while the hub is
// composed, never in a registry of the package, so that every hub has its
// own.
type EvidenceSlot struct {
	mu  sync.RWMutex
	src Evidence
}

// RegisterEvidence makes e the evidence source, replacing the one registered
// before; nil restores NoEvidence.
func (s *EvidenceSlot) RegisterEvidence(e Evidence) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.src = e
}

// InstalledEvidence is the evidence source registered last, NoEvidence when
// none is.
func (s *EvidenceSlot) InstalledEvidence() Evidence {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.src == nil {
		return NoEvidence
	}
	return s.src
}
