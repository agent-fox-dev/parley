package land

import (
	"fmt"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/server"
)

// conflictAnswer is the answer of GET …/changes/:number/conflict: the
// change's mergeable as it is now, clean or conflict, and its conflict
// report, null when clean.
type conflictAnswer struct {
	Mergeable string                  `json:"mergeable"`
	Conflict  *gitrepo.ConflictReport `json:"conflict"`
}

// conflictRoute answers the current conflict of the change :number of the
// workspace :slug (currentConflict). The change is resolved as every
// change route resolves it, so that a :number that names no change of the
// workspace, and, for a token bound to a fork calling on the fork's
// parent, a change whose source is not the fork, are both 404
// change_not_found. Unlike the land routes, which tell a bound token why
// they refuse it, a read of a change refuses a change the token may not
// see as if it did not exist.
func (c *Component) conflictRoute(w http.ResponseWriter, r *http.Request) error {
	ch, err := c.deps.Changes.RequestedChange(r)
	if err != nil {
		return err
	}
	report, err := c.currentConflict(r.Context(), ch)
	if err != nil {
		return err
	}
	answer := conflictAnswer{Mergeable: change.MergeableClean}
	if report != nil {
		answer = conflictAnswer{Mergeable: change.MergeableConflict, Conflict: report}
	}
	server.WriteJSON(w, http.StatusOK, answer)
	return nil
}

// conflictFileRoute answers the file the query's path names of the
// current conflict of the change :number of the workspace :slug
// (currentConflict) as text with conflict markers
// (gitrepo.Repo.ConflictText), text/plain. The change is resolved, and
// refused, as conflictRoute resolves it. A path that is missing or empty
// is 400 invalid_request; one that is no entry of the report, which every
// path of a clean change is, 404 path_not_found; an entry of another kind
// than content, which has no text both sides changed, 409
// not_text_conflict.
func (c *Component) conflictFileRoute(w http.ResponseWriter, r *http.Request) error {
	ch, err := c.deps.Changes.RequestedChange(r)
	if err != nil {
		return err
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		return server.InvalidRequest("path: must name a file of the change's conflict report")
	}
	report, err := c.currentConflict(r.Context(), ch)
	if err != nil {
		return err
	}
	var entry *gitrepo.ConflictFile
	if report != nil {
		for i := range report.Files {
			if report.Files[i].Path == path {
				entry = &report.Files[i]
				break
			}
		}
	}
	switch {
	case entry == nil:
		return server.NewError("path_not_found", http.StatusNotFound,
			fmt.Sprintf("%s has no conflict in %q", ch.Ref, path))
	case entry.Kind != gitrepo.ConflictContent:
		return server.NewError("not_text_conflict", http.StatusConflict,
			fmt.Sprintf("%s conflicts in %q as %s, which has no text to mark", ch.Ref, path, entry.Kind))
	}
	repo, err := c.deps.Repos(r.Context(), ch.WorkspaceSlug)
	if err != nil {
		return err
	}
	text, err := repo.ConflictText(r.Context(), *entry)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(text)
	return err
}
