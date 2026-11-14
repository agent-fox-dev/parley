package workspace

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// Object is the workspace object of the REST API: every field of the record,
// a null one written as null and never left out, and the members computed
// for the caller. The instance id, the maintenance marker and the creating
// token are the hub's own and not shown.
type Object struct {
	Slug              string          `json:"slug"`
	Kind              string          `json:"kind"`
	WorkspaceMode     string          `json:"workspace_mode"`
	GitURL            *string         `json:"git_url"`
	DefaultBranch     *string         `json:"default_branch"`
	OwnerID           string          `json:"owner_id"`
	OrgID             string          `json:"org_id"`
	OrgSlug           string          `json:"org_slug"`
	Status            string          `json:"status"`
	DisplayName       string          `json:"display_name"`
	Description       string          `json:"description"`
	HeadSHA           *string         `json:"head_sha"`
	OriginHeadSHA     *string         `json:"origin_head_sha"`
	CloneError        *string         `json:"clone_error"`
	LastSyncAt        *timefmt.Time   `json:"last_sync_at"`
	SyncError         *string         `json:"sync_error"`
	UpstreamURL       *string         `json:"upstream_url"`
	IntegrationBranch *string         `json:"integration_branch"`
	UpstreamHeadSHA   *string         `json:"upstream_head_sha"`
	ParentSlug        *string         `json:"parent_slug"`
	ForkPoint         *ForkPointValue `json:"fork_point"`
	ExpiresAt         *timefmt.Time   `json:"expires_at"`
	AutoChange        *bool           `json:"auto_change"`
	Purpose           *string         `json:"purpose"`
	SessionID         *string         `json:"session_id"`
	Settings          map[string]any  `json:"settings"`
	HubURL            *string         `json:"hub_url"`
	Access            string          `json:"access"`
	CreatedAt         timefmt.Time    `json:"created_at"`
	UpdatedAt         timefmt.Time    `json:"updated_at"`
}

// ForkPointValue is the fork_point member of a task fork's object.
type ForkPointValue struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

// ForkView completes the object o of the task fork w, such as with the
// settings of its parent. The fork feature registers it, so that this
// package shows a fork without importing that feature.
type ForkView func(ctx context.Context, w Workspace, o *Object) error

// RegisterForkView makes v the view RenderWorkspace applies to every record
// of kind fork; nil removes it.
func (s *Service) RegisterForkView(v ForkView) {
	s.viewMu.Lock()
	defer s.viewMu.Unlock()
	s.forkView = v
}

// RenderWorkspace returns the object of w for a caller whose level on it is
// level. It is the one place a workspace object is built, so that every
// route answers a workspace the same way: org_slug is read from the
// workspace's org, hub_url is the workspace's git remote on the hub, null
// when no external URL is configured, and settings holds every registered
// key with its effective value. A record of kind fork goes through the
// registered fork view last.
func (s *Service) RenderWorkspace(ctx context.Context, w Workspace, level access.Level) (Object, error) {
	org, err := identity.OrgByID(ctx, s.deps.Store.Main(), w.OrgID)
	if err != nil {
		return Object{}, fmt.Errorf("render workspace %q: the org: %w", w.Slug, err)
	}
	o := Object{
		Slug: w.Slug, Kind: w.Kind, WorkspaceMode: w.Mode, GitURL: w.GitURL, DefaultBranch: w.DefaultBranch,
		OwnerID: w.OwnerID, OrgID: w.OrgID, OrgSlug: org.Slug, Status: string(w.Status),
		DisplayName: w.DisplayName, Description: w.Description,
		HeadSHA: w.HeadSHA, OriginHeadSHA: w.OriginHeadSHA, CloneError: w.CloneError,
		LastSyncAt: timeValue(w.LastSyncAt), SyncError: w.SyncError,
		UpstreamURL: w.UpstreamURL, IntegrationBranch: w.IntegrationBranch, UpstreamHeadSHA: w.UpstreamHeadSHA,
		ParentSlug: w.ParentSlug, ExpiresAt: timeValue(w.ExpiresAt), AutoChange: w.AutoChange,
		Purpose: w.Purpose, SessionID: w.SessionID,
		Settings: s.settings.EffectiveSettings(w.Settings, w.DefaultBranch),
		HubURL:   s.hubURL(org.Slug, w.Slug), Access: string(level),
		CreatedAt: timefmt.Time(w.CreatedAt), UpdatedAt: timefmt.Time(w.UpdatedAt),
	}
	if w.ForkPoint != nil {
		o.ForkPoint = &ForkPointValue{Ref: w.ForkPoint.Ref, SHA: w.ForkPoint.SHA}
	}
	s.viewMu.RLock()
	view := s.forkView
	s.viewMu.RUnlock()
	if w.Kind == access.KindFork && view != nil {
		if err := view(ctx, w, &o); err != nil {
			return Object{}, fmt.Errorf("render workspace %q: the fork view: %w", w.Slug, err)
		}
	}
	return o, nil
}

// hubURL is the git remote of the workspace slug of the org orgSlug on the
// hub: the external URL without one trailing slash, then
// /git/<org>/<slug>.git; nil when no external URL is configured.
func (s *Service) hubURL(orgSlug, slug string) *string {
	if s.deps.ExternalURL == "" {
		return nil
	}
	u := strings.TrimSuffix(s.deps.ExternalURL, "/") + "/git/" + orgSlug + "/" + slug + ".git"
	return &u
}

// timeValue is t as a response member, nil when t is nil.
func timeValue(t *time.Time) *timefmt.Time {
	if t == nil {
		return nil
	}
	v := timefmt.Time(*t)
	return &v
}
