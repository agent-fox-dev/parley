package carrypatch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/jobs"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// patchStatus is the summary of a carry-patch workspace the patch-status
// route answers: every member present, null where it is unset.
type patchStatus struct {
	WorkspaceSlug        string             `json:"workspace_slug"`
	WorkspaceMode        string             `json:"workspace_mode"`
	Status               string             `json:"status"`
	GitURL               *string            `json:"git_url"`
	UpstreamURL          *string            `json:"upstream_url"`
	IntegrationBranch    string             `json:"integration_branch"`
	CloneError           *string            `json:"clone_error"`
	SyncError            *string            `json:"sync_error"`
	UpstreamHeadSHA      *string            `json:"upstream_head_sha"`
	LastSyncAt           *timefmt.Time      `json:"last_sync_at"`
	IntegrationHeadSHA   *string            `json:"integration_head_sha"`
	LastRebuild          *rebuildSummary    `json:"last_rebuild"`
	LastCompletedRebuild *rebuildSummary    `json:"last_completed_rebuild"`
	Patches              []patchStatusEntry `json:"patches"`
	Summary              patchCounts        `json:"summary"`
}

// rebuildSummary is a rebuild job as the patch status names it.
type rebuildSummary struct {
	ID          string        `json:"id"`
	Status      string        `json:"status"`
	CreatedAt   timefmt.Time  `json:"created_at"`
	CompletedAt *timefmt.Time `json:"completed_at"`
}

// patchStatusEntry is a patch as the patch status lists it, with the
// status the newest rebuild that has a result for it gave it.
type patchStatusEntry struct {
	ID                string        `json:"id"`
	BranchName        string        `json:"branch_name"`
	Position          *int          `json:"position"`
	Status            string        `json:"status"`
	ConflictFiles     []string      `json:"conflict_files"`
	LastRebuildResult *string       `json:"last_rebuild_result"`
	OriginSyncState   *string       `json:"origin_sync_state"`
	OriginSHA         *string       `json:"origin_sha"`
	OriginSyncedAt    *timefmt.Time `json:"origin_synced_at"`
}

// patchCounts are the counts of the patch status, each always present.
type patchCounts struct {
	TotalPatches           int `json:"total_patches"`
	Active                 int `json:"active"`
	Conflict               int `json:"conflict"`
	Disabled               int `json:"disabled"`
	MergedUpstream         int `json:"merged_upstream"`
	RerereResolutions      int `json:"rerere_resolutions"`
	PatchesDiverged        int `json:"patches_diverged"`
	PatchesMissingOnOrigin int `json:"patches_missing_on_origin"`
}

// resultsPageSize is how many rebuild jobs one read of the history takes
// while it looks for the last result of each patch.
const resultsPageSize = 100

// patchStatusRoutes are the routes of the patch status: a read route, which
// serves in every state of the workspace. The summary is the workspace's
// own data, not the patch list's, so it needs workspaces:read.
func (c *Component) patchStatusRoutes() []server.Route {
	rt := Route(ReadRoute, server.Route{Method: http.MethodGet, Pattern: "/workspaces/:slug/patch-status", Handler: c.patchStatusRoute})
	rt.Scope = scopeWorkspacesRead
	return []server.Route{rt}
}

// patchStatusRoute answers GET /workspaces/:slug/patch-status: the stored
// workspace record, the head of its integration branch in the copy, its
// latest rebuilds, its patches that are not deleted, never paged, and the
// counts, so that a reader sees the state at a glance even when the
// remotes are down. It reads the store and the copy's refs only: it
// contacts no remote and writes nothing.
func (c *Component) patchStatusRoute(w http.ResponseWriter, r *http.Request) error {
	ws, err := c.Guard(r)
	if err != nil {
		return err
	}
	if c.d.Jobs == nil {
		return errNotBound
	}
	ctx, q := r.Context(), c.d.Store.Main()
	branch := IntegrationBranch(ws)
	doc := patchStatus{
		WorkspaceSlug: ws.Slug, WorkspaceMode: ws.Mode, Status: string(ws.Status),
		GitURL: ws.GitURL, UpstreamURL: ws.UpstreamURL, IntegrationBranch: branch,
		CloneError: ws.CloneError, SyncError: ws.SyncError, UpstreamHeadSHA: ws.UpstreamHeadSHA, LastSyncAt: timeOf(ws.LastSyncAt),
		IntegrationHeadSHA: c.integrationHead(ctx, ws.Slug, branch),
	}
	if doc.LastRebuild, err = c.latestRebuild(ctx, ws.Slug, ""); err != nil {
		return err
	}
	if doc.LastCompletedRebuild, err = c.latestRebuild(ctx, ws.Slug, jobs.StatusCompleted); err != nil {
		return err
	}
	patches, err := ListLivePatches(ctx, q, ws.Slug)
	if err != nil {
		return fmt.Errorf("list the patches of %s: %w", ws.Slug, err)
	}
	results, err := c.lastRebuildResults(ctx, ws.Slug, patches)
	if err != nil {
		return err
	}
	if doc.Summary.RerereResolutions, err = CountRerereEntries(ctx, q, ws.Slug); err != nil {
		return err
	}
	doc.Patches = make([]patchStatusEntry, 0, len(patches))
	for _, p := range patches {
		e := patchStatusEntry{
			ID: p.ID, BranchName: p.BranchName, Position: p.Position, Status: p.Status, ConflictFiles: p.ConflictFiles,
			OriginSyncState: p.OriginSyncState, OriginSHA: p.OriginSHA, OriginSyncedAt: timeOf(p.OriginSyncedAt),
		}
		if e.ConflictFiles == nil {
			e.ConflictFiles = []string{}
		}
		if s, ok := results[p.ID]; ok {
			e.LastRebuildResult = &s
		}
		doc.Patches = append(doc.Patches, e)
		doc.Summary.count(p)
	}
	server.WriteJSON(w, http.StatusOK, doc)
	return nil
}

// count adds the patch p, which is not deleted, to the counts.
func (n *patchCounts) count(p Patch) {
	n.TotalPatches++
	switch p.Status {
	case StatusActive:
		n.Active++
	case StatusConflict:
		n.Conflict++
	case StatusDisabled:
		n.Disabled++
	case StatusMergedUpstream:
		n.MergedUpstream++
	}
	if p.OriginSyncState != nil {
		switch *p.OriginSyncState {
		case originDiverged:
			n.PatchesDiverged++
		case originMissing:
			n.PatchesMissingOnOrigin++
		}
	}
}

// integrationHead is the commit refs/heads/<branch> points at in the copy
// of the workspace slug, nil when there is no copy or no such branch. A
// copy git cannot read answers nil too: the summary answers from stored
// state whatever the copy's condition.
func (c *Component) integrationHead(ctx context.Context, slug, branch string) *string {
	if c.d.RepoPath == nil {
		return nil
	}
	var head *string
	_ = RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		repo, err := c.d.Git.Open(ctx, c.d.RepoPath(slug))
		if err != nil {
			return err
		}
		id, found, err := repo.ResolveBranch(ctx, branch)
		if err == nil && found {
			head = &id
		}
		return err
	})
	return head
}

// latestRebuild is the newest rebuild job of the workspace slug, in status
// when it is not "", nil when there is none.
func (c *Component) latestRebuild(ctx context.Context, slug, status string) (*rebuildSummary, error) {
	js, err := c.d.Jobs.List(ctx, RebuildJobType, slug, status, nil, 1)
	if err != nil || len(js) == 0 {
		return nil, err
	}
	j := js[0]
	return &rebuildSummary{ID: j.ID, Status: j.Status, CreatedAt: timefmt.Time(j.CreatedAt), CompletedAt: timeOf(j.CompletedAt)}, nil
}

// lastRebuildResults maps the id of each of the patches to its status in
// the newest rebuild job of the workspace slug whose result has an entry
// for it; a patch no job has a result for is absent. It reads the history
// newest first, a page at a time, and stops once every patch has its
// result. A result document that does not decode gives no entry.
func (c *Component) lastRebuildResults(ctx context.Context, slug string, patches []Patch) (map[string]string, error) {
	wanted := make(map[string]bool, len(patches))
	for _, p := range patches {
		wanted[p.ID] = true
	}
	found := make(map[string]string, len(patches))
	var after *jobs.ListPosition
	for len(found) < len(wanted) {
		js, err := c.d.Jobs.List(ctx, RebuildJobType, slug, "", after, resultsPageSize)
		if err != nil {
			return nil, err
		}
		for _, j := range js {
			var res struct {
				PatchResults []struct {
					PatchID string `json:"patch_id"`
					Status  string `json:"status"`
				} `json:"patch_results"`
			}
			if len(j.Result) == 0 || json.Unmarshal(j.Result, &res) != nil {
				continue
			}
			for _, pr := range res.PatchResults {
				if _, done := found[pr.PatchID]; wanted[pr.PatchID] && !done {
					found[pr.PatchID] = pr.Status
				}
			}
		}
		if len(js) < resultsPageSize {
			break
		}
		pos := js[len(js)-1].Position()
		after = &pos
	}
	return found, nil
}
