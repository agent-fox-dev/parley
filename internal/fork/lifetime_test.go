package fork_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestResolveTTL_AcceptsFiveMinutesToMaxTTLInWholeUnits(t *testing.T) {
	t.Parallel()
	// A zone east of UTC and a fraction of a second: the answer is still UTC
	// in whole seconds.
	now := time.Date(2026, 1, 1, 2, 0, 0, 999_999_999, time.FixedZone("+02:00", 2*60*60))
	month := config.Forks{DefaultTTL: 168 * time.Hour, MaxTTL: 720 * time.Hour, MaxPerWorkspace: 200}
	day := config.Forks{DefaultTTL: time.Hour, MaxTTL: 24 * time.Hour, MaxPerWorkspace: 200}
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct {
		ttl    string
		limits config.Forks
		want   time.Time // zero when refused
	}{
		{"5m", month, at("2026-01-01T00:05:00Z")},
		{"30d", month, at("2026-01-31T00:00:00Z")},
		{"720h", month, at("2026-01-31T00:00:00Z")},
		{"43200m", month, at("2026-01-31T00:00:00Z")},
		{"90m", month, at("2026-01-01T01:30:00Z")},
		{"4m", month, time.Time{}},
		{"31d", month, time.Time{}},
		{"721h", month, time.Time{}},
		{"43201m", month, time.Time{}},
		{"300s", month, time.Time{}},
		{"0m", month, time.Time{}},
		{"-1h", month, time.Time{}},
		{"1.5h", month, time.Time{}},
		{"1h30m", month, time.Time{}},
		{"10", month, time.Time{}},
		{"", month, time.Time{}},
		{"24h", day, at("2026-01-02T00:00:00Z")},
		{"25h", day, time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.ttl+" of "+tc.limits.MaxTTL.String(), func(t *testing.T) {
			t.Parallel()
			got, err := fork.ResolveTTL(tc.ttl, now, tc.limits)
			if tc.want.IsZero() {
				answer, ok := errors.AsType[*server.Error](err)
				if !ok || answer.Status() != 400 || answer.Type() != "invalid_request" || !strings.HasPrefix(answer.Message(), "ttl: ") {
					t.Fatalf("ResolveTTL(%q) = %v, %v; want a 400 invalid_request naming ttl", tc.ttl, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveTTL(%q) error = %v", tc.ttl, err)
			}
			if !got.Equal(tc.want) || got.Location() != time.UTC || got.Nanosecond() != 0 {
				t.Errorf("ResolveTTL(%q) = %v, want %v in whole seconds UTC", tc.ttl, got, tc.want)
			}
		})
	}
}

func TestDefaultExpiry_AddsTheDefaultTTL(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 500_000_000, time.UTC)
	got := fork.DefaultExpiry(now, config.Forks{DefaultTTL: 168 * time.Hour, MaxTTL: 720 * time.Hour})
	if want := time.Date(2026, 1, 8, 0, 0, 0, 0, time.UTC); !got.Equal(want) || got.Location() != time.UTC {
		t.Errorf("DefaultExpiry = %v, want %v", got, want)
	}
}

func TestUpdate_TTLSetsExpiresAtFromTheRequestTime(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	const path = "/workspaces/demo--t1"
	created(t, w.post(t, w.miaKey, forksPath, map[string]any{"name": "t1"}))
	w.advance(time.Hour)
	for _, caller := range []struct{ name, cred string }{{"creator", w.miaKey}, {"parent owner", w.devKey}, {"admin token", w.Token}} {
		// Fourteen days is later than the expiry before it, five minutes
		// earlier.
		for _, c := range []struct {
			ttl string
			d   time.Duration
		}{{"14d", 14 * 24 * time.Hour}, {"5m", 5 * time.Minute}} {
			t.Run(caller.name+" "+c.ttl, func(t *testing.T) {
				w.advance(time.Second)
				now := w.clock()
				got := w.patch(t, caller.cred, path, map[string]any{"ttl": c.ttl}).object(t, http.StatusOK)
				wantMembers(t, got, map[string]any{"expires_at": timefmt.Format(now.Add(c.d)), "updated_at": timefmt.Format(now)})
				wantMembers(t, w.get(t, w.devKey, path).object(t, http.StatusOK), map[string]any{"expires_at": got["expires_at"]})
			})
		}
	}
}

func TestUpdate_TTLWritesOneUpdateEvent(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	session := uuid.New().String()
	for _, c := range []struct {
		name, slug, cred string
		body             map[string]any
		session          *string
		actorType        string
		actor            *string
		fields           []string
	}{
		{"ttl alone", "demo--e1", w.devKey, map[string]any{"ttl": "2d"}, nil, identity.TypeAPIKey, &w.dev, []string{"ttl"}},
		{"with a description", "demo--e2", w.devKey, map[string]any{"description": "d", "ttl": "2d"}, nil, identity.TypeAPIKey, &w.dev, []string{"description", "ttl"}},
		{"of a fork in a session", "demo--e3", w.devKey, map[string]any{"ttl": "2d"}, &session, identity.TypeAPIKey, &w.dev, []string{"ttl"}},
		{"by the admin token", "demo--e4", w.Token, map[string]any{"ttl": "2d"}, nil, event.ActorAdminToken, nil, []string{"ttl"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			w.seedFork(t, c.slug, func(f *fork.Fork) { f.SessionID = c.session })
			w.patch(t, c.cred, "/workspaces/"+c.slug, c.body).object(t, http.StatusOK)
			evs := w.eventsFor(t, "parley.workspace.update", c.slug)
			if len(evs) != 1 {
				t.Fatalf("update events of %s = %d, want 1", c.slug, len(evs))
			}
			ev := evs[0]
			var meta struct {
				Fields []string `json:"fields"`
				Parent string   `json:"parent_slug"`
			}
			if err := json.Unmarshal(ev.Metadata, &meta); err != nil {
				t.Fatal(err)
			}
			if ev.ResourceID != c.slug || ev.ResourceType != "workspace" || ev.ActorType != c.actorType || !reflect.DeepEqual(ev.ActorID, c.actor) ||
				!reflect.DeepEqual(ev.SessionID, c.session) || !slices.Equal(meta.Fields, c.fields) || meta.Parent != parentSlug {
				t.Errorf("event = %+v with %s; want resource %s, actor %s %v, session %v, fields %v, parent %s",
					ev, ev.Metadata, c.slug, c.actorType, c.actor, c.session, c.fields, parentSlug)
			}
		})
	}
}

func TestUpdate_TTLNeedsWriteAccessToTheFork(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	ann, stranger := w.CreateUser(t, "ann"), w.CreateUser(t, "sam")
	w.AddMember(t, w.acme, ann)
	reader, _ := w.pat(t, "", nil, "workspaces:read")
	w.seedFork(t, "demo--t1")
	const path = "/workspaces/demo--t1"
	for _, c := range []struct {
		name, cred string
		archived   bool
		status     int
		want       string // the error type, "" for none
	}{
		{"org member", w.IssueKey(t, ann), false, http.StatusForbidden, "forbidden"},
		{"org member while the parent is archived", w.IssueKey(t, ann), true, http.StatusForbidden, "forbidden"},
		{"PAT with workspaces:read alone", reader, false, http.StatusForbidden, "insufficient_scope"},
		{"user outside the org", w.IssueKey(t, stranger), false, http.StatusNotFound, "not_found"},
		// The same request of the owner is taken: the refusals above are
		// the caller's, not the field's.
		{"parent owner", w.devKey, false, http.StatusOK, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.archived {
				w.setParent(t, workspace.StatusArchived, workspace.MarkerNone)
			}
			before := w.record(t, "demo--t1").ExpiresAt
			r := w.patch(t, c.cred, path, map[string]any{"ttl": "1d"})
			if c.want == "" {
				wantMembers(t, r.object(t, c.status), map[string]any{"expires_at": timefmt.Format(w.clock().Add(24 * time.Hour))})
				return
			}
			wantError(t, r, c.status, c.want)
			if got := w.record(t, "demo--t1").ExpiresAt; !got.Equal(*before) {
				t.Errorf("expires_at = %v, want it unchanged at %v", got, before)
			}
		})
	}
}
