package carrypatch

import (
	"context"
	"fmt"

	"github.com/agent-fox-dev/parley/internal/land"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// LandGuard is the carry-patch target guard of the landing: it holds a
// land onto a patch branch to the rules a push of that branch obeys, so
// that a landing is no way around them. It reads the workspace record, the
// settings and the patch list through the same readers as PushControl, the
// component's own unless a test replaces them, and forwards with
// Forwarder.
type LandGuard struct {
	Records   RecordReader
	Settings  SettingsReader
	Patches   PatchReader
	Forwarder Forwarder
}

// LandGuard is the component's landing guard, which reads the component's
// own store and settings registry and forwards with the component's
// Forward.
func (c *Component) LandGuard() *LandGuard {
	return &LandGuard{Records: c, Settings: c, Patches: c, Forwarder: c}
}

// Check answers for a land that would move the branch in.Branch of the
// workspace in.Workspace to in.New, before the land moves it:
//
//   - a workspace not in mode carry_patch, a branch that is no live
//     patch's, and a patch branch whose authority is the hub: allowed,
//     without contacting origin;
//   - a patch branch synced from origin without push_patches_to_origin:
//     refused with the pointer to origin a push is refused with, which
//     ends the attempt blocked;
//   - a patch branch synced from origin with push_patches_to_origin: in.New
//     is forwarded to origin from the hub's copy, where the land built it,
//     and the land allowed once origin took it; any failure of the forward,
//     an unreadable credential and the forward limit included, is origin
//     rejected with the forward's message, which ends the attempt failed
//     origin_rejected.
//
// A read that fails is an error, never an allow: the land engine ends the
// attempt failed internal. The guard never writes the hub's branch; the
// land does, after it.
func (g *LandGuard) Check(ctx context.Context, in land.TargetInput) (land.TargetAnswer, error) {
	rec, found, err := g.Records.WorkspaceRecord(ctx, in.Workspace)
	switch {
	case err != nil:
		return land.TargetAnswer{}, fmt.Errorf("judge the land onto %s of %s: %w", in.Branch, in.Workspace, err)
	case !found:
		return land.TargetAnswer{}, fmt.Errorf("judge the land onto %s of %s: no workspace record", in.Branch, in.Workspace)
	case rec.Mode != workspace.ModeCarryPatch:
		return land.TargetAnswer{}, nil
	}
	settings, err := g.Settings.EffectiveSettings(ctx, rec)
	if err != nil {
		return land.TargetAnswer{}, fmt.Errorf("judge the land onto %s of %s: read the settings: %w", in.Branch, in.Workspace, err)
	}
	_, live, err := g.Patches.LivePatchOfBranch(ctx, in.Workspace, in.Branch)
	switch {
	case err != nil:
		return land.TargetAnswer{}, fmt.Errorf("judge the land onto %s of %s: %w", in.Branch, in.Workspace, err)
	case !live:
		return land.TargetAnswer{}, nil
	}
	// A missing or mistyped value reads as the key's default: hub, false.
	source, _ := settings[patchBranchSourceKey].(string)
	forward, _ := settings[pushPatchesToOriginKey].(bool)
	switch {
	case source != "origin":
		return land.TargetAnswer{}, nil
	case !forward:
		return land.TargetAnswer{Refuse: fmt.Sprintf(msgPushToOrigin, shownOriginURL(rec.GitURL))}, nil
	case g.Forwarder == nil:
		return land.TargetAnswer{}, fmt.Errorf("judge the land onto %s of %s: %w", in.Branch, in.Workspace, errNoForwarder)
	}
	if err := g.Forwarder.Forward(ctx, nil, rec, in.Branch, in.New); err != nil {
		return land.TargetAnswer{OriginRejected: err.Error()}, nil
	}
	return land.TargetAnswer{}, nil
}
