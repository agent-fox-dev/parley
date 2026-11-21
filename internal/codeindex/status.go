package codeindex

import (
	"context"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/jobs"
	"github.com/agent-fox-dev/parley/internal/metrics"
	"github.com/agent-fox-dev/parley/internal/server"
)

// The states of a workspace's index, as the status route names them.
const (
	StateOff      = "off"
	StateQueued   = "queued"
	StateBuilding = "building"
	StateReady    = "ready"
	StateFailed   = "failed"
)

// maxErrorBytes is the most bytes of a failure's text the hub keeps.
const maxErrorBytes = 1024

// The scope a personal access token needs for the status route, and the op
// its requests are counted under, both those of the source control reads.
const (
	scopeRead = "git:read"
	opStatus  = "code_index"
)

// Status is what the status route answers for a workspace.
type Status struct {
	// Workspace is the workspace the status is of: the parent of a fork.
	Workspace string `json:"workspace"`
	// Enabled is true when the hub has the section and the workspace's
	// setting is on.
	Enabled bool   `json:"enabled"`
	State   string `json:"state"`
	// HeadSHA is the head of the default branch, nil while there is none.
	HeadSHA *string `json:"head_sha"`
	// IndexedSHA, Partial, FilesIndexed and BuiltAt describe the index
	// there is, in every state.
	IndexedSHA   *string `json:"indexed_sha"`
	Partial      bool    `json:"partial"`
	FilesIndexed int     `json:"files_indexed"`
	BuiltAt      *string `json:"built_at"`
	// Error is the failure of the last build, nil unless it failed.
	Error *string `json:"error"`
}

// Routes are the component's routes: the status of a workspace's index, a
// read of the source control API by its scope, its access rule and the
// metric its requests are counted in.
func (c *Component) Routes() []server.Route {
	return []server.Route{{
		Method: http.MethodGet, Pattern: "/workspaces/:slug/scm/code-index", Scope: scopeRead,
		Marks:   map[string]string{access.MarkNeed: string(access.NeedRead)},
		Handler: c.getStatus,
	}}
}

// getStatus answers GET …/scm/code-index. A fork has no index of its own:
// it is answered for its parent, whose slug the answer names.
func (c *Component) getStatus(w http.ResponseWriter, r *http.Request) (err error) {
	defer func() {
		result := "ok"
		if err != nil {
			result = "error"
		}
		_ = c.deps.Metrics.Add(metrics.SCMRequestsTotal, map[string]string{"op": opStatus, "result": result}, 1)
	}()
	ctx := r.Context()
	ws, found, err := c.deps.Workspace(ctx, server.Param(r, "slug"))
	if err == nil && found && ws.Parent != "" {
		ws, found, err = c.deps.Workspace(ctx, ws.Parent)
	}
	if err != nil {
		return err
	}
	if !found {
		// Deleted since the access check.
		return server.NotFound("workspace not found")
	}
	st, err := c.Status(ctx, ws)
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, st)
	return nil
}

// Status is the status of the index of ws: off where the hub keeps none of
// it; building while a build of it runs; ready when the index is at the
// head; failed when the last build of the head failed; queued otherwise,
// with a build pending or due, a workspace without a repository yet
// included.
func (c *Component) Status(ctx context.Context, ws Workspace) (Status, error) {
	st := Status{Workspace: ws.Slug, Enabled: c.indexes(ws), State: StateOff}
	head := ""
	if ws.DefaultBranch != "" {
		if repo, err := c.deps.Repos(ctx, ws.Slug); err == nil {
			id, found, err := repo.ResolveBranch(ctx, ws.DefaultBranch)
			if err != nil {
				return Status{}, err
			}
			if found {
				head, st.HeadSHA = id, &id
			}
		}
	}
	rec, _, err := c.Record(ctx, ws.Slug)
	if err != nil {
		return Status{}, err
	}
	if rec.IndexedSHA != "" {
		st.IndexedSHA, st.Partial, st.FilesIndexed = &rec.IndexedSHA, rec.Partial, rec.FilesIndexed
		if rec.BuiltAt != nil {
			st.BuiltAt = new(rec.BuiltAt.UTC().Format(time.RFC3339))
		}
	}
	if rec.Error != "" {
		st.Error = &rec.Error
	}
	if !st.Enabled {
		return st, nil
	}
	running, err := c.deps.Jobs.List(ctx, JobType, ws.Slug, jobs.StatusRunning, nil, 1)
	if err != nil {
		return Status{}, err
	}
	switch {
	case len(running) > 0:
		st.State = StateBuilding
	case head != "" && rec.IndexedSHA == head && c.stored(ws.Slug, head):
		st.State = StateReady
	case head != "" && rec.FailedSHA == head:
		st.State = StateFailed
	default:
		st.State = StateQueued
	}
	return st, nil
}

// failureText is what the hub keeps of a failure's text: without what it
// keeps secret, and cut to maxErrorBytes between two characters.
func (c *Component) failureText(err error) string {
	text := c.deps.Scrub(err.Error())
	if len(text) > maxErrorBytes {
		cut := maxErrorBytes
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut]
	}
	// Whoever cut it, the text ends on a whole character.
	for len(text) > 0 {
		r, size := utf8.DecodeLastRuneInString(text)
		if r != utf8.RuneError || size != 1 {
			break
		}
		text = text[:len(text)-1]
	}
	return text
}
