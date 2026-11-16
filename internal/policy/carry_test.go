package policy_test

import (
	"context"
	"testing"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/identity"
)

func TestVersionCreators_ExcludeTheCredentialThatCreatedAVersion(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.putPolicy(t, `{"default": {"approvals": 1, "approvers": ["owner"], "checks": []}}`)
	p := w.openPushed(t, "work")
	patOther, patOtherID := w.IssuePATID(t, identity.PATSpec{UserID: w.dana,
		Permissions: []string{"git:write", "changes:read", "changes:write", "changes:approve"}})
	files := map[string]string{"work.txt": "work\n"}
	steps := []struct {
		name string
		// do is what happens before the count is read.
		do      func(t *testing.T)
		counted float64
	}{
		{"dana approves with the PAT that created version 1", func(t *testing.T) {
			w.give(t, p.number, p.patPush, approves)
		}, 0},
		{"dana approves with another PAT", func(t *testing.T) {
			w.give(t, p.number, patOther, approves)
		}, 1},
		{"the hub restacks the change and dana approves its version with the other PAT", func(t *testing.T) {
			w.record(t, "work", w.rebased(t, "work", files), change.KindRestack, change.Caller{})
			w.give(t, p.number, patOther, approves)
		}, 1},
		{"dana rebases the change with the other PAT", func(t *testing.T) {
			w.record(t, "work", w.rebased(t, "work", files), change.KindRebase, change.Caller{UserID: w.dana, Username: "dana",
				Credential: change.Credential{Type: change.CredentialPAT, ID: patOtherID}})
		}, 0},
		{"dana approves the rebase with the PAT that created it", func(t *testing.T) {
			w.give(t, p.number, patOther, approves)
		}, 0},
		{"dana approves with her API key", func(t *testing.T) {
			w.give(t, p.number, w.danaKey, approves)
		}, 1},
	}
	for _, s := range steps {
		s.do(t)
		if got := w.counted(t, p.number); got != s.counted {
			t.Errorf("%s: counted %v, want %v", s.name, got, s.counted)
		}
	}

	versions, err := w.changes.ReadVersions(context.Background(), nil, "api", p.number)
	if err != nil {
		t.Fatal(err)
	}
	want := []change.Credential{
		{Type: change.CredentialPAT, ID: p.patPushID}, {}, {Type: change.CredentialPAT, ID: patOtherID},
	}
	if len(versions) != len(want) {
		t.Fatalf("versions %+v, want %d", versions, len(want))
	}
	for i, v := range versions {
		if v.Credential != want[i] {
			t.Errorf("version %d was created with %+v, want %+v", v.Number, v.Credential, want[i])
		}
	}
	if by := versions[1].CreatedBy.ActorType; by != change.ActorSystem {
		t.Errorf("version 2 was created by %s, want %s", by, change.ActorSystem)
	}
}
