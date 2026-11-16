package land_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/land"
)

// another opens a second change from the fork slug's feat into release
// once the land is recorded, a draft for draft, and abandons it for
// abandon.
func another(w *world, slug string, draft, abandon bool) func(point string) error {
	return func(point string) error {
		if point != "after-bookkeeping" {
			return nil
		}
		ctx := context.Background()
		return w.Store.Tx(ctx, func(tx *sql.Tx) error {
			ch, err := w.changes.Open(ctx, tx, change.OpenInput{Workspace: "api", SourceWorkspace: slug, SourceBranch: "feat",
				TargetBranch: new("release"), Intent: new("why"), Kind: change.KindRef, Caller: w.caller(), Draft: draft})
			if err != nil || !abandon {
				return err
			}
			return w.changes.SetAbandoned(ctx, tx, "api", ch.Number, "done")
		})
	}
}

// sourceDeletes are the lands of a change from feat of a fork, and whether
// each deletes feat.
var sourceDeletes = []struct {
	name     string
	settings string
	// arrange returns what runs at the points of the land, nil for
	// nothing; moved moves feat on before the swap.
	arrange func(t *testing.T, w *world, slug, dir string) func(point string) error
	moved   bool
	// deleted says whether feat is gone afterwards, else it is kept at its
	// head.
	deleted bool
}{
	{name: "by default", deleted: true},
	{name: "with land.delete_source false", settings: `,"land.delete_source":false`},
	{name: "moved since the attempt was created", moved: true},
	{name: "the source of another open change",
		arrange: func(_ *testing.T, w *world, slug, _ string) func(string) error { return another(w, slug, false, false) }},
	{name: "the source of another draft change",
		arrange: func(_ *testing.T, w *world, slug, _ string) func(string) error { return another(w, slug, true, false) }},
	{name: "the source of an abandoned change", deleted: true,
		arrange: func(_ *testing.T, w *world, slug, _ string) func(string) error { return another(w, slug, false, true) }},
	{name: "whose delete fails",
		arrange: func(t *testing.T, _ *world, _, dir string) func(string) error {
			if err := os.WriteFile(filepath.Join(dir, "refs", "heads", "feat.lock"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
}

func TestLand_DeletesTheSourceBranchOnlyWhenNothingElseNeedsIt(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	gitrepotest.Git(t, w.api, "branch", "release", w.base)
	points := w.atPoints()
	for i, c := range sourceDeletes {
		t.Run(c.name, func(t *testing.T) {
			slug := "api--d" + string(rune('a'+i))
			dir := w.fork(t, slug, w.owner)
			w.configure(t, `{"changes.auto_restack":false`+c.settings+`}`)
			from := commit(t, dir, "feat", w.base, "feat", map[string]string{slug + ".txt": "x\n"})
			ch := w.opened(t, change.OpenInput{Workspace: "api", SourceWorkspace: slug, SourceBranch: "feat", Intent: new("why"),
				Kind: change.KindRef, Caller: w.caller()})
			want := from
			var run func(string) error
			switch {
			case c.arrange != nil:
				run = c.arrange(t, w, slug, dir)
			case c.moved:
				run = func(point string) error {
					if point == "before-swap" {
						want = commit(t, dir, "feat", from, "later", map[string]string{"later.txt": "later\n"})
					}
					return nil
				}
			}
			failed := points.set(t, run)

			status, a := w.landCall(t, w.ownerKey, ch.Number, "", nil)

			if err := failed(); err != nil {
				t.Fatalf("arranging the row failed: %v", err)
			}
			if status != 200 || a["status"] != land.StatusLanded {
				t.Fatalf("land = %d %v, want 200 landed", status, a)
			}
			got := head(t, dir, "feat")
			if c.deleted && got != "" || !c.deleted && got != want {
				t.Errorf("feat at %q, want deleted %t or at %s", got, c.deleted, want)
			}
			if want != from {
				gitrepotest.Git(t, dir, "cat-file", "-e", want+"^{commit}")
			}
		})
	}
}

// recordingScheduler keeps the changes a land asked the mergeability
// checks of, and schedules them.
type recordingScheduler struct {
	next land.MergeabilityScheduler

	mu    sync.Mutex
	asked []int64
}

func (s *recordingScheduler) ScheduleMergeable(ctx context.Context, tx *sql.Tx, workspace string, number int64) error {
	s.mu.Lock()
	s.asked = append(s.asked, number)
	s.mu.Unlock()
	return s.next.ScheduleMergeable(ctx, tx, workspace, number)
}

func (s *recordingScheduler) take() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.asked
	s.asked = nil
	slices.Sort(out)
	return out
}

// hookCalls keeps what an after-landing hook was told.
type hookCalls struct {
	mu  sync.Mutex
	got []land.AfterLanding
}

func (h *hookCalls) hook(err error, panics bool) land.AfterLandingHook {
	return func(_ context.Context, in land.AfterLanding) error {
		h.mu.Lock()
		h.got = append(h.got, in)
		h.mu.Unlock()
		if panics {
			panic("hook")
		}
		return err
	}
}

func (h *hookCalls) told() []land.AfterLanding {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.got)
}

func TestLand_SchedulesTheOtherChangesOfTheTargetAndCallsEachHookOnce(t *testing.T) {
	t.Parallel()
	scheduler := &recordingScheduler{}
	w := newWorld(t, func(d *land.Deps) {
		scheduler.next = d.Changes
		d.Mergeability = scheduler
	})
	hooks := []*hookCalls{{}, {}, {}}
	w.land.AddAfterLanding(hooks[0].hook(nil, false))
	w.land.AddAfterLanding(hooks[1].hook(errors.New("hook failed"), false))
	w.land.AddAfterLanding(hooks[2].hook(nil, true))
	session := uuid.New().String()
	w.sessions.add(session, w.owner, "api")

	commit(t, w.api, "c1", w.base, "c1", map[string]string{"a.txt": "l1\nc1\nl3\n", "one.txt": "1\n"})
	one := w.openBranch(t, "c1", w.caller())
	two, three := w.open(t, "c2"), w.openDraft(t, "c3")
	gitrepotest.Git(t, w.api, "branch", "release", w.base)
	commit(t, w.api, "c4", w.base, "c4", map[string]string{"c4.txt": "4\n"})
	w.opened(t, change.OpenInput{Workspace: "api", SourceBranch: "c4", TargetBranch: new("release"), Intent: new("why"),
		Kind: change.KindRef, Caller: w.caller()})
	scheduler.take()

	// landed calls each hook once more, with the update before..after,
	// and checks what each was told.
	landed := func(t *testing.T, ch change.Change, body map[string]any, calls int, before string) {
		t.Helper()
		status, a := w.landCall(t, w.ownerKey, ch.Number, "", body)
		if status != 200 || a["status"] != land.StatusLanded {
			t.Fatalf("land of %s = %d %v, want 200 landed", ch.Ref, status, a)
		}
		after := head(t, w.api, "main")
		for i, h := range hooks {
			told := h.told()
			if len(told) != calls {
				t.Fatalf("hook %d was called %d times, want %d", i, len(told), calls)
			}
			in := told[calls-1]
			var sessionID *string
			if s, ok := body["session_id"].(string); ok {
				sessionID = &s
			}
			wantChanges := []land.LandedChange{{Number: ch.Number, SourceWorkspace: "api", SourceBranch: ch.Source.Branch, LandedSHA: after}}
			if in.Attempt.ID != a["id"] || in.Workspace != "api" ||
				in.Update != (land.RefUpdate{Ref: "refs/heads/main", Before: before, After: after}) ||
				!reflect.DeepEqual(in.SessionID, sessionID) || !reflect.DeepEqual(in.Changes, wantChanges) {
				t.Errorf("hook %d told %s of api? %s, %+v, session %v, %+v; want %v, %+v, %v, %+v", i, in.Attempt.ID,
					in.Workspace, in.Update, in.SessionID, in.Changes, a["id"], land.RefUpdate{Ref: "refs/heads/main",
						Before: before, After: after}, sessionID, wantChanges)
			}
		}
		if got := w.attempt(t, a["id"].(string)).Status; got != land.StatusLanded {
			t.Errorf("the attempt reads %s after the hooks, want landed", got)
		}
	}

	landed(t, one, map[string]any{"session_id": session}, 1, w.base)
	if got := scheduler.take(); !slices.Equal(got, []int64{two.Number, three.Number}) {
		t.Errorf("mergeability scheduled for %v, want %v", got, []int64{two.Number, three.Number})
	}

	// A land that moves nothing calls the hooks all the same.
	commit(t, w.api, "c5", w.base, "c5", map[string]string{"one.txt": "1\n"})
	main := head(t, w.api, "main")
	landed(t, w.openBranch(t, "c5", w.caller()), map[string]any{}, 2, main)

	// A land that conflicts calls none.
	commit(t, w.api, "c6", w.base, "c6", map[string]string{"a.txt": "l1\nc6\nl3\n"})
	if status, a := w.landCall(t, w.ownerKey, w.openBranch(t, "c6", w.caller()).Number, "", nil); status != 409 {
		t.Fatalf("land of c6 = %d %v, want 409 land_conflict", status, a)
	}
	for i, h := range hooks {
		if n := len(h.told()); n != 2 {
			t.Errorf("hook %d was called %d times after a conflict, want still 2", i, n)
		}
	}
}

func TestLand_ContactsNoOrigin(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	origin := filepath.Join(t.TempDir(), "origin.git")
	gitrepotest.Git(t, origin, "init", "--quiet", "--bare", "--initial-branch=main", origin)
	commit(t, origin, "main", "", "A", map[string]string{"a.txt": "A\n"})
	w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE workspaces SET git_url = ? WHERE slug = 'api'`, "file://"+origin)
		return err
	})
	objects := func() []string {
		return strings.Fields(gitrepotest.Git(t, origin, "cat-file", "--batch-all-objects", "--batch-check=%(objectname)"))
	}
	refs, before := gitrepotest.Refs(t, origin, "refs/"), objects()
	ch := w.open(t, "feature")

	status, a := w.landCall(t, w.ownerKey, ch.Number, "", nil)

	if status != 200 || head(t, w.api, "main") != ch.HeadSHA {
		t.Fatalf("land = %d %v with main at %s, want 200 and main at %s", status, a, head(t, w.api, "main"), ch.HeadSHA)
	}
	if got := gitrepotest.Refs(t, origin, "refs/"); !reflect.DeepEqual(got, refs) || !slices.Equal(objects(), before) {
		t.Errorf("origin holds %v and %d objects, want %v and the %d it held", got, len(objects()), refs, len(before))
	}
}
