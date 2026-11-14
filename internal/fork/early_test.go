package fork_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/hub"
)

const day = 24 * time.Hour

func TestRunPass_RemovesAForkADayAfterItsWorkLanded(t *testing.T) {
	t.Parallel()
	w, r := newRemovalWorld(t)
	task := []string{"task"}
	landed := []fork.SourceChange{{Branch: "task", State: "landed"}}
	rows := []struct {
		name    string
		changes []fork.SourceChange
		own     []string
		idle    time.Duration
		sandbox string
		removed bool
	}{
		{"no change", nil, task, 48 * time.Hour, "", false},
		{"an open change", []fork.SourceChange{{Branch: "task", State: "open"}}, task, 48 * time.Hour, "", false},
		{"a draft change", []fork.SourceChange{{Branch: "task", State: "draft"}}, task, 48 * time.Hour, "", false},
		{"an own branch no change proposes", landed, []string{"task", "spike"}, 48 * time.Hour, "", false},
		{"active less than a day ago", landed, task, day - time.Minute, "", false},
		{"active a day ago", landed, task, day, "", true},
		{"one change landed, one abandoned", []fork.SourceChange{{Branch: "task", State: "landed"}, {Branch: "fix", State: "abandoned"}},
			[]string{"task", "fix"}, 30 * time.Hour, "", true},
		{"its branch deleted", landed, nil, 30 * time.Hour, "", true},
		{"a branch landed and open again", []fork.SourceChange{{Branch: "task", State: "landed"}, {Branch: "task", State: "open"}},
			task, 30 * time.Hour, "", false},
		{"a sandbox provisioning", landed, task, 30 * time.Hour, "provisioning", false},
		{"a sandbox ready", landed, task, 30 * time.Hour, "ready", false},
		{"a sandbox stopped", landed, task, 30 * time.Hour, "stopped", true},
	}
	slug := func(i int) string { return fmt.Sprintf("demo--r%02d", i) }
	for i, row := range rows {
		w.seedLanded(t, r, slug(i), row.idle, row.own, row.changes...)
		r.set(func(r *remover) { r.live[slug(i)] = row.sandbox == "provisioning" || row.sandbox == "ready" })
	}
	w.pass(t, context.Background())
	for i, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if row.removed {
				w.wantExpired(t, slug(i), "landed")
			} else {
				w.wantExpired(t, slug(i))
			}
		})
	}
}

func TestRunPass_CountsTheDayFromTheLastActivity(t *testing.T) {
	t.Parallel()
	w, r := newRemovalWorld(t)
	for _, row := range []struct {
		name   string
		idle   time.Duration
		record bool
	}{
		{"activity recorded", 30 * time.Hour, true},
		{"just created", 0, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			slug := "demo--" + row.name[:4]
			w.seedLanded(t, r, slug, row.idle, []string{"task"}, fork.SourceChange{Branch: "task", State: "landed"})
			if row.record {
				if err := w.forks.RecordActivity(context.Background(), slug); err != nil {
					t.Fatal(err)
				}
			}
			w.pass(t, context.Background())
			w.wantExpired(t, slug)
			w.advance(day - time.Minute)
			w.pass(t, context.Background())
			w.wantExpired(t, slug)
			w.advance(time.Minute)
			w.pass(t, context.Background())
			w.wantExpired(t, slug, "landed")
		})
	}
}

func TestRecordActivity_IgnoresASlugThatIsNoFork(t *testing.T) {
	t.Parallel()
	w, _ := newRemovalWorld(t)
	for _, slug := range []string{parentSlug, "demo--none"} {
		if err := w.forks.RecordActivity(context.Background(), slug); err != nil {
			t.Errorf("RecordActivity(%s) = %v, want nil", slug, err)
		}
	}
	var at sql.NullInt64
	if err := w.Store.Main().QueryRowContext(context.Background(), `SELECT last_activity_at FROM workspaces WHERE slug = ?`,
		parentSlug).Scan(&at); err != nil || at.Valid {
		t.Errorf("last activity of %s = %v, %v; want none", parentSlug, at, err)
	}
}

func TestRunPass_KeepsEveryForkUntilItsLifetimeEndsWhenExpireOnLandIsOff(t *testing.T) {
	t.Parallel()
	w, r := newRemovalWorld(t, func(c *hub.Composition) {
		c.AdjustForks(func(d *fork.Deps) { d.Limits.ExpireOnLand = false })
	})
	w.seedLanded(t, r, "demo--t1", 48*time.Hour, []string{"task"}, fork.SourceChange{Branch: "task", State: "landed"})
	w.pass(t, context.Background())
	w.wantExpired(t, "demo--t1")
	var asked []string
	if r.set(func(r *remover) { asked = r.asked }); len(asked) != 0 {
		t.Errorf("change reader asked about %v, want never", asked)
	}
	w.advance(8 * day)
	w.pass(t, context.Background())
	w.wantExpired(t, "demo--t1", "ttl")
}

func TestRunPass_ExpiresByTTLAForkThatAlsoLanded(t *testing.T) {
	t.Parallel()
	w, r := newRemovalWorld(t)
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprintf("live sandbox %v", live), func(t *testing.T) {
			slug := fmt.Sprintf("demo--%v", live)
			w.seedLanded(t, r, slug, 48*time.Hour, []string{"task"}, fork.SourceChange{Branch: "task", State: "landed"})
			r.set(func(r *remover) { r.live[slug] = live })
			w.advance(8 * day)
			w.pass(t, context.Background())
			w.wantExpired(t, slug, "ttl")
		})
	}
}

func TestRunPass_GoesOnPastAForkItCannotJudge(t *testing.T) {
	t.Parallel()
	w, r := newRemovalWorld(t)
	for name, fail := range map[string]func(*remover){
		"change reader":       func(r *remover) { r.failChanges = true },
		"sandbox state query": func(r *remover) { r.failLive = true },
	} {
		t.Run(name, func(t *testing.T) {
			a, b := "demo--"+name[:2]+"a", "demo--"+name[:2]+"b"
			for _, slug := range []string{a, b} {
				w.seedLanded(t, r, slug, 48*time.Hour, []string{"task"}, fork.SourceChange{Branch: "task", State: "landed"})
			}
			r.set(func(r *remover) { r.only = a; fail(r) })
			t.Cleanup(func() { r.set(func(r *remover) { r.only, r.failChanges, r.failLive = "", false, false }) })
			w.pass(t, context.Background())
			w.wantExpired(t, a)
			w.wantExpired(t, b, "landed")
			if !strings.Contains(w.Log.String(), `"slug":"`+a+`"`) {
				t.Errorf("no log record names %s; log = %s", a, w.Log.String())
			}
			w.advance(8 * day)
			w.pass(t, context.Background())
			w.wantExpired(t, a, "ttl")
		})
	}
}

// seedLanded seeds the fork slug as seedFork does, created idle before the
// hub's time, with each of own as its own branch, one commit off the fork
// point, and has r report changes as the changes whose source it is.
func (w *world) seedLanded(t *testing.T, r *remover, slug string, idle time.Duration, own []string, changes ...fork.SourceChange) {
	t.Helper()
	f := w.seedFork(t, slug, func(f *fork.Fork) { f.CreatedAt = w.clock().Add(-idle) })
	for _, b := range own {
		w.commit(t, slug, b, f.ForkPoint.SHA)
	}
	r.set(func(r *remover) { r.changes[slug] = changes })
}

// wantExpired fails the test unless the fork slug's parley.fork.expire
// events have the reasons reasons, in order, and the fork answers 404 when
// it has one and 200 when it has none.
func (w *world) wantExpired(t *testing.T, slug string, reasons ...string) {
	t.Helper()
	var got []string
	for _, ev := range w.eventsFor(t, fork.EventExpire, slug) {
		var meta struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(ev.Metadata, &meta); err != nil {
			t.Fatal(err)
		}
		got = append(got, meta.Reason)
	}
	if !reflect.DeepEqual(got, reasons) {
		t.Errorf("expiries of %s = %v, want %v", slug, got, reasons)
	}
	want := map[bool]int{true: http.StatusNotFound, false: http.StatusOK}[len(reasons) > 0]
	if a := w.get(t, w.devKey, "/workspaces/"+slug); a.status != want {
		t.Errorf("GET %s = %d, want %d", slug, a.status, want)
	}
}
