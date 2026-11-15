package change_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// modifyRoute is one of the routes that modify a change, with a body it
// takes.
type modifyRoute struct {
	name, method, suffix string
	body                 any
}

var modifyRoutes = []modifyRoute{
	{"edit", http.MethodPatch, "", map[string]any{"labels": []string{"x"}}},
	{"abandon", http.MethodPost, "/abandon", nil},
	{"restore", http.MethodPost, "/restore", nil},
	{"restack", http.MethodPost, "/restack", nil},
}

// call sends the route on the change number (as the path names it) of the
// workspace slug with the credential, and returns the status, the decoded
// answer and the answer as received.
func (r modifyRoute) call(t *testing.T, w *world, slug, number, credential string) (int, map[string]any, []byte) {
	t.Helper()
	status, raw := w.Call(t, r.method, "/api/v1/workspaces/"+slug+"/changes/"+number+r.suffix, credential, r.body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s %s#%s answered %d %q: %v", r.name, slug, number, status, raw, err)
	}
	return status, out, raw
}

// miaWorld is a world with the org members mia, who owns the fork demo--t1
// of demo, and noa.
type miaWorld struct {
	*world
	mia, miaKey, noaKey, t1 string
}

func newMiaWorld(t *testing.T) *miaWorld {
	t.Helper()
	return newMiaWorldWith(t, hubtest.Config{})
}

// newMiaWorldWith is newMiaWorld on a hub started with cfg.
func newMiaWorldWith(t *testing.T, cfg hubtest.Config) *miaWorld {
	t.Helper()
	m := &miaWorld{world: newWorldWith(t, cfg)}
	m.mia, m.miaKey = m.member(t, "mia")
	_, m.noaKey = m.member(t, "noa")
	m.t1 = m.addFork(t, "demo--t1", "main", nil, nil)
	m.set(t, "demo--t1", "owner_id", m.mia)
	return m
}

// untouched moves main of demo on, so that a restack would move each
// change opened on the base, and returns a check that the changes numbered
// (as paths name them) still read as they do now, that no branch of demo or
// of demo--t1 moved, and that no event of a change was written since.
func (m *miaWorld) untouched(t *testing.T, numbers ...string) func(t *testing.T, what string) {
	t.Helper()
	commit(t, m.demo, "main", m.head(t, "main"), "main moved\n", map[string]string{"main.txt": "moved\n"})
	state := func(t *testing.T) string {
		t.Helper()
		out := gitrepotest.Git(t, m.demo, "for-each-ref", "refs/heads/") + "\n" + gitrepotest.Git(t, m.t1, "for-each-ref", "refs/heads/")
		for _, n := range numbers {
			_, _, raw := m.get(t, "/workspaces/demo/changes/"+n, m.ownerKey)
			out += "\n" + string(raw)
		}
		return out
	}
	before, seq := state(t), m.seq(t)
	return func(t *testing.T, what string) {
		t.Helper()
		if after := state(t); after != before {
			t.Errorf("the refused %s changed a change or a branch:\n%s\nwas\n%s", what, after, before)
		}
		for _, e := range m.eventsAfter(t, seq) {
			if strings.HasPrefix(e.Type, "parley.change.") {
				t.Errorf("the refused %s wrote %s", what, e.Type)
			}
		}
	}
}

// caller is mia using an API key of hers.
func (m *miaWorld) caller() change.Caller {
	return change.Caller{UserID: m.mia, Credential: change.Credential{Type: change.CredentialAPIKey, ID: "k"}}
}

// openAsMia opens a change of demo from a new branch of demo--t1 by mia,
// and returns its number as a path names it.
func (m *miaWorld) openAsMia(t *testing.T, branch string) string {
	t.Helper()
	commit(t, m.t1, branch, m.base, branch+"\n\nWhy.\n", map[string]string{branch + ".txt": branch + "\n"})
	ch := m.openBranch(t, branch, func(in *change.OpenInput) {
		in.SourceWorkspace = "demo--t1"
		in.Caller = m.caller()
	})
	return strconv.FormatInt(ch.Number, 10)
}

func TestModify_LetsTheAuthorAWriterAndTheAdminTokenModify(t *testing.T) {
	t.Parallel()
	m := newMiaWorld(t)
	miaPAT := m.IssuePAT(t, identity.PATSpec{UserID: m.mia, Permissions: []string{"changes:write"}, CreatedAt: time.Now().UTC()})
	for i, c := range []struct{ name, credential string }{
		{"the author's API key, with member access only", m.miaKey},
		{"the author's PAT with changes:write", miaPAT},
		{"the owner of the workspace, not the author", m.ownerKey},
		{"the admin token", m.Token},
	} {
		number := m.openAsMia(t, "c"+strconv.Itoa(i))
		// Each route in turn: the edit and the abandon on the open change,
		// the restore on the change the abandon left.
		for _, r := range modifyRoutes {
			if status, out, _ := r.call(t, m.world, "demo", number, c.credential); status != http.StatusOK {
				t.Errorf("%s: %s = %d %v, want 200", c.name, r.name, status, out)
			}
		}
	}
}

func TestModify_RefusesAReaderWhoIsNeitherTheAuthorNorAWriter(t *testing.T) {
	t.Parallel()
	m := newMiaWorld(t)
	number := m.openAsMia(t, "task")
	for _, r := range modifyRoutes {
		t.Run(r.name, func(t *testing.T) {
			untouched := m.untouched(t, number)
			status, out, _ := r.call(t, m.world, "demo", number, m.noaKey)
			if status != http.StatusForbidden || errorType(out) != "forbidden" {
				t.Errorf("%s by noa = %d %v, want 403 forbidden", r.name, status, out)
			}
			untouched(t, r.name)
		})
	}
}

func TestModify_RefusesATokenWithoutChangesWriteAlikeOnEverySlug(t *testing.T) {
	t.Parallel()
	m := newMiaWorld(t)
	number := m.openAsMia(t, "task")
	readPAT, _ := m.patFor(t, []string{"changes:read"}, nil)
	untouched := m.untouched(t, number)
	for _, r := range modifyRoutes {
		status, out, onDemo := r.call(t, m.world, "demo", number, readPAT)
		_, _, onNothing := r.call(t, m.world, "nope", number, readPAT)
		if status != http.StatusForbidden || errorType(out) != "insufficient_scope" {
			t.Errorf("%s = %d %v, want 403 insufficient_scope", r.name, status, out)
		}
		if !bytes.Equal(onDemo, onNothing) {
			t.Errorf("%s on demo answered %s, on a missing workspace %s", r.name, onDemo, onNothing)
		}
	}
	untouched(t, "requests without changes:write")
}

// demo#1 is mia's from demo--t1, demo#2 the owner's from a branch of demo.
func TestModify_AnswersChangeNotFoundAlikeForEveryChangeOutOfReach(t *testing.T) {
	t.Parallel()
	m := newMiaWorld(t)
	fromFork := m.openAsMia(t, "task")
	m.branch(t, "feat", "Feat\n\nWhy.\n")
	fromDemo := strconv.FormatInt(m.openBranch(t, "feat", nil).Number, 10)
	bound := m.IssuePAT(t, identity.PATSpec{UserID: m.mia, Permissions: []string{"changes:read", "changes:write"},
		Workspace: "demo--t1", CreatedAt: time.Now().UTC()})
	untouched := m.untouched(t, fromFork, fromDemo)
	for _, c := range []struct{ name, number, credential string }{
		{"a number with no change", "999", m.ownerKey},
		{"a number that is not decimal", "abc", m.ownerKey},
		{"number zero", "0", m.ownerKey},
		{"a fork-bound token on a change from elsewhere", fromDemo, bound},
	} {
		for _, r := range modifyRoutes {
			if status, out, _ := r.call(t, m.world, "demo", c.number, c.credential); status != http.StatusNotFound || errorType(out) != "change_not_found" {
				t.Errorf("%s: %s = %d %v, want 404 change_not_found", c.name, r.name, status, out)
			}
		}
	}
	untouched(t, "requests for changes out of reach")
	if status, out, _ := modifyRoutes[0].call(t, m.world, "demo", fromFork, bound); status != http.StatusOK {
		t.Errorf("an edit by the fork-bound token of a change from its fork = %d %v, want 200", status, out)
	}
}

func TestModify_HidesTheWorkspaceFromACallerWhoCannotReadIt(t *testing.T) {
	t.Parallel()
	m := newMiaWorld(t)
	number := m.openAsMia(t, "task")
	outsiderKey := m.IssueKey(t, m.CreateUser(t, "outsider"))
	untouched := m.untouched(t, number)
	hidden := func(credential string) {
		t.Helper()
		for _, r := range modifyRoutes {
			status, out, onDemo := r.call(t, m.world, "demo", number, credential)
			_, _, onNothing := r.call(t, m.world, "nope", number, credential)
			if status != http.StatusNotFound || errorType(out) != "not_found" || !bytes.Equal(onDemo, onNothing) {
				t.Errorf("%s = %d %s, want 404 not_found as on a missing workspace: %s", r.name, status, onDemo, onNothing)
			}
		}
		untouched(t, "requests of a caller who cannot read the workspace")
	}
	hidden(outsiderKey)
	if status, out := m.send(t, http.MethodDelete, "/orgs/"+m.org+"/members/"+m.mia, m.Token, nil); status != http.StatusNoContent {
		t.Fatalf("remove mia from acme = %d %v", status, out)
	}
	hidden(m.miaKey)
}

func TestModify_RefusesWhileTheTargetWorkspaceIsNotReady(t *testing.T) {
	t.Parallel()
	m := newMiaWorld(t)
	open := m.openAsMia(t, "task")
	abandoned := m.openAsMia(t, "gone")
	n, _ := strconv.ParseInt(abandoned, 10, 64)
	m.write(t, func(ctx context.Context, tx *sql.Tx) error { return m.changes.SetAbandoned(ctx, tx, "demo", n, "") })
	for _, c := range []struct {
		status    workspace.Status
		errorType string
	}{
		{workspace.StatusPending, "workspace_busy"},
		{workspace.StatusCloning, "workspace_busy"},
		{workspace.StatusFailed, "workspace_not_ready"},
		{workspace.StatusArchived, "workspace_archived"},
	} {
		m.set(t, "demo", "status", string(c.status))
		for _, r := range modifyRoutes {
			number := open
			if r.name == "restore" {
				number = abandoned
			}
			_, before, _ := m.get(t, "/workspaces/demo/changes/"+number, m.Token)
			status, out, _ := r.call(t, m.world, "demo", number, m.Token)
			if status != http.StatusConflict || errorType(out) != c.errorType {
				t.Errorf("%s on a %s workspace = %d %v, want 409 %s", r.name, c.status, status, out, c.errorType)
			}
			_, after, _ := m.get(t, "/workspaces/demo/changes/"+number, m.Token)
			for _, member := range []string{"state", "updated_at", "labels"} {
				if !reflect.DeepEqual(after[member], before[member]) {
					t.Errorf("%s on a %s workspace changed %s to %v, was %v", r.name, c.status, member, after[member], before[member])
				}
			}
		}
	}
}
