package scm

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// The states of a change in which its source branch is in use: a write
// does not delete the branch of a change in either.
const (
	ChangeDraft = "draft"
	ChangeOpen  = "open"
)

// ChangeLinker is how the writes reach the changes of a workspace, which
// the change component keeps: it names the change a branch is the source
// of, and checks and opens the change a write asks for. Recording a version
// is none of its operations: the ref writer's ref-updated stage records the
// version a moved source branch makes, from the version kind each write
// names.
type ChangeLinker interface {
	// FindOpen is the draft or open change whose source is the branch,
	// named without refs/heads/, of the workspace slug, with its current
	// version; nil with a nil error is none.
	FindOpen(ctx context.Context, slug, branch string) (*LinkedChange, error)
	// CheckOpening checks, before the source branch moves, that the change
	// o asks for may be opened; an error is the write's refusal, answered
	// as it is when it is an API error.
	CheckOpening(ctx context.Context, o ChangeOpening) error
	// Open opens the change o asks for, once its source branch has moved.
	Open(ctx context.Context, o ChangeOpening) (*LinkedChange, error)
}

// LinkedChange is a change as the linker names it: the workspace it
// belongs to, its number, its current version and its state.
type LinkedChange struct {
	WorkspaceSlug string `json:"workspace_slug"`
	Number        int    `json:"number"`
	Version       int    `json:"version"`
	// State is the change's state, such as ChangeDraft or ChangeOpen.
	State string `json:"-"`
}

// inUse reports whether l is a change whose source branch a write may not
// delete: a draft or open one.
func (l *LinkedChange) inUse() bool {
	return l != nil && (l.State == ChangeDraft || l.State == ChangeOpen)
}

// ChangeOpening is a change a write asks to open: who asks, the workspace
// it is opened in, the branch it proposes as the write moves it, and what
// the caller said of it.
type ChangeOpening struct {
	Caller identity.Principal
	// Workspace is the slug of the workspace the change is opened in.
	Workspace string
	// SourceWorkspace and SourceBranch name the source branch, without
	// refs/heads/, and Head the commit the write moves it to.
	SourceWorkspace, SourceBranch, Head string
	TargetBranch, Title, Intent         string
	Draft                               bool
}

// RegisterChangeLinker makes l the linker the writes ask, replacing the one
// registered before; nil removes it. The change component registers it
// while the hub is composed, on this instance rather than in a registry of
// the package, so that every hub has its own
// (docs/adr/08-register-the-push-collaborators-on-the-ref-writer.md).
// Without one, no branch is a change's source and every opening is
// refused.
func (c *Component) RegisterChangeLinker(l ChangeLinker) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.linker = l
}

// changeLinker is the registered linker, or the default.
func (c *Component) changeLinker() ChangeLinker {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.linker == nil {
		return noLinker{}
	}
	return c.linker
}

// noLinker is the linker of a hub without a change component: no branch is
// a change's source, and a write that asks for a change is refused, since
// nothing could open it.
type noLinker struct{}

var errNoChanges = server.InvalidRequest("this hub cannot open a change: no change component is registered")

func (noLinker) FindOpen(context.Context, string, string) (*LinkedChange, error) { return nil, nil }

func (noLinker) CheckOpening(context.Context, ChangeOpening) error { return errNoChanges }

func (noLinker) Open(context.Context, ChangeOpening) (*LinkedChange, error) { return nil, errNoChanges }
