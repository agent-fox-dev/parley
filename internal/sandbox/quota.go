package sandbox

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The names of the four sandbox limits, as the configuration and a
// sandbox_capacity error name them.
const (
	LimitSandboxes    = "max_sandboxes"
	LimitPerOrg       = "max_per_org"
	LimitPerWorkspace = "max_per_workspace"
	LimitPerUser      = "max_per_user"
)

// CheckQuota refuses one more running sandbox on the workspace w for the
// user userID when it would pass one of the four limits of the settings:
// it counts, through tx, the sandboxes in state provisioning or ready of
// the whole hub, of w's org, of w's family (a fork counts toward its
// parent's) and of the sandboxes userID created on any workspace, and
// answers 429 sandbox_capacity for the first of max_sandboxes, max_per_org,
// max_per_workspace and max_per_user whose count is not below its value,
// with the limit, its value and the count. A workspace in no org is not
// held to max_per_org.
//
// tx is the store transaction (store.Store.Tx) that then stores the
// sandbox or moves it to provisioning: the store runs its transactions one
// at a time, so no other request can count or write between the count and
// the write, and the limits hold under concurrent requests. The create
// calls it; a start calls it for a sandbox that leaves an ended state.
func (s *Service) CheckQuota(ctx context.Context, tx *sql.Tx, w workspace.Workspace, userID string) error {
	var hub, org, family, user int
	err := tx.QueryRowContext(ctx, `SELECT count(*),
		coalesce(sum(org_id = ?), 0),
		coalesce(sum(family_root = ?), 0),
		coalesce(sum(user_id = ?), 0)
		FROM sandboxes WHERE state IN (?, ?)`,
		w.OrgID, FamilyRoot(w), userID, StateProvisioning, StateReady).Scan(&hub, &org, &family, &user)
	if err != nil {
		return fmt.Errorf("count the running sandboxes: %w", err)
	}
	set := s.settings
	checks := []struct {
		limit          string
		maximum, count int
		skip           bool
	}{
		{LimitSandboxes, set.MaxSandboxes, hub, false},
		{LimitPerOrg, set.MaxPerOrg, org, w.OrgID == ""},
		{LimitPerWorkspace, set.MaxPerWorkspace, family, false},
		{LimitPerUser, set.MaxPerUser, user, false},
	}
	for _, c := range checks {
		if !c.skip && c.count >= c.maximum {
			return server.NewError("sandbox_capacity", http.StatusTooManyRequests, "sandbox limit reached: "+c.limit).
				With("limit", c.limit).With("maximum", c.maximum).With("current", c.count)
		}
	}
	return nil
}

// FamilyRoot is the slug of the root of w's workspace family: its parent's
// for a fork, its own otherwise. A family's sandboxes share max_per_workspace.
func FamilyRoot(w workspace.Workspace) string {
	if w.Kind == access.KindFork && w.ParentSlug != nil {
		return *w.ParentSlug
	}
	return w.Slug
}
