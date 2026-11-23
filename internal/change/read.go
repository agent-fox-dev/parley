package change

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
)

// confinedTo is the fork a request's token is bound to when it calls on the
// fork's parent, where it reaches only the changes whose source is the
// fork; false for every other caller.
func confinedTo(r *http.Request) (string, bool) {
	b, ok := access.BindingOf(r)
	if !ok || b.Relation != access.RelationParentOfBoundFork {
		return "", false
	}
	return b.Workspace, true
}

// changeOf is the change the request's :number names in the workspace
// :slug, read through q. Every route under /changes/:number resolves its
// change here, after the shared checks, so each refuses alike with 404
// change_not_found: a :number that is not a positive integer, a number the
// workspace does not have, any number on a fork's slug, since a fork holds
// no changes, and, for a token bound to a fork calling on the fork's
// parent, a change whose source is not the fork.
func (c *Component) changeOf(r *http.Request, q Querier) (Change, error) {
	slug := server.Param(r, "slug")
	number, ok := server.Positive(server.Param(r, "number"))
	if !ok {
		return Change{}, ErrChangeNotFound
	}
	ch, err := readChange(r.Context(), q, slug, number)
	if err != nil {
		return Change{}, err
	}
	if fork, ok := confinedTo(r); ok && ch.Source.WorkspaceSlug != fork {
		return Change{}, errChangeNotFound(slug, number)
	}
	return ch, nil
}

// RequestedChange is the change the request's :number names in the
// workspace :slug, refused as every route under /changes/:number refuses
// it (changeOf), for such a route that another package serves.
func (c *Component) RequestedChange(r *http.Request) (Change, error) {
	return c.changeOf(r, c.deps.Store.Main())
}

// getChange answers the change object, with the members include asks for.
func (c *Component) getChange(w http.ResponseWriter, r *http.Request) error {
	names, err := parseInclude(r.URL.Query()["include"])
	if err != nil {
		return err
	}
	q := c.deps.Store.Main()
	ch, err := c.changeOf(r, q)
	if err != nil {
		return err
	}
	members, err := c.expand(r.Context(), q, ch, names)
	if err != nil {
		return err
	}
	return writeWith(w, ch, members...)
}

// listVersions answers the change's versions, oldest first, in pages of the
// list envelope.
func (c *Component) listVersions(w http.ResponseWriter, r *http.Request) error {
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	var after int64
	if _, err := page.After(&after); err != nil {
		return err
	}
	q := c.deps.Store.Main()
	ch, err := c.changeOf(r, q)
	if err != nil {
		return err
	}
	vs, err := readVersionsAfter(r.Context(), q, ch.WorkspaceSlug, ch.Number, after, page.Limit+1)
	if err != nil {
		return err
	}
	return server.WritePage(w, page, vs, func(v Version) any { return v.Number })
}

// getVersion answers one version of the change, by number or latest, the
// current one, with the name of its version ref: a :version that is
// neither is 400 invalid_request, a number the change does not have 404
// not_found.
func (c *Component) getVersion(w http.ResponseWriter, r *http.Request) error {
	q := c.deps.Store.Main()
	ch, err := c.changeOf(r, q)
	if err != nil {
		return err
	}
	raw := server.Param(r, "version")
	number, ok := server.Positive(raw)
	switch {
	case raw == "latest":
		number = ch.Version
	case !ok:
		return server.InvalidRequest("version must be a positive integer or latest")
	}
	vs, err := readVersionsAfter(r.Context(), q, ch.WorkspaceSlug, ch.Number, number-1, 1)
	if err != nil {
		return err
	}
	if len(vs) == 0 || vs[0].Number != number {
		return server.NotFound(fmt.Sprintf("%s has no version %d", ch.Ref, number))
	}
	return writeWith(w, vs[0], server.Member{Name: "ref", Value: VersionRef(ch.Number, number)})
}

// commitAnswer is the answer of the by-commit lookup.
type commitAnswer struct {
	Change  Change `json:"change"`
	Version *int64 `json:"version"`
	Landed  bool   `json:"landed"`
}

// byCommit answers the change of the workspace :slug that landed the
// commit :sha or has it as a version head, as ChangeOfCommit finds it, with
// the version and whether the change landed it. A :sha that is not 40
// hexadecimal characters is 400 invalid_request. The lookup acts on :slug
// as the change's workspace: on a fork's slug, which holds no changes, it
// finds nothing, though ChangeOfCommit answers a fork's slug from its
// parent. A token bound to a fork, calling on the fork's parent, finds only
// the changes whose source is the fork.
func (c *Component) byCommit(w http.ResponseWriter, r *http.Request) error {
	slug, sha := server.Param(r, "slug"), strings.ToLower(server.Param(r, "sha"))
	if len(sha) != 40 || strings.Trim(sha, "0123456789abcdef") != "" {
		return server.InvalidRequest("sha must be a full commit id of 40 hexadecimal characters")
	}
	lookup := slug
	if fork, ok := confinedTo(r); ok {
		lookup = fork
	}
	q := c.deps.Store.Main()
	cc, found, err := c.ChangeOfCommit(r.Context(), q, lookup, sha)
	if err != nil {
		return err
	}
	if !found || cc.WorkspaceSlug != slug {
		return &notFound{server.NewError("change_not_found", http.StatusNotFound, "no change of "+slug+" has the commit "+sha)}
	}
	ch, err := readChange(r.Context(), q, cc.WorkspaceSlug, cc.Number)
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, commitAnswer{ch, cc.Version, cc.Landed})
	return nil
}

// writeWith answers 200 with the JSON object v followed by the members, in
// their order.
func writeWith(w http.ResponseWriter, v any, members ...server.Member) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b := bytes.NewBuffer(raw[:len(raw)-1])
	for _, m := range members {
		value, err := json.Marshal(m.Value)
		if err != nil {
			return fmt.Errorf("encode member %s: %w", m.Name, err)
		}
		name, _ := json.Marshal(m.Name)
		b.WriteByte(',')
		b.Write(name)
		b.WriteByte(':')
		b.Write(value)
	}
	b.WriteByte('}')
	server.WriteJSON(w, http.StatusOK, json.RawMessage(b.Bytes()))
	return nil
}
