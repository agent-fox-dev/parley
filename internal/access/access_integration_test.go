package access_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// accessHub is a real hub with the probe routes added: POST
// /workspaces/:slug/probe needs write, GET /workspaces/:slug/probe-read read
// and GET /workspaces/:slug/probe-owner owner. A probe answers 200 with the
// decision's level and counts its calls.
type accessHub struct {
	*hubtest.Hub
	calls atomic.Int64
}

func startAccessHub(t *testing.T) *accessHub {
	t.Helper()
	h := &accessHub{}
	h.Hub = hubtest.Start(t, func(c *hub.Composition) {
		c.AddRoute(h.probe(http.MethodPost, "/workspaces/:slug/probe", access.NeedWrite))
		c.AddRoute(h.probe(http.MethodGet, "/workspaces/:slug/probe-read", access.NeedRead))
		c.AddRoute(h.probe(http.MethodGet, "/workspaces/:slug/probe-owner", access.NeedOwner))
	})
	return h
}

// probe is a probe route method pattern that declares need.
func (h *accessHub) probe(method, pattern string, need access.Need) server.Route {
	return server.Route{
		Method: method, Pattern: pattern, Marks: map[string]string{access.MarkNeed: string(need)},
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			h.calls.Add(1)
			d, ok := access.DecisionOf(r)
			if !ok {
				return server.Internal("the probe ran without a decision")
			}
			server.WriteJSON(w, http.StatusOK, map[string]any{"level": d.Level})
			return nil
		},
	}
}

// createOrg creates the active org slug through the admin route and returns
// its id.
func (h *accessHub) createOrg(t *testing.T, slug string) string {
	t.Helper()
	a := h.call(t, http.MethodPost, "/orgs", h.Token, map[string]string{"name": slug, "slug": slug})
	var org struct {
		ID string `json:"id"`
	}
	if a.status != http.StatusCreated || json.Unmarshal(a.body, &org) != nil || org.ID == "" {
		t.Fatalf("create org %s = %d %s, want 201 with an id", slug, a.status, a.body)
	}
	return org.ID
}

// member adds (PUT) or removes (DELETE) the user userID as a member of the
// org orgID through the admin route.
func (h *accessHub) member(t *testing.T, method, orgID, userID string) {
	t.Helper()
	if a := h.call(t, method, "/orgs/"+orgID+"/members/"+userID, h.Token, nil); a.status != http.StatusNoContent {
		t.Fatalf("%s member %s of %s = %d %s, want 204", method, userID, orgID, a.status, a.body)
	}
}

// call sends method path, under /api/v1, with key as the bearer credential
// and body as JSON when it is not nil.
func (h *accessHub) call(t *testing.T, method, path, key string, body any) answer {
	t.Helper()
	var data io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		data = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.URL("/api/v1"+path), data)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("%s %s: read the body: %v", method, path, err)
	}
	return answer{res.StatusCode, res.Header.Get("Content-Type"), b, res.Header.Get("Allow")}
}

// readable builds the readable set of the user userID with a service bound
// to the hub's store as the hub binds its own.
func (h *accessHub) readable(t *testing.T, userID string) access.ReadableSet {
	t.Helper()
	db := h.Store.Main()
	svc := newService(access.StoreRecords(db), access.MembershipsFunc(func(ctx context.Context, id string) ([]string, error) {
		return identity.ActiveOrgIDs(ctx, db, id)
	}))
	set, err := svc.Readable(context.Background(), access.Caller{UserID: userID})
	if err != nil {
		t.Fatalf("Readable(%s): %v", userID, err)
	}
	return set
}

func TestHub_AnswersTheOwnerTheMemberAndAnOutsiderOnAWriteRoute(t *testing.T) {
	t.Parallel()
	h := startAccessHub(t)
	owner, member, outsider := h.CreateUser(t, "owner"), h.CreateUser(t, "member"), h.CreateUser(t, "outsider")
	org := h.createOrg(t, "team")
	h.member(t, http.MethodPut, org, member)
	seed(t, h.Store, repo("acme", owner, org, access.StatusReady))

	if a := h.call(t, http.MethodPost, "/workspaces/acme/probe", h.IssueKey(t, owner), map[string]any{}); a.status != http.StatusOK || decode(t, a).Level != access.LevelOwner {
		t.Errorf("the owner's write = %d %s, want 200 at level owner", a.status, a.body)
	}
	m := h.call(t, http.MethodPost, "/workspaces/acme/probe", h.IssueKey(t, member), map[string]any{})
	if m.status != http.StatusForbidden || decode(t, m).Error.ErrorType != "forbidden" {
		t.Errorf("the member's write = %d %s, want 403 forbidden", m.status, m.body)
	}
	key := h.IssueKey(t, outsider)
	a := h.call(t, http.MethodPost, "/workspaces/acme/probe", key, map[string]any{})
	b := h.call(t, http.MethodPost, "/workspaces/nope/probe", key, map[string]any{})
	if a.status != http.StatusNotFound || decode(t, a).Error.ErrorType != "not_found" || !bytes.Equal(a.body, b.body) || b.status != a.status {
		t.Errorf("the outsider's writes = %d %s on acme and %d %s on nope, want the same 404 not_found", a.status, a.body, b.status, b.body)
	}
	if n := h.calls.Load(); n != 1 {
		t.Errorf("the probe ran %d times, want once", n)
	}
}

func TestHub_ClosesTheForkOfAMemberWhoLeavesTheOrgAndKeepsItOpenToTheParentsOwner(t *testing.T) {
	t.Parallel()
	h := startAccessHub(t)
	owner, member := h.CreateUser(t, "owner"), h.CreateUser(t, "member")
	org := h.createOrg(t, "team")
	h.member(t, http.MethodPut, org, member)
	seed(t, h.Store, repo("acme", owner, org, access.StatusReady), fork("acme--m", member, org, "acme"))
	ownerKey, memberKey := h.IssueKey(t, owner), h.IssueKey(t, member)

	if a := h.call(t, http.MethodGet, "/workspaces/acme--m/probe-owner", memberKey, nil); a.status != http.StatusOK || decode(t, a).Level != access.LevelOwner {
		t.Fatalf("the member's owner route on the fork before leaving = %d %s, want 200 at level owner", a.status, a.body)
	}
	h.member(t, http.MethodDelete, org, member)
	for _, path := range []string{"/workspaces/acme--m/probe-owner", "/workspaces/acme/probe-read"} {
		if a := h.call(t, http.MethodGet, path, memberKey, nil); a.status != http.StatusNotFound || decode(t, a).Error.ErrorType != "not_found" {
			t.Errorf("the former member's GET %s = %d %s, want 404 not_found", path, a.status, a.body)
		}
	}
	if set := h.readable(t, member); set.All || len(set.Entries) != 0 {
		t.Errorf("the former member's readable set = %+v, want empty", set)
	}
	if a := h.call(t, http.MethodGet, "/workspaces/acme--m/probe-owner", ownerKey, nil); a.status != http.StatusOK || decode(t, a).Level != access.LevelOwner {
		t.Errorf("the parent owner's owner route on the fork = %d %s, want 200 at level owner", a.status, a.body)
	}
	want := map[string]access.Level{"acme": access.LevelOwner, "acme--m": access.LevelOwner}
	if set := h.readable(t, owner); set.All || !maps.Equal(set.Entries, want) {
		t.Errorf("the owner's readable set = %+v, want %v", set, want)
	}
}
