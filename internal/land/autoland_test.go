package land_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/land"
	"github.com/agent-fox-dev/parley/internal/server"
)

// autoLand makes the world's automatic lander follow the event log every
// interval and start its lands through lands when it is not nil.
func autoLand(interval time.Duration, lands land.Starter) func(*hub.Composition) {
	return func(c *hub.Composition) {
		c.AdjustAutoLand(func(d *land.AutoLandDeps) {
			d.Interval = interval
			if lands != nil {
				d.Lands = lands
			}
		})
	}
}

// checkPolicy registers the landing policy's gate on the world again and
// sets api's policy: a file under the paths globs needs the check test.
func (w *world) checkPolicy(t *testing.T, paths ...string) {
	t.Helper()
	w.land.RegisterGate(w.policyGate)
	policy := map[string]any{"default": map[string]any{"approvals": 0},
		"rules": []any{map[string]any{"name": "tested", "paths": paths, "require": map[string]any{"approvals": 0, "checks": []string{"test"}}}}}
	if status, raw := w.Call(t, http.MethodPut, "/api/v1/workspaces/api/policy", w.ownerKey, policy); status != http.StatusOK {
		t.Fatalf("PUT api's policy = %d %s, want 200", status, raw)
	}
}

// passCheck reports the check test of the change number of api passed.
func (w *world) passCheck(t *testing.T, number int64) {
	t.Helper()
	path := fmt.Sprintf("/api/v1/workspaces/api/changes/%d/checks/test", number)
	if status, raw := w.Call(t, http.MethodPut, path, w.ownerKey, map[string]any{"status": "passed"}); status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("PUT check test of change %d = %d %s, want it reported", number, status, raw)
	}
}

// lands are the land attempts of the change number of api, newest first,
// as the attempts route answers them.
func (w *world) lands(t *testing.T, number int64) []map[string]any {
	t.Helper()
	status, answer := w.get(t, fmt.Sprintf("/workspaces/api/changes/%d/lands", number), w.ownerKey)
	items, ok := answer["items"].([]any)
	if status != http.StatusOK || !ok {
		t.Fatalf("GET the lands of change %d = %d %v", number, status, answer)
	}
	out := make([]map[string]any, len(items))
	for i, it := range items {
		out[i] = it.(map[string]any)
	}
	return out
}

// landFails are the parley.change.land_fail events on the change number
// of api, oldest first.
func (w *world) landFails(t *testing.T, number int64) []event.Record {
	t.Helper()
	var out []event.Record
	for _, e := range w.eventsOf(t, land.EventLandFail) {
		if e.ResourceID == strconv.FormatInt(number, 10) {
			out = append(out, e)
		}
	}
	return out
}

// openWith opens, through the change component, a change of api by the
// caller by from a new branch of base that sets the files, with auto_land
// as given, and returns it.
func (w *world) openWith(t *testing.T, branch string, files map[string]string, autoLand bool, by change.Caller) change.Change {
	t.Helper()
	commit(t, w.api, branch, w.base, branch, files)
	return w.opened(t, change.OpenInput{Workspace: "api", SourceBranch: branch, Intent: new("why"), Kind: change.KindRef,
		AutoLand: new(autoLand), Caller: by})
}

// openAs opens, through the change component, a change of api by its
// author by from a new branch of base that sets the files, with auto_land
// off: from api for its owner, and from a task fork the author owns for
// anyone else, who may not write api.
func (w *world) openAs(t *testing.T, branch string, files map[string]string, by change.Caller) change.Change {
	t.Helper()
	if by.UserID == w.owner {
		return w.openWith(t, branch, files, false, by)
	}
	slug := "api--" + branch
	return w.openIn(t, slug, w.fork(t, slug, by.UserID), branch, files, by)
}

// notPermitted reports whether the event rec is the refusal of an
// automatic land whose setter may not land: by system, with no attempt.
func notPermitted(t *testing.T, rec event.Record) bool {
	t.Helper()
	m := metadata(t, rec)
	landID, hasLandID := m["land_id"]
	return rec.ActorType == event.ActorSystem && rec.ActorID == nil && hasLandID && landID == nil &&
		m["status"] == land.StatusBlocked && m["reason"] == "not_permitted"
}

func TestAutoLand_StartsOneAttemptAsTheSetterWhenTheChangeBecomesLandable(t *testing.T) {
	t.Parallel()
	w := startWorld(t, autoLand(50*time.Millisecond, nil))
	w.checkPolicy(t, "**")
	w.configure(t, `{"changes.auto_restack":false,"land.by_author":true}`)
	mia, _ := w.member(t, "mia")
	miaPAT := w.IssuePAT(t, identity.PATSpec{UserID: mia, Permissions: []string{"changes:read", "changes:write"}})
	miaCaller := change.Caller{UserID: mia, Username: "mia", Credential: change.Credential{Type: change.CredentialAPIKey, ID: "m1"}}
	for i, c := range []struct {
		name       string
		author     change.Caller
		credential string
		// requestedBy is the requested_by the attempt is to show.
		requestedBy map[string]any
	}{
		{"the workspace owner with an API key", w.caller(), w.ownerKey,
			map[string]any{"actor_type": change.CredentialAPIKey, "actor_id": w.owner, "username": "owner"}},
		{"an org member who authored the change, by land.by_author, with a PAT", miaCaller, miaPAT,
			map[string]any{"actor_type": change.CredentialPAT, "actor_id": mia, "username": "mia"}},
		{"the admin token", w.caller(), w.Token,
			map[string]any{"actor_type": change.CredentialAdminToken, "actor_id": nil, "username": nil}},
	} {
		t.Run(c.name, func(t *testing.T) {
			branch := "auto" + strconv.Itoa(i)
			ch := w.openAs(t, branch, map[string]string{branch + ".txt": "x\n"}, c.author)
			if status, answer := w.patchChange(t, c.credential, ch.Number, map[string]any{"auto_land": true}); status != http.StatusOK || answer["auto_land"] != true {
				t.Fatalf("PATCH auto_land true = %d %v, want 200 with auto_land true", status, answer)
			}
			w.mergeable(t, ch.Number, change.MergeableClean)

			w.passCheck(t, ch.Number)

			until(t, ch.Ref+" lands", func() bool { return w.readChange(t, ch.Number).State == change.StateLanded })
			for range 2 {
				if err := w.lander.Pass(context.Background()); err != nil {
					t.Fatalf("pass of the automatic lander: %v", err)
				}
			}
			lands := w.lands(t, ch.Number)
			if len(lands) != 1 {
				t.Fatalf("%s has %d land attempts, want 1: %v", ch.Ref, len(lands), lands)
			}
			a := lands[0]
			if a["trigger"] != land.TriggerAutoLand || !reflect.DeepEqual(a["changes"], []any{float64(ch.Number)}) ||
				a["override"] != nil || a["session_id"] != nil || !reflect.DeepEqual(a["requested_by"], c.requestedBy) {
				t.Errorf("the attempt is %v, want trigger auto_land, changes [%d], no override, no session, requested by %v",
					a, ch.Number, c.requestedBy)
			}
			events := w.landEvents(t, land.EventLand, a["id"].(string))
			if len(events) != 1 || events[0].ActorType != event.ActorSystem || events[0].ActorID != nil ||
				metadata(t, events[0])["trigger"] != land.TriggerAutoLand {
				t.Errorf("the land events of the attempt are %+v, want one by system with trigger auto_land", events)
			}
		})
	}
}

func TestAutoLand_ClearsTheFlagWhenTheSetterMayNoLongerLand(t *testing.T) {
	t.Parallel()
	w := startWorld(t, autoLand(50*time.Millisecond, nil))
	w.checkPolicy(t, "**")
	byAuthor := `{"changes.auto_restack":false,"land.by_author":true}`
	for i, c := range []struct {
		name string
		// arrange returns the author of the change and the credential and
		// user of the setter; grant, when not nil, runs once the change is
		// opened, so that the setter may land it; cause takes that away.
		arrange func(t *testing.T) (author change.Caller, credential, setter string)
		grant   func(t *testing.T, setter string)
		cause   func(t *testing.T, setter string)
	}{
		{name: "the setter is deleted",
			arrange: func(t *testing.T) (change.Caller, string, string) {
				id, key := w.member(t, "gone")
				return change.Caller{UserID: id, Username: "gone", Credential: change.Credential{Type: change.CredentialAPIKey}}, key, id
			},
			cause: func(t *testing.T, setter string) {
				if status, raw := w.Call(t, http.MethodDelete, "/api/v1/users/"+setter, w.Token, nil); status != http.StatusNoContent {
					t.Fatalf("DELETE the setter = %d %s, want 204", status, raw)
				}
			}},
		{name: "the setter is blocked",
			arrange: func(t *testing.T) (change.Caller, string, string) {
				id, key := w.member(t, "blocked")
				return change.Caller{UserID: id, Username: "blocked", Credential: change.Credential{Type: change.CredentialAPIKey}}, key, id
			},
			cause: func(t *testing.T, setter string) {
				if status, raw := w.Call(t, http.MethodPost, "/api/v1/users/"+setter+"/block", w.Token, nil); status != http.StatusOK {
					t.Fatalf("block the setter = %d %s, want 200", status, raw)
				}
			}},
		{name: "the setter lost write access and is not the author",
			arrange: func(t *testing.T) (change.Caller, string, string) {
				id, key := w.member(t, "keeper")
				return w.caller(), key, id
			},
			grant: func(t *testing.T, setter string) { w.ownedBy(t, setter) },
			cause: func(t *testing.T, _ string) { w.ownedBy(t, w.owner) }},
		{name: "the setter is the author without write access and land.by_author is off",
			arrange: func(t *testing.T) (change.Caller, string, string) {
				id, key := w.member(t, "author")
				return change.Caller{UserID: id, Username: "author", Credential: change.Credential{Type: change.CredentialAPIKey}}, key, id
			},
			cause: func(t *testing.T, _ string) { w.configure(t, `{"changes.auto_restack":false,"land.by_author":false}`) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			w.configure(t, byAuthor)
			author, credential, setter := c.arrange(t)
			branch := "np" + strconv.Itoa(i)
			ch := w.openAs(t, branch, map[string]string{branch + ".txt": "x\n"}, author)
			if c.grant != nil {
				c.grant(t, setter)
				t.Cleanup(func() { w.ownedBy(t, w.owner) })
			}
			if status, answer := w.patchChange(t, credential, ch.Number, map[string]any{"auto_land": true}); status != http.StatusOK || answer["auto_land"] != true {
				t.Fatalf("PATCH auto_land true = %d %v, want 200 with auto_land true", status, answer)
			}
			w.mergeable(t, ch.Number, change.MergeableClean)
			c.cause(t, setter)
			target := head(t, w.api, "main")

			w.passCheck(t, ch.Number)

			until(t, "the flag of "+ch.Ref+" is cleared", func() bool { return !w.readChange(t, ch.Number).AutoLand })
			if got := w.readChange(t, ch.Number); got.State != change.StateOpen {
				t.Errorf("%s is %s, want open", ch.Ref, got.State)
			}
			if lands := w.lands(t, ch.Number); len(lands) != 0 {
				t.Errorf("%s has land attempts %v, want none", ch.Ref, lands)
			}
			if fails := w.landFails(t, ch.Number); len(fails) != 1 || !notPermitted(t, fails[0]) {
				t.Errorf("the land_fail events of %s are %+v, want one by system: land_id null, blocked, not_permitted", ch.Ref, fails)
			}
			if got := head(t, w.api, "main"); got != target {
				t.Errorf("main moved to %s, want it at %s", got, target)
			}
		})
	}
}

// ownedBy makes the user id the owner of api.
func (w *world) ownedBy(t *testing.T, id string) {
	t.Helper()
	w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE workspaces SET owner_id = ? WHERE slug = 'api'`, id)
		return err
	})
}

// countingLands is a land component that starts nothing, counts what it
// is asked to start and answers that the changes running have a running
// attempt.
type countingLands struct {
	mu      sync.Mutex
	starts  int
	running map[int64]bool
}

func (s *countingLands) Start(context.Context, land.StartInput) (land.Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starts++
	return land.Attempt{}, nil
}

func (s *countingLands) Running(_ context.Context, _ string, number int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running[number], nil
}

func (s *countingLands) started() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts
}

func TestAutoLand_StartsNothingForAnAnnouncedChangeThatIsNoLongerLandable(t *testing.T) {
	t.Parallel()
	lands := &countingLands{running: map[int64]bool{}}
	w := startWorld(t, autoLand(time.Hour, lands))
	if status, raw := w.Call(t, http.MethodPut, "/api/v1/workspaces/api/policy", w.ownerKey, map[string]any{
		"default": map[string]any{"approvals": 0},
		"rules": []any{map[string]any{"name": "gated", "paths": []string{"gated/**"},
			"require": map[string]any{"approvals": 0, "checks": []string{"test"}}}},
	}); status != http.StatusOK {
		t.Fatalf("PUT api's policy = %d %s, want 200", status, raw)
	}
	for i, c := range []struct {
		name    string
		arrange func(t *testing.T, branch string) change.Change
	}{
		{"an abandoned change", func(t *testing.T, branch string) change.Change {
			ch := w.openWith(t, branch, map[string]string{branch + ".txt": "x\n"}, true, w.caller())
			w.mergeable(t, ch.Number, change.MergeableClean)
			w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
				return w.changes.SetAbandoned(ctx, tx, "api", ch.Number, "no longer needed")
			})
			return ch
		}},
		{"a change whose flag is off", func(t *testing.T, branch string) change.Change {
			ch := w.openWith(t, branch, map[string]string{branch + ".txt": "x\n"}, false, w.caller())
			w.mergeable(t, ch.Number, change.MergeableClean)
			return ch
		}},
		{"a change whose landing is pending", func(t *testing.T, branch string) change.Change {
			ch := w.openWith(t, branch, map[string]string{"gated/" + branch: "x\n"}, true, w.caller())
			w.mergeable(t, ch.Number, change.MergeableClean)
			return ch
		}},
		{"a change whose mergeable is conflict", func(t *testing.T, branch string) change.Change {
			ch := w.openWith(t, branch, map[string]string{branch + ".txt": "x\n"}, true, w.caller())
			w.mergeable(t, ch.Number, change.MergeableClean)
			w.setMergeable(t, ch.Number, change.MergeableConflict, nil)
			return ch
		}},
		{"a change with a running attempt", func(t *testing.T, branch string) change.Change {
			ch := w.openWith(t, branch, map[string]string{branch + ".txt": "x\n"}, true, w.caller())
			w.mergeable(t, ch.Number, change.MergeableClean)
			lands.mu.Lock()
			lands.running[ch.Number] = true
			lands.mu.Unlock()
			return ch
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			ch := c.arrange(t, "gone"+strconv.Itoa(i))
			flag := w.readChange(t, ch.Number).AutoLand
			if _, err := w.events.EmitNow(ctx, event.Input{Type: change.EventReady, Workspace: new("api"), ActorType: event.ActorSystem,
				ResourceType: "change", ResourceID: strconv.FormatInt(ch.Number, 10),
				Metadata: map[string]any{"number": ch.Number, "version": ch.Version, "head_sha": ch.HeadSHA}}); err != nil {
				t.Fatal(err)
			}
			events := w.eventCount(t)

			if err := w.lander.Pass(ctx); err != nil {
				t.Fatalf("pass of the automatic lander: %v", err)
			}

			if n := lands.started(); n != 0 {
				t.Errorf("the lander started %d lands, want none", n)
			}
			if got := w.readChange(t, ch.Number).AutoLand; got != flag {
				t.Errorf("auto_land is %t, want it kept at %t", got, flag)
			}
			if n := w.eventCount(t); n != events {
				t.Errorf("%d events were written, want none", n-events)
			}
		})
	}
}

func TestAutoLand_StartsAnAttemptWhenTheFlagIsSetOnALandableChange(t *testing.T) {
	t.Parallel()
	w := startWorld(t, autoLand(50*time.Millisecond, nil))
	ch := w.open(t, "ready")
	w.mergeable(t, ch.Number, change.MergeableClean)
	if err := w.lander.Pass(context.Background()); err != nil {
		t.Fatalf("pass of the automatic lander: %v", err)
	}
	if lands := w.lands(t, ch.Number); len(lands) != 0 {
		t.Fatalf("%s has land attempts %v before its flag is set, want none", ch.Ref, lands)
	}

	if status, answer := w.patchChange(t, w.ownerKey, ch.Number, map[string]any{"auto_land": true}); status != http.StatusOK || answer["auto_land"] != true {
		t.Fatalf("PATCH auto_land true = %d %v, want 200 with auto_land true", status, answer)
	}

	until(t, ch.Ref+" lands", func() bool { return w.readChange(t, ch.Number).State == change.StateLanded })
	if lands := w.lands(t, ch.Number); len(lands) != 1 || lands[0]["trigger"] != land.TriggerAutoLand {
		t.Errorf("%s has land attempts %v, want one with trigger auto_land", ch.Ref, lands)
	}
}

func TestAutoLand_StartsAnAttemptAtStartUpForALandableChangeWithTheFlagOn(t *testing.T) {
	t.Parallel()
	// The lander follows the event log once an hour: only the start-up
	// can start a land within the test.
	w := startWorld(t, autoLand(time.Hour, nil))
	a := w.openWith(t, "flagged", map[string]string{"flagged.txt": "a\n"}, true, w.caller())
	b := w.open(t, "unflagged")
	w.mergeable(t, a.Number, change.MergeableClean)
	w.mergeable(t, b.Number, change.MergeableClean)
	if n := w.attempts(t); n != 0 {
		t.Fatalf("%d land attempts before the restart, want none", n)
	}

	w.Restart(t)

	until(t, a.Ref+" lands", func() bool { return w.readChange(t, a.Number).State == change.StateLanded })
	if lands := w.lands(t, a.Number); len(lands) != 1 || lands[0]["trigger"] != land.TriggerAutoLand {
		t.Errorf("%s has land attempts %v, want one with trigger auto_land", a.Ref, lands)
	}
	if lands := w.lands(t, b.Number); len(lands) != 0 {
		t.Errorf("%s has land attempts %v, want none", b.Ref, lands)
	}
}

// openAbove opens, through the change component, a change of api by the
// owner from a new branch with one commit on the head of the change below
// that adds <branch>.txt, with auto_land as given, and returns it; the
// stack linker links it to below.
func (w *world) openAbove(t *testing.T, branch string, below change.Change, autoLand bool) change.Change {
	t.Helper()
	commit(t, w.api, branch, below.HeadSHA, branch, map[string]string{branch + ".txt": branch + "\n"})
	ch := w.opened(t, change.OpenInput{Workspace: "api", SourceBranch: branch, Intent: new("why"), Kind: change.KindRef,
		AutoLand: new(autoLand), Caller: w.caller()})
	if ch.DependsOn == nil || *ch.DependsOn != below.Number {
		t.Fatalf("%s depends on %v, want %d", ch.Ref, ch.DependsOn, below.Number)
	}
	return ch
}

func TestAutoLand_StartsNothingForAChangeAboveOneThatIsNotLanded(t *testing.T) {
	t.Parallel()
	// The lander follows the event log once an hour: the test runs it.
	w := startWorld(t, autoLand(time.Hour, nil))
	lower := w.openWith(t, "low", map[string]string{"low.txt": "low\n"}, false, w.caller())
	upper := w.openAbove(t, "up", lower, true)
	w.mergeable(t, upper.Number, change.MergeableClean)
	until(t, upper.Ref+" is announced landable", func() bool { return w.readyCount(t, upper.Number) > 0 })
	ctx := context.Background()

	// The announcement, then the start the flag made due and the look the
	// start-up takes at every change whose flag is on.
	if err := w.lander.Pass(ctx); err != nil {
		t.Fatalf("pass of the automatic lander: %v", err)
	}
	if err := w.lander.Recover(ctx); err != nil {
		t.Fatalf("start-up of the automatic lander: %v", err)
	}

	if lands := w.lands(t, upper.Number); len(lands) != 0 {
		t.Errorf("%s has land attempts %v while %s is not landed, want none", upper.Ref, lands, lower.Ref)
	}
	if got := w.readChange(t, upper.Number); !got.AutoLand || got.State != change.StateOpen {
		t.Errorf("%s is %s with auto_land %t, want open with auto_land true", upper.Ref, got.State, got.AutoLand)
	}
	if fails := w.landFails(t, upper.Number); len(fails) != 0 {
		t.Errorf("the land_fail events of %s are %+v, want none", upper.Ref, fails)
	}
}

func TestAutoLand_LandsAStackFromTheBottomUpOneChangeAtATime(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// moved, when not nil, is a file main gets once the stack is
		// opened, so that the change below lands as new commits and the
		// hub restacks the one above onto them.
		moved map[string]string
		// log is the subjects of main's commits once both landed, oldest
		// first.
		log string
	}{
		{name: "the change below lands as its own commits", log: "base\nlow\nup"},
		{name: "the change below lands on a moved main and the hub restacks the one above",
			moved: map[string]string{"m.txt": "m\n"}, log: "base\nmoved\nlow\nup"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := startWorld(t, autoLand(50*time.Millisecond, nil))
			w.checkPolicy(t, "**")
			w.configure(t, `{"changes.auto_restack":true}`)
			lower := w.openWith(t, "low", map[string]string{"low.txt": "low\n"}, true, w.caller())
			upper := w.openAbove(t, "up", lower, true)
			if c.moved != nil {
				commit(t, w.api, "main", w.base, "moved", c.moved)
			}
			w.mergeable(t, lower.Number, change.MergeableClean)
			w.mergeable(t, upper.Number, change.MergeableClean)
			ctx := context.Background()

			w.passCheck(t, upper.Number)
			until(t, upper.Ref+" is announced landable", func() bool { return w.readyCount(t, upper.Number) > 0 })
			if err := w.lander.Pass(ctx); err != nil {
				t.Fatalf("pass of the automatic lander: %v", err)
			}
			if lands := w.lands(t, upper.Number); len(lands) != 0 {
				t.Fatalf("%s has land attempts %v before %s landed, want none", upper.Ref, lands, lower.Ref)
			}
			w.passCheck(t, lower.Number)

			within(t, time.Minute, "both changes land", func() bool {
				return w.readChange(t, lower.Number).State == change.StateLanded && w.readChange(t, upper.Number).State == change.StateLanded
			})
			var ids []string
			for _, ch := range []change.Change{lower, upper} {
				lands := w.lands(t, ch.Number)
				if len(lands) != 1 || lands[0]["trigger"] != land.TriggerAutoLand || lands[0]["status"] != land.StatusLanded ||
					!reflect.DeepEqual(lands[0]["changes"], []any{float64(ch.Number)}) {
					t.Fatalf("%s has land attempts %v, want one landed of it alone with trigger auto_land", ch.Ref, lands)
				}
				ids = append(ids, lands[0]["id"].(string))
			}
			low, up := w.attempt(t, ids[0]), w.attempt(t, ids[1])
			if low.FinishedAt == nil || low.FinishedAt.After(up.CreatedAt) {
				t.Errorf("%s's attempt finished at %v, %s's was created at %v, want it created after",
					lower.Ref, low.FinishedAt, upper.Ref, up.CreatedAt)
			}
			if got := gitrepotest.Git(t, w.api, "log", "--reverse", "--format=%s", "main"); got != c.log {
				t.Errorf("main holds the commits %q, oldest first, want %q", got, c.log)
			}
		})
	}
}

func TestAutoLand_HoldsTheFlagToTheGlobsStoredWithIt(t *testing.T) {
	t.Parallel()
	w := startWorld(t, autoLand(50*time.Millisecond, nil))
	w.checkPolicy(t, "**")
	for i, c := range []struct {
		name string
		// after runs once the change is opened with the token, whose id it
		// is given, and before its check passes.
		after  func(t *testing.T, ch change.Change, tokenID string)
		landed bool
	}{
		{name: "a version adding a file outside the globs", after: func(t *testing.T, ch change.Change, _ string) {
			next := commit(t, w.api, ch.Source.Branch, ch.HeadSHA, "src", map[string]string{"src/b.go": "package b\n"})
			w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
				_, err := w.changes.RecordVersion(ctx, tx, change.VersionInput{Workspace: "api", Branch: ch.Source.Branch, Head: next, Kind: change.KindRef})
				return err
			})
		}},
		{name: "the token revoked, no other version", landed: true, after: func(t *testing.T, _ change.Change, tokenID string) {
			path := fmt.Sprintf("/api/v1/users/%s/tokens/%s", w.owner, tokenID)
			if status, raw := w.Call(t, http.MethodDelete, path, w.Token, nil); status != http.StatusNoContent && status != http.StatusOK {
				t.Fatalf("revoke the token = %d %s, want it revoked", status, raw)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			token, tokenID := w.IssuePATID(t, identity.PATSpec{UserID: w.owner,
				Permissions: []string{"changes:read", "changes:write"}, Paths: []string{"docs/**"}})
			branch := "globs" + strconv.Itoa(i)
			commit(t, w.api, branch, w.base, branch, map[string]string{"docs/" + branch + ".md": "a\n"})
			status, raw := w.Call(t, http.MethodPost, "/api/v1/workspaces/api/changes", token,
				map[string]any{"source": map[string]any{"branch": branch}, "intent": "why", "auto_land": true})
			if status != http.StatusCreated {
				t.Fatalf("open with the token = %d %s, want 201", status, raw)
			}
			ch := w.readChange(t, numberOf(t, raw))
			c.after(t, ch, tokenID)
			w.mergeable(t, ch.Number, change.MergeableClean)
			target := head(t, w.api, "main")

			w.passCheck(t, ch.Number)

			if c.landed {
				until(t, ch.Ref+" lands", func() bool { return w.readChange(t, ch.Number).State == change.StateLanded })
				if lands := w.lands(t, ch.Number); len(lands) != 1 || lands[0]["trigger"] != land.TriggerAutoLand {
					t.Errorf("%s has land attempts %v, want one with trigger auto_land", ch.Ref, lands)
				}
				return
			}
			until(t, "the flag of "+ch.Ref+" is cleared", func() bool { return !w.readChange(t, ch.Number).AutoLand })
			if lands := w.lands(t, ch.Number); len(lands) != 0 {
				t.Errorf("%s has land attempts %v, want none", ch.Ref, lands)
			}
			if fails := w.landFails(t, ch.Number); len(fails) != 1 || !notPermitted(t, fails[0]) {
				t.Errorf("the land_fail events of %s are %+v, want one by system: land_id null, blocked, not_permitted", ch.Ref, fails)
			}
			if got := head(t, w.api, "main"); got != target {
				t.Errorf("main moved to %s, want it at %s", got, target)
			}
		})
	}
}

// numberOf is the number of the change object raw.
func numberOf(t *testing.T, raw []byte) int64 {
	t.Helper()
	var answer struct {
		Number int64 `json:"number"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil || answer.Number == 0 {
		t.Fatalf("no number in %s: %v", raw, err)
	}
	return answer.Number
}

// endingLands is a land component whose attempts end as soon as they are
// started, with the statuses of ends in turn, the last one repeated, and
// which tells the lander it is bound to of each end, as the land
// component tells its final observers. It keeps what it was asked to
// start and the change's auto_land as each start found it, and answers
// that no attempt is running. While busy is true it refuses every start
// 409 workspace_busy, as the land component refuses one while the
// workspace is busy, and counts the refusals.
type endingLands struct {
	ends []string
	busy atomic.Bool

	mu      sync.Mutex
	lander  *land.AutoLander
	changes land.AutoLandChanges
	starts  []land.StartInput
	flags   []bool
	refused int
}

// bind makes s tell lander of the ends of its attempts and read the
// changes through changes.
func (s *endingLands) bind(lander *land.AutoLander, changes land.AutoLandChanges) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lander, s.changes = lander, changes
}

func (s *endingLands) Start(ctx context.Context, in land.StartInput) (land.Attempt, error) {
	s.mu.Lock()
	if s.busy.Load() {
		s.refused++
		s.mu.Unlock()
		return land.Attempt{}, server.NewError("workspace_busy", http.StatusConflict, in.Workspace+" is busy")
	}
	status := s.ends[min(len(s.starts), len(s.ends)-1)]
	s.starts = append(s.starts, in)
	n, lander, changes := len(s.starts), s.lander, s.changes
	s.mu.Unlock()
	ch, err := changes.ReadChange(ctx, nil, in.Workspace, in.Number)
	if err != nil {
		return land.Attempt{}, err
	}
	s.mu.Lock()
	s.flags = append(s.flags, ch.AutoLand)
	s.mu.Unlock()
	a := land.Attempt{ID: attemptID(n), Workspace: in.Workspace, Status: status, Trigger: in.Trigger, Attempts: 1,
		Covered: []land.CoveredChange{{Number: in.Number, Version: ch.Version}}}
	if status == land.StatusFailed {
		a.Error = new(land.ErrorContention)
	}
	lander.Finished(ctx, land.Final{Attempt: a, Result: status, Attempts: 1})
	return a, nil
}

func (s *endingLands) Running(context.Context, string, int64) (bool, error) { return false, nil }

// started counts the lands s was asked to start.
func (s *endingLands) started() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.starts)
}

// later is a new clock d after c, the clock of a hub started again d after
// it stopped.
func (c *clock) later(d time.Duration) *clock {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := newClock()
	next.now = c.now + d
	return next
}

// retryWorld is a world whose automatic lander runs every second on the
// clock clk returns at each start of the hub, and starts its lands
// through lands, or through the land component when lands is nil. It
// takes the first wait of the lander's loop, so that each move of the
// clock wakes the loop once and each wait it takes is the end of a run.
func retryWorld(t *testing.T, clk func() *clock, lands *endingLands) *world {
	t.Helper()
	w := startWorld(t, func(c *hub.Composition) {
		lander := c.AutoLander()
		c.AdjustAutoLand(func(d *land.AutoLandDeps) {
			now := clk()
			d.Now, d.After, d.Interval = now.Now, now.After, time.Second
			if lands != nil {
				d.Lands = lands
				lands.bind(lander, d.Changes)
			}
		})
	})
	clk().waitAsked(t)
	return w
}

// firstLand opens a change of api from branch by its owner with auto_land
// on, waits until it is landable, and lets the lander run once, which
// starts its first automatic land through lands.
func firstLand(t *testing.T, w *world, clk *clock, lands *endingLands, branch string) change.Change {
	t.Helper()
	ch := w.openWith(t, branch, map[string]string{branch + ".txt": "x\n"}, true, w.caller())
	w.mergeable(t, ch.Number, change.MergeableClean)
	clk.advance(time.Second)
	clk.waitAsked(t)
	if n := lands.started(); n != 1 {
		t.Fatalf("the lander started %d lands for the landable %s, want 1", n, ch.Ref)
	}
	return ch
}

// retryAfter moves clk on by d less a millisecond and fails unless lands
// started nothing, then by the millisecond and fails unless lands started
// exactly one land: the start of a retry d after the attempt before it
// ended.
func retryAfter(t *testing.T, clk *clock, lands *endingLands, d time.Duration) {
	t.Helper()
	n := lands.started()
	clk.advance(d - time.Millisecond)
	clk.waitAsked(t)
	if got := lands.started() - n; got != 0 {
		t.Fatalf("%d lands started %v after the attempt ended, want none before %v", got, d-time.Millisecond, d)
	}
	clk.advance(time.Millisecond)
	clk.waitAsked(t)
	if got := lands.started() - n; got != 1 {
		t.Fatalf("%d lands started %v after the attempt ended, want exactly one", got, d)
	}
}

// turnMergeable stores value as the mergeable of the change number of api
// and runs the change's announcement step, as a mergeability check does.
func (w *world) turnMergeable(t *testing.T, number int64, value string) {
	t.Helper()
	w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
		if err := w.changes.SetMergeable(ctx, tx, "api", number, value, nil); err != nil {
			return err
		}
		return w.changes.AnnounceLandable(ctx, tx, "api", number)
	})
}

// readyCount counts the announcements that the change number of api is
// landable.
func (w *world) readyCount(t *testing.T, number int64) (n int) {
	t.Helper()
	for _, e := range w.eventsOf(t, change.EventReady) {
		if e.ResourceID == strconv.FormatInt(number, 10) {
			n++
		}
	}
	return n
}

func TestAutoLand_KeepsTheFlagAndWaitsForTheChangeToBeLandableAgainAfterAConflictOrABlock(t *testing.T) {
	t.Parallel()
	t.Run("a conflict with a change that landed first on the same line", func(t *testing.T) {
		t.Parallel()
		clk := newClock()
		w := retryWorld(t, func() *clock { return clk }, nil)
		ch := w.openWith(t, "late", map[string]string{"a.txt": lines("late")}, true, w.caller())
		w.mergeable(t, ch.Number, change.MergeableClean)
		// main moves under the change's edit, unseen by its mergeability.
		commit(t, w.api, "main", w.base, "main", map[string]string{"a.txt": lines("main")})

		clk.advance(time.Second)
		clk.waitAsked(t)

		until(t, ch.Ref+"'s automatic land ends conflict", func() bool {
			lands := w.lands(t, ch.Number)
			return len(lands) == 1 && lands[0]["status"] == land.StatusConflict
		})
		if !w.readChange(t, ch.Number).AutoLand {
			t.Errorf("auto_land of %s is false after the conflict, want true", ch.Ref)
		}
		clk.advance(time.Hour)
		clk.waitAsked(t)
		if lands := w.lands(t, ch.Number); len(lands) != 1 {
			t.Fatalf("%s has %d land attempts an hour after the conflict, want 1: %v", ch.Ref, len(lands), lands)
		}

		resolved := commit(t, w.api, ch.Source.Branch, head(t, w.api, "main"), "resolved", map[string]string{"a.txt": lines("late")})
		w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
			_, err := w.changes.RecordVersion(ctx, tx, change.VersionInput{Workspace: "api", Branch: ch.Source.Branch, Head: resolved, Kind: change.KindRef})
			return err
		})
		w.mergeable(t, ch.Number, change.MergeableClean)
		clk.advance(time.Second)
		clk.waitAsked(t)

		until(t, ch.Ref+" lands", func() bool { return w.readChange(t, ch.Number).State == change.StateLanded })
		lands := w.lands(t, ch.Number)
		if len(lands) != 2 || lands[0]["trigger"] != land.TriggerAutoLand || lands[1]["trigger"] != land.TriggerAutoLand {
			t.Errorf("%s has land attempts %v, want two with trigger auto_land", ch.Ref, lands)
		}
	})
	t.Run("an attempt blocked", func(t *testing.T) {
		t.Parallel()
		clk, lands := newClock(), &endingLands{ends: []string{land.StatusBlocked}}
		w := retryWorld(t, func() *clock { return clk }, lands)
		ch := firstLand(t, w, clk, lands, "blocked")

		clk.advance(time.Hour)
		clk.waitAsked(t)

		if !w.readChange(t, ch.Number).AutoLand {
			t.Errorf("auto_land of %s is false after the block, want true", ch.Ref)
		}
		if n := lands.started(); n != 1 {
			t.Errorf("the lander started %d lands in the hour after the block, want 1 in all", n)
		}
	})
}

func TestAutoLand_RetriesAFailedAttemptWithADoublingDelayAndThenClearsTheFlag(t *testing.T) {
	t.Parallel()
	clk, lands := newClock(), &endingLands{ends: []string{land.StatusFailed}}
	w := retryWorld(t, func() *clock { return clk }, lands)
	ch := firstLand(t, w, clk, lands, "retried")

	for _, seconds := range []time.Duration{2, 4, 8, 16, 32, 64, 128, 256, 300, 300} {
		retryAfter(t, clk, lands, seconds*time.Second)
	}

	lands.mu.Lock()
	starts, flags := slices.Clone(lands.starts), slices.Clone(lands.flags)
	lands.mu.Unlock()
	if len(starts) != 11 {
		t.Fatalf("the lander started %d lands, want 11", len(starts))
	}
	for i, in := range starts {
		if in.Trigger != land.TriggerAutoLand || in.Number != ch.Number || !flags[i] {
			t.Errorf("start %d is of change %d with trigger %s and auto_land %t, want %d, auto_land, true",
				i+1, in.Number, in.Trigger, flags[i], ch.Number)
		}
	}
	if got := w.readChange(t, ch.Number); got.AutoLand || got.State != change.StateOpen {
		t.Errorf("%s is %s with auto_land %t after the tenth retry failed, want open with auto_land false", ch.Ref, got.State, got.AutoLand)
	}
	clk.advance(time.Hour)
	clk.waitAsked(t)
	if n := lands.started(); n != 11 {
		t.Errorf("the lander started %d lands, want no more than 11 an hour later", n)
	}
}

func TestAutoLand_DropsADueRetryOfAChangeThatIsNotToBeLandedNow(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// drop makes the change ch no longer one to land, and again makes
		// it landable with its flag on, to be started by the next run.
		drop, again func(t *testing.T, w *world, ch change.Change)
	}{
		{name: "the change abandoned",
			drop: func(t *testing.T, w *world, ch change.Change) {
				path := fmt.Sprintf("/api/v1/workspaces/api/changes/%d/abandon", ch.Number)
				if status, raw := w.Call(t, http.MethodPost, path, w.ownerKey, nil); status != http.StatusOK {
					t.Fatalf("abandon %s = %d %s, want 200", ch.Ref, status, raw)
				}
			},
			again: func(t *testing.T, w *world, ch change.Change) {
				ready := w.readyCount(t, ch.Number)
				path := fmt.Sprintf("/api/v1/workspaces/api/changes/%d/restore", ch.Number)
				if status, raw := w.Call(t, http.MethodPost, path, w.ownerKey, nil); status != http.StatusOK {
					t.Fatalf("restore %s = %d %s, want 200", ch.Ref, status, raw)
				}
				until(t, ch.Ref+" is announced landable again", func() bool { return w.readyCount(t, ch.Number) > ready })
			}},
		{name: "the flag set to false by PATCH",
			drop: func(t *testing.T, w *world, ch change.Change) {
				if status, answer := w.patchChange(t, w.ownerKey, ch.Number, map[string]any{"auto_land": false}); status != http.StatusOK {
					t.Fatalf("PATCH auto_land false = %d %v, want 200", status, answer)
				}
			},
			again: func(t *testing.T, w *world, ch change.Change) {
				if status, answer := w.patchChange(t, w.ownerKey, ch.Number, map[string]any{"auto_land": true}); status != http.StatusOK {
					t.Fatalf("PATCH auto_land true = %d %v, want 200", status, answer)
				}
			}},
		{name: "mergeable turned conflict",
			drop: func(t *testing.T, w *world, ch change.Change) {
				w.turnMergeable(t, ch.Number, change.MergeableConflict)
			},
			again: func(t *testing.T, w *world, ch change.Change) { w.turnMergeable(t, ch.Number, change.MergeableClean) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			clk, lands := newClock(), &endingLands{ends: []string{land.StatusFailed}}
			w := retryWorld(t, func() *clock { return clk }, lands)
			ch := firstLand(t, w, clk, lands, "dropped")
			retryAfter(t, clk, lands, 2*time.Second)
			retryAfter(t, clk, lands, 4*time.Second)
			c.drop(t, w, ch)
			flag := w.readChange(t, ch.Number).AutoLand

			// The third failed attempt's retry was due 8 s after it.
			clk.advance(time.Minute)
			clk.waitAsked(t)

			if n := lands.started(); n != 3 {
				t.Fatalf("the lander started %d lands, want none after the third", n-3)
			}
			if got := w.readChange(t, ch.Number).AutoLand; got != flag {
				t.Errorf("auto_land of %s is %t after the retry was dropped, want it kept at %t", ch.Ref, got, flag)
			}
			c.again(t, w, ch)
			clk.advance(time.Second)
			clk.waitAsked(t)
			if n := lands.started(); n != 4 {
				t.Fatalf("the lander started %d lands once %s is landable again, want 1", n-3, ch.Ref)
			}
			retryAfter(t, clk, lands, 2*time.Second)
		})
	}
}

func TestAutoLand_StartsTheRetriesAgainAfterAnAttemptThatDidNotFail(t *testing.T) {
	t.Parallel()
	clk := newClock()
	lands := &endingLands{ends: []string{land.StatusFailed, land.StatusFailed, land.StatusConflict, land.StatusFailed}}
	w := retryWorld(t, func() *clock { return clk }, lands)
	ch := firstLand(t, w, clk, lands, "reset")
	retryAfter(t, clk, lands, 2*time.Second)
	retryAfter(t, clk, lands, 4*time.Second)
	w.turnMergeable(t, ch.Number, change.MergeableConflict)
	w.turnMergeable(t, ch.Number, change.MergeableClean)
	clk.advance(time.Second)
	clk.waitAsked(t)
	if n := lands.started(); n != 4 {
		t.Fatalf("the lander started %d lands once %s is landable again, want 1", n-3, ch.Ref)
	}

	retryAfter(t, clk, lands, 2*time.Second)
}

func TestAutoLand_StartsALandTheWorkspaceRefusedAgainAnIntervalLater(t *testing.T) {
	t.Parallel()
	clk, lands := newClock(), &endingLands{ends: []string{land.StatusFailed}}
	w := retryWorld(t, func() *clock { return clk }, lands)
	lands.busy.Store(true)
	ch := w.openWith(t, "busy", map[string]string{"busy.txt": "x\n"}, true, w.caller())
	w.mergeable(t, ch.Number, change.MergeableClean)
	until(t, ch.Ref+" is announced landable", func() bool { return w.readyCount(t, ch.Number) > 0 })
	clk.advance(time.Second)
	clk.waitAsked(t)
	lands.mu.Lock()
	refused := lands.refused
	lands.mu.Unlock()
	if refused == 0 || lands.started() != 0 {
		t.Fatalf("the busy workspace refused %d starts and %d lands started, want a refusal and none started", refused, lands.started())
	}

	lands.busy.Store(false)
	clk.advance(time.Second)
	clk.waitAsked(t)

	if n := lands.started(); n != 1 {
		t.Fatalf("the lander started %d lands an interval after the refusal, want 1", n)
	}
	// The refusal is no failed attempt: the retry of the one that failed
	// is the first, 2 s after it.
	retryAfter(t, clk, lands, 2*time.Second)
}

// landerRows counts the rows the automatic lander keeps of the change
// number of api.
func (w *world) landerRows(t *testing.T, number int64) (n int) {
	t.Helper()
	if err := w.Store.Main().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM land_auto_land WHERE workspace_slug = 'api' AND number = ?`, number).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAutoLand_ForgetsAChangeOnceItsAutomaticLandLanded(t *testing.T) {
	t.Parallel()
	clk, lands := newClock(), &endingLands{ends: []string{land.StatusLanded}}
	w := retryWorld(t, func() *clock { return clk }, lands)

	ch := firstLand(t, w, clk, lands, "forgotten")

	if n := w.landerRows(t, ch.Number); n != 0 {
		t.Errorf("the automatic lander keeps %d rows of %s once its land landed, want none", n, ch.Ref)
	}
}

func TestAutoLand_MakesAPendingRetryAfterARestartWithItsStoredCount(t *testing.T) {
	t.Parallel()
	clk, lands := newClock(), &endingLands{ends: []string{land.StatusFailed}}
	w := retryWorld(t, func() *clock { return clk }, lands)
	firstLand(t, w, clk, lands, "restarted")
	// The second attempt fails: its retry is due 4 s later.
	retryAfter(t, clk, lands, 2*time.Second)

	// The hub stops, and starts again on the same store 10 s later.
	clk = clk.later(10 * time.Second)
	w.Restart(t)
	clk.waitAsked(t)

	if n := lands.started(); n != 3 {
		t.Fatalf("the lander started %d lands at the restart, want 1", n-2)
	}
	retryAfter(t, clk, lands, 8*time.Second)
}
