package awareness_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
)

// accessWorld is a served demo, in the organization acme, with the open
// change 1 in the index and one active claim of its owner, and callers of
// every kind.
type accessWorld struct {
	*served
	claim string
	// gitPAT holds only git:read and readPAT only changes:read, both the
	// owner's; memberKey is an API key of another member of acme and
	// strangerKey one of a user outside it.
	gitPAT, readPAT, memberKey, strangerKey string
}

func newAccessWorld(t *testing.T) *accessWorld {
	t.Helper()
	s := newServed(t)
	s.version(s.touching(1, 1, "", "api/a.go"))
	s.pass()
	_, memberKey := s.member(t, "member")
	return &accessWorld{
		served:      s,
		claim:       s.createClaim(t, "demo", map[string]any{"paths": []string{"api/**"}, "intent": "rework"}),
		gitPAT:      s.pat(t, s.owner, "git:read"),
		readPAT:     s.pat(t, s.owner, "changes:read"),
		memberKey:   memberKey,
		strangerKey: s.hub.IssueKey(t, s.hub.CreateUser(t, "stranger")),
	}
}

func TestAwarenessReads_NeedChangesReadAndReadAccess(t *testing.T) {
	t.Parallel()
	w := newAccessWorld(t)
	for _, route := range []string{"/activity?paths=api/**", "/changes/1/overlaps", "/claims", "/claims/" + w.claim} {
		t.Run(route, func(t *testing.T) {
			for _, slug := range []string{"demo", "no-such-workspace"} {
				if status, raw := w.call(t, http.MethodGet, slug+route, w.gitPAT, nil); status != http.StatusForbidden ||
					errorType(t, raw) != "insufficient_scope" {
					t.Errorf("GET %s%s with a PAT holding git:read = %d %s, want 403 insufficient_scope", slug, route, status, raw)
				}
			}
			status, hidden := w.call(t, http.MethodGet, "demo"+route, w.strangerKey, nil)
			_, missing := w.call(t, http.MethodGet, "no-such-workspace"+route, w.strangerKey, nil)
			if status != http.StatusNotFound || errorType(t, hidden) != "not_found" || string(hidden) != string(missing) {
				t.Errorf("GET demo%s outside the organization = %d %s, want 404 not_found as for no workspace: %s",
					route, status, hidden, missing)
			}
			for name, credential := range map[string]string{
				"a member with an API key":    w.memberKey,
				"the owner with changes:read": w.readPAT,
				"the admin token":             w.hub.Token,
			} {
				if status, raw := w.call(t, http.MethodGet, "demo"+route, credential, nil); status != http.StatusOK {
					t.Errorf("GET demo%s as %s = %d %s, want 200", route, name, status, raw)
				}
			}
		})
	}
}

func TestAwarenessWrites_NeedChangesWriteAndLetAMemberClaim(t *testing.T) {
	t.Parallel()
	w := newAccessWorld(t)
	body := map[string]any{"paths": []string{"api/**"}, "intent": "rework"}
	writes := []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/claims", body},
		{http.MethodPatch, "/claims/" + w.claim, map[string]any{"ttl": "2h"}},
		{http.MethodDelete, "/claims/" + w.claim, nil},
	}
	for _, refused := range []struct {
		name, credential string
		status           int
		errType          string
	}{
		{"a PAT holding only changes:read", w.readPAT, http.StatusForbidden, "insufficient_scope"},
		{"a user outside the organization", w.strangerKey, http.StatusNotFound, "not_found"},
	} {
		for _, req := range writes {
			rows := w.claimRows(t)
			status, raw := w.call(t, req.method, "demo"+req.path, refused.credential, req.body)
			if status != refused.status || errorType(t, raw) != refused.errType {
				t.Errorf("%s demo%s with %s = %d %s, want %d %s", req.method, req.path, refused.name, status, raw,
					refused.status, refused.errType)
			}
			if got := w.claimRows(t); !slices.Equal(got, rows) {
				t.Errorf("stored claims after the refusal of %s %s = %v, want %v", req.method, req.path, got, rows)
			}
		}
	}

	status, raw := w.call(t, http.MethodPost, "demo/claims", w.memberKey, body)
	if status != http.StatusCreated {
		t.Fatalf("POST demo/claims as a member = %d %s, want 201", status, raw)
	}
	var created claimAnswer
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	own := "demo/claims/" + created.Claim.ID
	if status, raw := w.call(t, http.MethodPatch, own, w.memberKey, map[string]any{"ttl": "2h"}); status != http.StatusOK {
		t.Errorf("PATCH the member's own claim = %d %s, want 200", status, raw)
	}
	if status, raw := w.call(t, http.MethodDelete, own, w.memberKey, nil); status != http.StatusNoContent {
		t.Errorf("DELETE the member's own claim = %d %s, want 204", status, raw)
	}
}

func TestAwarenessRoutes_ServeTheParentsDataThroughAForksSlug(t *testing.T) {
	t.Parallel()
	s := newServed(t)
	s.version(s.touching(1, 1, "", "api/a.go", "api/b.go"))
	s.version(s.touching(2, 1, "", "api/a.go"))
	s.pass()
	agent, key := s.member(t, "agent")
	s.hub.AddWorkspace(t, access.Record{Slug: "demo--task1", OwnerID: agent, OrgID: s.org, Kind: access.KindFork,
		ParentSlug: "demo", Status: access.StatusReady})

	status, raw := s.call(t, http.MethodPost, "demo--task1/claims", key, map[string]any{"paths": []string{"api/**"}, "intent": "x"})
	if status != http.StatusCreated {
		t.Fatalf("POST demo--task1/claims = %d %s, want 201", status, raw)
	}
	var created claimAnswer
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	id := created.Claim.ID
	if created.Claim.Workspace != "demo" {
		t.Errorf("workspace_slug of a claim made through the fork = %q, want demo", created.Claim.Workspace)
	}
	status, raw = s.call(t, http.MethodGet, "demo/claims", key, nil)
	var page claimPage
	if err := json.Unmarshal(raw, &page); status != http.StatusOK || err != nil || !slices.Contains(page.ids(), id) {
		t.Errorf("GET demo/claims = %d %s, want 200 with the claim %s", status, raw, id)
	}
	for _, path := range []string{"/claims/" + id, "/claims", "/changes/1/overlaps", "/activity?paths=api/**"} {
		status, viaFork := s.call(t, http.MethodGet, "demo--task1"+path, key, nil)
		_, viaParent := s.call(t, http.MethodGet, "demo"+path, key, nil)
		if status != http.StatusOK || string(viaFork) != string(viaParent) {
			t.Errorf("GET demo--task1%s = %d %s, want 200 and the parent's answer %s", path, status, viaFork, viaParent)
		}
		if path == "/changes/1/overlaps" && !strings.Contains(string(viaFork), `"number":2`) {
			t.Errorf("the overlaps of 1 through the fork = %s, want change 2", viaFork)
		}
	}
}
