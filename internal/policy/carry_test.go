package policy_test

import (
	"context"
	"database/sql"
	"net/http"
	"reflect"
	"strconv"
	"testing"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/identity"
)

// onVersion is the verdicts on version version of the change number of
// api, as the list answers them to dana, newest first.
func (w *world) onVersion(t *testing.T, number, version int64) []map[string]any {
	t.Helper()
	r := w.get(t, approvalsPath(number)+"?version="+strconv.FormatInt(version, 10), w.danaKey)
	if r.status != http.StatusOK {
		t.Fatalf("list the verdicts on version %d of %d = %d %s, want 200", version, number, r.status, r.body)
	}
	var out []map[string]any
	for _, item := range r.json["items"].([]any) {
		out = append(out, item.(map[string]any))
	}
	return out
}

// byUsername is the verdicts by the username of their user.
func byUsername(verdicts []map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, v := range verdicts {
		out[v["by"].(map[string]any)["username"].(string)] = v
	}
	return out
}

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
		{"the hub restacks the change", func(t *testing.T) {
			w.record(t, "work", w.rebased(t, "work", files), change.KindRestack, change.Caller{})
		}, 1},
		{"dana approves the restacked version with the other PAT", func(t *testing.T) {
			w.give(t, p.number, patOther, approves)
		}, 1},
		{"dana rebases the change with the other PAT", func(t *testing.T) {
			w.record(t, "work", w.rebased(t, "work", files), change.KindRebase, change.Caller{UserID: w.dana, Username: "dana",
				Credential: change.Credential{Type: change.CredentialPAT, ID: patOtherID}})
			carried := byUsername(w.onVersion(t, p.number, 3))["dana"]
			if carried["carried_from"] != 2.0 || !reflect.DeepEqual(carried["counts_for"], []any{}) {
				t.Errorf("dana's verdict on version 3 = %v, want carried from 2, counting for nothing", carried)
			}
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

func TestCarry_ANewVersionTakesTheVerdictsOfTheVersionItIsPatchEquivalentTo(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.putPolicy(t, `{"default": {"approvals": 1, "approvers": ["owner", "member"], "checks": []}}`)
	p := w.openPushed(t, "work")
	files := map[string]string{"work.txt": "work\n"}
	w.give(t, p.number, w.melKey, map[string]any{"verdict": "approve", "comment": "ok"})
	w.give(t, p.number, w.danaKey, rejects)
	originals := byUsername(w.onVersion(t, p.number, 1))
	seq := w.seq(t)

	w.record(t, "work", w.rebased(t, "work", files), change.KindRebase, w.danaCaller())
	carried := byUsername(w.onVersion(t, p.number, 2))
	if len(carried) != 2 {
		t.Errorf("verdicts on version 2 = %v, want the two of version 1", carried)
	}
	for user, o := range originals {
		c := carried[user]
		if c == nil {
			continue
		}
		if c["carried_from"] != 1.0 || c["id"] == o["id"] {
			t.Errorf("%s's verdict on version 2 = %v, want a new id carried from 1", user, c)
		}
		for _, key := range []string{"verdict", "comment", "by", "created_at"} {
			if !reflect.DeepEqual(c[key], o[key]) {
				t.Errorf("%s's verdict on version 2 has %s %v, want %v", user, key, c[key], o[key])
			}
		}
	}
	if got := byUsername(w.onVersion(t, p.number, 1)); len(got) != 2 || got["mel"]["id"] != originals["mel"]["id"] || got["dana"]["id"] != originals["dana"]["id"] {
		t.Errorf("verdicts on version 1 after the rebase = %v, want the originals %v", got, originals)
	}
	if l := w.landing(t, p.number); l["status"] != change.LandingPending || only(l)["satisfied"] != false || w.counted(t, p.number) != 1.0 {
		t.Errorf("landing after the rebase = %v, want pending, unsatisfied with mel's carried approval counted", l)
	}

	w.record(t, "work", w.rebased(t, "work", files), change.KindRebase, w.danaCaller())
	third := w.onVersion(t, p.number, 3)
	if len(third) != 2 {
		t.Errorf("verdicts on version 3 = %v, want two", third)
	}
	for _, c := range third {
		if c["carried_from"] != 1.0 {
			t.Errorf("verdict on version 3 = %v, want carried from 1", c)
		}
	}
	if got := len(w.eventsAfter(t, seq, "parley.change.approve")) + len(w.eventsAfter(t, seq, "parley.change.reject")); got != 0 {
		t.Errorf("%d verdict events written for the copies, want none", got)
	}

	w.edited(t, "work", "work.txt")
	if got := w.onVersion(t, p.number, 4); len(got) != 0 {
		t.Errorf("verdicts on version 4, patch-equivalent to none = %v, want none", got)
	}
	if l := w.landing(t, p.number); l["status"] != change.LandingPending || w.counted(t, p.number) != 0.0 {
		t.Errorf("landing after the edit = %v, want pending with none counted", l)
	}
	for user, v := range byUsername(w.onVersion(t, p.number, 1)) {
		if v["id"] != originals[user]["id"] || !reflect.DeepEqual(v["counts_for"], []any{}) {
			t.Errorf("%s's verdict on version 1 after the edit = %v, want the original counting for nothing", user, v)
		}
	}
}

func TestRecordApproval_OnAVersionTheCurrentOneIsPatchEquivalentToCarriesToTheCurrentOne(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.putPolicy(t, `{"default": {"approvals": 1, "approvers": ["owner"], "checks": []}}`)
	p := w.openPushed(t, "work")
	files := map[string]string{"work.txt": "work\n"}
	byPush := change.Caller{UserID: w.dana, Username: "dana", Credential: change.Credential{Type: change.CredentialPAT, ID: p.patPushID}}
	w.record(t, "work", w.rebased(t, "work", files), change.KindRebase, byPush)
	w.record(t, "work", w.rebased(t, "work", files), change.KindRebase, byPush)
	if l := w.landing(t, p.number); l["status"] != change.LandingPending {
		t.Fatalf("landing before any verdict = %v, want pending", l)
	}
	for _, s := range []struct {
		verdict string
		version float64
		status  string
		// countsFor is what the copy on version 3 counts toward.
		countsFor []any
	}{
		{"approve", 1, change.LandingReady, []any{"default"}},
		{"reject", 2, change.LandingPending, []any{"default"}},
	} {
		r := w.postVerdict(t, p.number, w.danaKey, map[string]any{"verdict": s.verdict, "version": s.version})
		if r.status != http.StatusCreated || r.json["version"] != s.version || r.json["carried_from"] != nil {
			t.Errorf("%s version %v = %d %s, want 201 with the verdict on that version", s.verdict, s.version, r.status, r.body)
		}
		cur := w.onVersion(t, p.number, 3)
		if len(cur) != 1 || cur[0]["verdict"] != s.verdict || cur[0]["carried_from"] != s.version ||
			cur[0]["by"].(map[string]any)["username"] != "dana" || !reflect.DeepEqual(cur[0]["counts_for"], s.countsFor) {
			t.Errorf("after %s version %v, verdicts on version 3 = %v, want dana's %s carried from %v counting for %v",
				s.verdict, s.version, cur, s.verdict, s.version, s.countsFor)
		}
		if l := w.landing(t, p.number); l["status"] != s.status {
			t.Errorf("landing after %s version %v = %v, want %s", s.verdict, s.version, l, s.status)
		}
	}
}

func TestCarry_AFailedCopyUndoesTheVersion(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	p := w.openPushed(t, "work")
	w.give(t, p.number, w.melKey, approves)
	head := w.rebased(t, "work", map[string]string{"work.txt": "work\n"})
	in := change.VersionInput{Workspace: "api", Branch: "work", Head: head, Kind: change.KindRebase, Caller: w.danaCaller()}
	recordIn := func() error {
		return w.Store.Tx(ctx, func(tx *sql.Tx) error {
			_, err := w.changes.RecordVersion(ctx, tx, in)
			return err
		})
	}

	if _, err := w.Store.Main().Exec(`CREATE TRIGGER refuse_verdicts BEFORE INSERT ON change_approvals
BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	if err := recordIn(); err == nil {
		t.Error("RecordVersion with the verdicts refused succeeded, want the store's error")
	}
	versions, err := w.changes.ReadVersions(ctx, nil, "api", p.number)
	if ch := w.readChange(t, "api", p.number); err != nil || ch.Version != 1 || len(versions) != 1 || w.count(t, "change_approvals") != 1 {
		t.Errorf("after the refusal: version %d, versions %d (%v), %d verdicts; want version 1 alone and one verdict",
			ch.Version, len(versions), err, w.count(t, "change_approvals"))
	}

	if _, err := w.Store.Main().Exec(`DROP TRIGGER refuse_verdicts`); err != nil {
		t.Fatal(err)
	}
	if err := recordIn(); err != nil {
		t.Fatalf("RecordVersion once the store takes verdicts again: %v", err)
	}
	if got := w.onVersion(t, p.number, 2); len(got) != 1 || got[0]["carried_from"] != 1.0 {
		t.Errorf("verdicts on version 2 = %v, want mel's carried from 1", got)
	}
}
