package land_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/land"
)

var stacked = map[string]any{"stack": true}

func TestLand_LandsAWholeStackBottomFirstWithOneMoveOfTheTarget(t *testing.T) {
	t.Parallel()
	for _, strategy := range []string{land.StrategyRebase, land.StrategySquash} {
		t.Run(strategy, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.configure(t, `{"changes.auto_restack":false,"land.strategy":"`+strategy+`"}`)
			s := w.stack(t, "s", false, nil, nil, nil)
			moved := commit(t, w.api, "main", w.base, "M", map[string]string{"m.txt": "m\n"})

			status, a := w.landCall(t, w.ownerKey, s[2].Number, "", stacked)

			if status != 200 || a["status"] != land.StatusLanded || !reflect.DeepEqual(a["changes"], numbers(s)) ||
				a["attempts"] != float64(1) {
				t.Fatalf("land = %d %v, want 200 landed of %v after 1 try", status, a, numbers(s))
			}
			// Each change's one commit, bottom first, on top of M.
			got := strings.Fields(gitrepotest.Git(t, w.api, "rev-list", "--reverse", moved+"..main"))
			commits, _ := a["commits"].([]any)
			if len(got) != 3 || len(commits) != 3 || a["landed_sha"] != got[2] {
				t.Fatalf("main gained %v, commits %v, landed_sha %v; want three commits, the last landed", got, commits, a["landed_sha"])
			}
			for i, ch := range s {
				want := map[string]any{"change": float64(ch.Number), "version_sha": ch.HeadSHA, "landed_sha": got[i]}
				if !reflect.DeepEqual(commits[i], any(want)) {
					t.Errorf("commits[%d] = %v, want %v", i, commits[i], want)
				}
				files := strings.Fields(gitrepotest.Git(t, w.api, "diff", "--name-only", got[i]+"^", got[i]))
				message := gitrepotest.Git(t, w.api, "log", "-1", "--format=%B", got[i])
				squashed := strings.HasSuffix(message, "\nParley-Change: "+ch.Ref)
				if !slices.Equal(files, []string{ch.Source.Branch + ".txt"}) || squashed != (strategy == land.StrategySquash) {
					t.Errorf("main's commit %d changes %v with message %q, want %s.txt, squashed %t", i, files, message,
						ch.Source.Branch, strategy == land.StrategySquash)
				}
				if l := w.readChange(t, ch.Number).Landed; l == nil || l.LandID != a["id"] || l.SHA != a["landed_sha"] {
					t.Errorf("%s landed %+v, want by %v at %v", ch.Ref, l, a["id"], a["landed_sha"])
				}
			}
			var landedOn []string
			for _, e := range w.landEvents(t, land.EventLand, a["id"].(string)) {
				landedOn = append(landedOn, e.ResourceID)
			}
			want := []string{strconv.FormatInt(s[0].Number, 10), strconv.FormatInt(s[1].Number, 10), strconv.FormatInt(s[2].Number, 10)}
			if !slices.Equal(landedOn, want) || len(w.landPushes(t)) != 1 {
				t.Errorf("land events on %v and %d pushes via land, want on %v and one", landedOn, len(w.landPushes(t)), want)
			}
		})
	}
}

func TestLand_LeavesTheLandedChangesBelowOutOfAStack(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	s := w.stack(t, "s", false, nil, nil, nil)
	status, first := w.landCall(t, w.ownerKey, s[0].Number, "", nil)
	if status != 200 {
		t.Fatalf("land of %s = %d %v, want 200", s[0].Ref, status, first)
	}
	// The change above keeps its link to the landed change.
	w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
		return w.changes.SetDependsOn(ctx, tx, "api", s[1].Number, &s[0].Number)
	})

	status, a := w.landCall(t, w.ownerKey, s[2].Number, "", stacked)

	if status != 200 || !reflect.DeepEqual(a["changes"], numbers(s[1:])) {
		t.Fatalf("land of the stack = %d %v, want 200 of %v", status, a, numbers(s[1:]))
	}
	if l := w.readChange(t, s[0].Number).Landed; l == nil || l.LandID != first["id"] {
		t.Errorf("%s landed %+v, want still by %v", s[0].Ref, l, first["id"])
	}
	var lands int
	for _, e := range w.eventsOf(t, land.EventLand) {
		if e.ResourceID == strconv.FormatInt(s[0].Number, 10) {
			lands++
		}
	}
	if lands != 1 {
		t.Errorf("%d land events on %s, want 1", lands, s[0].Ref)
	}
}

func TestLand_RefusesAStackWhoseChangeBelowCannotLand(t *testing.T) {
	t.Parallel()
	w := newWorld(t, func(d *land.Deps) { d.Paused = true })
	for i, c := range []struct {
		name  string
		draft bool
		// arrange puts the bottom change of the stack in the state the
		// row wants, and returns the running attempt the land must name.
		arrange func(t *testing.T, bottom change.Change) string
		want    string
	}{
		{name: "the change below is a draft", draft: true, want: "change_not_open"},
		{name: "the change below is abandoned", want: "change_not_open",
			arrange: func(t *testing.T, bottom change.Change) string {
				w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
					return w.changes.SetAbandoned(ctx, tx, "api", bottom.Number, "dropped")
				})
				return ""
			}},
		{name: "a running attempt covers the change below", want: "land_in_progress",
			arrange: func(t *testing.T, bottom change.Change) string {
				status, a := w.landCall(t, w.ownerKey, bottom.Number, "wait=0", nil)
				if status != 202 {
					t.Fatalf("land of %s = %d %v, want 202", bottom.Ref, status, a)
				}
				return a["id"].(string)
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := w.stack(t, "r"+strconv.Itoa(i)+"-", c.draft, nil, nil)
			var runs string
			if c.arrange != nil {
				runs = c.arrange(t, s[0])
			}
			before, main := w.attempts(t), head(t, w.api, "main")

			status, r := w.landCall(t, w.ownerKey, s[1].Number, "wait=0", stacked)

			if status != 409 || errorType(r) != c.want {
				t.Fatalf("land of the stack = %d %v, want 409 %s", status, r, c.want)
			}
			if runs != "" {
				d, _ := r["error"].(map[string]any)["details"].(map[string]any)
				if changes, _ := d["changes"].([]any); d["id"] != runs || !slices.Contains(changes, any(float64(s[0].Number))) {
					t.Errorf("details %v, want the running attempt %s covering %d", d, runs, s[0].Number)
				}
			}
			if n, got := w.attempts(t), head(t, w.api, "main"); n != before || got != main {
				t.Errorf("the refused land made %d attempts and moved main to %s from %s", n-before, got, main)
			}
		})
	}

	s := w.stack(t, "whole", false, nil, nil)
	if status, a := w.landCall(t, w.ownerKey, s[1].Number, "wait=0", stacked); status != 202 ||
		!reflect.DeepEqual(a["changes"], numbers(s)) {
		t.Fatalf("land of the stack = %d %v, want 202 covering %v", status, a, numbers(s))
	}
	if status, r := w.landCall(t, w.ownerKey, s[0].Number, "wait=0", nil); status != 409 || errorType(r) != "land_in_progress" {
		t.Errorf("land of %s alone = %d %v, want 409 land_in_progress", s[0].Ref, status, r)
	}
}

func TestLand_LandsNothingOfAStackOneOfWhoseChangesStopsIt(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	gate := &stubGate{}
	w.land.RegisterGate(gate)
	for i, c := range []struct {
		name string
		// middle is what the middle change of the stack sets, arrange what
		// stops it once the stack is open.
		middle    map[string]string
		arrange   func(t *testing.T, middle change.Change)
		errorType string
		// stopped is the member of the attempt that names the change.
		stopped string
	}{
		{name: "a commit that does not apply", middle: map[string]string{"a.txt": "l1\nl2\nmiddle\n"},
			errorType: "land_conflict", stopped: "conflict",
			arrange: func(t *testing.T, middle change.Change) {
				w.mergeable(t, middle.Number, change.MergeableClean)
				commit(t, w.api, "main", head(t, w.api, "main"), "clash", map[string]string{"a.txt": "l1\nl2\nmain\n"})
			}},
		{name: "a gate that is not ready", errorType: "policy_blocked", stopped: "blocked",
			arrange: func(_ *testing.T, middle change.Change) {
				gate.answer(middle.Number, land.GateAnswer{Status: change.LandingPending, Requirements: []json.RawMessage{}})
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := w.stack(t, "x"+strconv.Itoa(i)+"-", false, nil, c.middle, nil)
			c.arrange(t, s[1])
			main := head(t, w.api, "main")

			status, r := w.landCall(t, w.ownerKey, s[2].Number, "", stacked)

			errObj, _ := r["error"].(map[string]any)
			a, _ := errObj["details"].(map[string]any)
			stop, _ := a[c.stopped].(map[string]any)
			if status != 409 || errorType(r) != c.errorType || stop["change"] != float64(s[1].Number) {
				t.Fatalf("land of the stack = %d %v, want 409 %s with %s.change %d", status, r, c.errorType, c.stopped, s[1].Number)
			}
			if c.stopped == "conflict" {
				if stop["commit_sha"] != s[1].HeadSHA || stop["target_sha"] != main {
					t.Errorf("the report names commit %v on %v, want %s's %s on %s", stop["commit_sha"], stop["target_sha"],
						s[1].Ref, s[1].HeadSHA, main)
				}
				if got := w.readChange(t, s[1].Number).Mergeable; got != change.MergeableConflict {
					t.Errorf("%s reads mergeable %s, want conflict", s[1].Ref, got)
				}
			}
			if got := head(t, w.api, "main"); got != main {
				t.Errorf("main moved to %s from %s", got, main)
			}
			for _, ch := range s {
				if got := w.readChange(t, ch.Number).State; got != change.StateOpen {
					t.Errorf("%s is %s, want open", ch.Ref, got)
				}
			}
			id := a["id"].(string)
			fails := w.landEvents(t, land.EventLandFail, id)
			if len(fails) != 1 || fails[0].ResourceID != strconv.FormatInt(s[1].Number, 10) {
				t.Errorf("land_fail events %v, want one on %s", fails, s[1].Ref)
			}
			if lands := w.landEvents(t, land.EventLand, id); len(lands) != 0 {
				t.Errorf("%d land events, want none", len(lands))
			}
		})
	}
}
