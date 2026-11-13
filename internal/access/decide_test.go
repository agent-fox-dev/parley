package access_test

import (
	"context"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
)

func TestDecide_AnswersByLevelAndNeedAndAnInvisibleWorkspaceAsAMissingOne(t *testing.T) {
	t.Parallel()
	st := openStore(t, t.TempDir())
	acme := repo("acme", "u-owner", "o1", access.StatusReady)
	seed(t, st, acme)
	svc := newService(access.StoreRecords(st.Main()), memberships{of: map[string][]string{"u-member": {"o1"}}, active: []string{"o1"}})
	ctx := context.Background()
	stranger := access.Caller{UserID: "u-stranger"}

	// What another surface, such as the git server, is given for a
	// workspace it cannot read carries nothing of it.
	for _, need := range []access.Need{access.NeedRead, access.NeedWrite, access.NeedOwner} {
		a, errA := svc.Decide(ctx, stranger, "acme", need)
		b, errB := svc.Decide(ctx, stranger, "nope", need)
		if errA != nil || errB != nil || a != b || a != (access.Decision{Outcome: access.NotFound}) {
			t.Errorf("need %s: Decide on acme = %+v, %v; on nope = %+v, %v; want both not_found with no level and no record", need, a, errA, b, errB)
		}
	}

	for _, c := range []struct {
		name   string
		caller access.Caller
		need   access.Need
		want   access.Decision
	}{
		{"a member reads", access.Caller{UserID: "u-member"}, access.NeedRead, access.Decision{Outcome: access.Granted, Level: access.LevelMember, Record: acme}},
		{"a member writes", access.Caller{UserID: "u-member"}, access.NeedWrite, access.Decision{Outcome: access.ReadOnly, Level: access.LevelMember, Record: acme}},
		{"a member needs the owner", access.Caller{UserID: "u-member"}, access.NeedOwner, access.Decision{Outcome: access.ReadOnly, Level: access.LevelMember, Record: acme}},
		{"the owner", access.Caller{UserID: "u-owner"}, access.NeedOwner, access.Decision{Outcome: access.Granted, Level: access.LevelOwner, Record: acme}},
		{"the admin token", access.Caller{Admin: true}, access.NeedWrite, access.Decision{Outcome: access.Granted, Level: access.LevelAdmin, Record: acme}},
	} {
		if got, err := svc.Decide(ctx, c.caller, "acme", c.need); err != nil || got != c.want {
			t.Errorf("%s: Decide = %+v, %v, want %+v", c.name, got, err, c.want)
		}
	}

	if got, err := svc.Decide(ctx, access.Caller{UserID: "u-owner"}, "acme", "admin"); err == nil || got != (access.Decision{}) {
		t.Errorf("Decide with need admin = %+v, %v, want no decision and an error", got, err)
	}
	failing := newService(failingRecords{RecordReader: access.StoreRecords(st.Main()), fail: []string{"acme"}}, failingMemberships)
	if got, err := failing.Decide(ctx, access.Caller{Admin: true}, "acme", access.NeedRead); err == nil || got != (access.Decision{}) {
		t.Errorf("Decide on a failed read = %+v, %v, want no decision and an error", got, err)
	}
}
