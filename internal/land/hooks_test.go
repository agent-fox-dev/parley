package land_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/land"
	"github.com/agent-fox-dev/parley/internal/server"
)

// stubTargetGuard answers every move with answer, or fails with err, and
// keeps what it was asked.
type stubTargetGuard struct {
	mu     sync.Mutex
	answer land.TargetAnswer
	err    error
	asked  []land.TargetInput
}

func (g *stubTargetGuard) set(answer land.TargetAnswer, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.answer, g.err, g.asked = answer, err, nil
}

func (g *stubTargetGuard) calls() []land.TargetInput {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.asked)
}

func (g *stubTargetGuard) Check(_ context.Context, in land.TargetInput) (land.TargetAnswer, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked = append(g.asked, in)
	return g.answer, g.err
}

func TestRun_EndsTheAttemptAsTheTargetGuardSays(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	guard := &stubTargetGuard{}
	w.land.AddTargetGuard(guard)
	// The rows that do not allow come first, so that main is at base for
	// each of them.
	for _, c := range []struct {
		name   string
		answer land.TargetAnswer
		err    error
		// status, error and reason are the attempt's and its land_fail
		// event's; blocked is set for an attempt that ends blocked.
		status, error, reason string
		blocked               bool
	}{
		{name: "refused", answer: land.TargetAnswer{Refuse: "push_patches_to_origin"}, status: land.StatusBlocked,
			reason: "push_patches_to_origin", blocked: true},
		{name: "rejected by origin", answer: land.TargetAnswer{OriginRejected: "pre-receive hook declined"},
			status: land.StatusFailed, error: land.ErrorOriginRejected, reason: land.ErrorOriginRejected},
		{name: "unable to decide", err: errors.New("origin unreachable"), status: land.StatusFailed,
			error: land.ErrorInternal, reason: land.ErrorInternal},
		{name: "allowed", status: land.StatusLanded},
	} {
		t.Run(c.name, func(t *testing.T) {
			guard.set(c.answer, c.err)
			ch := w.open(t, "g-"+c.name[:4])
			before := head(t, w.api, "main")

			a, _ := w.runLand(t, ch, w.caller(), nil)

			if a.Status != c.status || c.error != "" && (a.Error == nil || *a.Error != c.error) {
				t.Fatalf("attempt %s (%v), want %s %s", a.Status, a.Error, c.status, c.error)
			}
			newHead := ch.HeadSHA
			if a.LandedSHA != nil {
				newHead = *a.LandedSHA
			}
			want := []land.TargetInput{{Workspace: "api", Branch: "main", Old: before, New: newHead}}
			if got := guard.calls(); !reflect.DeepEqual(got, want) {
				t.Errorf("guard asked %+v, want %+v", got, want)
			}
			if c.status == land.StatusLanded {
				return
			}
			if got := head(t, w.api, "main"); got != before || w.readChange(t, ch.Number).State != change.StateOpen {
				t.Errorf("main at %s and %s %s, want main unchanged at %s and the change open", got, ch.Ref,
					w.readChange(t, ch.Number).State, before)
			}
			wantBlocked := map[string]any(nil)
			if c.blocked {
				wantBlocked = map[string]any{"change": float64(ch.Number), "status": "blocked", "requirements": []any{}}
			}
			if got, _ := asJSON(t, a)["blocked"].(map[string]any); !reflect.DeepEqual(got, wantBlocked) {
				t.Errorf("blocked = %v, want %v", got, wantBlocked)
			}
			fails := w.landEvents(t, land.EventLandFail, a.ID)
			if len(fails) != 1 || metadata(t, fails[0])["reason"] != c.reason {
				t.Errorf("land_fail events %v, want one with reason %s", fails, c.reason)
			}
		})
	}
}

// scriptedTargetGuard answers its nth question with answers[n], allowing
// every question past the last answer, and keeps what it was asked.
type scriptedTargetGuard struct {
	mu      sync.Mutex
	answers []land.TargetAnswer
	asked   []land.TargetInput
}

func (g *scriptedTargetGuard) Check(_ context.Context, in land.TargetInput) (land.TargetAnswer, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := len(g.asked)
	g.asked = append(g.asked, in)
	if n < len(g.answers) {
		return g.answers[n], nil
	}
	return land.TargetAnswer{}, nil
}

func (g *scriptedTargetGuard) calls() []land.TargetInput {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.asked)
}

// A guard that forwards the head it allows to origin, as carry-patch push
// control does, must be asked about the head a retry rebuilt on a target
// that moved, or the land would swap in a head origin never received.
func TestRun_AsksTheTargetGuardsAgainAboutTheHeadARetryRebuilt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// second is the guard's answer about the rebuilt head.
		second land.TargetAnswer
		status string
		error  string
	}{
		{name: "the rebuilt head is allowed too", status: land.StatusLanded},
		{name: "origin rejects the rebuilt head", second: land.TargetAnswer{OriginRejected: "non-fast-forward"},
			status: land.StatusFailed, error: land.ErrorOriginRejected},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			guard := &scriptedTargetGuard{answers: []land.TargetAnswer{{}, c.second}}
			w.land.AddTargetGuard(guard)
			ch := w.open(t, "feature")
			before := head(t, w.api, "main")
			var moved string
			land.SetSeams(w.land, land.Seams{BeforeSwap: func(_ context.Context, _ string, try int) {
				if try == 1 {
					// Another writer moves the target after the guard
					// allowed the first head and before it is swapped.
					moved = commit(t, w.api, "main", before, "moved", map[string]string{"moved.txt": "m\n"})
				}
			}})

			a := w.landNow(t, ch, w.caller(), nil)

			if a.Status != c.status || a.Attempts != 2 || c.error != "" && (a.Error == nil || *a.Error != c.error) {
				t.Fatalf("attempt %s (%v) after %d tries, want %s %s after 2", a.Status, a.Error, a.Attempts, c.status, c.error)
			}
			asked := guard.calls()
			if len(asked) != 2 {
				t.Fatalf("guard asked %+v, want once about each try's head", asked)
			}
			if asked[0].Old != before || asked[1].Old != moved || asked[1].New == asked[0].New {
				t.Errorf("guard asked %+v, want the first head onto %.7s, then a rebuilt head onto the moved %.7s",
					asked, before, moved)
			}
			if parent := gitrepotest.Git(t, w.api, "rev-parse", asked[1].New+"^"); parent != moved {
				t.Errorf("the head of the second question has parent %.7s, want the moved head %.7s", parent, moved)
			}
			want := moved
			if c.status == land.StatusLanded {
				want = asked[1].New
			}
			if got := head(t, w.api, "main"); got != want {
				t.Errorf("main is at %.7s, want %.7s", got, want)
			}
		})
	}
}

// finals keeps what the final observer was told.
type finals struct {
	mu  sync.Mutex
	got []land.Final
}

func (f *finals) observe(_ context.Context, in land.Final) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, in)
}

func (f *finals) told() []land.Final {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.got)
}

func TestRun_ReportsEveryFinalAttemptOnceToEachObserver(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	seen := &finals{}
	w.land.AddFinalObserver(func(context.Context, land.Final) { panic("observer") })
	w.land.AddFinalObserver(seen.observe)
	gate := &stubGate{}
	w.land.RegisterGate(gate)

	landed := w.open(t, "o-landed")
	commit(t, w.api, "conflict", w.base, "conflict", map[string]string{"a.txt": "l1\nconflict\nl3\n"})
	conflicting := w.openBranch(t, "conflict", w.caller())
	blocked := w.open(t, "o-blocked")
	gate.answer(blocked.Number, land.GateAnswer{Status: change.LandingPending})
	failing := attemptOf(w.open(t, "o-failed"), w.caller(), nil, time.Now().UTC())
	failing.TargetBranch = "gone"

	var ran []land.Attempt
	for _, a := range []land.Attempt{
		attemptOf(landed, w.caller(), nil, time.Now().UTC()),
		attemptOf(conflicting, w.caller(), nil, time.Now().UTC()),
		attemptOf(blocked, w.caller(), nil, time.Now().UTC()),
		failing,
	} {
		if len(ran) == 1 {
			// main moves under the conflicting change's edit of a.txt.
			commit(t, w.api, "main", head(t, w.api, "main"), "main", map[string]string{"a.txt": "l1\nmain\nl3\n"})
		}
		got, _ := w.run(t, a)
		ran = append(ran, got)
	}

	told := seen.told()
	if len(told) != len(ran) {
		t.Fatalf("the observer was told of %d attempts, want %d", len(told), len(ran))
	}
	for i, a := range ran {
		f := told[i]
		want := []string{land.StatusLanded, land.StatusConflict, land.StatusBlocked, land.StatusFailed}[i]
		if a.Status != want || f.Result != want || f.Attempt.ID != a.ID || f.Attempt.Trigger != land.TriggerManual ||
			!slices.Equal(f.Attempt.Numbers(), a.Numbers()) || f.Attempts != a.Attempts || f.Duration < 0 ||
			f.Duration != a.FinishedAt.Sub(a.CreatedAt) {
			t.Errorf("told %s %s of %v after %d tries in %v; want %s (stored %s) of %v after %d in %v", f.Result,
				f.Attempt.ID, f.Attempt.Numbers(), f.Attempts, f.Duration, want, a.Status, a.Numbers(), a.Attempts,
				a.FinishedAt.Sub(a.CreatedAt))
		}
	}
}

// requestGuard answers every request with refusal and keeps what it was
// asked; while it is asked it runs during, when set.
type requestGuard struct {
	refusal error
	during  func()

	mu    sync.Mutex
	asked []land.RequestInput
}

func (g *requestGuard) Check(_ context.Context, in land.RequestInput) error {
	if g.during != nil {
		g.during()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked = append(g.asked, in)
	return g.refusal
}

func (g *requestGuard) calls() []land.RequestInput {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.asked)
}

func TestLand_AsksTheRequestGuardsBeforeThePreconditions(t *testing.T) {
	t.Parallel()
	t.Run("a guard that allows", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		guard := &requestGuard{}
		w.land.AddRequestGuard(guard)
		s := w.stack(t, "s", false, nil, nil)

		status, a := w.landCall(t, w.ownerKey, s[1].Number, "", map[string]any{"stack": true, "override": map[string]any{"reason": "r"}})

		if status != 200 || a["status"] != land.StatusLanded {
			t.Fatalf("land = %d %v, want 200 landed", status, a)
		}
		calls := guard.calls()
		if len(calls) != 1 {
			t.Fatalf("the guard was asked %d times, want once", len(calls))
		}
		in := calls[0]
		var got [][2]int64
		for _, cov := range in.Changes {
			got = append(got, [2]int64{cov.Number, cov.Version})
		}
		want := [][2]int64{{s[0].Number, s[0].Version}, {s[1].Number, s[1].Version}}
		if in.Caller.UserID != w.owner || in.Workspace != "api" || !reflect.DeepEqual(got, want) || in.Override == nil ||
			in.Override.Reason != "r" {
			t.Errorf("the guard was asked by %s about %s %v with %v, want the owner, api, %v and reason r",
				in.Caller.UserID, in.Workspace, got, in.Override, want)
		}
	})
	t.Run("guards that refuse", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		first := &requestGuard{refusal: server.PathNotAllowed("src/a.go is outside the token's paths").With("path", "src/a.go")}
		second := &requestGuard{}
		w.land.AddRequestGuard(first)
		w.land.AddRequestGuard(second)
		for _, ch := range []change.Change{w.open(t, "open"), w.openDraft(t, "draft")} {
			status, answer := w.landCall(t, w.ownerKey, ch.Number, "", nil)

			errObj, _ := answer["error"].(map[string]any)
			if status != http.StatusForbidden || errorType(answer) != "path_not_allowed" || errObj["path"] != "src/a.go" {
				t.Errorf("land of %s = %d %v, want 403 path_not_allowed with path src/a.go", ch.Ref, status, answer)
			}
		}
		if n, main := w.attempts(t), head(t, w.api, "main"); n != 0 || main != w.base {
			t.Errorf("%d attempts and main at %s, want none and main at base", n, main)
		}
		if n := len(second.calls()); n != 0 {
			t.Errorf("the second guard was asked %d times, want never", n)
		}

		// An automatic land asks no request guard.
		a, err := w.land.Start(context.Background(), land.StartInput{Workspace: "api", Number: 1,
			Trigger: land.TriggerAutoLand, RequestedBy: change.Actor{ActorType: change.ActorSystem}, Caller: w.caller()})
		if err != nil || len(first.calls()) != 2 {
			t.Errorf("Start = %+v, %v after %d guard calls; want an attempt and no further call", a, err, len(first.calls()))
		}
	})
}

func TestLand_RefusesALandWhoseChangesMovedOnWhileItWasJudged(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	var moving atomic.Pointer[change.Change]
	w.land.AddRequestGuard(&requestGuard{during: func() {
		if ch := moving.Load(); ch != nil {
			w.nextVersion(t, *ch)
		}
	}})
	for i, c := range []struct {
		name  string
		stack bool
		// moved is the index, in the stack, of the change that gets a new
		// version while the request guards judge the land.
		moved int
	}{
		{"the change", false, 0},
		{"the change below in a stack", true, 0},
		{"the top change of a stack", true, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := []change.Change{w.open(t, "m"+strconv.Itoa(i))}
			if c.stack {
				s = w.stack(t, "m"+strconv.Itoa(i)+"-", false, nil, nil)
			}
			moving.Store(&s[c.moved])
			defer moving.Store(nil)
			before, main := w.attempts(t), head(t, w.api, "main")

			status, answer := w.landCall(t, w.ownerKey, s[len(s)-1].Number, "", map[string]any{"stack": c.stack})

			if status != http.StatusConflict || errorType(answer) != "version_mismatch" {
				t.Errorf("land = %d %v, want 409 version_mismatch", status, answer)
			}
			if n, got := w.attempts(t), head(t, w.api, "main"); n != before || got != main {
				t.Errorf("the refused land made %d attempts and moved main to %s from %s", n-before, got, main)
			}
		})
	}
}
