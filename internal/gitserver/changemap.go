package gitserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/server"
)

// ChangeService is what the mapping of a push asks of the changes: the
// lookup of a branch's change, the open of a change, and the version of a
// change whose open another push won. The ordinary version of a push is not
// among them: the change feature records it inside the ref update, and the
// mapping reads it from the ref writer's result.
type ChangeService interface {
	// OpenChangeOfBranch returns the draft or open change whose source is
	// the branch of the workspace source, or nil when it has none.
	OpenChangeOfBranch(ctx context.Context, source, branch string) (*BranchChange, error)
	// RecordPushVersion records a version of kind push of the change the
	// request names, at its head. It returns nil when the head already is
	// the change's current version, which records nothing.
	RecordPushVersion(ctx context.Context, req VersionRequest) (*pushrule.Version, error)
	// OpenChange opens a change from the request's source. It refuses
	// with a *ChangeExistsError when the source already has a draft or
	// open change.
	OpenChange(ctx context.Context, req OpenRequest) (OpenedChange, error)
}

// BranchChange is the draft or open change of a source branch.
type BranchChange struct {
	Workspace string
	Number    int
}

// VersionRequest asks for a version of kind push of the change Number of
// Workspace, whose source is Branch of Source, at Head. Caller is who
// pushed, and SessionID the session of the push, nil for none.
type VersionRequest struct {
	Workspace string
	Number    int
	Source    string
	Branch    string
	Head      string
	Caller    PushCaller
	SessionID *string
}

// OpenRequest asks to open a change in Workspace from Branch of Source at
// Head, by Caller, who is the change's author and whose credential opens
// it. Target, Title and Intent are nil when the push left them to the
// change's defaults; SessionID is the session of the push, nil for none.
type OpenRequest struct {
	Workspace string
	Source    string
	Branch    string
	Head      string
	Target    *string
	Title     *string
	Intent    *string
	Draft     bool
	SessionID *string
	Caller    PushCaller
}

// OpenedChange is the change an open made: its workspace, its number, and
// whether it was opened as a draft.
type OpenedChange struct {
	Workspace string
	Number    int
	Draft     bool
}

// ChangeExistsError refuses the open of a change whose source already has
// the draft or open change Number of Workspace, which another push opened
// in the meantime.
type ChangeExistsError struct {
	Workspace string
	Number    int
}

func (e *ChangeExistsError) Error() string {
	return fmt.Sprintf("the source is already the source of change %s#%d", e.Workspace, e.Number)
}

// OutcomeKind is what the mapping made of a pushed branch.
type OutcomeKind int

// The outcomes of a pushed branch: nothing, a new version of its change,
// a change opened for it, or a warning that no change was opened.
const (
	OutcomeNone OutcomeKind = iota
	OutcomeVersion
	OutcomeOpened
	OutcomeWarning
)

// Outcome is what the mapping made of one pushed branch. Workspace, Number
// and Version name the change and its version for OutcomeVersion and
// OutcomeOpened, Draft marks a change opened as a draft, and Reason is why
// no change was opened for OutcomeWarning.
type Outcome struct {
	Branch    string
	Kind      OutcomeKind
	Workspace string
	Number    int
	Version   int
	Draft     bool
	Reason    string
}

// ChangeMapper is the mapping of an accepted push to changes.
type ChangeMapper struct {
	changes ChangeService
	// mu guards logger, which Bind may set once the mapper is registered.
	mu     sync.RWMutex
	logger *slog.Logger
}

// NewChangeMapper returns the mapping that works with changes and logs
// the failures that are not the client's doing to logger, nil for none.
func NewChangeMapper(changes ChangeService, logger *slog.Logger) *ChangeMapper {
	return &ChangeMapper{changes: changes, logger: logger}
}

// Bind sets the logger of the failures that are not the client's doing,
// for a mapper registered before the hub's logger exists.
func (m *ChangeMapper) Bind(logger *slog.Logger) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logger = logger
}

// MapPush is the mapping stage: it maps the push in and returns the lines
// to send the client.
func (m *ChangeMapper) MapPush(ctx context.Context, in MappingInput) []string {
	var lines []string
	m.Map(ctx, in, func(l string) { lines = append(lines, l) })
	return lines
}

// Map maps the push in and sends the line of each outcome that has one to
// send. It returns one outcome per created or updated branch, in command
// order; tags, other refs and deletes have none.
func (m *ChangeMapper) Map(ctx context.Context, in MappingInput, send func(line string)) []Outcome {
	// The options stage refused the push already if they were invalid,
	// so the parse cannot fail here. The session is the one the stage
	// accepted, which may differ in case from the option's value.
	opts, _ := ParsePushOptions(in.Options)
	ws := in.Workspace
	autoChange := ws.Kind == access.KindFork && ws.AutoChange != nil && *ws.AutoChange
	var outcomes []Outcome
	for _, u := range in.Updates {
		branch, ok := strings.CutPrefix(u.Ref, "refs/heads/")
		if !ok || u.Deleted {
			continue
		}
		o := m.mapBranch(ctx, in, opts, autoChange, branch, u)
		outcomes = append(outcomes, o)
		if line := o.Line(); line != "" {
			send(line)
		}
	}
	return outcomes
}

// mapBranch decides the outcome of the created or updated branch of the
// update u by the first rule that fits.
func (m *ChangeMapper) mapBranch(ctx context.Context, in MappingInput, opts PushOptions, autoChange bool, branch string, u pushrule.UpdateResult) Outcome {
	// The change feature recorded the version of a push to a change's
	// source inside the ref update, whatever the push asked.
	if v := u.Version; v != nil {
		return versionOutcome(branch, v)
	}
	source := in.Workspace.Slug
	found, err := m.changes.OpenChangeOfBranch(ctx, source, branch)
	if err != nil {
		return m.failed(ctx, in, branch, err)
	}
	// A change with no version reported already has the pushed head as
	// its current version, a move that records nothing.
	if found != nil {
		return Outcome{Branch: branch}
	}
	switch {
	case u.Direct:
		if opts.Change {
			return Outcome{Branch: branch, Kind: OutcomeWarning, Reason: reasonDirectPush}
		}
		return Outcome{Branch: branch}
	case !opts.Change && !autoChange:
		return Outcome{Branch: branch}
	}
	opened, err := m.changes.OpenChange(ctx, openRequest(in, opts, branch, u.After))
	var exists *ChangeExistsError
	switch {
	case errors.As(err, &exists):
		// Another push opened the change first: this push is a version of
		// it, as it would have been had that push finished first.
		v, err := m.changes.RecordPushVersion(ctx, VersionRequest{
			Workspace: exists.Workspace,
			Number:    exists.Number,
			Source:    source,
			Branch:    branch,
			Head:      u.After,
			Caller:    in.Caller,
			SessionID: in.SessionID,
		})
		if err != nil {
			return m.failed(ctx, in, branch, err)
		}
		if v == nil {
			return Outcome{Branch: branch}
		}
		return versionOutcome(branch, v)
	case err != nil:
		return m.failed(ctx, in, branch, err)
	}
	return Outcome{Branch: branch, Kind: OutcomeOpened, Workspace: opened.Workspace, Number: opened.Number, Version: 1, Draft: opened.Draft}
}

// openRequest is the open of the change of branch at head: in the pushed
// workspace, or in the parent of a pushed fork, where the change lands,
// from the pushed branch, by the pusher. Every open of one push carries the
// same options, and an option the push left out is left to the change's
// defaults.
func openRequest(in MappingInput, opts PushOptions, branch, head string) OpenRequest {
	ws := in.Workspace
	target := ws.Slug
	if ws.Kind == access.KindFork && ws.ParentSlug != nil {
		target = *ws.ParentSlug
	}
	return OpenRequest{
		Workspace: target,
		Source:    ws.Slug,
		Branch:    branch,
		Head:      head,
		Target:    opts.Target,
		Title:     opts.Title,
		Intent:    opts.Intent,
		Draft:     opts.Draft,
		SessionID: in.SessionID,
		Caller:    in.Caller,
	}
}

func versionOutcome(branch string, v *pushrule.Version) Outcome {
	return Outcome{Branch: branch, Kind: OutcomeVersion, Workspace: v.WorkspaceSlug, Number: v.Number, Version: v.Version}
}

// failed is the warning for a branch whose change could not be opened or
// recorded. The push stands: its refs moved before the mapping ran. The
// error is logged and its text never sent, since it may name what the
// client must not see.
func (m *ChangeMapper) failed(ctx context.Context, in MappingInput, branch string, err error) Outcome {
	m.mu.RLock()
	logger := m.logger
	m.mu.RUnlock()
	if logger != nil {
		logger.LogAttrs(ctx, slog.LevelError, "git push change mapping failed",
			slog.String("request_id", server.RequestID(ctx)),
			slog.String("workspace", in.Workspace.Slug),
			slog.String("branch", branch),
			slog.String("error", err.Error()),
		)
	}
	return Outcome{Branch: branch, Kind: OutcomeWarning, Reason: reasonInternal}
}
