package workspace

import (
	"context"
	"encoding/json"

	"github.com/agent-fox-dev/parley/internal/identity"
)

// SyncRun is one sync of a carry-patch workspace as a sync extension sees
// it.
type SyncRun struct {
	// Workspace is the workspace's record as the sync read it before the
	// fetch.
	Workspace Workspace
	// Actor is the caller of the sync.
	Actor identity.Principal
	// ResetToOrigin is the caller's reset_to_origin.
	ResetToOrigin bool
	// OriginHeads maps every branch of origin, without refs/heads/, to its
	// head as the sync's fetch found it, which the copy's origin heads now
	// hold. The map is the extension's own.
	OriginHeads map[string]string
}

// SyncAddition is what the second stage of a sync extension adds to a
// completed sync.
type SyncAddition struct {
	// Members are added to the top level of the sync's answer, by name,
	// beside the workspace object's members and branches; a member whose
	// name one of those has already is left out. Each value must be JSON.
	Members map[string]json.RawMessage
	// UpstreamHeadSHA, when set, is written as the workspace's
	// upstream_head_sha in the transaction that records the completion; nil
	// leaves upstream_head_sha as it is.
	UpstreamHeadSHA *string
}

// SyncExtension is the part of a sync of a carry-patch workspace that the
// feature maintaining the patches adds, called at two points of the sync so
// that this package needs no knowledge of that feature. Neither stage runs
// for a standard workspace. Both run on the sync's own context, which the
// caller's disconnect does not cancel.
type SyncExtension interface {
	// AfterFetch runs once the fetch of origin has succeeded, before any tag
	// or branch of the copy is created or moved. An error ends the sync as
	// a fetch that failed: nothing moves, sync_error holds the error's
	// scrubbed text and the answer is 502 upstream_error with it.
	AfterFetch(ctx context.Context, run SyncRun) error
	// BeforeCompletion runs once the per-branch pass has handled every
	// branch, with its results in the answer's order, before the completion
	// is recorded. An error stops the sync with 500 internal and its
	// scrubbed text, without recording the completion; the branch moves
	// made by then stay.
	BeforeCompletion(ctx context.Context, run SyncRun, results []BranchResult) (SyncAddition, error)
}

// RegisterSyncExtension makes ext the extension of the syncs of carry-patch
// workspaces, replacing the one registered before; nil removes it. The
// feature that maintains the patches registers it when the hub is composed;
// without one a carry-patch workspace syncs like a standard one, less the
// branches excludedBranches leaves out.
func (s *Service) RegisterSyncExtension(ext SyncExtension) {
	s.extMu.Lock()
	defer s.extMu.Unlock()
	s.syncExt = ext
}

// syncExtension is the extension of a sync of w: the registered one for a
// carry-patch workspace, nil for any other or when none is registered.
func (s *Service) syncExtension(w Workspace) SyncExtension {
	if w.Mode != ModeCarryPatch {
		return nil
	}
	s.extMu.RLock()
	defer s.extMu.RUnlock()
	return s.syncExt
}
