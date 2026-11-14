package access_test

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// coverageWorld is a service over a store in which alice owns w1 and w5 in
// her personal org and is a member of o-team, whose w7 bob owns. Of the
// forks of w1, alice created f1 with her token tokA and f2 with tokA2;
// carol has left o-team.
func coverageWorld(t *testing.T) *access.Service {
	t.Helper()
	st := openStore(t, t.TempDir())
	seed(t, st,
		repo("w1", "alice", "o-alice", access.StatusReady),
		repo("w5", "alice", "o-alice", access.StatusArchived),
		repo("w7", "bob", "o-team", access.StatusReady),
		createdBy(fork("f1", "alice", "o-alice", "w1"), "tokA"),
		createdBy(fork("f2", "alice", "o-alice", "w1"), "tokA2"),
	)
	return newService(access.StoreRecords(st.Main()), memberships{
		of:     map[string][]string{"alice": {"o-alice", "o-team"}, "bob": {"o-team"}},
		active: []string{"o-alice", "o-team"},
	})
}

func TestCoveredSet_CutsABoundTokensReadableSetDownToItsWorkspaceAndTheForksItCreated(t *testing.T) {
	t.Parallel()
	svc := coverageWorld(t)
	ctx := context.Background()
	for _, c := range []struct {
		name    string
		caller  server.Caller
		covered []string
	}{
		{"bound to a repository and the creator of a fork of it", boundPAT("alice", "tokA", "w1"), []string{"f1", "w1"}},
		{"bound to a repository of whose forks it created none", boundPAT("alice", "tokB", "w1"), []string{"w1"}},
		{"bound to a fork", boundPAT("alice", "tokF", "f1"), []string{"f1"}},
		{"bound to a workspace of an org its user has left", boundPAT("carol", "tokC", "w7"), []string{"w7"}},
		{"bound to a slug with no record", boundPAT("alice", "tokG", "gone"), []string{"gone"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			caller := access.CallerFrom(c.caller)
			if got, err := svc.CoveredSet(ctx, caller); err != nil || !slices.Equal(got, c.covered) {
				t.Errorf("CoveredSet = %q, %v, want %q", got, err, c.covered)
			}
			unbound, err := svc.Readable(ctx, access.Caller{UserID: c.caller.UserID})
			if err != nil {
				t.Fatalf("Readable(%s unbound): %v", c.caller.UserID, err)
			}
			want := maps.Clone(unbound.Entries)
			maps.DeleteFunc(want, func(slug string, _ access.Level) bool { return !slices.Contains(c.covered, slug) })
			if got, err := svc.Readable(ctx, caller); err != nil || got.All || !maps.Equal(got.Entries, want) {
				t.Errorf("Readable = %+v, %v, want the entries %v", got, err, want)
			}
		})
	}
}

func TestReadable_IsUnchangedForACallerWithoutABinding(t *testing.T) {
	t.Parallel()
	svc := coverageWorld(t)
	ctx := context.Background()
	unboundPAT := byCredential(identity.TypePAT, "alice", identity.RoleUser)
	for _, c := range []struct {
		name   string
		caller server.Caller
		as     access.Caller
	}{
		{"an API key", byKey("alice"), access.Caller{UserID: "alice"}},
		{"an unbound PAT", unboundPAT, access.Caller{UserID: "alice"}},
		{"the admin token", adminToken, access.Caller{Admin: true}},
	} {
		caller := access.CallerFrom(c.caller)
		if caller.Workspace != "" {
			t.Errorf("%s: CallerFrom gives the binding %q, want none", c.name, caller.Workspace)
		}
		if covered, err := svc.CoveredSet(ctx, caller); err != nil || covered != nil {
			t.Errorf("%s: CoveredSet = %q, %v, want none", c.name, covered, err)
		}
		got, err := svc.Readable(ctx, caller)
		want, wantErr := svc.Readable(ctx, c.as)
		if err != nil || wantErr != nil || got.All != want.All || !maps.Equal(got.Entries, want.Entries) {
			t.Errorf("%s: Readable = %+v, %v, want %+v, %v", c.name, got, err, want, wantErr)
		}
	}
	keySet, _ := svc.Readable(ctx, access.CallerFrom(byKey("alice")))
	for _, slug := range []string{"w1", "w5", "w7", "f1", "f2"} {
		if !keySet.Contains(slug) {
			t.Errorf("alice's API key cannot read %s", slug)
		}
	}
}

func TestRelationOf_TellsHowANamedWorkspaceStandsToTheBinding(t *testing.T) {
	t.Parallel()
	st := openStore(t, t.TempDir())
	seed(t, st,
		repo("w1", "alice", "o-alice", access.StatusReady),
		repo("w5", "alice", "o-alice", access.StatusReady),
		createdBy(fork("f1", "alice", "o-alice", "w1"), "tokA"),
		createdBy(fork("f2", "alice", "o-alice", "w1"), "tokA2"),
		createdBy(fork("g1", "alice", "o-alice", "w5"), "tokA"),
	)
	svc := newService(access.StoreRecords(st.Main()), memberships{})
	ctx := context.Background()
	for _, c := range []struct {
		caller server.Caller
		slug   string
		want   access.Relation
	}{
		{boundPAT("alice", "tokA", "w1"), "w1", access.RelationBound},
		{boundPAT("alice", "tokA", "w1"), "f1", access.RelationCreatedFork},
		{boundPAT("alice", "tokA", "w1"), "f2", ""},
		{boundPAT("alice", "tokA", "w1"), "g1", ""},
		{boundPAT("alice", "tokA", "w1"), "w5", ""},
		{boundPAT("alice", "tokA", "w1"), "nope", ""},
		{boundPAT("alice", "tokF", "f1"), "f1", access.RelationBound},
		{boundPAT("alice", "tokF", "f1"), "w1", access.RelationParentOfBoundFork},
		{boundPAT("alice", "tokF", "f1"), "f2", ""},
		{boundPAT("alice", "tokF", "f1"), "w5", ""},
		{boundPAT("alice", "tokG", "gone"), "gone", access.RelationBound},
		{byKey("alice"), "w1", ""},
	} {
		if got, err := svc.RelationOf(ctx, access.CallerFrom(c.caller), c.slug); err != nil || got != c.want {
			t.Errorf("RelationOf(%s, %s) = %q, %v, want %q", c.caller.CredentialID, c.slug, got, err, c.want)
		}
	}

	// A fork the token creates after an earlier answer is covered at once.
	tokB := access.CallerFrom(boundPAT("alice", "tokB", "w1"))
	if got, err := svc.RelationOf(ctx, tokB, "f3"); err != nil || got != "" {
		t.Fatalf("RelationOf(tokB, f3) before f3 exists = %q, %v, want none", got, err)
	}
	seed(t, st, createdBy(fork("f3", "alice", "o-alice", "w1"), "tokB"))
	if got, err := svc.RelationOf(ctx, tokB, "f3"); err != nil || got != access.RelationCreatedFork {
		t.Errorf("RelationOf(tokB, f3) after it was created = %q, %v, want created_fork", got, err)
	}
}
