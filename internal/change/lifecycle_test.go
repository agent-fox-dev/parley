package change_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
)

// abandon sends POST …/abandon to the change number of demo with the
// credential and the body (nil for none), and returns the status and the
// decoded answer.
func (w *world) abandon(t *testing.T, number int64, credential string, body any) (int, map[string]any) {
	t.Helper()
	return w.send(t, http.MethodPost, "/workspaces/demo/changes/"+strconv.FormatInt(number, 10)+"/abandon", credential, body)
}

// stack opens in demo a change from each of the new branches, each linked
// through the change store to the one before, and returns their numbers,
// lowest first.
func (w *world) stack(t *testing.T, branches ...string) []int64 {
	t.Helper()
	var numbers []int64
	for _, b := range branches {
		w.branch(t, b, b+"\n\nWhy.\n")
		numbers = append(numbers, w.openBranch(t, b, nil).Number)
	}
	w.write(t, func(ctx context.Context, tx *sql.Tx) error {
		for i := 1; i < len(numbers); i++ {
			if err := w.changes.SetDependsOn(ctx, tx, "demo", numbers[i], &numbers[i-1]); err != nil {
				return err
			}
		}
		return nil
	})
	return numbers
}

// abandonedChanges are the changes of demo that parley.change.abandon
// events after seq name, in event order.
func (w *world) abandonedChanges(t *testing.T, seq int64) []string {
	t.Helper()
	var out []string
	for _, e := range w.eventsAfter(t, seq) {
		if e.Type == change.EventAbandon {
			out = append(out, e.ResourceID)
		}
	}
	return out
}

func TestAbandon_AbandonsTheChangeAndKeepsItsBranchAndVersions(t *testing.T) {
	t.Parallel()
	// Checks are spaced by an hour, so that the check a row schedules
	// waits until the abandon drops it.
	m := newMiaWorldWith(t, hubtest.Config{TOML: "[changes]\nmergeable_debounce = \"1h\"\n"})
	m.branch(t, "open", "Open\n\nWhy.\n")
	m.openBranch(t, "open", nil)
	m.branch(t, "draft", "Draft\n\nWhy.\n")
	m.openBranch(t, "draft", func(in *change.OpenInput) { in.Draft = true })
	m.openAsMia(t, "task")
	m.branch(t, "lone", "Lone\n\nWhy.\n")
	m.openBranch(t, "lone", nil)
	for _, c := range []struct {
		name              string
		number            int64
		dir, branch       string
		credential, actor string
		body              any
		reason            string
	}{
		{"an open change, no body", 1, m.demo, "open", m.ownerKey, m.owner, nil, ""},
		{"a draft, an empty object", 2, m.demo, "draft", m.ownerKey, m.owner, map[string]any{}, ""},
		{"the author's change from a fork, with a reason", 3, m.t1, "task", m.miaKey, m.mia, map[string]any{"reason": "superseded"}, "superseded"},
		{"a change without dependants told to abandon them", 4, m.demo, "lone", m.ownerKey, m.owner, map[string]any{"reason": "x", "dependants": "abandon"}, "x"},
	} {
		t.Run(c.name, func(t *testing.T) {
			head := gitrepotest.Refs(t, c.dir, "refs/heads/")[c.branch]
			m.trigger(t, c.number)
			seq := m.seq(t)
			status, out := m.abandon(t, c.number, c.credential, c.body)
			if status != http.StatusOK || out["state"] != change.StateAbandoned || out["number"] != float64(c.number) {
				t.Fatalf("abandon = %d %v, want 200 with the change abandoned", status, out)
			}
			if got := gitrepotest.Refs(t, c.dir, "refs/heads/")[c.branch]; got != head {
				t.Errorf("the source branch is at %q, was %s", got, head)
			}
			if got := gitrepotest.Refs(t, m.demo, "refs/parley/changes/")[strconv.FormatInt(c.number, 10)+"/1"]; got != head {
				t.Errorf("the version ref is at %q, want %s", got, head)
			}
			if !change.MergeableIdle(m.changes, "demo", c.number) {
				t.Error("the change's mergeability check still waits")
			}
			events := m.changeEvents(t, seq, change.EventAbandon, c.number)
			if len(events) != 1 {
				t.Fatalf("abandon events = %+v, want one", events)
			}
			want := map[string]any{"number": float64(c.number), "ref": change.Ref("demo", c.number), "reason": c.reason,
				"version": float64(1), "head_sha": head}
			if e := events[0]; e.ActorID == nil || *e.ActorID != c.actor || !reflect.DeepEqual(metadata(t, e), want) {
				t.Errorf("the event by %v has %v, want by %s with %v", e.ActorID, metadata(t, e), c.actor, want)
			}
		})
	}
}

func TestAbandon_RefusesAChangeWithDependantsUnlessTheBodySaysWhatBecomesOfThem(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.stack(t, "s1", "s2", "s3")
	for _, body := range []any{nil, map[string]any{}, map[string]any{"reason": "drop"}} {
		seq := w.seq(t)
		if status, out := w.abandon(t, 1, w.ownerKey, body); status != http.StatusConflict || errorType(out) != "has_dependants" {
			t.Errorf("abandon with %v = %d %v, want 409 has_dependants", body, status, out)
		}
		one, two := w.read(t, 1), w.read(t, 2)
		if one.State != change.StateOpen || two.State != change.StateOpen || two.DependsOn == nil || *two.DependsOn != 1 {
			t.Errorf("after %v: demo#1 is %s, demo#2 %s on %v; want both open, demo#2 on 1", body, one.State, two.State, two.DependsOn)
		}
		if got := w.abandonedChanges(t, seq); len(got) != 0 {
			t.Errorf("the refused abandon wrote abandon events for %v", got)
		}
	}
}

func TestAbandon_AbandonsTheWholeStackAboveTheChange(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.stack(t, "s1", "s2", "s3")
	heads := gitrepotest.Refs(t, w.demo, "refs/heads/")
	seq := w.seq(t)
	status, out := w.abandon(t, 1, w.ownerKey, map[string]any{"reason": "drop", "dependants": "abandon"})
	if status != http.StatusOK || out["number"] != float64(1) || out["state"] != change.StateAbandoned {
		t.Fatalf("abandon = %d %v, want 200 with demo#1 abandoned", status, out)
	}
	for _, n := range []int64{2, 3} {
		if ch := w.read(t, n); ch.State != change.StateAbandoned {
			t.Errorf("demo#%d is %s, want abandoned", n, ch.State)
		}
	}
	var ids []string
	for _, e := range w.eventsAfter(t, seq) {
		if e.Type != change.EventAbandon {
			continue
		}
		ids = append(ids, e.ResourceID)
		if metadata(t, e)["reason"] != "drop" || e.ActorID == nil || *e.ActorID != w.owner {
			t.Errorf("the event of demo#%s is by %v with %v, want by the owner for drop", e.ResourceID, e.ActorID, metadata(t, e))
		}
	}
	if slices.Sort(ids); !slices.Equal(ids, []string{"1", "2", "3"}) {
		t.Errorf("abandon events of %v, want one each of 1, 2 and 3", ids)
	}
	if after := gitrepotest.Refs(t, w.demo, "refs/heads/"); after["s1"] != heads["s1"] || after["s2"] != heads["s2"] || after["s3"] != heads["s3"] {
		t.Errorf("the source branches are %v, were %v", after, heads)
	}
}

// restacks is a restacker that records what it is asked, marks the change
// as needing a restack in the transaction it is given, and fails while
// fail is set.
type restacks struct {
	changes *change.Component
	fail    bool

	mu    sync.Mutex
	calls []restackCall
}

// restackCall is one request: the change, its depends_on then, and the
// trigger.
type restackCall struct {
	number, dependsOn int64
	trigger           string
}

func (r *restacks) RequestRestack(ctx context.Context, tx *sql.Tx, ch change.Change, trigger string) error {
	r.mu.Lock()
	call := restackCall{number: ch.Number, trigger: trigger}
	if ch.DependsOn != nil {
		call.dependsOn = *ch.DependsOn
	}
	r.calls = append(r.calls, call)
	r.mu.Unlock()
	if r.fail {
		return errors.New("the restack failed")
	}
	return r.changes.SetNeedsRestack(ctx, tx, ch.WorkspaceSlug, ch.Number, true, &trigger)
}

func (r *restacks) told() []restackCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

func TestAbandon_ReparentsTheDirectDependants(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	t.Run("with a restacker", func(t *testing.T) {
		w.stack(t, "s1", "s2", "s3")
		seq := w.seq(t)
		w.changes.RegisterRestacker(&restacks{changes: w.changes, fail: true})
		if status, out := w.abandon(t, 2, w.ownerKey, map[string]any{"dependants": "reparent"}); status != http.StatusInternalServerError {
			t.Errorf("abandon with a failing restacker = %d %v, want 500", status, out)
		}
		if two, three := w.read(t, 2), w.read(t, 3); two.State != change.StateOpen || three.DependsOn == nil || *three.DependsOn != 2 {
			t.Errorf("after the failed restack demo#2 is %s and demo#3 on %v, want open and on 2", two.State, three.DependsOn)
		}
		rec := &restacks{changes: w.changes}
		w.changes.RegisterRestacker(rec)
		status, out := w.abandon(t, 2, w.ownerKey, map[string]any{"dependants": "reparent"})
		if status != http.StatusOK || out["state"] != change.StateAbandoned || !reflect.DeepEqual(out["dependants"], []any{}) {
			t.Fatalf("abandon = %d %v, want 200 abandoned with no dependants", status, out)
		}
		three := w.read(t, 3)
		if three.State != change.StateOpen || three.DependsOn == nil || *three.DependsOn != 1 || !three.NeedsRestack {
			t.Errorf("demo#3 is %s on %v, needs restack %v; want open on 1, as the restacker marked it", three.State, three.DependsOn, three.NeedsRestack)
		}
		if got := w.read(t, 1).Dependants; !slices.Equal(got, []int64{3}) {
			t.Errorf("demo#1's dependants = %v, want [3]", got)
		}
		if got := rec.told(); !reflect.DeepEqual(got, []restackCall{{number: 3, dependsOn: 1, trigger: "abandon"}}) {
			t.Errorf("the restacker was asked %+v, want demo#3, on 1, for abandon", got)
		}
		if got := w.abandonedChanges(t, seq); !slices.Equal(got, []string{"2"}) {
			t.Errorf("abandon events of %v, want [2]", got)
		}
		w.changes.RegisterRestacker(nil)
	})
	t.Run("without a restacker", func(t *testing.T) {
		numbers := w.stack(t, "t1", "t2")
		g := newGate()
		t.Cleanup(g.release)
		change.SetMergeableSeams(w.changes, change.MergeableSeams{Gate: g.pass})
		if status, out := w.abandon(t, numbers[0], w.ownerKey, map[string]any{"dependants": "reparent"}); status != http.StatusOK {
			t.Fatalf("abandon = %d %v, want 200", status, out)
		}
		if ch := w.read(t, numbers[1]); ch.State != change.StateOpen || ch.DependsOn != nil || ch.Mergeable != change.MergeableUnknown {
			t.Errorf("the dependant is %s on %v with mergeable %s, want open on none and unknown", ch.State, ch.DependsOn, ch.Mergeable)
		}
		g.release()
		w.settled(t, numbers[1])
	})
}

func TestAbandon_RefusesAClosedChange(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	_, abandoned, landed := closedChanges(t, w)
	for _, n := range []int64{abandoned, landed} {
		_, before := w.change(t, n)
		seq := w.seq(t)
		if status, out := w.abandon(t, n, w.ownerKey, map[string]any{}); status != http.StatusConflict || errorType(out) != "change_closed" {
			t.Errorf("abandon of demo#%d = %d %v, want 409 change_closed", n, status, out)
		}
		if _, after := w.change(t, n); !bytes.Equal(after, before) {
			t.Errorf("the refused abandon changed demo#%d:\n%s\nwas\n%s", n, after, before)
		}
		if got := w.abandonedChanges(t, seq); len(got) != 0 {
			t.Errorf("the refused abandon wrote abandon events for %v", got)
		}
	}
}

func TestAbandon_RefusesAnInvalidBody(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.branch(t, "open", "Open\n\nWhy.\n")
	n := w.openBranch(t, "open", nil).Number
	for _, body := range []map[string]any{
		{"dependants": "delete"},
		{"dependants": true},
		{"dependants": ""},
		{"reason": 5},
		{"reason": []string{"a"}},
	} {
		if status, out := w.abandon(t, n, w.ownerKey, body); status != http.StatusBadRequest || errorType(out) != "invalid_request" {
			t.Errorf("abandon with %v = %d %v, want 400 invalid_request", body, status, out)
		}
		if ch := w.read(t, n); ch.State != change.StateOpen {
			t.Errorf("after %v the change is %s, want open", body, ch.State)
		}
	}
}

func TestAbandon_KeepsTheWaitingCheckWhenItsTransactionIsUndone(t *testing.T) {
	t.Parallel()
	// Checks are spaced by an hour, so that the check the trigger
	// schedules waits.
	w := newWorldWith(t, hubtest.Config{TOML: "[changes]\nmergeable_debounce = \"1h\"\n"})
	w.branch(t, "b", "B\n\nWhy.\n")
	n := w.openBranch(t, "b", nil).Number
	w.settled(t, n)
	w.trigger(t, n)
	ctx := context.Background()
	err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		if err := w.changes.Abandon(ctx, tx, "demo", n, "undone", change.Caller{}); err != nil {
			return err
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatal(err)
	}
	if ch := w.read(t, n); ch.State != change.StateOpen || ch.Mergeable != change.MergeableUnknown {
		t.Fatalf("after the undone abandon the change is %s and %s, want open and unknown", ch.State, ch.Mergeable)
	}
	if change.MergeableIdle(w.changes, "demo", n) {
		t.Error("the undone abandon dropped the change's waiting check")
	}
}

// restore sends POST …/restore to the change number of demo with the
// credential, and returns the status and the decoded answer.
func (w *world) restore(t *testing.T, number int64, credential string) (int, map[string]any) {
	t.Helper()
	return w.send(t, http.MethodPost, "/workspaces/demo/changes/"+strconv.FormatInt(number, 10)+"/restore", credential, nil)
}

// abandonedTask opens in demo, by mia, a change from the new branch of
// demo--t1, a draft when asked, records its version 2, abandons it, and
// returns its number and the head of version 2.
func (m *miaWorld) abandonedTask(t *testing.T, branch string, draft bool) (int64, string) {
	t.Helper()
	mia := m.caller()
	one := commit(t, m.t1, branch, m.base, branch+"\n\nWhy.\n", map[string]string{branch + ".txt": "1\n"})
	n := m.openBranch(t, branch, func(in *change.OpenInput) {
		in.SourceWorkspace, in.Caller, in.Draft = "demo--t1", mia, draft
	}).Number
	head := commit(t, m.t1, branch, one, "Two\n", map[string]string{branch + ".txt": "2\n"})
	m.mustRecord(t, change.VersionInput{Workspace: "demo--t1", Branch: branch, Head: head, Kind: change.KindPush, Caller: mia})
	if status, out := m.abandon(t, n, m.miaKey, nil); status != http.StatusOK {
		t.Fatalf("abandon demo#%d = %d %v", n, status, out)
	}
	return n, head
}

func TestRestore_ReopensTheChangeAtTheHeadOfItsSourceBranch(t *testing.T) {
	t.Parallel()
	m := newMiaWorld(t)
	restored := []string{change.EventRestore}
	for i, c := range []struct {
		name  string
		draft bool
		// move changes the source branch, at last after the abandon, and
		// returns the head it is to have after the restore.
		move    func(t *testing.T, branch, last string) string
		version int64
		events  []string
	}{
		{"its branch at the last version", false, nil, 2, restored},
		{"a draft when it was abandoned", true, nil, 2, restored},
		{"its branch deleted", false, func(t *testing.T, branch, last string) string {
			gitrepotest.Git(t, m.t1, "update-ref", "-d", "refs/heads/"+branch)
			return last
		}, 2, restored},
		{"its branch moved on", false, func(t *testing.T, branch, last string) string {
			return commit(t, m.t1, branch, last, "Three\n", map[string]string{branch + ".txt": "3\n"})
		}, 3, []string{change.EventRestore, change.EventVersion}},
	} {
		t.Run(c.name, func(t *testing.T) {
			branch := "r" + strconv.Itoa(i)
			n, last := m.abandonedTask(t, branch, c.draft)
			head := last
			if c.move != nil {
				head = c.move(t, branch, last)
			}
			// A fork activity recorder that fails changes nothing.
			rec := &activity{fail: true}
			m.changes.RegisterForkActivity(rec)
			g := newGate()
			t.Cleanup(g.release)
			change.SetMergeableSeams(m.changes, change.MergeableSeams{Gate: g.pass})
			seq := m.seq(t)
			status, out := m.restore(t, n, m.miaKey)
			landing, _ := out["landing"].(map[string]any)
			if status != http.StatusOK || out["state"] != change.StateOpen || out["version"] != float64(c.version) ||
				out["head_sha"] != head || out["mergeable"] != change.MergeableUnknown || landing["status"] != change.LandingReady {
				t.Fatalf("restore = %d %v, want 200 open at version %d, %s, mergeable unknown and landing ready", status, out, c.version, head)
			}
			if got := gitrepotest.Refs(t, m.t1, "refs/heads/")[branch]; got != head {
				t.Errorf("the source branch is at %q, want %s", got, head)
			}
			var types []string
			for _, e := range m.eventsAfter(t, seq) {
				types = append(types, e.Type)
			}
			if !slices.Equal(types, c.events) {
				t.Errorf("events after the restore = %v, want %v", types, c.events)
			}
			events := m.changeEvents(t, seq, change.EventRestore, n)
			want := map[string]any{"number": float64(n), "ref": change.Ref("demo", n), "version": float64(2), "head_sha": last}
			if len(events) != 1 || events[0].ActorID == nil || *events[0].ActorID != m.mia || !reflect.DeepEqual(metadata(t, events[0]), want) {
				t.Errorf("restore events = %+v, want one by mia with %v", events, want)
			}
			versions := m.versions(t, "demo", n)
			if len(versions) != int(c.version) {
				t.Fatalf("%d versions, want %d", len(versions), c.version)
			}
			if c.version == 3 {
				var credential string
				if err := m.Store.Main().QueryRow(`SELECT credential_type FROM change_versions WHERE workspace_slug = 'demo' AND number = ? AND version = 3`, n).
					Scan(&credential); err != nil {
					t.Fatal(err)
				}
				v := versions[2]
				if by, _ := v["created_by"].(map[string]any); v["kind"] != change.KindRef || v["head_sha"] != head || by["actor_id"] != m.mia || credential != change.CredentialAPIKey {
					t.Errorf("version 3 = %v with credential %q, want of kind ref at %s by mia with her API key", v, credential, head)
				}
			}
			if got := rec.told(); !slices.Equal(got, []string{"demo--t1"}) {
				t.Errorf("the fork activity recorder was told %v, want [demo--t1]", got)
			}
			g.release()
			if ch := m.settled(t, n); ch.Mergeable != change.MergeableClean {
				t.Errorf("mergeable = %s after the check, want clean", ch.Mergeable)
			}
		})
	}
}

func TestRestore_RefusesAChangeThatIsNotAbandonedFirst(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	open, _, landed := closedChanges(t, w)
	w.branch(t, "draft", "Draft\n\nWhy.\n")
	draft := w.openBranch(t, "draft", func(in *change.OpenInput) { in.Draft = true }).Number
	for _, n := range []int64{draft, open, landed} {
		_, before := w.change(t, n)
		seq := w.seq(t)
		// An open or draft change has its own source, which the later
		// check of another change with the source would refuse.
		if status, out := w.restore(t, n, w.ownerKey); status != http.StatusConflict || errorType(out) != "change_not_abandoned" {
			t.Errorf("restore of demo#%d = %d %v, want 409 change_not_abandoned", n, status, out)
		}
		if _, after := w.change(t, n); !bytes.Equal(after, before) {
			t.Errorf("the refused restore changed demo#%d:\n%s\nwas\n%s", n, after, before)
		}
		if events := w.eventsAfter(t, seq); len(events) != 0 {
			t.Errorf("the refused restore wrote %+v", events)
		}
	}
}

func TestRestore_RefusesAChangeWhoseForkIsGoneAndKeepsItsVersions(t *testing.T) {
	t.Parallel()
	m := newMiaWorld(t)
	n, _ := m.abandonedTask(t, "task", false)
	if status, out := m.send(t, http.MethodDelete, "/workspaces/demo--t1", m.Token, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE demo--t1 = %d %v, want 204", status, out)
	}
	if status, out := m.restore(t, n, m.ownerKey); status != http.StatusConflict || errorType(out) != "source_workspace_gone" {
		t.Errorf("restore = %d %v, want 409 source_workspace_gone", status, out)
	}
	if ch := m.read(t, n); ch.State != change.StateAbandoned {
		t.Errorf("the change is %s, want abandoned", ch.State)
	}
	refs := gitrepotest.Refs(t, m.demo, "refs/parley/changes/")
	for _, v := range m.versions(t, "demo", n) {
		if ref := strconv.FormatInt(n, 10) + "/" + strconv.FormatFloat(v["number"].(float64), 'f', 0, 64); refs[ref] != v["head_sha"] {
			t.Errorf("refs/parley/changes/%s is at %q, want %v", ref, refs[ref], v["head_sha"])
		}
	}
}

func TestRestore_RefusesWhileAnotherChangeHasItsSource(t *testing.T) {
	t.Parallel()
	m := newMiaWorld(t)
	first, _ := m.abandonedTask(t, "feat", false)
	second := m.openBranch(t, "feat", func(in *change.OpenInput) {
		in.SourceWorkspace, in.Caller = "demo--t1", m.caller()
	}).Number
	status, out := m.restore(t, first, m.miaKey)
	if msg, _ := out["error"].(map[string]any)["message"].(string); status != http.StatusConflict || errorType(out) != "change_exists" ||
		!strings.Contains(msg, change.Ref("demo", second)) {
		t.Errorf("restore = %d %v, want 409 change_exists naming demo#%d", status, out, second)
	}
	if one, two := m.read(t, first), m.read(t, second); one.State != change.StateAbandoned || two.State != change.StateOpen {
		t.Errorf("demo#%d is %s and demo#%d %s, want abandoned and open", first, one.State, second, two.State)
	}
	t.Run("two restores at once", func(t *testing.T) {
		if status, out := m.abandon(t, second, m.ownerKey, nil); status != http.StatusOK {
			t.Fatalf("abandon = %d %v", status, out)
		}
		var wg sync.WaitGroup
		answers := make([]string, 2)
		for i, n := range []int64{first, second} {
			wg.Go(func() {
				status, out := m.restore(t, n, m.ownerKey)
				answers[i] = strconv.Itoa(status) + " " + fmt.Sprint(errorType(out))
			})
		}
		wg.Wait()
		if slices.Sort(answers); !slices.Equal(answers, []string{"200 <nil>", "409 change_exists"}) {
			t.Errorf("the restores answered %v, want one 200 and one 409 change_exists", answers)
		}
	})
}

func TestRestore_RefusesWhenTheBranchAndTheLastVersionAreGone(t *testing.T) {
	t.Parallel()
	m := newMiaWorld(t)
	n, head := m.abandonedTask(t, "task", false)
	gitrepotest.Git(t, m.t1, "update-ref", "-d", "refs/heads/task")
	// The fork reads the parent's objects, so both lose the commit, as
	// after a reclone.
	for _, dir := range []string{m.t1, m.demo} {
		for ref := range gitrepotest.Refs(t, dir, "refs/parley/") {
			gitrepotest.Git(t, dir, "update-ref", "-d", "refs/parley/"+ref)
		}
		gitrepotest.Git(t, dir, "reflog", "expire", "--expire=now", "--all")
		gitrepotest.Git(t, dir, "gc", "--quiet", "--prune=now")
	}
	if _, err := gitrepotest.Run(m.t1, "", "cat-file", "-e", head); err == nil {
		t.Fatalf("the fork's repository still has %s", head)
	}
	if status, out := m.restore(t, n, m.miaKey); status != http.StatusConflict || errorType(out) != "conflict" {
		t.Errorf("restore = %d %v, want 409 conflict", status, out)
	}
	if ch := m.read(t, n); ch.State != change.StateAbandoned {
		t.Errorf("the change is %s, want abandoned", ch.State)
	}
	if got, found := gitrepotest.Refs(t, m.t1, "refs/heads/")["task"]; found {
		t.Errorf("the source branch was created at %s", got)
	}
}

func TestRestore_RefusesAChangeWithoutAnIntentLastOfAllChecks(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	head := w.branch(t, "quiet", "Quiet\n")
	n := w.openBranch(t, "quiet", func(in *change.OpenInput) { in.Draft, in.Intent = true, new("") }).Number
	if status, out := w.abandon(t, n, w.ownerKey, nil); status != http.StatusOK {
		t.Fatalf("abandon = %d %v", status, out)
	}
	gitrepotest.Git(t, w.demo, "update-ref", "-d", "refs/heads/quiet")
	if status, out := w.restore(t, n, w.ownerKey); status != http.StatusBadRequest || errorType(out) != "intent_required" {
		t.Errorf("restore = %d %v, want 400 intent_required", status, out)
	}
	if got, found := gitrepotest.Refs(t, w.demo, "refs/heads/")["quiet"]; found {
		t.Errorf("the refused restore created the source branch at %s", got)
	}
	gitrepotest.Git(t, w.demo, "update-ref", "refs/heads/quiet", head)
	other := w.openBranch(t, "quiet", nil).Number
	if status, out := w.restore(t, n, w.ownerKey); status != http.StatusConflict || errorType(out) != "change_exists" {
		t.Errorf("restore while demo#%d has the source = %d %v, want 409 change_exists", other, status, out)
	}
	if ch := w.read(t, n); ch.State != change.StateAbandoned {
		t.Errorf("the change is %s, want abandoned", ch.State)
	}
	if status, out := w.abandon(t, other, w.ownerKey, nil); status != http.StatusOK {
		t.Fatalf("abandon demo#%d = %d %v", other, status, out)
	}
	w.set(t, "demo", "settings", `{"changes.require_intent":false}`)
	if status, out := w.restore(t, n, w.ownerKey); status != http.StatusOK || out["state"] != change.StateOpen {
		t.Errorf("restore without the setting = %d %v, want 200 open", status, out)
	}
}

func TestRestore_ChecksTheChangesIntoTheSourceBranchItCreates(t *testing.T) {
	t.Parallel()
	w := newWorldWith(t, hubtest.Config{TOML: zeroDebounce})
	lowerHead := w.branch(t, "lower", "Lower\n\nWhy.\n")
	lower := w.openBranch(t, "lower", nil).Number
	commit(t, w.demo, "upper", lowerHead, "Upper\n\nWhy.\n", map[string]string{"u.txt": "u\n"})
	upper := w.openBranch(t, "upper", func(in *change.OpenInput) { in.TargetBranch = new("lower") }).Number
	w.settled(t, upper)
	w.push(t, "lower", "", nil)
	w.idle(t, upper)
	if a, b := w.read(t, lower), w.read(t, upper); a.State != change.StateAbandoned || b.Mergeable != change.MergeableUnknown {
		t.Fatalf("with lower deleted demo#%d is %s and demo#%d %s, want abandoned and unknown", lower, a.State, upper, b.Mergeable)
	}
	if status, out := w.restore(t, lower, w.ownerKey); status != http.StatusOK {
		t.Fatalf("restore = %d %v, want 200", status, out)
	}
	w.idle(t, upper)
	if ch := w.read(t, upper); ch.Mergeable != change.MergeableClean || ch.Landing.Status != change.LandingReady {
		t.Errorf("after the restore demo#%d is %s with landing %+v, want clean and ready", upper, ch.Mergeable, ch.Landing)
	}
}

// racingBranch reads the workspace records through WorkspaceReader and,
// once armed, creates the branch of dir at head the first time demo is
// read after demo--t1: a restore of a change of demo from demo--t1 without
// an intent reads demo's setting after it found the source branch gone and
// before it creates the branch.
type racingBranch struct {
	change.WorkspaceReader
	mu                sync.Mutex
	dir, branch, head string
	sourceRead        bool
	err               error
}

func (r *racingBranch) arm(dir, branch, head string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dir, r.branch, r.head = dir, branch, head
}

func (r *racingBranch) Workspace(ctx context.Context, q change.Querier, slug string) (change.Workspace, bool, error) {
	r.mu.Lock()
	switch {
	case r.head == "":
	case slug == "demo--t1":
		r.sourceRead = true
	case slug == "demo" && r.sourceRead:
		_, r.err = gitrepotest.Run(r.dir, "", "update-ref", "refs/heads/"+r.branch, r.head)
		r.head = ""
	}
	r.mu.Unlock()
	return r.WorkspaceReader.Workspace(ctx, q, slug)
}

func TestRestore_TakesASourceBranchCreatedWhileItRuns(t *testing.T) {
	t.Parallel()
	race := &racingBranch{}
	m := newMiaWorldWith(t, hubtest.Config{Compose: func(c *hub.Composition) {
		c.AdjustChange(func(d *change.Deps) { race.WorkspaceReader, d.Workspaces = d.Workspaces, race })
	}})
	m.set(t, "demo", "settings", `{"changes.require_intent":false}`)
	last := commit(t, m.t1, "quiet", m.base, "Quiet\n", map[string]string{"q.txt": "q\n"})
	n := m.openBranch(t, "quiet", func(in *change.OpenInput) {
		in.SourceWorkspace, in.Intent, in.Caller = "demo--t1", new(""), m.caller()
	}).Number
	if status, out := m.abandon(t, n, m.miaKey, nil); status != http.StatusOK {
		t.Fatalf("abandon = %d %v", status, out)
	}
	gitrepotest.Git(t, m.t1, "update-ref", "-d", "refs/heads/quiet")
	other := commit(t, m.t1, "elsewhere", last, "Elsewhere\n", map[string]string{"q.txt": "e\n"})
	m.quiet(t)
	race.arm(m.t1, "quiet", other)
	status, out := m.restore(t, n, m.miaKey)
	race.mu.Lock()
	created, err := race.head == "", race.err
	race.mu.Unlock()
	if !created || err != nil {
		t.Fatalf("the branch was not created during the restore (created %v): %v", created, err)
	}
	if status != http.StatusOK || out["state"] != change.StateOpen || out["head_sha"] != other || out["version"] != 2.0 {
		t.Errorf("restore = %d %v, want 200 open at version 2, %s", status, out, other)
	}
	if got := gitrepotest.Refs(t, m.t1, "refs/heads/")["quiet"]; got != other {
		t.Errorf("the source branch is at %q, want %s as it was created", got, other)
	}
}
