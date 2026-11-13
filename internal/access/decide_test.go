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

	// The outcomes another surface tells apart; the HTTP check maps every
	// level and need (middleware_test.go).
	if got, err := svc.Decide(ctx, access.Caller{UserID: "u-member"}, "acme", access.NeedWrite); err != nil || got.Outcome != access.ReadOnly {
		t.Errorf("a member's write: Decide = %+v, %v, want read_only", got, err)
	}
	if got, err := svc.Decide(ctx, access.Caller{UserID: "u-owner"}, "acme", access.NeedOwner); err != nil || got != (access.Decision{Outcome: access.Granted, Level: access.LevelOwner, Record: acme}) {
		t.Errorf("the owner: Decide = %+v, %v, want granted at level owner with the record", got, err)
	}

	if got, err := svc.Decide(ctx, access.Caller{UserID: "u-owner"}, "acme", "admin"); err == nil || got != (access.Decision{}) {
		t.Errorf("Decide with need admin = %+v, %v, want no decision and an error", got, err)
	}
}
