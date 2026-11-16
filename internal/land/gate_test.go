package land_test

import (
	"context"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/land"
)

// stubRule is an override rule that lets an answer whose status is one of
// passes pass, and keeps every answer it was asked about.
type stubRule struct {
	passes []string

	mu    sync.Mutex
	calls []land.GateAnswer
	with  []land.Override
}

func (r *stubRule) Passes(_ context.Context, o land.Override, answer land.GateAnswer) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls, r.with = append(r.calls, answer), append(r.with, o)
	return slices.Contains(r.passes, answer.Status), nil
}

func (r *stubRule) asked() ([]land.GateAnswer, []land.Override) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls), slices.Clone(r.with)
}

// overridden is a running attempt of the owner landing the change ch,
// asked with the override reason.
func overridden(w *world, ch change.Change, reason string) land.Attempt {
	a := attemptOf(ch, w.caller(), nil, time.Now().UTC())
	a.RequestedOverride = &land.Override{Reason: reason}
	return a
}

func TestRun_AsksTheGateOnceForEachChangeAndNotAgainOnARetry(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	gate, rule := &stubGate{}, &stubRule{passes: []string{change.LandingPending, change.LandingBlocked}}
	w.land.RegisterGate(gate)
	w.land.RegisterOverrideRule(rule)
	ch := w.open(t, "feature")
	gate.answer(ch.Number, land.GateAnswer{Status: change.LandingReady, Revision: 7})
	land.SetSeams(w.land, land.Seams{BeforeSwap: func(_ context.Context, _ string, try int) {
		if try == 1 {
			commit(t, w.api, "main", head(t, w.api, "main"), "moved", map[string]string{"m.txt": "m\n"})
		}
	}})

	a, err := w.run(t, overridden(w, ch, "r"))

	if err != nil || a.Status != land.StatusLanded || a.Attempts != 2 || a.PolicyRevision == nil || *a.PolicyRevision != 7 {
		t.Fatalf("run = %v, attempt %s after %d tries, revision %v; want landed after 2 by revision 7",
			err, a.Status, a.Attempts, a.PolicyRevision)
	}
	want := land.GateInput{Workspace: "api", Version: ch.Version, Caller: w.caller()}
	calls := gate.asked()
	if len(calls) != 1 || calls[0].Workspace != want.Workspace || calls[0].Change.Number != ch.Number ||
		calls[0].Version != want.Version || !reflect.DeepEqual(calls[0].Caller, want.Caller) {
		t.Errorf("the gate was asked %+v, want once about %s at version %d by %+v", calls, ch.Ref, ch.Version, want.Caller)
	}
	if answers, _ := rule.asked(); len(answers) != 0 || a.Override != nil {
		t.Errorf("the rule was asked %v and the attempt applied %v; want neither for a ready change", answers, a.Override)
	}
	land.SetSeams(w.land, land.Seams{})

	w.land.RegisterGate(nil)
	w.land.RegisterOverrideRule(nil)
	if a, _ := w.run(t, overridden(w, w.open(t, "defaults"), "r")); a.Status != land.StatusLanded ||
		a.PolicyRevision == nil || *a.PolicyRevision != 0 || a.Override != nil {
		t.Errorf("with no gate and no rule the attempt is %s by revision %v with override %v; want landed by 0, none",
			a.Status, a.PolicyRevision, a.Override)
	}

	pending := w.open(t, "pending")
	gate.answer(pending.Number, land.GateAnswer{Status: change.LandingPending})
	w.land.RegisterGate(gate)
	if a, _ := w.run(t, overridden(w, pending, "r")); a.Status != land.StatusBlocked || a.Override != nil {
		t.Errorf("a pending change with no rule ended %s with override %v; want blocked, none", a.Status, a.Override)
	}
}

func TestRun_AsksTheGateAboutTheVersionTheAttemptLands(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	gate := &stubGate{}
	w.land.RegisterGate(gate)
	ch := w.open(t, "feature")
	gate.answer(ch.Number, land.GateAnswer{Status: change.LandingBlocked})
	a := attemptOf(ch, w.caller(), nil, time.Now().UTC())
	newer := w.nextVersion(t, ch)

	if _, err := w.run(t, a); err != nil {
		t.Fatal(err)
	}

	calls := gate.asked()
	if len(calls) != 1 {
		t.Fatalf("the gate was asked %d times, want once", len(calls))
	}
	if in := calls[0]; in.Version != ch.Version || in.Head != ch.HeadSHA || in.Base != *ch.BaseSHA {
		t.Errorf("the gate was asked about version %d at %s on %s, want the landed version %d at %s on %s "+
			"(the change is at version %d at %s)", in.Version, in.Head, in.Base, ch.Version, ch.HeadSHA, *ch.BaseSHA,
			newer.Version, newer.HeadSHA)
	}
}

func TestRun_RecordsTheOverrideOnlyWhenTheRuleLetAChangePass(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	gate := &stubGate{}
	w.land.RegisterGate(gate)
	for i, c := range []struct {
		name   string
		answer land.GateAnswer
		// asked is whether the rule is asked; status and override what
		// the attempt ends with.
		asked    bool
		status   string
		override any
	}{
		{"a pending change the rule lets pass", land.GateAnswer{Status: change.LandingPending, Revision: 3}, true,
			land.StatusLanded, map[string]any{"reason": "hotfix"}},
		{"a ready change", land.GateAnswer{Status: change.LandingReady}, false, land.StatusLanded, nil},
		{"a blocked change the rule does not let pass", land.GateAnswer{Status: change.LandingBlocked}, true,
			land.StatusBlocked, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			rule := &stubRule{passes: []string{change.LandingPending}}
			w.land.RegisterOverrideRule(rule)
			ch := w.open(t, "o"+string(rune('a'+i)))
			gate.answer(ch.Number, c.answer)

			a, _ := w.run(t, overridden(w, ch, "hotfix"))

			answers, with := rule.asked()
			if c.asked && (len(answers) != 1 || !reflect.DeepEqual(answers[0], c.answer) || with[0].Reason != "hotfix") {
				t.Errorf("the rule was asked %+v with %+v, want once with the gate's %+v and hotfix", answers, with, c.answer)
			}
			if !c.asked && len(answers) != 0 {
				t.Errorf("the rule was asked %+v, want never", answers)
			}
			if got := asJSON(t, a); a.Status != c.status || !reflect.DeepEqual(got["override"], c.override) {
				t.Errorf("the attempt is %s with override %v, want %s with %v", a.Status, got["override"], c.status, c.override)
			}
			if c.status != land.StatusLanded {
				return
			}
			if lands := w.landEvents(t, land.EventLand, a.ID); len(lands) != 1 ||
				!reflect.DeepEqual(metadata(t, lands[0])["override"], c.override) {
				t.Errorf("parley.change.land events %d, want one whose override is %v", len(lands), c.override)
			}
		})
	}
}
