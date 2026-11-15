package change_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
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
