package secret_test

import (
	"context"
	"slices"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/secret"
)

// origins are the origins of steps, in order.
func origins(steps []secret.Step) []secret.Origin {
	out := []secret.Origin{}
	for _, s := range steps {
		out = append(out, s.Origin)
	}
	return out
}

func TestChain_OrdersTheScopesAWorkspaceStillReaches(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	// A family is an org of its own, with its owner's repository, named as
	// the org, and the fork <org>-fork of it that a second member created,
	// so that what one case changes reaches no other.
	type family struct{ org, orgID, owner, forker string }
	ws, parent, org, user := secret.OriginWorkspace, secret.OriginParent, secret.OriginOrg, secret.OriginUser
	for _, c := range []struct {
		name, org string
		fork      bool
		change    func(f family)
		want      []secret.Origin
		// undo, when set, gives the step back, and the next chain has it
		// again.
		undo func(f family)
	}{
		{"the fork's owner left the org", "leaver", true,
			func(f family) { w.admin(t, "DELETE", "/orgs/"+f.orgID+"/members/"+f.forker) },
			[]secret.Origin{ws, parent, user},
			func(f family) { w.AddMember(t, f.orgID, f.forker) }},
		{"the org is blocked", "blocked", true,
			func(f family) { w.admin(t, "POST", "/orgs/"+f.orgID+"/block") },
			[]secret.Origin{ws, parent, user},
			func(f family) { w.admin(t, "POST", "/orgs/"+f.orgID+"/unblock") }},
		{"the org is deleted", "deleted", true,
			func(f family) { w.admin(t, "DELETE", "/orgs/"+f.orgID) },
			[]secret.Origin{ws, parent, user}, nil},
		{"the repository's owner left the org", "orgless", false,
			func(f family) { w.admin(t, "DELETE", "/orgs/"+f.orgID+"/members/"+f.owner) },
			[]secret.Origin{ws, user}, nil},
		{"the fork's owner is deleted", "ownerless", true,
			func(f family) { w.admin(t, "DELETE", "/users/"+f.forker) },
			[]secret.Origin{ws, parent}, nil},
		{"the parent's record is gone", "orphan", true,
			func(f family) { w.exec(t, `DELETE FROM workspaces WHERE slug = ?`, f.org) },
			[]secret.Origin{ws, org, user}, nil},
	} {
		f := family{org: c.org, orgID: w.CreateOrg(t, c.org), owner: w.CreateUser(t, c.org+"-owner"), forker: w.CreateUser(t, c.org+"-forker")}
		w.AddMember(t, f.orgID, f.owner)
		w.AddMember(t, f.orgID, f.forker)
		w.AddWorkspace(t, access.Record{Slug: f.org, OwnerID: f.owner, OrgID: f.orgID, Kind: access.KindRepository, Status: access.StatusReady})
		w.AddWorkspace(t, access.Record{Slug: f.org + "-fork", OwnerID: f.forker, OrgID: f.orgID,
			Kind: access.KindFork, ParentSlug: f.org, Status: access.StatusReady})
		orgStep := secret.Step{Origin: org, Scope: secret.ScopeOrg, ScopeID: f.orgID}
		slug, whole := f.org, []secret.Step{{Origin: ws, Scope: secret.ScopeWorkspace, ScopeID: f.org}, orgStep,
			{Origin: user, Scope: secret.ScopeUser, ScopeID: f.owner}}
		if c.fork {
			slug, whole = f.org+"-fork", []secret.Step{{Origin: ws, Scope: secret.ScopeWorkspace, ScopeID: f.org + "-fork"},
				{Origin: parent, Scope: secret.ScopeWorkspace, ScopeID: f.org}, orgStep, {Origin: user, Scope: secret.ScopeUser, ScopeID: f.forker}}
		}
		chain := func() []secret.Step {
			steps, err := w.secrets.Chain(ctx, slug)
			if err != nil {
				t.Fatalf("%s: chain(%s): %v", c.name, slug, err)
			}
			return steps
		}
		if got := chain(); !slices.Equal(got, whole) {
			t.Fatalf("%s: before the change chain(%s) = %v, want %v", c.name, slug, got, whole)
		}
		c.change(f)
		if got := origins(chain()); !slices.Equal(got, c.want) {
			t.Errorf("%s: chain(%s) = %v, want %v", c.name, slug, got, c.want)
		}
		if c.undo != nil {
			c.undo(f)
			if got := chain(); !slices.Equal(got, whole) {
				t.Errorf("%s: once undone chain(%s) = %v, want %v", c.name, slug, got, whole)
			}
		}
	}
}
