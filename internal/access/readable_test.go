package access_test

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/store"
)

// readableRecords are the records of the readable-set tests: o1 and o-u1
// are active, o3 is blocked, and no user is a member of o2.
var readableRecords = []access.Record{
	repo("mine", "u1", "o-u1", access.StatusReady),
	repo("team", "u2", "o1", access.StatusReady),
	repo("both", "u1", "o1", access.StatusReady),
	repo("held", "u2", "o3", access.StatusReady),
	repo("other", "u2", "o2", access.StatusReady),
	repo("archived-team", "u2", "o1", access.StatusArchived),
	fork("mine--x", "u3", "o-u1", "mine"),
	fork("team--a", "u1", "o1", "team"),
	fork("team--b", "u3", "o1", "team"),
	fork("other--z", "u1", "o2", "other"),
}

// readableWorld is a store holding readableRecords and the membership
// source of the readable-set tests: u1 is a member of o-u1, o1 and the
// blocked o3; u3 of o-u1; u2 and u9 of nothing.
func readableWorld(t *testing.T) (*store.Store, access.Memberships) {
	t.Helper()
	st := openStore(t, t.TempDir())
	seed(t, st, readableRecords...)
	return st, memberships{
		of:     map[string][]string{"u1": {"o-u1", "o1", "o3"}, "u3": {"o-u1"}},
		active: []string{"o-u1", "o1"},
	}
}

func TestReadable_HoldsExactlyTheWorkspacesOnWhichTheCallerHasALevel(t *testing.T) {
	t.Parallel()
	st, m := readableWorld(t)
	svc := newService(access.StoreRecords(st.Main()), m)
	ctx := context.Background()

	all, err := svc.Readable(ctx, access.Caller{Admin: true})
	if err != nil || !all.All {
		t.Fatalf("Readable(admin token) = %+v, %v, want all and no error", all, err)
	}
	for _, rec := range readableRecords {
		if !all.Contains(rec.Slug) || all.Level(rec.Slug) != access.LevelAdmin {
			t.Errorf("the admin token's set: %s contained %t at %q, want contained at admin", rec.Slug, all.Contains(rec.Slug), all.Level(rec.Slug))
		}
	}

	// Owned beats member on both; the fork of another owner's repository
	// in o2 and the repository of the blocked o3 are left out.
	u1, err := svc.Readable(ctx, access.Caller{UserID: "u1"})
	want := map[string]access.Level{
		"mine": access.LevelOwner, "mine--x": access.LevelOwner, "both": access.LevelOwner,
		"team": access.LevelMember, "team--a": access.LevelOwner, "team--b": access.LevelMember,
		"archived-team": access.LevelMember,
	}
	if err != nil || u1.All || !maps.Equal(u1.Entries, want) {
		t.Errorf("Readable(u1) = %+v, %v, want the entries %v and no error", u1, err, want)
	}

	if u9, err := svc.Readable(ctx, access.Caller{UserID: "u9"}); err != nil || u9.All || len(u9.Entries) != 0 {
		t.Errorf("Readable(u9) = %+v, %v, want an empty set and no error", u9, err)
	}

	// The set agrees with the level the rule resolves, slug by slug.
	for _, user := range []string{"u1", "u2", "u3", "u9"} {
		c := access.Caller{UserID: user}
		set, err := svc.Readable(ctx, c)
		if err != nil {
			t.Fatalf("Readable(%s): %v", user, err)
		}
		for _, rec := range append(readableRecords, access.Record{Slug: "nope"}) {
			level, _, _, err := svc.Level(ctx, c, rec.Slug)
			if err != nil {
				t.Fatalf("Level(%s, %s): %v", user, rec.Slug, err)
			}
			if set.Contains(rec.Slug) != (level != access.LevelNone) || set.Level(rec.Slug) != level {
				t.Errorf("%s on %s: the set holds it %t at %q, the rule resolves %q", user, rec.Slug, set.Contains(rec.Slug), set.Level(rec.Slug), level)
			}
		}
	}
}

func TestReadable_AFailedReadIsTheErrorAndNoSet(t *testing.T) {
	t.Parallel()
	st, m := readableWorld(t)
	records := access.StoreRecords(st.Main())
	for _, c := range []struct {
		name string
		svc  *access.Service
	}{
		{"the memberships", newService(records, failingMemberships)},
		{"the owned and org records", newService(failingRecords{RecordReader: records, failOwned: true}, m)},
		{"the forks of the owned repositories", newService(failingRecords{RecordReader: records, failForks: true}, m)},
	} {
		set, err := c.svc.Readable(context.Background(), access.Caller{UserID: "u1"})
		if !errors.Is(err, errRead) || set.All || len(set.Entries) != 0 {
			t.Errorf("a failed read of %s: Readable(u1) = %+v, %v, want no entries and the read's error", c.name, set, err)
		}
	}
}
