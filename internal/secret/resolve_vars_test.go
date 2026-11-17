package secret_test

import (
	"bytes"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/secret"
)

func TestResolvedVars_AnswerTheFirstEntryOfEachKeyAlongTheChain(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	const path = "/workspaces/acme/vars/resolved"
	tu := stamp(w.clock())
	w.putVars(t, w.ownerKey, "/user/vars", "A", "u", "b", "u", "D", "u")
	to := stamp(w.advance(time.Minute))
	w.putVars(t, w.ownerKey, "/orgs/acme/vars", "a", "o1", "C", "o")
	tw := stamp(w.advance(time.Minute))
	w.putVars(t, w.ownerKey, "/workspaces/acme/vars", "B", "w")
	tp := stamp(w.advance(time.Minute))
	if a := w.do(t, w.ownerKey, http.MethodPatch, "/orgs/acme/vars/A", map[string]any{"value": "o"}); a.status != http.StatusOK {
		t.Fatalf("PATCH the org's a = %d %s, want 200", a.status, a.body)
	}
	want := []resolved{
		{"a", "o", "org", to, tp},
		{"B", "w", "workspace", tw, tw},
		{"C", "o", "org", to, to},
		{"D", "u", "user", tu, tu},
	}
	got, next := resolvedPageOf(t, w.do(t, w.ownerKey, http.MethodGet, path, nil))
	if !slices.Equal(got, want) || next != nil {
		t.Errorf("resolved = %v, next_cursor %v; want %v and null", got, next, want)
	}

	first, next := resolvedPageOf(t, w.do(t, w.ownerKey, http.MethodGet, path+"?limit=2", nil))
	if len(first) != 2 || next == nil {
		t.Fatalf("page 1 = %v, next_cursor %v; want 2 items and a cursor", first, next)
	}
	second, last := resolvedPageOf(t, w.do(t, w.ownerKey, http.MethodGet, path+"?limit=2&cursor="+url.QueryEscape(*next), nil))
	if got := append(first, second...); !slices.Equal(got, want) || last != nil {
		t.Errorf("pages = %v, last next_cursor %v; want %v and null", got, last, want)
	}

	// A workspace none of whose scopes holds a variable.
	quiet := w.CreateOrg(t, "quiet")
	w.AddMember(t, quiet, w.outsiderID)
	w.AddWorkspace(t, access.Record{Slug: "quiet", OwnerID: w.outsiderID, OrgID: quiet, Kind: access.KindRepository, Status: access.StatusReady})
	empty := w.do(t, w.outsiderKey, http.MethodGet, "/workspaces/quiet/vars/resolved", nil)
	if got, next := resolvedPageOf(t, empty); len(got) != 0 || next != nil || !bytes.Contains(empty.body, []byte(`"items":[]`)) {
		t.Errorf("an empty chain resolves to %s, want items [] and next_cursor null", empty.body)
	}
}

func TestResolvedVars_AnswerEveryReaderTheChainOfTheOwner(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.putVars(t, w.ownerKey, "/user/vars", "X", "owner's", "OwnerOnly", "1")
	w.putVars(t, w.memberKey, "/user/vars", "X", "member's")
	const path = "/workspaces/acme/vars/resolved"
	want := w.resolve(t, w.ownerKey, "acme")
	if keys := keysOfResolved(want); !slices.Equal(keys, []string{"OwnerOnly", "X"}) || want[1].Value != "owner's" || want[0].Origin != "user" || want[1].Origin != "user" {
		t.Fatalf("the owner's resolved = %v, want OwnerOnly and the owner's X, both from the user scope", want)
	}
	readers := []struct{ name, cred string }{
		{"a member", w.memberKey},
		{"the admin token", w.Token},
		{"a member's PAT with vars:read", w.IssuePAT(t, identity.PATSpec{UserID: w.memberID, Permissions: []string{"vars:read"}})},
		{"a member's PAT with vars:write", w.IssuePAT(t, identity.PATSpec{UserID: w.memberID, Permissions: []string{"vars:write"}})},
	}
	for _, r := range readers {
		if got := w.resolve(t, r.cred, "acme"); !slices.Equal(got, want) {
			t.Errorf("%s is answered %v, want the owner's %v", r.name, got, want)
		}
	}
	pat := w.IssuePAT(t, identity.PATSpec{UserID: w.memberID, Permissions: []string{"secrets:write"}})
	if a := w.do(t, pat, http.MethodGet, path, nil); a.status != http.StatusForbidden {
		t.Errorf("a PAT with neither vars scope = %d %s, want 403", a.status, a.body)
	} else if typ, _ := errorOf(t, a); typ != "insufficient_scope" {
		t.Errorf("a PAT with neither vars scope = %s, want insufficient_scope", a.body)
	}
	a := w.do(t, w.outsiderKey, http.MethodGet, path, nil)
	if typ, msg := errorOf(t, a); a.status != http.StatusNotFound || typ != "not_found" || msg != "workspace not found" {
		t.Errorf("a caller of no workspace's org = %d %s, want 404 workspace not found", a.status, a.body)
	}
	if got := keysOf(w.listVars(t, w.memberKey, "/user/vars")); slices.Contains(got, "OwnerOnly") {
		t.Errorf("the member's own variables %v hold the owner's OwnerOnly", got)
	}
}

func TestResolvedVars_PutAForksOwnAboveItsParents(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	at := stamp(w.clock())
	w.putVars(t, w.ownerKey, "/workspaces/acme/vars", "REGION", "eu", "Mode", "p")
	w.putVars(t, w.ownerKey, "/orgs/acme/vars", "MODE", "o", "Org", "1")
	w.putVars(t, w.memberKey, "/workspaces/acme-fork/vars", "mode", "f")
	w.putVars(t, w.memberKey, "/user/vars", "Mine", "m", "region", "m")
	want := []resolved{{"Mine", "m", "user", at, at}, {"mode", "f", "workspace", at, at}, {"Org", "1", "org", at, at}, {"REGION", "eu", "parent", at, at}}
	// The fork's owner, a member who cannot write to the parent, reads the
	// parent's, the org's and their own variables in the fork's.
	if got := w.resolve(t, w.memberKey, "acme-fork"); !slices.Equal(got, want) {
		t.Errorf("the fork's resolved = %v, want %v", got, want)
	}
	if a := w.do(t, w.ownerKey, http.MethodDelete, "/workspaces/acme-fork/vars/mode", nil); a.status != http.StatusNoContent {
		t.Fatalf("DELETE the fork's mode as the parent's owner = %d %s, want 204", a.status, a.body)
	}
	want[1] = resolved{"Mode", "p", "parent", at, at}
	if got := w.resolve(t, w.memberKey, "acme-fork"); !slices.Equal(got, want) {
		t.Errorf("once the fork's mode is gone the fork's resolved = %v, want %v", got, want)
	}
}

func TestResolvedVars_LeaveOutTheOrgOfAForkWhoseOwnerLeftIt(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.putVars(t, w.ownerKey, "/workspaces/acme/vars", "P", "1")
	w.putVars(t, w.ownerKey, "/orgs/acme/vars", "Q", "1")
	w.putVars(t, w.memberKey, "/user/vars", "R", "1")
	w.admin(t, http.MethodDelete, "/orgs/"+w.orgID+"/members/"+w.memberID)
	for _, r := range []struct{ name, cred string }{{"the parent's owner", w.ownerKey}, {"the admin token", w.Token}} {
		got := w.resolve(t, r.cred, "acme-fork")
		if keys := keysOfResolved(got); !slices.Equal(keys, []string{"P", "R"}) || got[0].Origin != "parent" || got[1].Origin != "user" {
			t.Errorf("%s is answered %v, want P from the parent and R from the user scope", r.name, got)
		}
	}
	a := w.do(t, w.memberKey, http.MethodGet, "/workspaces/acme-fork/vars/resolved", nil)
	if typ, msg := errorOf(t, a); a.status != http.StatusNotFound || typ != "not_found" || msg != "workspace not found" {
		t.Errorf("the former member = %d %s, want 404 workspace not found", a.status, a.body)
	}
}

func TestResolvedVars_FailWholeWhenAScopeCannotBeRead(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.putVars(t, w.ownerKey, "/workspaces/acme/vars", "W", "1")
	w.putVars(t, w.ownerKey, "/orgs/acme/vars", "O", "1")
	w.putVars(t, w.ownerKey, "/user/vars", "U", "1")
	const cause = "the org scope is unreadable 3c9e"
	w.secrets.FailVarReadsOf(secret.ScopeOrg, errors.New(cause))
	a := w.do(t, w.ownerKey, http.MethodGet, "/workspaces/acme/vars/resolved", nil)
	if a.status != http.StatusInternalServerError {
		t.Fatalf("resolved with an unreadable org scope = %d %s, want 500", a.status, a.body)
	}
	if typ, msg := errorOf(t, a); typ != "internal" || strings.Contains(msg, "3c9e") || bytes.Contains(a.body, []byte(`"items"`)) {
		t.Errorf("resolved with an unreadable org scope = %s, want internal, without the cause and without items", a.body)
	}
}
