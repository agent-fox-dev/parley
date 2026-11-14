package workspace_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// operation is a route that retires or repairs a workspace: the method and
// the part of the path after /api/v1/workspaces/<slug>.
type operation struct{ name, method, suffix string }

// operations are the four such routes.
var operations = []operation{
	{"archive", http.MethodPost, "/archive"},
	{"reactivate", http.MethodPost, "/reactivate"},
	{"reclone", http.MethodPost, "/reclone"},
	{"delete", http.MethodDelete, ""},
}

// operationNamed is the operation named name.
func operationNamed(name string) operation {
	return operations[slices.IndexFunc(operations, func(o operation) bool { return o.name == name })]
}

// operate sends the operation named op on slug, with no body, with cred as
// the bearer credential.
func (w *world) operate(t *testing.T, op, cred, slug string) answer {
	t.Helper()
	o := operationNamed(op)
	status, b := w.Call(t, o.method, "/api/v1/workspaces/"+slug+o.suffix, cred, nil)
	return answer{status, b}
}

// outcome is a's status, followed by its error_type when it is a refusal.
func (a answer) outcome(t *testing.T) string {
	t.Helper()
	if a.status < 300 {
		return fmt.Sprint(a.status)
	}
	return fmt.Sprintf("%d %s", a.status, a.errorType(t))
}

// recordingCollaborators starts a world with opts whose safety-copy pusher
// and teardown set are stubs alone, which record every call: the pusher in
// the stub it returns, the session closer, the archive observer, the
// sandbox remover and a remover of owned data in the log.
func recordingCollaborators(t *testing.T, opts ...option) (*world, *stubBranchesPusher, *teardownLog) {
	t.Helper()
	p := &stubBranchesPusher{answer: func(context.Context, gitrepo.PushBranchesRequest) ([]gitrepo.BranchPush, error) {
		return nil, nil
	}}
	calls := &teardownLog{}
	stub := &stubTeardown{name: "stub", log: calls}
	w := newWorld(t, append(opts, withSafetyPusher(p), withTeardown(func(td *workspace.Teardown) {
		td.Clear()
		td.RegisterSessionCloser(stub)
		td.RegisterArchiveObserver(stub)
		td.RegisterSandboxRemover(stub)
		td.RegisterRemover(stub.remove)
	}))...)
	return w, p, calls
}

func TestMaintenance_EveryOperationIsBusyWhileTheMarkerIsSet(t *testing.T) {
	t.Parallel()
	w, p, calls := recordingCollaborators(t)
	url := "https://origin.test/r.git"
	w.put(t, workspace.Workspace{Slug: "demo", GitURL: &url})
	// alone is what each operation answers by the status alone.
	alone := map[string]map[workspace.Status]string{
		"archive":    {workspace.StatusReady: "200", workspace.StatusFailed: "200", workspace.StatusArchived: "409 workspace_archived"},
		"reactivate": {workspace.StatusReady: "409 workspace_not_archived", workspace.StatusFailed: "409 workspace_not_archived", workspace.StatusArchived: "200"},
		"reclone":    {workspace.StatusReady: "200", workspace.StatusFailed: "200", workspace.StatusArchived: "409 workspace_archived"},
		"delete":     {workspace.StatusReady: "409 workspace_not_archived", workspace.StatusFailed: "409 workspace_not_archived", workspace.StatusArchived: "204"},
	}
	for _, status := range []workspace.Status{workspace.StatusReady, workspace.StatusFailed, workspace.StatusArchived} {
		if _, err := w.Store.Main().Exec(`UPDATE workspaces SET status = ?, maintenance = ? WHERE slug = 'demo'`,
			string(status), string(workspace.MarkerArchive)); err != nil {
			t.Fatal(err)
		}
		before, _ := w.workspace(t, "demo")
		var guard *server.Error
		if !errors.As(workspace.RequireReady(before), &guard) || guard.Type() != "workspace_busy" {
			t.Errorf("the state guard on %s demo with the marker set answers %v, want workspace_busy", status, guard)
		}
		if a := w.postSync(t, w.uKey, "demo", ""); a.outcome(t) != "409 workspace_busy" {
			t.Errorf("sync of %s demo with the marker set = %d %s, want 409 workspace_busy", status, a.status, a.body)
		}
		for _, op := range operations {
			if a := w.operate(t, op.name, w.uKey, "demo"); a.outcome(t) != "409 workspace_busy" {
				t.Errorf("%s of %s demo with the marker set = %d %s, want 409 workspace_busy", op.name, status, a.status, a.body)
			}
			// The same request on a workspace without the marker.
			other := fmt.Sprintf("other-%s-%s", status, op.name)
			w.put(t, workspace.Workspace{Slug: other, Status: status, GitURL: &url})
			if got, want := w.operate(t, op.name, w.uKey, other).outcome(t), alone[op.name][status]; got != want {
				t.Errorf("%s of %s %s = %s, want %s", op.name, status, other, got, want)
			}
		}
		if after, _ := w.workspace(t, "demo"); !reflect.DeepEqual(after, before) {
			t.Errorf("busy requests changed demo from %+v to %+v", before, after)
		}
	}
	recs, _, err := w.events.ReadAfter(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range recs {
		if deref(ev.Workspace) == "demo" {
			t.Errorf("a busy request wrote the event %s for demo", ev.Type)
		}
	}
	for _, line := range calls.recorded() {
		if strings.Contains(line, "demo") {
			t.Errorf("a busy request called a collaborator: %s", line)
		}
	}
	if heads, _ := p.calls(); len(heads) != 0 {
		t.Errorf("the safety copy offered branches %d times, want never", len(heads))
	}
}

func TestMaintenance_RefusesCallersWithoutWriteAccess(t *testing.T) {
	t.Parallel()
	w, p, calls := recordingCollaborators(t)
	acme, url := w.acme(t), "https://origin.test/r.git"
	for _, rec := range []workspace.Workspace{
		{Slug: "team", OrgID: acme, Status: workspace.StatusReady},
		{Slug: "team-archived", OrgID: acme, Status: workspace.StatusArchived},
		{Slug: "own", Status: workspace.StatusReady},
	} {
		rec.GitURL = &url
		w.put(t, rec)
	}
	readOnly := w.pat(t, w.u, "workspaces:read")
	events := w.eventCount(t)
	for _, op := range operations {
		// notFound is the first 404 of the operation, which every other
		// one repeats.
		var notFound [2]string
		for _, c := range []struct {
			name, cred, slug, want string
		}{
			{"an organization member on a ready workspace", w.vKey, "team", "403 forbidden"},
			{"an organization member on an archived workspace", w.vKey, "team-archived", "403 forbidden"},
			{"a user outside the organization", w.vKey, "own", "404 not_found"},
			{"a slug no workspace has", w.uKey, "nothing", "404 not_found"},
			{"the owner's PAT with only workspaces:read", readOnly, "own", "403 insufficient_scope"},
		} {
			before, _ := w.workspace(t, c.slug)
			a := w.operate(t, op.name, c.cred, c.slug)
			if got := a.outcome(t); got != c.want {
				t.Errorf("%s by %s = %d %s, want %s", op.name, c.name, a.status, a.body, c.want)
			}
			if a.status == http.StatusNotFound {
				errorType, message := a.refusal(t)
				if notFound[0] == "" {
					notFound = [2]string{errorType, message}
				} else if notFound != [2]string{errorType, message} {
					t.Errorf("%s by %s answers %s %q, want the same 404 as for the others: %v", op.name, c.name, errorType, message, notFound)
				}
			}
			if after, _ := w.workspace(t, c.slug); !reflect.DeepEqual(after, before) {
				t.Errorf("%s by %s changed the record from %+v to %+v", op.name, c.name, before, after)
			}
		}
	}
	if n := w.eventCount(t); n != events {
		t.Errorf("refused requests wrote %d events, want none", n-events)
	}
	if got := calls.recorded(); len(got) != 0 {
		t.Errorf("refused requests called collaborators: %v, want no call", got)
	}
	if heads, _ := p.calls(); len(heads) != 0 {
		t.Errorf("refused requests offered branches to origin %d times, want never", len(heads))
	}
	if a := w.archive(t, w.Token, "own"); a.status != http.StatusOK {
		t.Errorf("archive by the admin token = %d %s, want 200", a.status, a.body)
	}
}

// tripwire stands for collaborators of an operation: the one named where
// fails, or panics, at each call while mode is "fail" or "panic".
type tripwire struct {
	mu          sync.Mutex
	where, mode string
}

func (tw *tripwire) set(where, mode string) {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	tw.where, tw.mode = where, mode
}

func (tw *tripwire) hit(what string) error {
	tw.mu.Lock()
	where, mode := tw.where, tw.mode
	tw.mu.Unlock()
	switch {
	case what != where:
	case mode == "panic":
		panic("the " + what + " panicked")
	case mode == "fail":
		return errors.New("the " + what + " failed")
	}
	return nil
}

func (tw *tripwire) RemoveSandboxes(context.Context, []string) error {
	return tw.hit("sandbox remover")
}

func (tw *tripwire) WorkspaceArchived(context.Context, string, []string) error {
	return tw.hit("archive observer")
}

func (tw *tripwire) remove(context.Context, *sql.Tx, string) error { return tw.hit("remover") }

func TestMaintenance_TheMarkerIsReleasedWhenTheCallEnds(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, op string
		// status is one the operation takes; where and mode make a
		// collaborator fail or panic, where "event writer" fails the write
		// of the operation's event; want is the answer.
		status      workspace.Status
		where, mode string
		want        string
	}{
		{name: "archive succeeds", op: "archive", status: workspace.StatusReady, want: "200"},
		{name: "archive whose transaction fails", op: "archive", status: workspace.StatusReady,
			where: "event writer", want: "500 internal"},
		{name: "archive whose observer panics", op: "archive", status: workspace.StatusReady,
			where: "archive observer", mode: "panic", want: "500 internal"},
		{name: "reclone whose transaction fails", op: "reclone", status: workspace.StatusReady,
			where: "event writer", want: "500 internal"},
		{name: "reclone whose copy removal panics", op: "reclone", status: workspace.StatusReady,
			where: "copy removal", mode: "panic", want: "500 internal"},
		{name: "delete whose sandbox remover fails", op: "delete", status: workspace.StatusArchived,
			where: "sandbox remover", mode: "fail", want: "500 internal"},
		{name: "delete whose sandbox remover panics", op: "delete", status: workspace.StatusArchived,
			where: "sandbox remover", mode: "panic", want: "500 internal"},
		{name: "delete whose removal step fails", op: "delete", status: workspace.StatusArchived,
			where: "remover", mode: "fail", want: "500 internal"},
		{name: "delete succeeds", op: "delete", status: workspace.StatusArchived, want: "204"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tw := &tripwire{}
			w := newWorld(t, withTeardown(func(td *workspace.Teardown) {
				td.RegisterSandboxRemover(tw)
				td.RegisterArchiveObserver(tw)
				td.RegisterRemover(tw.remove)
			}), withRemoveCopy(func(dir string) error {
				if err := tw.hit("copy removal"); err != nil {
					return err
				}
				return gitrepo.Remove(dir)
			}))
			url := "https://origin.test/r.git"
			w.put(t, workspace.Workspace{Slug: "demo", Status: c.status, GitURL: &url})
			if c.where == "event writer" {
				w.fail(faultEvent)
			} else {
				tw.set(c.where, c.mode)
			}

			if a := w.operate(t, c.op, w.uKey, "demo"); a.outcome(t) != c.want {
				t.Fatalf("%s = %d %s, want %s", c.op, a.status, a.body, c.want)
			}

			rec, found := w.workspace(t, "demo")
			if gone := c.want == "204"; found == gone {
				t.Fatalf("the record of demo is there = %t after the call, want %t", found, !gone)
			}
			if found && rec.Marker != workspace.MarkerNone {
				t.Errorf("the marker is %q after the call, want it released", rec.Marker)
			}
			w.fail("")
			tw.set("", "")
			if !found {
				if a := w.operate(t, c.op, w.uKey, "demo"); a.outcome(t) != "404 not_found" {
					t.Errorf("%s of the deleted workspace = %d %s, want 404 not_found", c.op, a.status, a.body)
				}
				return
			}
			// The workspace qualifies again: the next call is judged by its
			// status alone.
			if _, err := w.Store.Main().Exec(`UPDATE workspaces SET status = ? WHERE slug = 'demo'`, string(c.status)); err != nil {
				t.Fatal(err)
			}
			if a := w.operate(t, c.op, w.uKey, "demo"); a.status >= 300 {
				t.Errorf("the next %s = %d %s, want it to succeed", c.op, a.status, a.body)
			}
		})
	}
}

// withGitOrigins is withGitOrigin for several test origins: the hub's git
// trusts the certificate of each.
func withGitOrigins(t *testing.T, origins ...*gitrepotest.Origin) option {
	t.Helper()
	var bundle []byte
	for _, o := range origins {
		pem, err := os.ReadFile(o.CAFile())
		if err != nil {
			t.Fatal(err)
		}
		bundle = append(bundle, pem...)
	}
	file := filepath.Join(t.TempDir(), "origins.pem")
	if err := os.WriteFile(file, bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	return func(c *hubtest.Config, w *world) {
		withGitOrigin(origins[0])(c, w)
		c.Env["GIT_SSL_CAINFO"] = file
	}
}

// cloneFrom stores the workspace slug with o as its origin and clones it.
func (w *world) cloneFrom(t *testing.T, slug string, o *gitrepotest.Origin) {
	t.Helper()
	url := o.URL()
	w.put(t, workspace.Workspace{Slug: slug, GitURL: &url})
	if rec := w.clone(t, slug); rec.Status != workspace.StatusReady {
		t.Fatalf("the clone of %s left it %s (%v)", slug, rec.Status, deref(rec.CloneError))
	}
}

// send sends method path, with no body, with cred as the bearer
// credential, from its own goroutine, and delivers the status on the
// channel it returns; 0 for a request that got no answer.
func (w *world) send(method, cred, path string) <-chan int {
	done := make(chan int, 1)
	go func() {
		req, err := http.NewRequest(method, w.URL(path), nil)
		if err != nil {
			done <- 0
			return
		}
		req.Header.Set("Authorization", "Bearer "+cred)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- 0
			return
		}
		res.Body.Close()
		done <- res.StatusCode
	}()
	return done
}

func TestMaintenance_OneOperationAtATimeOnAWorkspace(t *testing.T) {
	t.Parallel()
	a, b := gitrepotest.SmartOrigin(t), gitrepotest.SmartOrigin(t)
	a.History()
	b.History()
	w := newWorld(t, withGitOrigins(t, a, b))
	w.cloneFrom(t, "demo", a)
	w.cloneFrom(t, "other", b)
	a.Block()
	asked := a.Requests()

	first := w.send(http.MethodPost, w.uKey, "/api/v1/workspaces/demo/archive")
	eventually(t, "the archive of demo asks origin", func() bool { return a.Requests() > asked })

	for _, op := range []string{"archive", "reclone", "delete"} {
		if got := w.operate(t, op, w.uKey, "demo"); got.outcome(t) != "409 workspace_busy" {
			t.Errorf("%s of demo while it is archived = %d %s, want 409 workspace_busy", op, got.status, got.body)
		}
	}
	if got := w.archive(t, w.uKey, "other"); got.status != http.StatusOK {
		t.Errorf("archive of other while demo is archived = %d %s, want 200", got.status, got.body)
	}
	a.Release()
	if status := <-first; status != http.StatusOK {
		t.Errorf("the first archive of demo = %d, want 200", status)
	}
	counts := map[string]int{}
	recs, _, err := w.events.ReadAfter(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range recs {
		if deref(ev.Workspace) == "demo" {
			counts[ev.Type]++
		}
	}
	if counts[archiveEvent] != 1 || counts[recloneEvent] != 0 || counts[deleteEvent] != 0 {
		t.Errorf("demo's events %v, want one archive event and no reclone or delete", counts)
	}
}

// blockingSandboxes is a sandbox remover that tells started of its call and
// then waits for release.
type blockingSandboxes struct{ started, release chan struct{} }

func (s *blockingSandboxes) RemoveSandboxes(context.Context, []string) error {
	close(s.started)
	<-s.release
	return nil
}

func TestMaintenance_ACallerWhoDisconnectsDoesNotStopTheOperation(t *testing.T) {
	t.Parallel()
	o := gitrepotest.SmartOrigin(t)
	o.History()
	sandboxes := &blockingSandboxes{started: make(chan struct{}), release: make(chan struct{})}
	w := newWorld(t, withGitOrigin(o), withTeardown(func(td *workspace.Teardown) { td.RegisterSandboxRemover(sandboxes) }))
	w.cloneFrom(t, "arc", o)
	w.cloneFrom(t, "rec", o)
	url := o.URL()
	w.put(t, workspace.Workspace{Slug: "del", Status: workspace.StatusArchived, GitURL: &url})
	for _, c := range []struct {
		op, slug, event string
		// done tells whether the record is as the operation leaves it.
		done func(rec workspace.Workspace, found bool) bool
	}{
		{"archive", "arc", archiveEvent, func(rec workspace.Workspace, found bool) bool {
			return found && rec.Status == workspace.StatusArchived && rec.Marker == workspace.MarkerNone
		}},
		{"reclone", "rec", recloneEvent, func(rec workspace.Workspace, found bool) bool {
			return found && rec.Status == workspace.StatusPending && rec.Marker == workspace.MarkerNone
		}},
		{"delete", "del", deleteEvent, func(_ workspace.Workspace, found bool) bool { return !found }},
	} {
		t.Run(c.op, func(t *testing.T) {
			started, release := sandboxes.started, func() { close(sandboxes.release) }
			if c.op != "delete" {
				o.Block()
				asked, reached := o.Requests(), make(chan struct{})
				go func() {
					for o.Requests() == asked {
						time.Sleep(5 * time.Millisecond)
					}
					close(reached)
				}()
				started, release = reached, o.Release
			}
			op := operationNamed(c.op)

			w.sendAndLeave(t, op.method, w.uKey, "/api/v1/workspaces/"+c.slug+op.suffix, started)
			release()

			eventually(t, c.op+" ends as for a connected caller", func() bool {
				rec, found, err := workspace.BySlug(context.Background(), w.Store.Main(), c.slug)
				return err == nil && c.done(rec, found)
			})
			var n int
			for _, ev := range w.eventsOf(t, c.event) {
				if deref(ev.Workspace) == c.slug {
					n++
				}
			}
			if n != 1 {
				t.Errorf("%d %s events for %s, want 1", n, c.event, c.slug)
			}
		})
	}
}
