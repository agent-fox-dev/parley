package workspace_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// recordingCollaborators starts a world with opts whose safety-copy pusher
// and teardown set are stubs alone, which record every call: the pusher in
// the stub it returns, the session closer, the archive observer, the
// sandbox remover and a remover of owned data in the log.
func recordingCollaborators(t *testing.T, opts ...option) (*world, *stubBranchesPusher, *teardownLog) {
	t.Helper()
	p := &stubBranchesPusher{}
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
	w.put(t, workspace.Workspace{Slug: "demo"})
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
			w.put(t, workspace.Workspace{Slug: other, Status: status})
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
	acme := w.acme(t)
	for _, rec := range []workspace.Workspace{
		{Slug: "team", OrgID: acme, Status: workspace.StatusReady},
		{Slug: "team-archived", OrgID: acme, Status: workspace.StatusArchived},
		{Slug: "own", Status: workspace.StatusReady},
	} {
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
	if a := w.operate(t, "archive", w.Token, "own"); a.status != http.StatusOK {
		t.Errorf("archive by the admin token = %d %s, want 200", a.status, a.body)
	}
}

func TestMaintenance_RefusesAWorkspaceItCannotTakeWithoutTouchingIt(t *testing.T) {
	t.Parallel()
	w, p, calls := recordingCollaborators(t)
	// Each status names a repository workspace and one with a fork, the
	// fork in the same status as its parent; each has a copy.
	for _, status := range []workspace.Status{workspace.StatusReady, workspace.StatusFailed, workspace.StatusPending,
		workspace.StatusCloning, workspace.StatusArchived} {
		s := string(status)
		w.put(t, workspace.Workspace{Slug: s, Status: status})
		w.put(t, workspace.Workspace{Slug: s + "-forked", Status: status})
		w.put(t, workspace.Workspace{Slug: s + "-forked--t1", Kind: access.KindFork, ParentSlug: new(s + "-forked"), Status: status})
		for _, slug := range []string{s, s + "-forked", s + "-forked--t1"} {
			makeCopy(t, w.svc.RepoPath(slug))
		}
	}
	events := w.eventCount(t)
	for _, c := range []struct{ op, slug, want string }{
		{"archive", "ready-forked--t1", "400 not_supported_on_fork"},
		{"archive", "archived", "409 workspace_archived"},
		{"archive", "pending", "409 workspace_busy"},
		{"archive", "cloning", "409 workspace_busy"},
		{"reactivate", "archived-forked--t1", "400 not_supported_on_fork"},
		{"reactivate", "ready", "409 workspace_not_archived"},
		{"reactivate", "failed", "409 workspace_not_archived"},
		{"reactivate", "pending", "409 workspace_not_archived"},
		{"reactivate", "cloning", "409 workspace_not_archived"},
		{"reclone", "ready-forked--t1", "400 fork_has_no_origin"},
		{"reclone", "archived", "409 workspace_archived"},
		{"reclone", "archived-forked", "409 workspace_archived"},
		{"reclone", "pending", "409 workspace_busy"},
		{"reclone", "cloning-forked", "409 workspace_busy"},
		{"reclone", "ready-forked", "409 has_forks"},
		{"reclone", "failed-forked", "409 has_forks"},
		{"delete", "ready", "409 workspace_not_archived"},
		{"delete", "failed", "409 workspace_not_archived"},
		{"delete", "pending", "409 workspace_not_archived"},
		{"delete", "cloning", "409 workspace_not_archived"},
	} {
		before, _ := w.workspace(t, c.slug)
		if a := w.operate(t, c.op, w.uKey, c.slug); a.outcome(t) != c.want {
			t.Errorf("%s of %s = %d %s, want %s", c.op, c.slug, a.status, a.body, c.want)
		}
		if after, _ := w.workspace(t, c.slug); !reflect.DeepEqual(after, before) {
			t.Errorf("%s of %s changed the record from %+v to %+v", c.op, c.slug, before, after)
		}
		if !exists(filepath.Join(w.svc.RepoPath(c.slug), "HEAD")) {
			t.Errorf("%s of %s removed its copy", c.op, c.slug)
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
	if got := w.notifications(); len(got) != 0 {
		t.Errorf("refused requests told the clone workers about %v, want nothing", got)
	}
}

func TestMaintenance_TheCallEndsCleanlyWhateverItsCollaboratorsDo(t *testing.T) {
	t.Parallel()
	var log hubtest.LogBuffer
	stubs := map[string]*stubTeardown{}
	for _, name := range []string{"archive observer", "copy removal", "sandbox remover", "remover"} {
		stubs[name] = &stubTeardown{name: name}
	}
	w := newWorld(t, withLog(&log), withTeardown(func(td *workspace.Teardown) {
		td.RegisterSandboxRemover(stubs["sandbox remover"])
		td.RegisterArchiveObserver(stubs["archive observer"])
		td.RegisterRemover(stubs["remover"].remove)
	}), withRemoveCopy(func(dir string) error {
		if err := stubs["copy removal"].remove(context.Background(), nil, dir); err != nil {
			return err
		}
		return gitrepo.Remove(dir)
	}))
	for i, c := range []struct {
		name, op string
		// status is one the operation takes; the collaborator where fails,
		// or panics when panics is set; want is the answer.
		status workspace.Status
		where  string
		panics bool
		want   string
	}{
		{name: "archive succeeds", op: "archive", status: workspace.StatusReady, want: "200"},
		{name: "archive whose observer panics", op: "archive", status: workspace.StatusReady,
			where: "archive observer", panics: true, want: "200"},
		{name: "reclone whose copy removal fails", op: "reclone", status: workspace.StatusReady,
			where: "copy removal", want: "200"},
		{name: "reclone whose copy removal panics", op: "reclone", status: workspace.StatusReady,
			where: "copy removal", panics: true, want: "200"},
		{name: "delete whose sandbox remover fails", op: "delete", status: workspace.StatusArchived,
			where: "sandbox remover", want: "500 internal"},
		{name: "delete whose sandbox remover panics", op: "delete", status: workspace.StatusArchived,
			where: "sandbox remover", panics: true, want: "500 internal"},
		{name: "delete whose removal step fails", op: "delete", status: workspace.StatusArchived,
			where: "remover", want: "500 internal"},
		{name: "delete succeeds", op: "delete", status: workspace.StatusArchived, want: "204"},
	} {
		t.Run(c.name, func(t *testing.T) {
			slug := fmt.Sprintf("demo%d", i)
			w.put(t, workspace.Workspace{Slug: slug, Status: c.status})
			if c.where != "" {
				stubs[c.where].set(errors.New("the "+c.where+" failed"), c.panics)
			}

			if a := w.operate(t, c.op, w.uKey, slug); a.outcome(t) != c.want {
				t.Fatalf("%s = %d %s, want %s", c.op, a.status, a.body, c.want)
			}

			rec, found := w.workspace(t, slug)
			if c.op == "reclone" && (rec.Status != workspace.StatusPending || !slices.Contains(w.notifications(), slug)) {
				t.Errorf("after the reclone %s is %s and the clone workers were told of %v, want pending and told", slug, rec.Status, w.notifications())
			}
			// A failure the call answers success despite is logged with the slug.
			failure := "the " + c.where + " failed"
			if c.panics {
				failure = "the " + c.where + " panicked"
			}
			if c.where != "" && c.want == "200" && (!strings.Contains(log.String(), "slug="+slug) || !strings.Contains(log.String(), failure)) {
				t.Errorf("the log does not report %q with the slug %s:\n%s", failure, slug, log.String())
			}
			if gone := c.want == "204"; found == gone {
				t.Fatalf("the record of %s is there = %t after the call, want %t", slug, found, !gone)
			}
			if found && rec.Marker != workspace.MarkerNone {
				t.Errorf("the marker is %q after the call, want it released", rec.Marker)
			}
			if c.where != "" {
				stubs[c.where].set(nil, false)
			}
			if !found {
				if a := w.operate(t, c.op, w.uKey, slug); a.outcome(t) != "404 not_found" {
					t.Errorf("%s of the deleted workspace = %d %s, want 404 not_found", c.op, a.status, a.body)
				}
				return
			}
			// The workspace qualifies again: the next call is judged by its
			// status alone.
			if _, err := w.Store.Main().Exec(`UPDATE workspaces SET status = ? WHERE slug = ?`, string(c.status), slug); err != nil {
				t.Fatal(err)
			}
			if a := w.operate(t, c.op, w.uKey, slug); a.status >= 300 {
				t.Errorf("the next %s = %d %s, want it to succeed", c.op, a.status, a.body)
			}
		})
	}
}

func TestMaintenance_TheMarkerAdmitsOneOperationPerWorkspaceAtATime(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(t, workspace.Workspace{Slug: "demo", Status: workspace.StatusReady})
	w.put(t, workspace.Workspace{Slug: "other", Status: workspace.StatusReady})
	ctx := context.Background()
	const n = 8
	start, release := make(chan struct{}), make(chan struct{})
	free := sync.OnceFunc(func() { close(release) })
	defer free()
	var ran atomic.Int32
	results := make(chan error, n)
	for range n {
		go func() {
			<-start
			results <- w.svc.WithMarker(ctx, "demo", workspace.MarkerArchive, func() error {
				ran.Add(1)
				<-release
				return nil
			})
		}()
	}
	close(start)
	for i := range n - 1 {
		select {
		case err := <-results:
			var guard *server.Error
			if !errors.As(err, &guard) || guard.Type() != "workspace_busy" {
				t.Errorf("a call that lost the race answered %v, want workspace_busy", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%d of %d calls were refused while one held the marker of demo, want %d", i, n, n-1)
		}
	}
	other := false
	if err := w.svc.WithMarker(ctx, "other", workspace.MarkerReclone, func() error { other = true; return nil }); err != nil || !other {
		t.Errorf("the marker of other while demo holds one: ran %t, %v; want it run", other, err)
	}
	free()
	if err := <-results; err != nil {
		t.Errorf("the call that holds the marker answered %v, want nil", err)
	}
	if got := ran.Load(); got != 1 {
		t.Errorf("%d calls ran while holding the marker of demo, want 1", got)
	}
	for _, slug := range []string{"demo", "other"} {
		if rec, _ := w.workspace(t, slug); rec.Marker != workspace.MarkerNone {
			t.Errorf("the marker of %s is %q afterwards, want it released", slug, rec.Marker)
		}
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

// send sends method path, with body as JSON (nil for none) and cred as the
// bearer credential, from its own goroutine, and delivers the status on
// the channel it returns; 0 for a request that got no answer.
func (w *world) send(method, cred, path string, body any) <-chan int {
	done := make(chan int, 1)
	go func() {
		var r io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			r = bytes.NewReader(b)
		}
		req, err := http.NewRequest(method, w.URL(path), r)
		if err != nil {
			done <- 0
			return
		}
		req.Header.Set("Authorization", "Bearer "+cred)
		req.Header.Set("Content-Type", "application/json")
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

	first := w.send(http.MethodPost, w.uKey, "/api/v1/workspaces/demo/archive", nil)
	eventually(t, "the archive of demo asks origin", func() bool { return a.Requests() > asked })

	for _, op := range []string{"archive", "reclone", "delete"} {
		if got := w.operate(t, op, w.uKey, "demo"); got.outcome(t) != "409 workspace_busy" {
			t.Errorf("%s of demo while it is archived = %d %s, want 409 workspace_busy", op, got.status, got.body)
		}
	}
	if got := w.operate(t, "archive", w.uKey, "other"); got.status != http.StatusOK {
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

func TestMaintenance_ACallerWhoDisconnectsDoesNotStopTheOperation(t *testing.T) {
	t.Parallel()
	o := gitrepotest.SmartOrigin(t)
	o.History()
	sandboxes := &stubTeardown{name: "sandboxes", started: make(chan struct{}), release: make(chan struct{})}
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
