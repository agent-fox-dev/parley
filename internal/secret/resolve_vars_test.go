package secret_test

import (
	"bytes"
	"errors"
	"net/http"
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
	w.create(t, w.ownerKey, "/user/vars", "A=u", "b=u", "D=u")
	to := stamp(w.advance(time.Minute))
	w.create(t, w.ownerKey, "/orgs/acme/vars", "a=o1", "C=o")
	tw := stamp(w.advance(time.Minute))
	w.create(t, w.ownerKey, "/workspaces/acme/vars", "B=w")
	tp := stamp(w.advance(time.Minute))
	w.must(t, w.ownerKey, http.MethodPatch, "/orgs/acme/vars/A", map[string]any{"value": "o"}, http.StatusOK)
	want := []variable{{"a", "o", "org", to, tp}, {"B", "w", "workspace", tw, tw}, {"C", "o", "org", to, to}, {"D", "u", "user", tu, tu}}
	for limit, pages := range map[int]int{0: 1, 2: 2} {
		if p := w.pages(t, w.ownerKey, path, limit); len(p) != pages || !slices.Equal(slices.Concat(p...), want) {
			t.Errorf("resolved with limit %d = %v, want %v in %d pages", limit, p, want, pages)
		}
	}
	// A workspace none of whose scopes holds a variable.
	quiet := w.CreateOrg(t, "quiet")
	w.AddMember(t, quiet, w.outsiderID)
	w.AddWorkspace(t, access.Record{Slug: "quiet", OwnerID: w.outsiderID, OrgID: quiet, Kind: access.KindRepository, Status: access.StatusReady})
	if got := w.resolve(t, w.outsiderKey, "quiet"); len(got) != 0 {
		t.Errorf("an empty chain resolves to %v, want none", got)
	}
}

func TestResolvedVars_AnswerEveryReaderTheChainOfTheOwner(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.create(t, w.ownerKey, "/user/vars", "X=owner's", "OwnerOnly=1")
	w.create(t, w.memberKey, "/user/vars", "X=member's")
	want := w.resolve(t, w.ownerKey, "acme")
	if keys := keysOf(want); !slices.Equal(keys, []string{"OwnerOnly", "X"}) || want[1].Value != "owner's" || want[0].Origin != "user" || want[1].Origin != "user" {
		t.Fatalf("the owner's resolved = %v, want OwnerOnly and the owner's X, both from the user scope", want)
	}
	for _, r := range []struct{ name, cred string }{
		{"a member", w.memberKey},
		{"the admin token", w.Token},
		{"a member's PAT with vars:read", w.IssuePAT(t, identity.PATSpec{UserID: w.memberID, Permissions: []string{"vars:read"}})},
		{"a member's PAT with vars:write", w.IssuePAT(t, identity.PATSpec{UserID: w.memberID, Permissions: []string{"vars:write"}})},
	} {
		if got := w.resolve(t, r.cred, "acme"); !slices.Equal(got, want) {
			t.Errorf("%s is answered %v, want the owner's %v", r.name, got, want)
		}
	}
	if got := keysOf(w.list(t, w.memberKey, "/user/vars")); slices.Contains(got, "OwnerOnly") {
		t.Errorf("the member's own variables %v hold the owner's OwnerOnly", got)
	}
}

func TestResolvedVars_PutAForksOwnAboveItsParentsAndLeaveOutAnOrgItsOwnerLeft(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	at := stamp(w.clock())
	w.create(t, w.ownerKey, "/workspaces/acme/vars", "REGION=eu", "Mode=p")
	w.create(t, w.ownerKey, "/orgs/acme/vars", "MODE=o", "Org=1")
	w.create(t, w.memberKey, "/workspaces/acme-fork/vars", "mode=f")
	w.create(t, w.memberKey, "/user/vars", "Mine=m", "region=m")
	want := []variable{{"Mine", "m", "user", at, at}, {"mode", "f", "workspace", at, at}, {"Org", "1", "org", at, at}, {"REGION", "eu", "parent", at, at}}
	// The fork's owner, a member who cannot write to the parent, reads the
	// parent's, the org's and their own variables in the fork's.
	if got := w.resolve(t, w.memberKey, "acme-fork"); !slices.Equal(got, want) {
		t.Errorf("the fork's resolved = %v, want %v", got, want)
	}
	w.must(t, w.ownerKey, http.MethodDelete, "/workspaces/acme-fork/vars/mode", nil, http.StatusNoContent)
	want[1] = variable{"Mode", "p", "parent", at, at}
	if got := w.resolve(t, w.memberKey, "acme-fork"); !slices.Equal(got, want) {
		t.Errorf("once the fork's mode is gone the fork's resolved = %v, want %v", got, want)
	}
	// Once its owner leaves the org, the fork resolves without the org's.
	w.admin(t, http.MethodDelete, "/orgs/"+w.orgID+"/members/"+w.memberID)
	want = slices.Delete(want, 2, 3)
	for _, r := range []struct{ name, cred string }{{"the parent's owner", w.ownerKey}, {"the admin token", w.Token}} {
		if got := w.resolve(t, r.cred, "acme-fork"); !slices.Equal(got, want) {
			t.Errorf("%s is answered %v, want %v", r.name, got, want)
		}
	}
	wantError(t, "the former member's read", w.do(t, w.memberKey, http.MethodGet, "/workspaces/acme-fork/vars/resolved", nil),
		http.StatusNotFound, "not_found", "workspace not found")
}

func TestResolvedVars_FailWholeWhenAScopeCannotBeRead(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.create(t, w.ownerKey, "/workspaces/acme/vars", "W=1")
	w.create(t, w.ownerKey, "/orgs/acme/vars", "O=1")
	w.create(t, w.ownerKey, "/user/vars", "U=1")
	w.secrets.FailVarReadsOf(secret.ScopeOrg, errors.New("the org scope is unreadable 3c9e"))
	a := w.do(t, w.ownerKey, http.MethodGet, "/workspaces/acme/vars/resolved", nil)
	wantError(t, "resolved with an unreadable org scope", a, http.StatusInternalServerError, "internal", "")
	if strings.Contains(string(a.body), "3c9e") || bytes.Contains(a.body, []byte(`"items"`)) {
		t.Errorf("resolved with an unreadable org scope = %s, want no cause and no items", a.body)
	}
}
