package land_test

import (
	"context"
	"database/sql"
	"reflect"
	"strconv"
	"testing"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/change"
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
