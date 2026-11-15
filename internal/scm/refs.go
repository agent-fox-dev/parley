package scm

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/server"
)

// The namespaces the refs routes list, with the kind of a ref in each.
// Refs elsewhere, such as refs/notes/, have no kind and are not listed,
// though a revision may still name them in full.
var refKinds = []struct{ prefix, kind string }{
	{"refs/heads/", "branch"},
	{"refs/tags/", "tag"},
	{"refs/parley/", "hub"},
}

// defaultRefPrefix is the prefix of a refs list that names none.
const defaultRefPrefix = "refs/heads/"

// refObject is a ref as the refs routes answer it.
type refObject struct {
	Name      string  `json:"name"`
	Kind      string  `json:"kind"`
	SHA       string  `json:"sha"`
	Protected bool    `json:"protected"`
	Tracking  bool    `json:"tracking"`
	Change    *Change `json:"change"`
}

// listRefs answers GET …/scm/refs: a page of the listed refs whose names
// begin with prefix, in byte order of name.
func (c *Component) listRefs(w http.ResponseWriter, r *http.Request) error {
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	var after string
	if _, err := page.After(&after); err != nil {
		return err
	}
	prefix := defaultRefPrefix
	if q := r.URL.Query(); q.Has("prefix") {
		prefix = q.Get("prefix")
	}
	rd, err := c.open(r)
	if err != nil {
		return err
	}
	refs, err := rd.view.Refs(r.Context())
	if err != nil {
		return err
	}
	refs = slices.DeleteFunc(slices.Clone(refs), func(ref Ref) bool {
		return refKind(ref.Name) == "" || !strings.HasPrefix(ref.Name, prefix) || ref.Name <= after
	})
	slices.SortFunc(refs, func(a, b Ref) int { return strings.Compare(a.Name, b.Name) })
	objs := make([]refObject, 0, page.Limit+1)
	for _, ref := range refs {
		if len(objs) > page.Limit {
			break
		}
		obj, ok, err := c.refObject(r, rd, ref)
		if err != nil {
			return err
		}
		if ok {
			objs = append(objs, obj)
		}
	}
	return server.WritePage(w, page, objs, func(o refObject) any { return o.Name })
}

// getRef answers GET …/scm/refs/*name: the listed ref of the full name, or
// of the branch, when the name does not begin with refs/.
func (c *Component) getRef(w http.ResponseWriter, r *http.Request) error {
	name := server.Param(r, "name")
	if !strings.HasPrefix(name, "refs/") {
		name = "refs/heads/" + name
	}
	missing := refNotFound(fmt.Sprintf("ref %q does not exist", name))
	if refKind(name) == "" {
		return missing
	}
	rd, err := c.open(r)
	if err != nil {
		return err
	}
	refs, err := rd.view.Refs(r.Context())
	if err != nil {
		return err
	}
	i := slices.IndexFunc(refs, func(ref Ref) bool { return ref.Name == name })
	if i < 0 {
		return missing
	}
	obj, ok, err := c.refObject(r, rd, refs[i])
	if err != nil {
		return err
	}
	if !ok {
		return missing
	}
	server.WriteJSON(w, http.StatusOK, obj)
	return nil
}

// refObject is the object of the listed ref ref of the read rd; ok is
// false when the ref reaches no commit, such as a tag of a tree.
func (c *Component) refObject(r *http.Request, rd *read, ref Ref) (obj refObject, ok bool, err error) {
	ctx := r.Context()
	sha := ref.Commit
	if sha == "" {
		sha, err = rd.repo.PeelCommit(ctx, ref.SHA)
		if errors.Is(err, gitrepo.ErrNotCommit) || errors.Is(err, gitrepo.ErrNotFound) {
			return refObject{}, false, nil
		}
		if err != nil {
			return refObject{}, false, err
		}
	}
	obj = refObject{Name: ref.Name, Kind: refKind(ref.Name), SHA: sha, Tracking: ref.Tracking}
	switch obj.Kind {
	case "branch":
		branch := strings.TrimPrefix(ref.Name, "refs/heads/")
		protected, err := rd.protected(r, c.deps.Protected)
		if err != nil {
			return refObject{}, false, err
		}
		obj.Protected = protected(branch)
		if obj.Change, err = c.deps.Changes.OpenChangeOfBranch(ctx, rd.ws.Slug, branch); err != nil {
			return refObject{}, false, err
		}
	case "hub":
		// Hub-owned refs are written by the hub alone.
		obj.Protected = true
	}
	return obj, true, nil
}

// protected is the protected-branch test of the read's workspace, asked of
// lookup once per read.
func (rd *read) protected(r *http.Request, lookup ProtectedLookup) (func(string) bool, error) {
	if rd.isProtected == nil {
		test, err := lookup.ProtectedBranches(r.Context(), rd.ws)
		if err != nil {
			return nil, err
		}
		rd.isProtected = test
	}
	return rd.isProtected, nil
}

// refKind is the kind of the ref of the full name name, "" outside the
// listed namespaces.
func refKind(name string) string {
	for _, k := range refKinds {
		if strings.HasPrefix(name, k.prefix) {
			return k.kind
		}
	}
	return ""
}
