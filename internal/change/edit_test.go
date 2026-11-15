package change_test

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// patch sends the PATCH body (a JSON text as a string, or a value to
// encode) to the change number of demo with the credential, and returns
// the status and the decoded answer.
func (w *world) patch(t *testing.T, number int64, credential string, body any) (int, map[string]any) {
	t.Helper()
	return w.send(t, http.MethodPatch, "/workspaces/demo/changes/"+strconv.FormatInt(number, 10), credential, body)
}

// change reads the change number of demo through the route, decoded and as
// received.
func (w *world) change(t *testing.T, number int64) (map[string]any, []byte) {
	t.Helper()
	_, out, raw := w.get(t, "/workspaces/demo/changes/"+strconv.FormatInt(number, 10), w.ownerKey)
	return out, raw
}

// updatedAt is the stored updated_at of the change number of demo, finer
// than the second the change object shows.
func (w *world) updatedAt(t *testing.T, number int64) int64 {
	t.Helper()
	var at int64
	if err := w.Store.Main().QueryRow(`SELECT updated_at FROM changes WHERE workspace_slug = 'demo' AND number = ?`, number).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

// labels are n distinct labels.
func labels(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "l" + strconv.Itoa(i)
	}
	return out
}

func TestEdit_StoresTheNamedMembersAndKeepsTheOthers(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.branch(t, "feat", "Feat\n\nWhy.\n")
	n := w.openBranch(t, "feat", func(in *change.OpenInput) {
		in.Title, in.Intent, in.Labels = new("T"), new("i"), []string{"old"}
	}).Number
	long := "a" + strings.Repeat(".", 62)
	for _, c := range []struct {
		name string
		body any
		want map[string]any
	}{
		{"only the title", map[string]any{"title": "T2"}, map[string]any{"title": "T2"}},
		{"only the intent", map[string]any{"intent": "why"}, map[string]any{"intent": "why"}},
		{"labels with a duplicate", map[string]any{"labels": []string{"a", "b", "a"}}, map[string]any{"labels": []string{"a", "b"}}},
		{"no labels", map[string]any{"labels": []string{}}, map[string]any{"labels": []string{}}},
		{"a title of 200 characters", map[string]any{"title": strings.Repeat("é", 200)}, map[string]any{"title": strings.Repeat("é", 200)}},
		{"an intent of 16000 characters", map[string]any{"intent": strings.Repeat("é", 16000)}, map[string]any{"intent": strings.Repeat("é", 16000)}},
		{"20 distinct labels", map[string]any{"labels": labels(20)}, map[string]any{"labels": labels(20)}},
		{"a label of 63 characters", map[string]any{"labels": []string{long}}, map[string]any{"labels": []string{long}}},
		{"the title and the labels", map[string]any{"title": "T3", "labels": []string{"x"}}, map[string]any{"title": "T3", "labels": []string{"x"}}},
		{"auto_land", map[string]any{"auto_land": true}, map[string]any{"auto_land": true}},
		{"an unknown member besides", map[string]any{"title": "T4", "foo": 1}, map[string]any{"title": "T4"}},
	} {
		before, _ := w.change(t, n)
		seq, at := w.seq(t), w.updatedAt(t, n)
		status, got := w.patch(t, n, w.ownerKey, c.body)
		if status != http.StatusOK {
			t.Errorf("%s: PATCH = %d %v, want 200", c.name, status, got)
			continue
		}
		if w.updatedAt(t, n) <= at {
			t.Errorf("%s: updated_at did not advance", c.name)
		}
		want := asJSON(t, c.want)
		for member, value := range before {
			switch wanted, named := want[member]; {
			case named && !reflect.DeepEqual(got[member], wanted):
				t.Errorf("%s: %s = %v, want %v", c.name, member, got[member], wanted)
			case !named && member != "updated_at" && !reflect.DeepEqual(got[member], value):
				t.Errorf("%s: %s = %v, was %v", c.name, member, got[member], value)
			}
		}
		if after, _ := w.change(t, n); !reflect.DeepEqual(after, got) {
			t.Errorf("%s: GET = %v, the PATCH answered %v", c.name, after, got)
		}
		if events := w.eventsAfter(t, seq); len(events) != 0 {
			t.Errorf("%s: the edit wrote %d events, e.g. %s", c.name, len(events), events[0].Type)
		}
	}
}

func TestEdit_RefusesAnInvalidBodyWhole(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.branch(t, "feat", "Feat\n\nWhy.\n")
	n := w.openBranch(t, "feat", nil).Number
	for _, c := range []struct {
		name string
		body any
	}{
		{"an empty title", map[string]any{"title": ""}},
		{"a title of 201 characters", map[string]any{"title": strings.Repeat("é", 201)}},
		{"a title with a line feed", map[string]any{"title": "a\nb"}},
		{"a title with a carriage return", map[string]any{"title": "a\rb"}},
		{"a null title", `{"title":null}`},
		{"an intent of 16001 characters", map[string]any{"intent": strings.Repeat("é", 16001)}},
		{"21 distinct labels", map[string]any{"labels": labels(21)}},
		{"an upper-case label", map[string]any{"labels": []string{"Upper"}}},
		{"a label starting with a dash", map[string]any{"labels": []string{"-x"}}},
		{"a label of 64 characters", map[string]any{"labels": []string{"a" + strings.Repeat(".", 63)}}},
		{"labels as a string", map[string]any{"labels": "x"}},
		{"a number as the title", map[string]any{"title": 5}},
		{"a string as auto_land", map[string]any{"auto_land": "yes"}},
		{"the state landed", map[string]any{"state": "landed"}},
		{"the state abandoned", map[string]any{"state": "abandoned"}},
		{"a number as target_branch", map[string]any{"target_branch": 7}},
		{"a valid title with an invalid label", map[string]any{"title": "ok", "labels": []string{"Bad Label"}}},
	} {
		_, before := w.change(t, n)
		if status, got := w.patch(t, n, w.ownerKey, c.body); status != http.StatusBadRequest || errorType(got) != "invalid_request" {
			t.Errorf("%s: PATCH = %d %v, want 400 invalid_request", c.name, status, got)
		}
		if _, after := w.change(t, n); !bytes.Equal(after, before) {
			t.Errorf("%s: the refused edit changed the change:\n%s\nwas\n%s", c.name, after, before)
		}
	}
}

func TestEdit_RefusesToLeaveAnOpenChangeWithoutAnIntent(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for i, c := range []struct {
		name   string
		draft  bool
		intent string
		lax    bool
		body   map[string]any
		status int
		state  string
	}{
		{"an open change losing its intent", false, "why", false, map[string]any{"intent": ""}, http.StatusBadRequest, ""},
		{"a draft without an intent opened", true, "", false, map[string]any{"state": "open"}, http.StatusBadRequest, ""},
		{"a draft without an intent opened and renamed", true, "", false, map[string]any{"state": "open", "title": "new"}, http.StatusBadRequest, ""},
		{"a draft opened with an intent", true, "", false, map[string]any{"state": "open", "intent": "why"}, http.StatusOK, "open"},
		{"a draft losing its intent", true, "why", false, map[string]any{"intent": ""}, http.StatusOK, "draft"},
		{"an open change losing its intent where none is required", false, "why", true, map[string]any{"intent": ""}, http.StatusOK, "open"},
		{"a draft without an intent opened where none is required", true, "", true, map[string]any{"state": "open"}, http.StatusOK, "open"},
	} {
		branch := "r" + strconv.Itoa(i)
		w.branch(t, branch, "R\n")
		n := w.openBranch(t, branch, func(in *change.OpenInput) { in.Draft, in.Intent = c.draft, new(c.intent) }).Number
		settings := `{}`
		if c.lax {
			settings = `{"changes.require_intent":false}`
		}
		w.set(t, "demo", "settings", settings)
		_, before := w.change(t, n)
		status, got := w.patch(t, n, w.ownerKey, c.body)
		switch {
		case status != c.status:
			t.Errorf("%s: PATCH = %d %v, want %d", c.name, status, got, c.status)
		case status == http.StatusBadRequest && errorType(got) != "intent_required":
			t.Errorf("%s: PATCH = %v, want intent_required", c.name, got)
		case status == http.StatusOK && got["state"] != c.state:
			t.Errorf("%s: state = %v, want %s", c.name, got["state"], c.state)
		}
		if _, after := w.change(t, n); status != http.StatusOK && !bytes.Equal(after, before) {
			t.Errorf("%s: the refused edit changed the change:\n%s\nwas\n%s", c.name, after, before)
		}
	}
}

func TestEdit_RecordsWhoTurnedAutoLandOn(t *testing.T) {
	t.Parallel()
	m := newMiaWorld(t)
	n, _ := strconv.ParseInt(m.openAsMia(t, "task"), 10, 64)
	owner := m.owner
	for _, c := range []struct {
		name, credential string
		value            bool
		// setter is the user who set the flag, "" for no setter.
		setter string
	}{
		{"mia turns it on", m.miaKey, true, m.mia},
		{"the owner asks for it on again", m.ownerKey, true, m.mia},
		{"the owner turns it off", m.ownerKey, false, ""},
		{"the owner turns it on", m.ownerKey, true, owner},
	} {
		status, got := m.patch(t, n, c.credential, map[string]any{"auto_land": c.value})
		if status != http.StatusOK || got["auto_land"] != c.value {
			t.Errorf("%s: PATCH = %d with auto_land %v, want 200 with %v", c.name, status, got["auto_land"], c.value)
		}
		s, found, err := m.changes.AutoLandSetter(context.Background(), nil, "demo", n)
		switch {
		case err != nil:
			t.Fatalf("%s: AutoLandSetter: %v", c.name, err)
		case c.setter == "" && found:
			t.Errorf("%s: the setter is %+v, want none", c.name, s)
		case c.setter == "":
		case !found || s.UserID == nil || *s.UserID != c.setter || s.CredentialType != change.CredentialAPIKey || s.Paths != nil:
			t.Errorf("%s: the setter is %+v (found %t), want user %s with an api_key without paths", c.name, s, found, c.setter)
		}
	}
}

// closedChanges opens in demo an open change, one it abandons and one it
// lands, and returns their numbers.
func closedChanges(t *testing.T, w *world) (open, abandoned, landed int64) {
	t.Helper()
	var numbers []int64
	for _, branch := range []string{"open", "abandoned", "landed"} {
		w.branch(t, branch, branch+"\n\nWhy.\n")
		numbers = append(numbers, w.openBranch(t, branch, nil).Number)
	}
	w.write(t, func(ctx context.Context, tx *sql.Tx) error {
		if err := w.changes.SetAbandoned(ctx, tx, "demo", numbers[1], "gone"); err != nil {
			return err
		}
		return w.changes.SetLanded(ctx, tx, "demo", numbers[2], change.Landed{SHA: w.base, At: time.Now(), By: change.Actor{ActorType: change.ActorSystem}, LandID: "l1"})
	})
	return numbers[0], numbers[1], numbers[2]
}

func TestEdit_RefusesEveryEditOfAClosedChangeButTheLabelsOfALandedOne(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	_, abandoned, landed := closedChanges(t, w)
	for _, c := range []struct {
		name   string
		number int64
		body   map[string]any
	}{
		{"the title of an abandoned change", abandoned, map[string]any{"title": "t"}},
		{"the labels of an abandoned change", abandoned, map[string]any{"labels": []string{"x"}}},
		{"an abandoned change opened", abandoned, map[string]any{"state": "open"}},
		{"an abandoned change retargeted", abandoned, map[string]any{"target_branch": "release"}},
		{"the title of a landed change", landed, map[string]any{"title": "t"}},
		{"the intent of a landed change", landed, map[string]any{"intent": "why not"}},
		{"auto_land on a landed change", landed, map[string]any{"auto_land": true}},
		{"a landed change made a draft", landed, map[string]any{"state": "draft"}},
		{"a landed change retargeted", landed, map[string]any{"target_branch": "dev"}},
		{"an abandoned change named its own target", abandoned, map[string]any{"target_branch": "main"}},
		{"a landed change named its own target", landed, map[string]any{"target_branch": "main"}},
		{"the labels and the own target of a landed change", landed, map[string]any{"labels": []string{"x"}, "target_branch": "main"}},
		{"the labels and the title of a landed change", landed, map[string]any{"labels": []string{"x"}, "title": "t"}},
	} {
		_, before := w.change(t, c.number)
		if status, got := w.patch(t, c.number, w.ownerKey, c.body); status != http.StatusConflict || errorType(got) != "change_closed" {
			t.Errorf("%s: PATCH = %d %v, want 409 change_closed", c.name, status, got)
		}
		if _, after := w.change(t, c.number); !bytes.Equal(after, before) {
			t.Errorf("%s: the refused edit changed the change:\n%s\nwas\n%s", c.name, after, before)
		}
	}

	before, _ := w.change(t, landed)
	status, got := w.patch(t, landed, w.ownerKey, map[string]any{"labels": []string{"released"}})
	if status != http.StatusOK || !reflect.DeepEqual(got["labels"], []any{"released"}) || got["state"] != "landed" || !reflect.DeepEqual(got["landed"], before["landed"]) {
		t.Errorf("PATCH the labels of a landed change = %d %v, want 200 with labels [released], still landed as before (%v)", status, got, before["landed"])
	}
}

func TestEdit_ChangesNothingWithoutAnEditableMember(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	open, abandoned, landed := closedChanges(t, w)
	for _, body := range []string{`{}`, `{"foo":1}`, `{"depends_on":null}`} {
		for _, n := range []int64{open, abandoned, landed} {
			before, _ := w.change(t, n)
			seq, at := w.seq(t), w.updatedAt(t, n)
			if status, got := w.patch(t, n, w.ownerKey, body); status != http.StatusOK || !reflect.DeepEqual(got, before) || w.updatedAt(t, n) != at {
				t.Errorf("PATCH %s on %v = %d %v, want 200 with the change as it was, updated_at included:\n%v", body, before["state"], status, got, before)
			}
			if events := w.eventsAfter(t, seq); len(events) != 0 {
				t.Errorf("PATCH %s on %v wrote %d events", body, before["state"], len(events))
			}
		}
	}
}

// readingGuard is an auto_land guard that records each call with the
// change's title as the transaction it is asked in holds it, and refuses
// with err.
type readingGuard struct {
	changes *change.Component
	mu      sync.Mutex
	err     error
	calls   []readCall
}

type readCall struct {
	caller change.Caller
	number int64
	title  string
	value  bool
}

func (g *readingGuard) CheckAutoLand(ctx context.Context, tx *sql.Tx, caller change.Caller, ch change.Change, value bool) error {
	stored, err := g.changes.ReadChange(ctx, tx, ch.WorkspaceSlug, ch.Number)
	if err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, readCall{caller, ch.Number, stored.Title, value})
	return g.err
}

// told are the calls since the last, which it forgets.
func (g *readingGuard) told() []readCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	calls := g.calls
	g.calls = nil
	return calls
}

func TestEdit_AsksTheAutoLandGuardBeforeWritingAnything(t *testing.T) {
	t.Parallel()
	m := newMiaWorld(t)
	n, _ := strconv.ParseInt(m.openAsMia(t, "task"), 10, 64)
	pat, patID := m.IssuePATID(t, identity.PATSpec{UserID: m.mia, Permissions: []string{"changes:write"}, Paths: []string{"docs/**"}, CreatedAt: time.Now().UTC()})
	g := &readingGuard{changes: m.changes}
	m.changes.RegisterAutoLandGuard(g)

	g.err = server.PathNotAllowed("auto_land: src/x.go is outside the token's paths").With("path", "src/x.go")
	before, raw := m.change(t, n)
	status, got := m.patch(t, n, pat, map[string]any{"auto_land": true, "title": "T2"})
	if errObj, _ := got["error"].(map[string]any); status != http.StatusForbidden || errObj["error_type"] != "path_not_allowed" || errObj["path"] != "src/x.go" {
		t.Errorf("a refused PATCH = %d %v, want 403 path_not_allowed naming src/x.go", status, got)
	}
	if _, after := m.change(t, n); !bytes.Equal(after, raw) {
		t.Errorf("the refused edit changed the change:\n%s\nwas\n%s", after, raw)
	}
	want := []readCall{{change.Caller{UserID: m.mia, Username: "mia", Credential: change.Credential{Type: change.CredentialPAT, ID: patID},
		Paths: []string{"docs/**"}}, n, before["title"].(string), true}}
	if calls := g.told(); !reflect.DeepEqual(calls, want) {
		t.Errorf("guard calls = %+v, want %+v", calls, want)
	}

	g.err = nil
	for _, c := range []struct {
		name      string
		body      map[string]any
		unguarded bool
		autoLand  bool
		// asked is the value the guard is asked about, nil for no call.
		asked *bool
	}{
		{"the guard accepts", map[string]any{"auto_land": true}, false, true, new(true)},
		{"auto_land turned off", map[string]any{"auto_land": false}, false, false, new(false)},
		{"no auto_land", map[string]any{"title": "T3"}, false, false, nil},
		{"no guard registered", map[string]any{"auto_land": true}, true, true, nil},
	} {
		if c.unguarded {
			m.changes.RegisterAutoLandGuard(nil)
		}
		if status, got := m.patch(t, n, m.miaKey, c.body); status != http.StatusOK || got["auto_land"] != c.autoLand {
			t.Errorf("%s: PATCH = %d with auto_land %v, want 200 with %v", c.name, status, got["auto_land"], c.autoLand)
		}
		calls := g.told()
		if c.asked == nil && len(calls) != 0 || c.asked != nil && (len(calls) != 1 || calls[0].value != *c.asked) {
			t.Errorf("%s: guard calls = %+v, want one with %v", c.name, calls, c.asked)
		}
	}
}

func TestEdit_OpensADraftAndTellsWhoMarkedItReady(t *testing.T) {
	t.Parallel()
	m := newMiaWorld(t)
	head := commit(t, m.t1, "task", m.base, "task\n\nWhy.\n", map[string]string{"task.txt": "task\n"})
	n := m.openBranch(t, "task", func(in *change.OpenInput) {
		in.SourceWorkspace, in.Draft, in.Title = "demo--t1", true, new("T")
		in.Caller = m.caller()
	}).Number
	m.settled(t, n)
	seq := m.seq(t)
	status, got := m.patch(t, n, m.miaKey, map[string]any{"state": "open"})
	landing, _ := got["landing"].(map[string]any)
	if status != http.StatusOK || got["state"] != change.StateOpen || landing["reason"] == "draft" || landing["status"] != change.LandingReady {
		t.Fatalf("PATCH state open = %d %v, want 200 open with landing ready", status, got)
	}
	events := m.changeEvents(t, seq, change.EventOpen, n)
	if len(events) != 1 {
		t.Fatalf("%d open events, want 1", len(events))
	}
	if e := events[0]; e.ActorID == nil || *e.ActorID != m.mia || e.ActorType != change.CredentialAPIKey {
		t.Errorf("the open event is by %s %v, want mia's api_key", e.ActorType, e.ActorID)
	}
	want := map[string]any{"number": float64(n), "ref": "demo#" + strconv.FormatInt(n, 10), "version": 1.0, "head_sha": head,
		"title": "T", "target_branch": "main", "source_workspace": "demo--t1", "source_branch": "task", "depends_on": nil, "was_draft": true}
	if meta := metadata(t, events[0]); !reflect.DeepEqual(meta, want) {
		t.Errorf("metadata of the open event = %v, want %v", meta, want)
	}
}

func TestEdit_MovesAnOpenChangeBackToDraftKeepingWhatItHas(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.branch(t, "b", "B\n")
	n := w.openBranch(t, "b", func(in *change.OpenInput) { in.AutoLand, in.Labels = new(true), []string{"x"} }).Number
	if ch := w.settled(t, n); ch.Mergeable != change.MergeableClean || w.announced(t, n) == "" {
		t.Fatalf("the change is %s and announced at %q, want clean and announced", ch.Mergeable, w.announced(t, n))
	}
	before, _ := w.change(t, n)
	seq := w.seq(t)
	status, got := w.patch(t, n, w.ownerKey, map[string]any{"state": "draft"})
	landing, _ := got["landing"].(map[string]any)
	if status != http.StatusOK || got["state"] != change.StateDraft || landing["status"] != change.LandingBlocked || landing["reason"] != "draft" {
		t.Fatalf("PATCH state draft = %d %v, want 200 draft with landing blocked for draft", status, got)
	}
	for _, member := range []string{"version", "mergeable", "auto_land", "labels"} {
		if !reflect.DeepEqual(got[member], before[member]) {
			t.Errorf("%s = %v, was %v", member, got[member], before[member])
		}
	}
	if events := w.eventsAfter(t, seq); len(events) != 0 {
		t.Errorf("the move to draft wrote %d events, e.g. %s", len(events), events[0].Type)
	}
	if head := w.announced(t, n); head != "" {
		t.Errorf("the draft is still announced landable at %s", head)
	}
}

func TestEdit_ChangesNothingForAValueTheChangeHas(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for i, c := range []struct {
		draft bool
		// noIntent opens the change without an intent before the
		// workspace requires one.
		noIntent bool
		body     map[string]any
	}{
		{false, false, map[string]any{"state": change.StateOpen}},
		{true, false, map[string]any{"state": change.StateDraft}},
		{false, false, map[string]any{"target_branch": "main"}},
		{false, true, map[string]any{"state": change.StateOpen}},
	} {
		branch := "s" + strconv.Itoa(i)
		w.branch(t, branch, branch+"\n")
		if c.noIntent {
			w.set(t, "demo", "settings", `{"changes.require_intent":false}`)
		}
		n := w.openBranch(t, branch, func(in *change.OpenInput) {
			if in.Draft = c.draft; c.noIntent {
				in.Intent = new("")
			}
		}).Number
		w.set(t, "demo", "settings", `{}`)
		w.settled(t, n)
		before, _ := w.change(t, n)
		seq, at := w.seq(t), w.updatedAt(t, n)
		if status, got := w.patch(t, n, w.ownerKey, c.body); status != http.StatusOK || !reflect.DeepEqual(got, before) || w.updatedAt(t, n) != at {
			t.Errorf("PATCH %v on a change that has it = %d %v, want 200 with the change as it was, updated_at included:\n%v", c.body, status, got, before)
		}
		if events := w.eventsAfter(t, seq); len(events) != 0 {
			t.Errorf("PATCH %v on a change that has it wrote %d events, e.g. %s", c.body, len(events), events[0].Type)
		}
	}
}

// diverged commits m.txt on demo's main and r.txt on its release, then a
// change of a.txt on top of main in mia's fork, which it opens by mia into
// main; it returns the change's number and head.
func diverged(t *testing.T, m *miaWorld) (int64, string) {
	t.Helper()
	main := commit(t, m.demo, "main", "main", "M\n", map[string]string{"m.txt": "m\n"})
	commit(t, m.demo, "release", "release", "R\n", map[string]string{"r.txt": "r\n"})
	head := commit(t, m.t1, "task", main, "Task\n\nWhy.\n", map[string]string{"a.txt": "l1\nl2\nl3\nl4\n"})
	ch := m.openBranch(t, "task", func(in *change.OpenInput) {
		in.SourceWorkspace = "demo--t1"
		in.Caller = m.caller()
	})
	return ch.Number, head
}

func TestEdit_RetargetsTheChangeAtItsVersion(t *testing.T) {
	t.Parallel()
	m := newMiaWorld(t)
	n, head := diverged(t, m)
	m.settled(t, n)
	if got := m.announced(t, n); got != head {
		t.Fatalf("before the retarget the change is announced landable at %q, want %s", got, head)
	}
	versions := m.versions(t, "demo", n)
	seq := m.seq(t)

	status, got := m.patch(t, n, m.miaKey, map[string]any{"target_branch": "release"})
	base := gitrepotest.Git(t, m.demo, "merge-base", head, "release")
	stats, _ := got["stats"].(map[string]any)
	if status != http.StatusOK || got["target_branch"] != "release" || got["base_sha"] != base || got["version"] != 1.0 ||
		got["head_sha"] != head || got["mergeable"] != change.MergeableUnknown {
		t.Fatalf("PATCH target_branch release = %d %v, want 200 into release from %s at version 1, %s, unknown", status, got, base, head)
	}
	if commits, _ := strconv.ParseFloat(gitrepotest.Git(t, m.demo, "rev-list", "--count", "release.."+head), 64); stats["commits"] != commits {
		t.Errorf("stats = %v, want %v commits", stats, commits)
	}
	if want := []any{"a.txt", "m.txt"}; !reflect.DeepEqual(got["paths"], want) {
		t.Errorf("paths = %v, want %v", got["paths"], want)
	}
	if after := m.versions(t, "demo", n); !reflect.DeepEqual(after, versions) {
		t.Errorf("versions = %v, were %v", after, versions)
	}
	events := m.changeEvents(t, seq, change.EventVersion, n)
	if len(events) != 1 {
		t.Fatalf("%d version events, want 1", len(events))
	}
	if e := events[0]; e.ActorID == nil || *e.ActorID != m.mia {
		t.Errorf("the version event is by %s %v, want mia", e.ActorType, e.ActorID)
	}
	want := map[string]any{"number": float64(n), "ref": "demo#" + strconv.FormatInt(n, 10), "version": 1.0, "head_sha": head,
		"kind": "retarget", "base_sha": base, "patch_equivalent_to": nil, "draft": false}
	if meta := metadata(t, events[0]); !reflect.DeepEqual(meta, want) {
		t.Errorf("metadata of the version event = %v, want %v", meta, want)
	}
	if ch := m.settled(t, n); ch.TargetBranch != "release" || ch.Mergeable != change.MergeableClean {
		t.Errorf("after its check the change is into %s and %s, want release and clean", ch.TargetBranch, ch.Mergeable)
	}
	// The retarget forgot the announcement, so that the change is
	// announced landable into release, at the same head.
	if ready := m.changeEvents(t, seq, change.EventReady, n); len(ready) != 1 || metadata(t, ready[0])["head_sha"] != head {
		t.Errorf("ready events after the retarget = %+v, want one at %s", ready, head)
	}
}

func TestEdit_RefusesATargetAnOpenWouldRefuse(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	cp := w.addWorkspace(t, "cp")
	w.set(t, "cp", "workspace_mode", "carry_patch")
	w.set(t, "cp", "integration_branch", "integration")
	commit(t, cp, "integration", "refs/heads/main", "Upstream plus patches\n", map[string]string{"i.txt": "i\n"})
	commit(t, cp, "patch", "refs/heads/main", "Patch\n\nWhy.\n", map[string]string{"p.txt": "p\n"})
	if _, err := w.open(t, change.OpenInput{Workspace: "cp", SourceBranch: "patch", Intent: new("why"), Caller: w.ownerCaller(), Kind: change.KindRef}); err != nil {
		t.Fatal(err)
	}
	w.branch(t, "feat", "Feat\n\nWhy.\n")
	w.openBranch(t, "feat", nil)
	for _, c := range []struct {
		name, path, target string
		status             int
		errorType          string
	}{
		{"a branch that does not exist", "/workspaces/demo/changes/1", "nope", http.StatusBadRequest, "target_not_found"},
		{"the change's own source branch", "/workspaces/demo/changes/1", "feat", http.StatusBadRequest, "invalid_request"},
		{"the integration branch", "/workspaces/cp/changes/1", "integration", http.StatusForbidden, "integration_branch"},
	} {
		_, _, before := w.get(t, c.path, w.ownerKey)
		seq := w.seq(t)
		if status, got := w.send(t, http.MethodPatch, c.path, w.ownerKey, map[string]any{"target_branch": c.target}); status != c.status || errorType(got) != c.errorType {
			t.Errorf("%s: PATCH = %d %v, want %d %s", c.name, status, got, c.status, c.errorType)
		}
		if _, _, after := w.get(t, c.path, w.ownerKey); !bytes.Equal(after, before) {
			t.Errorf("%s: the refused retarget changed the change:\n%s\nwas\n%s", c.name, after, before)
		}
		if events := w.eventsAfter(t, seq); len(events) != 0 {
			t.Errorf("%s: the refused retarget wrote %d events, e.g. %s", c.name, len(events), events[0].Type)
		}
	}
}

func TestEdit_RefusesToRetargetAChangeOfAStack(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	b1 := commit(t, w.demo, "b1", w.base, "B1", map[string]string{"one.txt": "1\n"})
	w.openBranch(t, "b1", nil)
	commit(t, w.demo, "b2", b1, "B2", map[string]string{"two.txt": "2\n"})
	w.openBranch(t, "b2", nil)
	w.write(t, func(ctx context.Context, tx *sql.Tx) error {
		return w.changes.SetDependsOn(ctx, tx, "demo", 2, new(int64(1)))
	})
	w.settled(t, 1)
	w.settled(t, 2)
	for _, c := range []struct {
		name   string
		number int64
	}{
		{"the lower change", 1},
		{"the upper change", 2},
	} {
		_, before := w.change(t, c.number)
		seq := w.seq(t)
		if status, got := w.patch(t, c.number, w.ownerKey, map[string]any{"target_branch": "release"}); status != http.StatusConflict || errorType(got) != "conflict" {
			t.Errorf("%s: PATCH = %d %v, want 409 conflict", c.name, status, got)
		}
		if _, after := w.change(t, c.number); !bytes.Equal(after, before) {
			t.Errorf("%s: the refused retarget changed the change:\n%s\nwas\n%s", c.name, after, before)
		}
		if events := w.eventsAfter(t, seq); len(events) != 0 {
			t.Errorf("%s: the refused retarget wrote %d events, e.g. %s", c.name, len(events), events[0].Type)
		}
	}
}

func TestEdit_RetargetsOntoABranchItCannotLandOnAndSaysWhy(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	side := commit(t, w.demo, "side", w.base, "Side\n", map[string]string{"s.txt": "s\n"})
	first := commit(t, w.demo, "main", "main", "First\n", map[string]string{"f.txt": "f\n"})
	if _, err := gitrepotest.Run(w.demo, "commit refs/heads/main\n"+committer+data("Merge side\n")+
		"from "+first+"\nmerge "+side+"\n\n", "fast-import", "--quiet"); err != nil {
		t.Fatal(err)
	}
	head := commit(t, w.demo, "feat", "main", "Feat\n\nWhy.\n", map[string]string{"feat.txt": "feat\n"})
	n := w.openBranch(t, "feat", nil).Number
	gitrepotest.Git(t, w.demo, "update-ref", "refs/heads/ahead", head)
	gitrepotest.Git(t, w.demo, "update-ref", "refs/heads/old", w.base)
	for _, c := range []struct{ target, reason string }{
		{"ahead", "nothing_to_land"},
		{"old", "merge_commits"},
	} {
		status, got := w.patch(t, n, w.ownerKey, map[string]any{"target_branch": c.target})
		landing, _ := got["landing"].(map[string]any)
		if status != http.StatusOK || got["target_branch"] != c.target || landing["status"] != change.LandingBlocked || landing["reason"] != c.reason {
			t.Errorf("PATCH target_branch %s = %d %v, want 200 into %s, blocked by %s", c.target, status, got, c.target, c.reason)
		}
	}
}

func TestEdit_RetargetsAChangeWhoseTargetBranchIsGone(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.branch(t, "b", "B\n\nWhy.\n")
	n := w.openBranch(t, "b", func(in *change.OpenInput) { in.TargetBranch = new("release") }).Number
	w.settled(t, n)
	w.push(t, "release", "", nil)
	w.idle(t, n)
	if ch := w.read(t, n); ch.Landing.Reason == nil || *ch.Landing.Reason != "target_not_found" {
		t.Fatalf("with release gone the landing is %+v, want blocked by target_not_found", ch.Landing)
	}
	status, got := w.patch(t, n, w.ownerKey, map[string]any{"target_branch": "main"})
	landing, _ := got["landing"].(map[string]any)
	if status != http.StatusOK || got["target_branch"] != "main" || landing["reason"] == "target_not_found" {
		t.Errorf("PATCH target_branch main = %d %v, want 200 into main, not blocked by target_not_found", status, got)
	}
}
