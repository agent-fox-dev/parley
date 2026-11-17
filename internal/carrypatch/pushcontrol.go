package carrypatch

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// pushPatchesToOriginKey is the workspace setting that tells whether the
// hub pushes patch branches to origin: forwarded before an update is
// accepted when origin is their authority, mirrored after it when the hub
// is.
const pushPatchesToOriginKey = "push_patches_to_origin"

// The messages carry-patch push control refuses an update with; the %s of
// msgIntegrationBranch is the integration branch, that of msgPushToOrigin
// the origin URL without user information.
const (
	msgIntegrationBranch = "integration branch %s is maintained by the hub; push a patch branch and rebuild instead"
	msgPushToOrigin      = "branch is synced from origin; push to %s instead"
	msgDeleteOnOrigin    = "branch is synced from origin; delete it on origin instead"
)

// RecordReader reads the record of the workspace slug; found is false when
// there is none.
type RecordReader interface {
	WorkspaceRecord(ctx context.Context, slug string) (w workspace.Workspace, found bool, err error)
}

// SettingsReader reads the effective settings of the workspace w, every
// registered key with its stored value or its default, read anew on every
// call so that a change of the settings counts from the next decision on.
type SettingsReader interface {
	EffectiveSettings(ctx context.Context, w workspace.Workspace) (map[string]any, error)
}

// PatchReader looks up the patch of branch in the workspace slug whose
// status is not deleted; found is false when there is none.
type PatchReader interface {
	LivePatchOfBranch(ctx context.Context, slug, branch string) (p Patch, found bool, err error)
}

// Forwarder pushes commit, read from repo, to refs/heads/<branch> on the
// origin of the workspace w, without force, and returns nil once origin
// took it, or an error whose text is the message the update is refused
// with. repo is the repository the update is written to, which holds the
// commit (for a git push the view of its quarantine); nil is the hub's
// copy of the workspace, for an update whose commit is already there.
type Forwarder interface {
	Forward(ctx context.Context, repo *gitrepo.Repo, w workspace.Workspace, branch, commit string) error
}

// PushControl is the carry-patch controller of the push rules: it decides
// how the integration branch and the patch branches of a carry-patch
// workspace may move, by whatever surface the write comes. It only reads
// while it decides; the forward it may answer with runs later, when the
// ref writer runs it.
type PushControl struct {
	Records   RecordReader
	Settings  SettingsReader
	Patches   PatchReader
	Forwarder Forwarder
}

// PushControl is the component's carry-patch controller, which reads the
// component's own store and settings registry and forwards with the
// component's Forward.
func (c *Component) PushControl() *PushControl {
	return &PushControl{Records: c, Settings: c, Patches: c, Forwarder: c}
}

// Decide answers for one update of the carry-patch workspace ws:
//
//   - the integration branch: refused for every caller and every setting,
//     before anything else is read, since only the hub's rebuild writes it;
//   - a ref outside refs/heads/: allowed;
//   - a branch that is no live patch's: allowed;
//   - a patch branch: allowed when the hub is its authority
//     (patch_branch_source hub, the default); with origin, refused with a
//     pointer to origin unless push_patches_to_origin is on, in which case a
//     delete is refused and a create or update is allowed after a forward
//     to origin.
//
// The settings are read once for every branch that is not the integration
// branch, before the patch list, so that a decision never stands on
// settings that could not be read. Any read that fails is an error, which
// the engine turns into a retryable refusal: an update is never let through
// unchecked.
func (p *PushControl) Decide(ctx context.Context, ws pushrule.Workspace, _ pushrule.Caller, u pushrule.Update) (pushrule.Answer, error) {
	rec, found, err := p.Records.WorkspaceRecord(ctx, ws.Slug)
	switch {
	case err != nil:
		return pushrule.Answer{}, fmt.Errorf("decide %s of %s: %w", u.Ref, ws.Slug, err)
	case !found:
		return pushrule.Answer{}, fmt.Errorf("decide %s of %s: no workspace record", u.Ref, ws.Slug)
	}
	branch, isBranch := strings.CutPrefix(u.Ref, headsPrefix)
	if !isBranch || branch == "" {
		return pushrule.Allow(), nil
	}
	if integration := IntegrationBranch(rec); branch == integration {
		return pushrule.Refuse(fmt.Sprintf(msgIntegrationBranch, integration), pushrule.ErrorTypeIntegrationBranch), nil
	}
	settings, err := p.Settings.EffectiveSettings(ctx, rec)
	if err != nil {
		return pushrule.Answer{}, fmt.Errorf("decide %s of %s: read the settings: %w", u.Ref, ws.Slug, err)
	}
	_, live, err := p.Patches.LivePatchOfBranch(ctx, ws.Slug, branch)
	switch {
	case err != nil:
		return pushrule.Answer{}, fmt.Errorf("decide %s of %s: %w", u.Ref, ws.Slug, err)
	case !live:
		return pushrule.Allow(), nil
	}
	// A missing or mistyped value reads as the key's default: hub, false.
	source, _ := settings[patchBranchSourceKey].(string)
	forward, _ := settings[pushPatchesToOriginKey].(bool)
	switch {
	case source != "origin":
		return pushrule.Allow(), nil
	case !forward:
		return pushrule.Refuse(fmt.Sprintf(msgPushToOrigin, shownOriginURL(rec.GitURL)), pushrule.ErrorTypePatchBranch), nil
	case u.After == pushrule.ZeroID:
		// The hub never deletes a branch on origin.
		return pushrule.Refuse(msgDeleteOnOrigin, pushrule.ErrorTypePatchBranch), nil
	case p.Forwarder == nil:
		return pushrule.Answer{}, fmt.Errorf("decide %s of %s: %w", u.Ref, ws.Slug, errNoForwarder)
	}
	f, after := p.Forwarder, u.After
	return pushrule.AllowAfterForward(func(ctx context.Context, repo *gitrepo.Repo) error {
		return f.Forward(ctx, repo, rec, branch, after)
	}), nil
}

// errNoForwarder is the failure of a decision that needs a forward from a
// controller that has nothing to forward with.
var errNoForwarder = errors.New("no forwarder to push to origin with")

// shownOriginURL is the origin URL as a refusal shows it: without the user
// name and password a stored URL may carry, and as stored when it is no URL
// with a host, such as git@host:path. A workspace without one is pointed
// to its origin by name.
func shownOriginURL(gitURL *string) string {
	if gitURL == nil || *gitURL == "" {
		return "origin"
	}
	u, err := url.Parse(*gitURL)
	if err != nil || u.Host == "" {
		return *gitURL
	}
	u.User = nil
	return u.String()
}

// WorkspaceRecord reads the record of the workspace slug from the store.
func (c *Component) WorkspaceRecord(ctx context.Context, slug string) (workspace.Workspace, bool, error) {
	if c.d.Store == nil {
		return workspace.Workspace{}, false, errNotBound
	}
	return workspace.BySlug(ctx, c.d.Store.Main(), slug)
}

// EffectiveSettings are the effective settings of w from the settings
// registry; without one every key reads as missing, which is its default.
func (c *Component) EffectiveSettings(_ context.Context, w workspace.Workspace) (map[string]any, error) {
	if c.d.Settings == nil {
		return map[string]any{}, nil
	}
	return c.d.Settings.EffectiveSettings(w.Settings, w.DefaultBranch), nil
}

// LivePatchOfBranch reads the patch of branch in the workspace slug whose
// status is not deleted.
func (c *Component) LivePatchOfBranch(ctx context.Context, slug, branch string) (Patch, bool, error) {
	if c.d.Store == nil {
		return Patch{}, false, errNotBound
	}
	return LivePatchOfBranch(ctx, c.d.Store.Main(), slug, branch)
}
