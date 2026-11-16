package land_test

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/land"
)

func TestRun_RecordsTheLandWithTheMoveOfTheTarget(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// protected protects main, and member has a member of the org,
		// who may not push to it, land the owner's change.
		protected, member bool
	}{
		{"an unprotected target, by its owner", false, false},
		{"a protected target, by a member", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			ctx := context.Background()
			by := w.caller()
			if c.member {
				id, _ := w.member(t, "ann")
				by = change.Caller{UserID: id, Username: "ann", Credential: change.Credential{Type: change.CredentialAPIKey, ID: "k-ann"}}
			}
			if c.protected {
				if err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, `UPDATE workspaces SET settings = ? WHERE slug = 'api'`,
						`{"changes.auto_restack":false,"protected_branches":["main"]}`)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			c1 := commit(t, w.api, "feature", w.base, "c1", map[string]string{"c1.txt": "c1\n"})
			c2 := commit(t, w.api, "feature", c1, "c2", map[string]string{"c2.txt": "c2\n"})
			onto := commit(t, w.api, "main", w.base, "moved", map[string]string{"b.txt": "b\n"})
			ch := w.openBranch(t, "feature", w.caller())
			session := uuid.New().String()

			a := w.landNow(t, ch, by, &session)

			if a.Status != land.StatusLanded || a.FinishedAt == nil || a.LandedSHA == nil || len(a.Commits) != 2 {
				t.Fatalf("attempt = %s, finished %v, landed %v, commits %+v; want landed with two commits",
					a.Status, a.FinishedAt, a.LandedSHA, a.Commits)
			}
			landed := *a.LandedSHA
			if got := head(t, w.api, "main"); got != landed {
				t.Errorf("main is at %s, want the landed %s", got, landed)
			}
			actor := change.Actor{ActorType: change.CredentialAPIKey, ActorID: new(by.UserID), Username: new(by.Username)}
			status, got := w.get(t, "/workspaces/api/changes/"+strconv.FormatInt(ch.Number, 10), w.ownerKey)
			want := asJSON(t, change.Landed{SHA: landed, At: *a.FinishedAt, By: actor, LandID: a.ID, Commits: []change.LandedCommit{
				{VersionSHA: c1, LandedSHA: a.Commits[0].LandedSHA}, {VersionSHA: c2, LandedSHA: landed}}})
			if status != 200 || got["state"] != change.StateLanded || !reflect.DeepEqual(got["landed"], any(want)) {
				t.Errorf("GET the change = %d, state %v, landed %v; want landed %v", status, got["state"], got["landed"], want)
			}
			for _, cm := range a.Commits {
				cc, found, err := w.changes.ChangeOfCommit(ctx, nil, "api", cm.LandedSHA)
				if err != nil || !found || cc.Number != ch.Number || !cc.Landed {
					t.Errorf("the change of %s = %+v, %v, %v; want change %d, landed", cm.LandedSHA, cc, found, err, ch.Number)
				}
			}

			var lands []map[string]any
			for _, e := range w.eventsOf(t, "parley.change.land") {
				if e.ResourceType != "change" || e.ResourceID != strconv.FormatInt(ch.Number, 10) || e.ActorType != "api_key" ||
					e.ActorID == nil || *e.ActorID != by.UserID || e.SessionID == nil || *e.SessionID != session {
					t.Errorf("parley.change.land of %s %s by %s %v in session %v; want of change %d by the api key of %s in %s",
						e.ResourceType, e.ResourceID, e.ActorType, e.ActorID, e.SessionID, ch.Number, by.UserID, session)
				}
				lands = append(lands, metadata(t, e))
			}
			wantMeta := map[string]any{"land_id": a.ID, "target_branch": "main", "onto_sha": onto, "landed_sha": landed,
				"strategy": "rebase", "attempts": float64(1), "noop": false, "trigger": "manual", "override": nil,
				"policy_revision": float64(0), "number": float64(ch.Number), "ref": ch.Ref}
			if len(lands) != 1 || !reflect.DeepEqual(lands[0], wantMeta) {
				t.Errorf("parley.change.land metadata %v, want one event with %v", lands, wantMeta)
			}

			pushes := w.landPushes(t)
			if len(pushes) != 1 {
				t.Fatalf("%d parley.git.push events via land, want 1", len(pushes))
			}
			m := metadata(t, pushes[0])
			if m["before"] != onto || m["after"] != landed || m["direct"] != false || m["forced"] != false {
				t.Errorf("parley.git.push metadata %v, want main from %s to %s, not direct, not forced", m, onto, landed)
			}
		})
	}
}

func TestRun_BuildsAgainOnATargetThatMovedBeforeTheSwap(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// byLand moves main by landing another change, else by a commit
		// written to it directly.
		byLand bool
	}{
		{"by the land of another change", true},
		{"by a direct update", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			other, ch := w.open(t, "left"), w.open(t, "right")
			var moved string
			var first land.Attempt
			tries := 0
			land.SetSeams(w.land, land.Seams{BeforeSwap: func(context.Context, string, int) {
				if tries++; tries > 1 {
					return
				}
				if c.byLand {
					first = w.landNow(t, other, w.caller(), nil)
					moved = *first.LandedSHA
				} else {
					moved = commit(t, w.api, "main", w.base, "direct", map[string]string{"d.txt": "d\n"})
				}
			}})

			a := w.landNow(t, ch, w.caller(), nil)

			if a.Status != land.StatusLanded || a.Attempts != 2 || a.OntoSHA == nil || *a.OntoSHA != moved {
				t.Fatalf("attempt = %s after %d tries onto %v; want landed after 2 onto %s", a.Status, a.Attempts, a.OntoSHA, moved)
			}
			if got := head(t, w.api, "main"); got != *a.LandedSHA {
				t.Errorf("main is at %s, want the landed %s", got, *a.LandedSHA)
			}
			if parent := gitrepotest.Git(t, w.api, "rev-parse", *a.LandedSHA+"^"); parent != moved {
				t.Errorf("the landed commit's parent is %s, want the moved head %s", parent, moved)
			}
			if c.byLand && first.Attempts != 1 {
				t.Errorf("the other change's attempt took %d tries, want 1", first.Attempts)
			}
		})
	}
}

func TestRun_EndsAsContentionWhenEveryTryLostTheSwap(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		limit int // 0 is the default
		want  int
	}{
		{"with a limit of 3", 3, 3},
		{"with the default limit", 0, 10},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t, func(d *land.Deps) { d.MaxAttempts = c.limit })
			ch := w.open(t, "feature")
			moves := []string{"base"}
			land.SetSeams(w.land, land.Seams{BeforeSwap: func(context.Context, string, int) {
				n := strconv.Itoa(len(moves))
				msg := "moved " + n
				commit(t, w.api, "main", head(t, w.api, "main"), msg, map[string]string{"moved" + n + ".txt": n})
				moves = append([]string{msg}, moves...)
			}})

			a := w.landNow(t, ch, w.caller(), nil)

			if a.Status != land.StatusFailed || a.Error == nil || *a.Error != land.ErrorContention || a.Attempts != c.want ||
				a.LandedSHA != nil || a.FinishedAt == nil {
				t.Fatalf("attempt = %s (%v) after %d tries, landed %v; want failed contention after %d, nothing landed",
					a.Status, a.Error, a.Attempts, a.LandedSHA, c.want)
			}
			if got := gitrepotest.Git(t, w.api, "log", "--format=%s", "main"); got != strings.Join(moves, "\n") || len(moves) != c.want+1 {
				t.Errorf("main holds %q, want only the %d commits that moved it on base", got, c.want)
			}
			if got := w.readChange(t, ch.Number).State; got != change.StateOpen {
				t.Errorf("the change is %s, want open", got)
			}
			land.SetSeams(w.land, land.Seams{})
			if again := w.landNow(t, ch, w.caller(), nil); again.Status != land.StatusLanded {
				t.Errorf("a new land of the change ended %s, want landed", again.Status)
			}
		})
	}
}

// failingGate is a policy gate that cannot answer.
type failingGate struct{}

func (failingGate) Evaluate(context.Context, land.GateInput) (land.GateAnswer, error) {
	return land.GateAnswer{}, errors.New("the policy cannot be read")
}

func TestRun_EndsAsInternalWhenTheTargetOrAStepFails(t *testing.T) {
	t.Parallel()
	deleteMain := func(t *testing.T, w *world) { gitrepotest.Git(t, w.api, "update-ref", "-d", "refs/heads/main") }
	for _, c := range []struct {
		name string
		// before prepares the world before the run, swap runs before the
		// compare-and-swap, and change alters the change the attempt
		// covers.
		before, swap func(t *testing.T, w *world)
		change       func(ch *change.Change)
		// deleted is set when main is gone.
		deleted bool
	}{
		{name: "the target deleted between the replay and the swap", swap: deleteMain, deleted: true},
		{name: "the target deleted before the run", before: deleteMain, deleted: true},
		{name: "the policy gate failing", before: func(_ *testing.T, w *world) { w.land.RegisterGate(failingGate{}) }},
		{name: "git failing in the replay", change: func(ch *change.Change) { ch.HeadSHA = strings.Repeat("f", 40) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			ch := w.open(t, "feature")
			if c.before != nil {
				c.before(t, w)
			}
			if c.swap != nil {
				land.SetSeams(w.land, land.Seams{BeforeSwap: func(context.Context, string, int) { c.swap(t, w) }})
			}
			covered := ch
			if c.change != nil {
				c.change(&covered)
			}

			a, err := w.runLand(t, covered, w.caller(), nil)

			if err == nil || a.Status != land.StatusFailed || a.Error == nil || *a.Error != land.ErrorInternal || a.LandedSHA != nil {
				t.Fatalf("run = %v, attempt %s (%v) landed %v; want an error and failed internal", err, a.Status, a.Error, a.LandedSHA)
			}
			if got := w.readChange(t, ch.Number).State; got != change.StateOpen {
				t.Errorf("the change is %s, want open", got)
			}
			want := w.base
			if c.deleted {
				want = ""
			}
			if got := head(t, w.api, "main"); got != want {
				t.Errorf("main is at %q, want %q", got, want)
			}
		})
	}
}
