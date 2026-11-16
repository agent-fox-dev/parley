package land_test

import (
	"context"
	"database/sql"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/land"
)

func TestRecover_RunsTheAttemptsAStopLeftRunningToTheirEnd(t *testing.T) {
	t.Parallel()
	// The pool of the first start runs nothing, so that every attempt is
	// still running when the hub stops; the second start runs them.
	paused := true
	w := newWorld(t, func(d *land.Deps) { d.Paused = paused })
	ctx := context.Background()
	one := w.open(t, "one")
	commit(t, w.api, "two", w.base, "two", map[string]string{"a.txt": "l1\ntwo\nl3\n"})
	two := w.openBranch(t, "two", w.caller())
	three := w.open(t, "three")
	moved := commit(t, w.api, "main", w.base, "main", map[string]string{"a.txt": "l1\nmain\nl3\n"})

	// Change 1's attempt was created an hour ago, past its deadline: the
	// run after the start has a deadline of its own.
	s := uuid.New().String()
	w.sessions.add(s, w.owner, "api")
	first := attemptOf(one, w.caller(), &s, time.Now().UTC().Add(-time.Hour))
	if err := land.InsertAttempt(ctx, w.Store, first); err != nil {
		t.Fatal(err)
	}
	status, second := w.landCall(t, w.ownerKey, two.Number, "wait=0", nil)
	if status != 202 {
		t.Fatalf("land of change 2 = %d %v, want 202", status, second)
	}
	third, err := w.land.Start(ctx, land.StartInput{
		Workspace: "api", Number: three.Number, Trigger: land.TriggerAutoLand,
		RequestedBy: change.Actor{ActorType: change.CredentialAPIKey, ActorID: new(w.owner), Username: new("owner")},
		Caller:      w.caller(),
	})
	if err != nil {
		t.Fatalf("start the land of change 3: %v", err)
	}
	ids := []string{first.ID, second["id"].(string), third.ID}
	before := make([]land.Attempt, len(ids))
	for i, id := range ids {
		before[i] = w.attempt(t, id)
	}
	w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
		return w.changes.SetAbandoned(ctx, tx, "api", three.Number, "no longer needed")
	})

	paused = false
	w.Restart(t)

	after := make([]land.Attempt, len(ids))
	for i, id := range ids {
		after[i] = w.ended(t, id)
	}
	landed := after[0]
	if landed.Status != land.StatusLanded || landed.LandedSHA == nil || head(t, w.api, "main") != *landed.LandedSHA {
		t.Fatalf("attempt of change 1 = %s (%v), main at %s; want landed, main at its landed head",
			landed.Status, landed.Error, head(t, w.api, "main"))
	}
	if got := strings.Fields(gitrepotest.Git(t, w.api, "rev-list", moved+".."+*landed.LandedSHA)); len(got) != 1 ||
		len(landed.Commits) != 1 || landed.Commits[0].LandedSHA != got[0] {
		t.Errorf("main gained %v with the commits %+v, want change 1's one commit once", got, landed.Commits)
	}
	if c := after[1]; c.Status != land.StatusConflict || c.Conflict == nil || len(c.Conflict.Files) != 1 ||
		c.Conflict.Files[0].Path != "a.txt" {
		t.Errorf("attempt of change 2 = %s (%v) with the report %+v, want conflict on a.txt", c.Status, c.Error, c.Conflict)
	}
	if f := after[2]; f.Status != land.StatusFailed || f.Error == nil || *f.Error != land.ErrorInternal {
		t.Errorf("attempt of change 3 = %s (%v), want failed internal", f.Status, f.Error)
	}
	if got := w.readChange(t, three.Number).State; got != change.StateAbandoned {
		t.Errorf("change 3 is %s, want still abandoned", got)
	}
	for i := range ids {
		b, a := before[i], after[i]
		if a.ID != b.ID || !a.CreatedAt.Equal(b.CreatedAt) || !reflect.DeepEqual(a.RequestedBy, b.RequestedBy) ||
			a.Trigger != b.Trigger || !reflect.DeepEqual(a.SessionID, b.SessionID) {
			t.Errorf("attempt %d after the start = %s created %v by %+v (%s, session %v), want %s created %v by %+v (%s, session %v)",
				i+1, a.ID, a.CreatedAt, a.RequestedBy, a.Trigger, a.SessionID, b.ID, b.CreatedAt, b.RequestedBy, b.Trigger, b.SessionID)
		}
	}
	events := w.landEvents(t, land.EventLand, first.ID)
	if len(events) != 1 || events[0].ResourceID != strconv.FormatInt(one.Number, 10) ||
		events[0].ActorType != change.CredentialAPIKey || events[0].ActorID == nil || *events[0].ActorID != w.owner ||
		events[0].SessionID == nil || *events[0].SessionID != s {
		t.Errorf("parley.change.land events of change 1 = %+v, want one by the owner's API key in session %s", events, s)
	}
}
