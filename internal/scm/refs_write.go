package scm

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/server"
)

// The ops the writes of refs report.
const (
	opRefCreate = "ref_create"
	opRefMove   = "ref_move"
	opRefDelete = "ref_delete"
)

// refCreation is the body of POST …/scm/refs.
type refCreation struct {
	Name *string `json:"name"`
	SHA  *string `json:"sha"`
	Ref  *string `json:"ref"`
}

// refMove is the body of PATCH …/scm/refs/*name.
type refMove struct {
	SHA         *string `json:"sha"`
	ExpectedSHA *string `json:"expected_sha"`
}

// createRef answers POST …/scm/refs: the ref name, a branch when the name
// does not begin with refs/, made at the commit sha or the revision ref
// names, of which the body gives exactly one. A tag made so is
// lightweight. The ref is created through the shared ref update step, so
// the push rules decide it: a name below refs/parley/ is the hub's.
func (c *Component) createRef(w http.ResponseWriter, r *http.Request, wr *write) error {
	var body refCreation
	if err := server.DecodeJSON(r, &body); err != nil {
		return err
	}
	if body.Name == nil || *body.Name == "" {
		return server.InvalidRequest("name is required")
	}
	name, err := writeRefName(*body.Name)
	if err != nil {
		return err
	}
	if (body.SHA == nil) == (body.Ref == nil) {
		return server.InvalidRequest("give exactly one of sha and ref")
	}
	if body.SHA != nil && !isFullID(*body.SHA) {
		return server.InvalidRequest("sha must be a full 40-character commit id")
	}
	ctx := r.Context()
	t, err := c.target(ctx, wr)
	if err != nil {
		return err
	}
	var sha string
	if body.SHA != nil {
		if sha, err = t.git.ResolveCommit(ctx, *body.SHA); err != nil {
			return notFound(*body.SHA, err)
		}
	} else {
		res, err := t.resolver.Resolve(ctx, *body.Ref)
		if err != nil {
			return err
		}
		sha = res.SHA
	}
	if _, exists, err := t.lookup(ctx, name); err != nil {
		return err
	} else if exists {
		return server.NewError("ref_exists", http.StatusConflict, fmt.Sprintf("ref %s already exists", name))
	}
	if _, err := c.updateRef(ctx, wr, t.git, refUpdate{ref: name, before: pushrule.ZeroID, after: sha,
		versionKind: pushrule.VersionKindRef}); err != nil {
		return err
	}
	return t.writeRef(w, r, c, http.StatusCreated, Ref{Name: name, SHA: sha, Commit: sha})
}

// moveRef answers PATCH …/scm/refs/*name: the ref of the full name, or of
// the branch when the name does not begin with refs/, moved to the commit
// sha when it is at expected_sha, through the shared ref update step with
// the version kind ref, so that the source branch of a change gets a
// version. A move to the commit the ref is at changes nothing and emits
// nothing, as a push with no change does.
func (c *Component) moveRef(w http.ResponseWriter, r *http.Request, wr *write) error {
	name, err := writeRefName(server.Param(r, "name"))
	if err != nil {
		return err
	}
	var body refMove
	if err := server.DecodeJSON(r, &body); err != nil {
		return err
	}
	if body.SHA == nil || !isFullID(*body.SHA) || body.ExpectedSHA == nil || !isFullID(*body.ExpectedSHA) {
		return server.InvalidRequest("sha and expected_sha must both be full 40-character commit ids")
	}
	ctx := r.Context()
	t, err := c.target(ctx, wr)
	if err != nil {
		return err
	}
	cur, err := t.current(r, name)
	if err != nil {
		return err
	}
	sha, err := t.git.ResolveCommit(ctx, *body.SHA)
	if err != nil {
		return notFound(*body.SHA, err)
	}
	at := commitOf(cur)
	if strings.ToLower(*body.ExpectedSHA) != at {
		return refMoved(name, &at)
	}
	if sha == at {
		return t.writeRef(w, r, c, http.StatusOK, Ref{Name: name, SHA: cur.ID, Commit: cur.Commit})
	}
	if _, err := c.updateRef(ctx, wr, t.git, refUpdate{ref: name, before: cur.ID, after: sha,
		versionKind: pushrule.VersionKindRef}); err != nil {
		return err
	}
	return t.writeRef(w, r, c, http.StatusOK, Ref{Name: name, SHA: sha, Commit: sha})
}

// deleteRef answers DELETE …/scm/refs/*name: the ref of the full name, or
// of the branch when the name does not begin with refs/, deleted when it is
// at expected_sha, if the query gives one, through the shared ref update
// step against the value it has, so that the push rules keep the default
// and the protected branches. A branch that is the source of a draft or
// open change is in use. The ref's commits stay in the repository.
func (c *Component) deleteRef(w http.ResponseWriter, r *http.Request, wr *write) error {
	name, err := writeRefName(server.Param(r, "name"))
	if err != nil {
		return err
	}
	q := r.URL.Query()
	expected := strings.ToLower(q.Get("expected_sha"))
	if q.Has("expected_sha") && !isFullID(expected) {
		return server.InvalidRequest("expected_sha must be a full 40-character commit id")
	}
	ctx := r.Context()
	t, err := c.target(ctx, wr)
	if err != nil {
		return err
	}
	cur, err := t.current(r, name)
	if err != nil {
		return err
	}
	if at := commitOf(cur); q.Has("expected_sha") && expected != at {
		return refMoved(name, &at)
	}
	if branch, ok := strings.CutPrefix(name, "refs/heads/"); ok {
		change, err := c.changeLinker().FindOpen(ctx, wr.ws.Slug, branch)
		if err != nil {
			return err
		}
		if change.inUse() {
			return server.NewError("ref_in_use", http.StatusConflict,
				fmt.Sprintf("branch %s is the source of change %d and cannot be deleted", branch, change.Number))
		}
	}
	if _, err := c.updateRef(ctx, wr, t.git, refUpdate{ref: name, before: cur.ID, after: pushrule.ZeroID,
		versionKind: pushrule.VersionKindNone}); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// writeRefName is the full ref name a write names by name: name itself
// when it begins with refs/, else the branch of that name. It must be a
// name git takes for a ref, below refs/heads/ or refs/tags/, or below
// refs/parley/, which the push rules refuse as the hub's; HEAD and every
// other namespace are 400 invalid_request.
func writeRefName(name string) (string, error) {
	full := name
	if !strings.HasPrefix(name, "refs/") {
		full = "refs/heads/" + name
	}
	switch {
	case name == "HEAD" || !gitrepo.ValidRefName(full):
		return "", server.InvalidRequest(fmt.Sprintf("%q is not a valid ref name", name))
	case refKind(full) == "":
		return "", server.InvalidRequest(fmt.Sprintf("%q is outside refs/heads/, refs/tags/ and refs/parley/", name))
	}
	return full, nil
}

// current is the ref of the full name name as the repository holds it, or
// 404 ref_not_found when there is none.
func (t *target) current(r *http.Request, name string) (gitrepo.Ref, error) {
	ref, found, err := t.lookup(r.Context(), name)
	if err != nil {
		return gitrepo.Ref{}, err
	}
	if !found {
		return gitrepo.Ref{}, refNotFound(fmt.Sprintf("ref %q does not exist", name))
	}
	return ref, nil
}

// writeRef answers status with the ref object of ref.
func (t *target) writeRef(w http.ResponseWriter, r *http.Request, c *Component, status int, ref Ref) error {
	obj, _, err := c.refObject(r, t.read, ref)
	if err != nil {
		return err
	}
	server.WriteJSON(w, status, obj)
	return nil
}

// commitOf is the commit ref reaches, or the object it points at when it
// reaches none.
func commitOf(ref gitrepo.Ref) string {
	if ref.Commit != "" {
		return ref.Commit
	}
	return ref.ID
}

// isFullID reports whether s is a full 40-character commit id.
func isFullID(s string) bool { return len(s) == commitIDLen && isCommitID(s) }
