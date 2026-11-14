package gitserver_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitserver"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// recordingSink is the git server's event sink in the tests: it hands every
// event to the hub's event log and keeps the records the log stored, or
// refuses every event with fail when it is set.
type recordingSink struct {
	next gitserver.EventSink
	fail error

	mu      sync.Mutex
	records []event.Record
}

func (s *recordingSink) EmitNow(ctx context.Context, in event.Input) (event.Record, error) {
	if s.fail != nil {
		return event.Record{}, s.fail
	}
	rec, err := s.next.EmitNow(ctx, in)
	if err == nil {
		s.mu.Lock()
		s.records = append(s.records, rec)
		s.mu.Unlock()
	}
	return rec, err
}

// fetches are the parley.git.fetch events stored since the last clear.
func (s *recordingSink) fetches() []event.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []event.Record
	for _, r := range s.records {
		if r.Type == "parley.git.fetch" {
			out = append(out, r)
		}
	}
	return out
}

func (s *recordingSink) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = nil
}

// newAuditWorld is newGitWorld with sink in front of the hub's event log,
// adjust, when not nil, applied to the git server's dependencies after it,
// and audit_fetches set on demo.
func newAuditWorld(t *testing.T, sink *recordingSink, adjust func(*gitserver.Deps)) *world {
	t.Helper()
	w := newGitWorld(t, hubtest.Config{Compose: func(c *hub.Composition) {
		c.AdjustGitServer(func(d *gitserver.Deps) {
			sink.next, d.Events = d.Events, sink
			if adjust != nil {
				adjust(d)
			}
		})
	}})
	w.auditFetches(t, true)
	return w
}

// auditFetches sets the setting audit_fetches of demo to on.
func (w *world) auditFetches(t *testing.T, on bool) {
	t.Helper()
	doc, _ := json.Marshal(map[string]bool{"audit_fetches": on})
	w.set(t, "demo", "settings", string(doc))
}

// clone clones demo with stock git under protocol version protocol, the
// options args coming before the URL, with password, into dir/d.
func (w *world) clone(t *testing.T, dir, protocol, password string, args ...string) {
	t.Helper()
	runGit(t, dir, append(append([]string{"-c", "protocol.version=" + protocol, "clone"}, args...), w.remote(password, "acme/demo.git"), "d")...)
}

func TestEvent_RecordsAnAuditedFetchThatSendsAPack(t *testing.T) {
	t.Parallel()
	sink := &recordingSink{}
	w := newAuditWorld(t, sink, nil)
	rows := []struct {
		name, password string
		args           []string
		actorType      string
		actorID        any
		shallow        bool
		filter         any
	}{
		{"api key", w.ownerKey, nil, "api_key", w.owner, false, nil},
		{"pat, depth 1", w.ownerPAT, []string{"--depth", "1"}, "pat", w.owner, true, nil},
		{"admin token, blob:none", w.Token, []string{"--filter=blob:none", "--no-checkout"}, "admin_token", nil, false, "blob:none"},
		// git sends the limit in bytes, whatever unit its user gave.
		{"depth 1, blob:limit=1k", w.ownerKey, []string{"--depth", "1", "--filter=blob:limit=1k"}, "api_key", w.owner, true, "blob:limit=1024"},
	}
	for _, protocol := range []string{"0", "2"} {
		for _, row := range rows {
			t.Run("protocol "+protocol+"/"+row.name, func(t *testing.T) {
				sink.clear()
				w.clone(t, t.TempDir(), protocol, row.password, row.args...)
				events := sink.fetches()
				if len(events) != 1 {
					t.Fatalf("%d parley.git.fetch events, want 1: %+v", len(events), events)
				}
				e := events[0]
				var actorID any
				if e.ActorID != nil {
					actorID = *e.ActorID
				}
				if e.Workspace == nil || *e.Workspace != "demo" || e.ResourceType != "workspace" || e.ResourceID != "demo" ||
					e.Action != "fetch" || e.ActorType != row.actorType || actorID != row.actorID {
					t.Errorf("event = workspace %v %s %s %s actor %s %v, want demo workspace demo fetch actor %s %v",
						e.Workspace, e.ResourceType, e.ResourceID, e.Action, e.ActorType, actorID, row.actorType, row.actorID)
				}
				var meta map[string]any
				if err := json.Unmarshal(e.Metadata, &meta); err != nil {
					t.Fatal(err)
				}
				if want := map[string]any{"shallow": row.shallow, "filter": row.filter}; !reflect.DeepEqual(meta, want) {
					t.Errorf("metadata = %v, want %v", meta, want)
				}
			})
		}
	}
}

// orphaning reads the records as Records does, but for the parent demo,
// which it does not find while orphan is set.
type orphaning struct {
	gitserver.Records
	orphan atomic.Bool
}

func (o *orphaning) Workspace(ctx context.Context, slug string) (workspace.Workspace, bool, error) {
	if slug == "demo" && o.orphan.Load() {
		return workspace.Workspace{}, false, nil
	}
	return o.Records.Workspace(ctx, slug)
}

// A fork has no settings of its own: it audits its fetches when its parent
// does, as the workspace object of a fork shows the parent's settings, and
// not when its parent's record is not found.
func TestEvent_AuditsAForkByItsParentsSetting(t *testing.T) {
	t.Parallel()
	sink, records := &recordingSink{}, &orphaning{}
	w := newAuditWorld(t, sink, func(d *gitserver.Deps) { records.Records, d.Records = d.Records, records })
	w.addFork(t)
	// The fork's own stored document says the opposite of its parent's in
	// each row, so that only the parent's can decide.
	for _, row := range []struct {
		name           string
		parent, orphan bool
		want           int
	}{{"parent audits", true, false, 1}, {"parent does not audit", false, false, 0}, {"parent not found", false, true, 0}} {
		t.Run(row.name, func(t *testing.T) {
			w.auditFetches(t, row.parent)
			doc, _ := json.Marshal(map[string]bool{"audit_fetches": !row.parent})
			w.set(t, "demo--t1", "settings", string(doc))
			records.orphan.Store(row.orphan)
			defer records.orphan.Store(false)
			sink.clear()
			runGit(t, t.TempDir(), "clone", "--no-checkout", w.remote(w.memberKey, "acme/demo--t1.git"), "d")
			events := sink.fetches()
			if len(events) != row.want {
				t.Fatalf("%d parley.git.fetch events, want %d: %+v", len(events), row.want, events)
			}
			if row.want == 1 && (events[0].Workspace == nil || *events[0].Workspace != "demo--t1" || events[0].ResourceID != "demo--t1") {
				t.Errorf("event of workspace %v, resource %s; want the fork demo--t1", events[0].Workspace, events[0].ResourceID)
			}
		})
	}
}

func TestEvent_RecordsNoFetchThatSendsNoPackOrIsNotAudited(t *testing.T) {
	t.Parallel()
	sink := &recordingSink{}
	w := newAuditWorld(t, sink, nil)
	unregisteredSink := &recordingSink{}
	unregistered := newAuditWorld(t, unregisteredSink, func(d *gitserver.Deps) { d.Settings = &workspace.SettingsRegistry{} })
	send := func(auth, body string, status int) func(*testing.T, string) {
		return func(t *testing.T, _ string) {
			if a := w.send(t, http.MethodPost, uploadPack, auth, nil, body); a.status != status {
				t.Fatalf("answer = %d %q, want %d", a.status, a.body, status)
			}
		}
	}
	rows := []struct {
		name    string
		w       *world
		sink    *recordingSink
		arrange func(*testing.T, string)
		run     func(*testing.T, string)
	}{
		{"ls-remote", w, sink, nil, func(t *testing.T, _ string) { lsRemote(t, w.remote(w.ownerKey, "acme/demo.git")) }},
		{"fetch in an up-to-date clone", w, sink,
			func(t *testing.T, dir string) { w.clone(t, dir, "2", w.ownerKey) },
			func(t *testing.T, dir string) { runGit(t, filepath.Join(dir, "d"), "fetch", "origin") }},
		{"refused fetch", w, sink, nil, send(basic("x", w.strangerKey), fetchRequest(w.head), http.StatusNotFound)},
		{"fetch answered with an ERR packet", w, sink, nil, send(basic("x", w.ownerKey), "", http.StatusOK)},
		{"clone with audit_fetches false", w, sink,
			func(t *testing.T, _ string) {
				w.auditFetches(t, false)
				t.Cleanup(func() { w.auditFetches(t, true) })
			},
			func(t *testing.T, dir string) { w.clone(t, dir, "2", w.ownerKey) }},
		{"clone with no audit_fetches key registered", unregistered, unregisteredSink, nil,
			func(t *testing.T, dir string) { unregistered.clone(t, dir, "2", unregistered.ownerKey) }},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			dir := t.TempDir()
			if row.arrange != nil {
				row.arrange(t, dir)
			}
			row.sink.clear()
			row.run(t, dir)
			if events := row.sink.fetches(); len(events) != 0 {
				t.Errorf("%d parley.git.fetch events, want none: %+v", len(events), events)
			}
		})
	}
}

func TestEvent_CompletesAnAuditedFetchWhateverBecomesOfItsEvent(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		noSink bool
	}{
		{"the sink fails", false},
		{"no sink", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			sink := &recordingSink{fail: errors.New("event log unavailable")}
			w := newAuditWorld(t, sink, func(d *gitserver.Deps) {
				if c.noSink {
					d.Events = nil
				}
			})
			served := gitserver.RequestLabels{Service: gitserver.LabelUploadPack, Result: gitserver.ResultOK}
			before := w.git.Requests()
			dir := t.TempDir()
			w.clone(t, dir, "0", w.ownerKey)
			if head, _ := clientGit(t, filepath.Join(dir, "d"), nil, "rev-parse", "HEAD"); strings.TrimSpace(head) != w.head {
				t.Errorf("HEAD of the clone = %s, want %s", head, w.head)
			}
			if n := grown(before, w.git.Requests())[served]; n != 1 {
				t.Errorf("the counter of %v grew by %d, want 1", served, n)
			}
			want := 1
			if c.noSink {
				want = 0
			}
			if n := countLines(w.Log.String(), "parley.git.fetch"); n != want {
				t.Errorf("%d log records name parley.git.fetch, want %d; log:\n%s", n, want, w.Log.String())
			}
		})
	}
}

// countLines is the number of lines of log that hold s.
func countLines(log, s string) int {
	n := 0
	for line := range strings.Lines(log) {
		if strings.Contains(line, s) {
			n++
		}
	}
	return n
}
