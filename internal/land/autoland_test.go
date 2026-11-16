package land_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/land"
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

// until waits until cond holds, at most 30 s.
func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within 30 s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
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
