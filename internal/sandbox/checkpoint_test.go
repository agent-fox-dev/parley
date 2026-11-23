package sandbox_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	mathrand "math/rand/v2"
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

	"github.com/agent-fox-dev/parley/internal/hubtest"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

func TestCheckpoint_SavesTheBranchesAndChangesTheHubLacks(t *testing.T) {
	t.Parallel()
	h := startCheckpointHub(t)
	fork := h.workspaces.RepoPath("demo--t1")
	for _, tc := range []struct {
		name string
		// work changes the cloned working directory dir and returns the
		// refs the checkpoint must store; a dirty one's sha is checked
		// through files and absent.
		work func(t *testing.T, dir string) []sandbox.CheckpointRef
		// files are paths and contents the tree of the dirty ref holds,
		// absent paths it does not.
		files  map[string]string
		absent []string
		result string
	}{
		{name: "a clean tree whose heads the fork has", result: "empty",
			work: func(*testing.T, string) []sandbox.CheckpointRef { return nil }},
		{name: "an unpushed commit on t1", result: "ok", work: func(t *testing.T, dir string) []sandbox.CheckpointRef {
			return []sandbox.CheckpointRef{{Branch: "t1", SHA: commitWork(t, dir, "work", map[string]string{"f": "x\n"})}}
		}},
		{name: "unpushed commits on t1 and on x", result: "ok", work: func(t *testing.T, dir string) []sandbox.CheckpointRef {
			// Another content than the row before, whose commit the fork
			// holds now, so that this commit is another.
			t1 := commitWork(t, dir, "work", map[string]string{"f": "on t1\n"})
			workGit(t, dir, "checkout", "--quiet", "-b", "x")
			x := commitWork(t, dir, "more", map[string]string{"g": "y\n"})
			workGit(t, dir, "checkout", "--quiet", "t1")
			return []sandbox.CheckpointRef{{Branch: "t1", SHA: t1}, {Branch: "x", SHA: x}}
		}},
		{name: "t1 pushed and a dirty tree", result: "ok",
			files: map[string]string{"README": "demo\nchanged\n", "new.txt": "new\n"}, absent: []string{"build.log"},
			work: func(t *testing.T, dir string) []sandbox.CheckpointRef {
				commitWork(t, dir, "ignore", map[string]string{".gitignore": "build.log\n"})
				workGit(t, dir, "push", "--quiet", fork, "t1")
				writeWork(t, dir, map[string]string{"README": "demo\nchanged\n", "new.txt": "new\n", "build.log": "log\n"})
				return []sandbox.CheckpointRef{{Branch: "t1", Dirty: true}}
			}},
		{name: "no branch checked out and a modified file", result: "ok", files: map[string]string{"README": "detached\n"},
			work: func(t *testing.T, dir string) []sandbox.CheckpointRef {
				workGit(t, dir, "checkout", "--quiet", "--detach")
				writeWork(t, dir, map[string]string{"README": "detached\n"})
				return []sandbox.CheckpointRef{{Branch: "HEAD", Dirty: true}}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, dir := h.workSandbox(t, nil)
			h.cloneWork(t, dir)
			want := tc.work(t, dir)
			parent := workGit(t, dir, "rev-parse", "HEAD")
			before := workState(t, dir)
			metric := h.rec.values(sandbox.CheckpointsMetric)

			cp, err := h.svc.Checkpoint(context.Background(), storedRecord(t, h.Hub, id), sandbox.SystemActor)
			if err != nil || cp == nil {
				t.Fatalf("Checkpoint = %+v, %v; want a checkpoint", cp, err)
			}
			if !cp.OK || cp.Error != nil || !cp.At.Equal(h.clock.Now()) {
				t.Errorf("Checkpoint = ok %t, error %v, at %v; want ok, no error, at %v", cp.OK, cp.Error, cp.At, h.clock.Now())
			}
			if len(cp.Refs) != len(want) {
				t.Fatalf("refs = %+v; want %+v", cp.Refs, want)
			}
			refs := map[string]string{}
			for i, w := range want {
				got := cp.Refs[i]
				if got.Branch != w.Branch || got.Dirty != w.Dirty || (w.SHA != "" && got.SHA != w.SHA) || len(got.SHA) != 40 {
					t.Errorf("ref %d = %+v; want %+v", i, got, w)
				}
				refs[got.Branch] = got.SHA
				if got.Dirty {
					if parents := gitrepotest.Git(t, fork, "rev-list", "--parents", "-n", "1", got.SHA); parents != got.SHA+" "+parent {
						t.Errorf("the dirty commit and its parents are %q; want the head %s as its only parent", parents, parent)
					}
					for name, content := range tc.files {
						if b := gitrepotest.Blob(t, fork, got.SHA+":"+name); string(b) != content {
							t.Errorf("%s in the dirty commit = %q; want %q", name, b, content)
						}
					}
					files := strings.Fields(gitrepotest.Git(t, fork, "ls-tree", "-r", "--name-only", got.SHA))
					for _, name := range tc.absent {
						if slices.Contains(files, name) {
							t.Errorf("the dirty commit holds %s, which git ignores", name)
						}
					}
				}
			}
			if stored := h.checkpointRefs(t, id); !maps.Equal(stored, refs) {
				t.Errorf("the workspace's checkpoint refs = %v; want %v", stored, refs)
			}
			if stored := h.storedCheckpoint(t, id); !reflect.DeepEqual(stored, cp) {
				t.Errorf("the stored checkpoint = %+v; want %+v", stored, cp)
			}
			if after := workState(t, dir); after != before {
				t.Errorf("the working directory changed:\n%s\nwant\n%s", after, before)
			}
			if left, _ := filepath.Glob(filepath.Join(dir, ".git", "parley-checkpoint*")); len(left) != 0 {
				t.Errorf("the checkpoint left %v in the sandbox", left)
			}
			evs := h.sandboxEvents(t, sandbox.EventCheckpoint, id)
			if len(evs) != 1 {
				t.Fatalf("%d checkpoint events; want 1", len(evs))
			}
			ev := evs[0]
			if ev.actorType != "system" || ev.resourceType != "sandbox" || ev.workspace == nil || *ev.workspace != "demo--t1" ||
				ev.session == nil || *ev.session != t1Session || !reflect.DeepEqual(ev.metadata, checkpointMeta(t, cp)) {
				t.Errorf("the event = %+v (workspace %v, session %v); want one by the system about the sandbox with %v",
					ev, ev.workspace, ev.session, checkpointMeta(t, cp))
			}
			if d := metricDelta(metric, h.rec.values(sandbox.CheckpointsMetric)); !maps.Equal(d, map[string]float64{"result=" + tc.result: 1}) {
				t.Errorf("%s added %v; want result=%s 1", sandbox.CheckpointsMetric, d, tc.result)
			}
		})
	}
}

func TestCheckpoint_WritesNoChangeVersionOrOtherEventAndMovesItsRef(t *testing.T) {
	t.Parallel()
	h := startCheckpointHub(t)
	id, dir := h.workSandbox(t, nil)
	h.cloneWork(t, dir)
	commitWork(t, dir, "work", map[string]string{"f": "x\n"})
	writeWork(t, dir, map[string]string{"f": "dirty\n"})
	heads := func() string {
		return strings.Join([]string{
			gitrepotest.Git(t, h.workspaces.RepoPath("demo"), "for-each-ref", "refs/heads/"),
			gitrepotest.Git(t, h.workspaces.RepoPath("demo--t1"), "for-each-ref", "refs/heads/"),
		}, "\n")
	}
	var seq int64
	if err := h.Store.Main().QueryRow(`SELECT coalesce(max(seq), 0) FROM hub_events`).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	changes, versions, branches := h.count(t, `SELECT count(*) FROM changes`), h.count(t, `SELECT count(*) FROM change_versions`), heads()

	if _, err := h.svc.Checkpoint(context.Background(), storedRecord(t, h.Hub, id), sandbox.SystemActor); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	var types []string
	rows, err := h.Store.Main().Query(`SELECT type FROM hub_events WHERE seq > ? ORDER BY seq`, seq)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var typ string
		if err := rows.Scan(&typ); err != nil {
			t.Fatal(err)
		}
		types = append(types, typ)
	}
	_ = rows.Close()
	if !slices.Equal(types, []string{sandbox.EventCheckpoint}) {
		t.Errorf("the checkpoint wrote the events %v; want one %s", types, sandbox.EventCheckpoint)
	}
	if c, v := h.count(t, `SELECT count(*) FROM changes`), h.count(t, `SELECT count(*) FROM change_versions`); c != changes || v != versions {
		t.Errorf("changes %d → %d, versions %d → %d; want none added", changes, c, versions, v)
	}
	if got := heads(); got != branches {
		t.Errorf("the branches of demo and demo--t1 are\n%s\nwant\n%s", got, branches)
	}

	head := commitWork(t, dir, "again", nil)
	cp, err := h.svc.Checkpoint(context.Background(), storedRecord(t, h.Hub, id), sandbox.SystemActor)
	if err != nil {
		t.Fatalf("the second Checkpoint: %v", err)
	}
	if want := []sandbox.CheckpointRef{{Branch: "t1", SHA: head}}; !slices.Equal(cp.Refs, want) {
		t.Errorf("the second checkpoint's refs = %+v; want %+v", cp.Refs, want)
	}
	if refs := h.checkpointRefs(t, id); !maps.Equal(refs, map[string]string{"t1": head}) {
		t.Errorf("the checkpoint refs are %v; want t1 at %s", refs, head)
	}

	if err := h.svc.Delete(context.Background(), storedRecord(t, h.Hub, id), sandbox.DeleteAPI, sandbox.SystemActor, false); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if refs := h.checkpointRefs(t, id); !maps.Equal(refs, map[string]string{"t1": head}) {
		t.Errorf("after the sandbox was deleted the checkpoint refs are %v; want t1 at %s", refs, head)
	}
}

// Git keeps no ref "a/b" beside a ref "a", so a branch of a later
// checkpoint can clash with the ref of a branch an earlier one saved.
func TestCheckpoint_ReplacesTheEarlierRefItsBranchClashesWith(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, earlier, later string }{
		{"a branch below an earlier one", "feature", "feature/a"},
		{"a branch above an earlier one", "feature/a", "feature"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := startCheckpointHub(t)
			id, dir := h.workSandbox(t, nil)
			h.cloneWork(t, dir)
			branch := func(name string) string {
				workGit(t, dir, "checkout", "--quiet", "-b", name, "t1")
				head := commitWork(t, dir, name, map[string]string{"f": name + "\n"})
				workGit(t, dir, "checkout", "--quiet", "t1")
				return head
			}
			kept, earlier := branch("kept"), branch(tc.earlier)
			if _, err := h.svc.Checkpoint(context.Background(), storedRecord(t, h.Hub, id), sandbox.SystemActor); err != nil {
				t.Fatalf("the first Checkpoint: %v", err)
			}
			if refs := h.checkpointRefs(t, id); !maps.Equal(refs, map[string]string{"kept": kept, tc.earlier: earlier}) {
				t.Fatalf("after the first checkpoint the refs are %v", refs)
			}

			workGit(t, dir, "branch", "--quiet", "-D", "kept", tc.earlier)
			later := branch(tc.later)
			cp, err := h.svc.Checkpoint(context.Background(), storedRecord(t, h.Hub, id), sandbox.SystemActor)
			if err != nil {
				t.Fatalf("the second Checkpoint: %v", err)
			}
			if want := []sandbox.CheckpointRef{{Branch: tc.later, SHA: later}}; !cp.OK || !slices.Equal(cp.Refs, want) {
				t.Errorf("the second checkpoint is ok %v with refs %+v; want ok with %+v", cp.OK, cp.Refs, want)
			}
			if refs, want := h.checkpointRefs(t, id), map[string]string{"kept": kept, tc.later: later}; !maps.Equal(refs, want) {
				t.Errorf("after the second checkpoint the refs are %v; want %v", refs, want)
			}
		})
	}
}

func TestCheckpoint_WithoutARepositoryStoresNoRef(t *testing.T) {
	t.Parallel()
	h := startCheckpointHub(t)
	for _, tc := range []struct {
		name string
		edit func(*sandbox.Record)
		// clone makes the working directory a clone with an unpushed
		// commit rather than a directory of files.
		clone bool
		// none is true for a sandbox without a hub credential, which gets
		// no checkpoint at all.
		none bool
	}{
		{name: "a sandbox created without a checkout", edit: func(r *sandbox.Record) { r.Checkout = nil }},
		{name: "a working directory that is no git repository"},
		{name: "a sandbox without hub access", clone: true, none: true, edit: func(r *sandbox.Record) {
			r.HubAccess, r.Snapshot.HubAccess = nil, "none"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, dir := h.workSandbox(t, tc.edit)
			if tc.clone {
				h.cloneWork(t, dir)
				commitWork(t, dir, "work", map[string]string{"f": "x\n"})
			} else {
				writeWork(t, dir, map[string]string{"notes.txt": "plain\n"})
			}
			metric := h.rec.values(sandbox.CheckpointsMetric)
			cp, err := h.svc.Checkpoint(context.Background(), storedRecord(t, h.Hub, id), sandbox.SystemActor)
			if err != nil {
				t.Fatalf("Checkpoint: %v", err)
			}
			evs := h.sandboxEvents(t, sandbox.EventCheckpoint, id)
			delta := metricDelta(metric, h.rec.values(sandbox.CheckpointsMetric))
			if tc.none {
				execs := 0
				for _, c := range h.rt.Calls() {
					if c.Op == runtime.OpExec && c.Target == id {
						execs++
					}
				}
				if cp != nil || h.storedCheckpoint(t, id) != nil || len(evs) != 0 || execs != 0 || len(delta) != 0 {
					t.Errorf("Checkpoint = %+v, stored %+v, %d events, %d commands, metric %v; want nothing at all",
						cp, h.storedCheckpoint(t, id), len(evs), execs, delta)
				}
			} else {
				want := &sandbox.CheckpointState{At: h.clock.Now(), OK: true, Refs: []sandbox.CheckpointRef{}}
				if !reflect.DeepEqual(cp, want) || !reflect.DeepEqual(h.storedCheckpoint(t, id), want) {
					t.Errorf("Checkpoint = %+v, stored %+v; want %+v", cp, h.storedCheckpoint(t, id), want)
				}
				if len(evs) != 1 || !reflect.DeepEqual(evs[0].metadata, map[string]any{"ok": true, "refs": []any{}, "error": nil}) {
					t.Errorf("the checkpoint events = %+v; want one ok without refs", evs)
				}
				if !maps.Equal(delta, map[string]float64{"result=empty": 1}) {
					t.Errorf("%s added %v; want result=empty 1", sandbox.CheckpointsMetric, delta)
				}
			}
			if refs := h.checkpointRefs(t, id); len(refs) != 0 {
				t.Errorf("checkpoint refs %v; want none", refs)
			}
		})
	}
}

func TestCheckpoint_LeavesOutUntrackedFilesOverTheSizeLimit(t *testing.T) {
	t.Parallel()
	h := startCheckpointHub(t)
	fork := h.workspaces.RepoPath("demo--t1")
	for _, tc := range []struct {
		name  string
		limit int64
		size  int64
		saved bool
	}{
		{"a file a byte over a limit of 1 KiB", 1024, 1025, false},
		{"a file of exactly 1 KiB", 1024, 1024, true},
		{"a file a byte over the default limit", 0, 100*1024*1024 + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sandbox.SetCheckpointFileLimit(h.svc, tc.limit)
			t.Cleanup(func() { sandbox.SetCheckpointFileLimit(h.svc, 0) })
			id, dir := h.workSandbox(t, nil)
			h.cloneWork(t, dir)
			writeWork(t, dir, map[string]string{"a.txt": "small\n"})
			f, err := os.Create(filepath.Join(dir, "big.bin"))
			if err == nil {
				err = errors.Join(f.Truncate(tc.size), f.Close())
			}
			if err != nil {
				t.Fatal(err)
			}
			calls := len(h.rt.Calls())

			cp, err := h.svc.Checkpoint(context.Background(), storedRecord(t, h.Hub, id), sandbox.SystemActor)
			if err != nil {
				t.Fatalf("Checkpoint: %v", err)
			}
			// The sizes come from one command in the sandbox, not a runtime
			// call per untracked file, of which there may be many.
			if ops := h.sandboxCalls(id, calls); slices.Contains(ops, runtime.OpStat) {
				t.Errorf("the runtime calls of the checkpoint = %v; want no stat", ops)
			}
			wantErr := "skipped: big.bin"
			if tc.saved {
				wantErr = ""
			}
			gotErr := ""
			if cp.Error != nil {
				gotErr = *cp.Error
			}
			if !cp.OK || gotErr != wantErr || len(cp.Refs) != 1 || !cp.Refs[0].Dirty {
				t.Fatalf("Checkpoint = ok %t, error %q, refs %+v; want ok, error %q and one dirty ref", cp.OK, gotErr, cp.Refs, wantErr)
			}
			files := strings.Fields(gitrepotest.Git(t, fork, "ls-tree", "-r", "--name-only", cp.Refs[0].SHA))
			if !slices.Contains(files, "a.txt") || slices.Contains(files, "big.bin") != tc.saved {
				t.Errorf("the dirty commit holds %v; want a.txt, and big.bin %t", files, tc.saved)
			}
		})
	}
}

func TestCheckpoint_IsHeldToTheMaximumPushSize(t *testing.T) {
	t.Parallel()
	const limit = 64 << 10
	h := startCheckpointHubWith(t, fmt.Sprintf("[git]\nmax_push_size = \"%d\"\n", limit))
	for _, tc := range []struct {
		name string
		// size is the number of incompressible bytes committed on t1.
		size  int
		saved bool
	}{
		{"a commit whose bundle is under the limit", 1 << 10, true},
		{"a commit whose bundle is over the limit", 4 * limit, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, dir := h.workSandbox(t, nil)
			h.cloneWork(t, dir)
			content := make([]byte, tc.size)
			for i := range content {
				content[i] = byte(mathrand.IntN(256))
			}
			head := commitWork(t, dir, "big", map[string]string{"big.bin": string(content)})
			metric := h.rec.values(sandbox.CheckpointsMetric)

			cp, err := h.svc.Checkpoint(context.Background(), storedRecord(t, h.Hub, id), sandbox.SystemActor)
			if tc.saved {
				if err != nil || !cp.OK || !slices.Equal(cp.Refs, []sandbox.CheckpointRef{{Branch: "t1", SHA: head}}) {
					t.Fatalf("Checkpoint = %+v, %v; want ok with t1 at %s", cp, err, head)
				}
				return
			}
			if err == nil || cp == nil || cp.OK || cp.Error == nil || !strings.Contains(*cp.Error, fmt.Sprintf("maximum push size (%d bytes)", limit)) {
				t.Fatalf("Checkpoint = %+v, %v; want a failure naming the maximum push size of %d bytes", cp, err, limit)
			}
			if len(cp.Refs) != 0 {
				t.Errorf("the failed checkpoint has refs %+v; want none", cp.Refs)
			}
			if refs := h.checkpointRefs(t, id); len(refs) != 0 {
				t.Errorf("the checkpoint refs are %v; want none stored", refs)
			}
			if d := metricDelta(metric, h.rec.values(sandbox.CheckpointsMetric)); !maps.Equal(d, map[string]float64{"result=failed": 1}) {
				t.Errorf("%s added %v; want result=failed 1", sandbox.CheckpointsMetric, d)
			}
		})
	}
}

func TestCheckpoint_RecordsAFailureAndKeepsTheLastRefs(t *testing.T) {
	t.Parallel()
	h := startCheckpointHub(t)
	fork := h.workspaces.RepoPath("demo--t1")
	id, dir := h.workSandbox(t, nil)
	h.cloneWork(t, dir)
	commitWork(t, dir, "work", map[string]string{"f": "x\n"})
	previous := []sandbox.CheckpointRef{{Branch: "t1", SHA: h.base}}
	for _, fault := range []struct {
		name string
		// set makes the fault and returns what ends it; held, when set, is
		// closed once the attempt waits in it for the hub's clock.
		set func(held chan struct{}) func()
		// bound is true for the fault the attempt's bound ends.
		bound bool
	}{
		{name: "the runtime cannot be reached", set: func(chan struct{}) func() {
			h.rt.SetUnreachable(true)
			return func() { h.rt.SetUnreachable(false) }
		}},
		{name: "git exits with 128 in the sandbox", set: func(chan struct{}) func() {
			h.hooks.setExec(func(argv []string) (int, string, error) {
				if slices.Contains(argv, "bundle") {
					return 128, "fatal: cannot create the bundle", nil
				}
				return 0, "", nil
			})
			return func() { h.hooks.setExec(nil) }
		}},
		{name: "the download fails after half the bytes", set: func(chan struct{}) func() {
			h.hooks.setDownloadErr(errors.New("connection reset by peer"))
			return func() { h.hooks.setDownloadErr(nil) }
		}},
		{name: "a command runs past the bound", bound: true, set: func(held chan struct{}) func() {
			var once sync.Once
			h.hooks.set(func(ctx context.Context, op, _ string) error {
				if op != runtime.OpExec {
					return nil
				}
				once.Do(func() { close(held) })
				<-ctx.Done()
				return runtime.NewError(runtime.ErrUnavailable, op, ctx.Err().Error())
			})
			return func() { h.hooks.set(nil) }
		}},
	} {
		for _, prior := range []struct {
			name string
			refs []sandbox.CheckpointRef
		}{{"a first checkpoint", []sandbox.CheckpointRef{}}, {"after a successful one", previous}} {
			t.Run(fault.name+", "+prior.name, func(t *testing.T) {
				h.insert(t, func(ctx context.Context, tx *sql.Tx) error {
					var raw json.RawMessage
					if len(prior.refs) > 0 {
						b, err := json.Marshal(sandbox.CheckpointState{At: recordStart, OK: true, Refs: prior.refs})
						if err != nil {
							return err
						}
						raw = b
					}
					return sandbox.Update(ctx, tx, id, sandbox.SetCheckpoint(raw))
				})
				ref := sandbox.CheckpointRefPrefix + id + "/t1"
				if len(prior.refs) > 0 {
					gitrepotest.Git(t, fork, "update-ref", ref, h.base)
				} else {
					gitrepotest.Git(t, fork, "update-ref", "-d", ref)
				}
				refsBefore := h.checkpointRefs(t, id)
				events := len(h.sandboxEvents(t, sandbox.EventCheckpoint, id))
				metric := h.rec.values(sandbox.CheckpointsMetric)
				held := make(chan struct{})
				t.Cleanup(fault.set(held))

				type result struct {
					cp  *sandbox.CheckpointState
					err error
				}
				done := make(chan result, 1)
				go func() {
					cp, err := h.svc.Checkpoint(context.Background(), storedRecord(t, h.Hub, id), sandbox.SystemActor)
					done <- result{cp, err}
				}()
				if fault.bound {
					hubtestWait(t, held, "the attempt to wait in a command")
					hubtest.Eventually(t, "the attempt's bound", func() bool { return h.timers.live(sandbox.CheckpointBound) > 0 })
					h.clock.Advance(sandbox.CheckpointBound)
				}
				var r result
				select {
				case r = <-done:
				case <-time.After(10 * time.Second):
					t.Fatal("the checkpoint did not end within 10 s")
				}
				cp := r.cp
				if r.err == nil || cp == nil || cp.OK || cp.Error == nil || *cp.Error == "" || len(*cp.Error) > 1024 {
					t.Fatalf("Checkpoint = %+v, %v; want a failure with its reason", cp, r.err)
				}
				if !slices.Equal(cp.Refs, prior.refs) || !cp.At.Equal(h.clock.Now()) {
					t.Errorf("the failed checkpoint has refs %+v at %v; want %+v at %v", cp.Refs, cp.At, prior.refs, h.clock.Now())
				}
				if stored := h.storedCheckpoint(t, id); !reflect.DeepEqual(stored, cp) {
					t.Errorf("the stored checkpoint = %+v; want %+v", stored, cp)
				}
				if refs := h.checkpointRefs(t, id); !maps.Equal(refs, refsBefore) {
					t.Errorf("the checkpoint refs are %v; want them as they were, %v", refs, refsBefore)
				}
				evs := h.sandboxEvents(t, sandbox.EventCheckpoint, id)
				if len(evs) != events+1 || !reflect.DeepEqual(evs[len(evs)-1].metadata, checkpointMeta(t, cp)) {
					t.Errorf("the checkpoint events = %+v; want one more with %v", evs, checkpointMeta(t, cp))
				}
				if d := metricDelta(metric, h.rec.values(sandbox.CheckpointsMetric)); !maps.Equal(d, map[string]float64{"result=failed": 1}) {
					t.Errorf("%s added %v; want result=failed 1", sandbox.CheckpointsMetric, d)
				}
				if state := storedRecord(t, h.Hub, id).State; state != sandbox.StateReady {
					t.Errorf("the sandbox is %s; want it left ready", state)
				}
			})
		}
	}

	t.Run("a deletion with the fault in place ends deleted", func(t *testing.T) {
		h.hooks.setExec(func(argv []string) (int, string, error) {
			if slices.Contains(argv, "bundle") {
				return 128, "fatal: cannot create the bundle", nil
			}
			return 0, "", nil
		})
		t.Cleanup(func() { h.hooks.setExec(nil) })
		h.clock.Advance(time.Second)
		if err := h.svc.Delete(context.Background(), storedRecord(t, h.Hub, id), sandbox.DeleteAPI, sandbox.SystemActor, true); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		rec := storedRecord(t, h.Hub, id)
		if rec.State != sandbox.StateDeleted || rec.StateReason != sandbox.DeleteAPI {
			t.Errorf("the sandbox is %s (%q); want it deleted for api", rec.State, rec.StateReason)
		}
		if cp := h.storedCheckpoint(t, id); cp == nil || cp.OK || !cp.At.Equal(h.clock.Now()) {
			t.Errorf("the stored checkpoint = %+v; want the failed attempt of the deletion", cp)
		}
	})
}

// The checkpoint route's behaviours share one hub: each row has a sandbox of
// its own.
func TestCheckpointRoute(t *testing.T) {
	t.Parallel()
	h := startCheckpointHub(t)
	path := func(id string) string { return "/workspaces/demo--t1/sandboxes/" + id + "/checkpoint" }
	// The request comes a minute after the sandbox was made, so that the
	// checkpoint's time is the request's.
	h.clock.Advance(time.Minute)

	t.Run("saves a ready sandbox for the caller and answers with it", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			edit func(*sandbox.Record)
			none bool
		}{
			{name: "uncommitted work on t1"},
			{name: "a sandbox without hub access", none: true, edit: func(r *sandbox.Record) {
				r.HubAccess, r.Snapshot.HubAccess = nil, "none"
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				id, dir := h.workSandbox(t, tc.edit)
				h.cloneWork(t, dir)
				writeWork(t, dir, map[string]string{"notes.txt": "unsaved\n"})

				a := h.call(t, http.MethodPost, path(id), h.key, nil)
				var got struct {
					ID         string                   `json:"id"`
					State      string                   `json:"state"`
					Checkpoint *sandbox.CheckpointState `json:"checkpoint"`
				}
				if a.status != http.StatusOK || json.Unmarshal(a.body, &got) != nil || got.ID != id || got.State != sandbox.StateReady {
					t.Fatalf("POST checkpoint = %d %s; want 200 with the ready sandbox", a.status, a.body)
				}
				evs := h.sandboxEvents(t, sandbox.EventCheckpoint, id)
				if tc.none {
					if got.Checkpoint != nil || len(evs) != 0 {
						t.Errorf("checkpoint = %+v with %d events; want null and none", got.Checkpoint, len(evs))
					}
					return
				}
				cp := got.Checkpoint
				if cp == nil || !cp.OK || !cp.At.Equal(h.clock.Now()) || len(cp.Refs) != 1 || !cp.Refs[0].Dirty {
					t.Fatalf("checkpoint = %+v; want ok at %v with one dirty ref", cp, h.clock.Now())
				}
				if stored := h.storedCheckpoint(t, id); !reflect.DeepEqual(stored, cp) {
					t.Errorf("the stored checkpoint = %+v; want the answer's %+v", stored, cp)
				}
				if len(evs) != 1 || evs[0].actorType != "api_key" || evs[0].actorID == nil || *evs[0].actorID != h.dana {
					t.Errorf("the checkpoint events = %+v; want one by dana's API key", evs)
				}
			})
		}
	})

	t.Run("checkpoints of one sandbox store the refs the workspace holds", func(t *testing.T) {
		id, dir := h.workSandbox(t, nil)
		h.cloneWork(t, dir)
		first := commitWork(t, dir, "first", map[string]string{"f": "1\n"})
		// The first attempt is held after its refs were stored and before
		// its checkpoint is, in its clean-up of the sandbox.
		var holding atomic.Bool
		held, release := make(chan struct{}), make(chan struct{})
		h.hooks.setExec(func(argv []string) (int, string, error) {
			if argv[0] == "rm" && holding.CompareAndSwap(false, true) {
				close(held)
				<-release
			}
			return 0, "", nil
		})
		t.Cleanup(func() { h.hooks.setExec(nil) })
		statuses := make([]int, 2)
		var wg sync.WaitGroup
		wg.Go(func() { statuses[0] = h.call(t, http.MethodPost, path(id), h.key, nil).status })
		hubtestWait(t, held, "the first checkpoint to be held")
		second := commitWork(t, dir, "second", map[string]string{"f": "2\n"})
		done := make(chan struct{})
		wg.Go(func() {
			defer close(done)
			statuses[1] = h.call(t, http.MethodPost, path(id), h.key, nil).status
		})
		hubtest.Eventually(t, "the second checkpoint to end or wait for its turn", func() bool {
			select {
			case <-done:
				return true
			default:
				return sandbox.CheckpointTurns(h.svc, id) == 2
			}
		})
		close(release)
		wg.Wait()

		if statuses[0] != http.StatusOK || statuses[1] != http.StatusOK {
			t.Fatalf("the checkpoints answered %v; want 200 twice", statuses)
		}
		refs, cp := h.checkpointRefs(t, id), h.storedCheckpoint(t, id)
		if cp == nil || len(cp.Refs) != 1 || refs["t1"] != cp.Refs[0].SHA || len(refs) != 1 {
			t.Errorf("the workspace holds the checkpoint refs %v and the stored checkpoint is %+v; want both the same", refs, cp)
		}
		if refs["t1"] != second {
			t.Errorf("the checkpoint ref of t1 is %s; want the later attempt's %s, not the first's %s", refs["t1"], second, first)
		}
	})

	t.Run("refuses a sandbox that is not ready without calling the runtime", func(t *testing.T) {
		prior, err := json.Marshal(sandbox.CheckpointState{At: recordStart, OK: true, Refs: []sandbox.CheckpointRef{{Branch: "t1", SHA: h.base}}})
		if err != nil {
			t.Fatal(err)
		}
		for _, state := range []string{sandbox.StateProvisioning, sandbox.StateStopped, sandbox.StateCompleted, sandbox.StateFailed, sandbox.StateDeleted} {
			t.Run(state, func(t *testing.T) {
				id := h.seedRecord(t, "demo--t1", state, func(r *sandbox.Record) {
					r.Checkout = &sandbox.Checkout{Path: sandbox.WorkDir, Ref: "main", SHA: h.base, Branch: new("t1")}
					r.HubAccess = &sandbox.HubAccess{Scopes: allHubScopes, ExpiresAt: r.ExpiresAt}
					r.Checkpoint = prior
				})
				calls := len(h.rt.Calls())

				a := h.call(t, http.MethodPost, path(id), h.key, nil)
				if a.status != http.StatusConflict || a.err.ErrorType != "sandbox_not_ready" {
					t.Errorf("POST checkpoint = %d %s; want 409 sandbox_not_ready", a.status, a.body)
				}
				if stored := storedRecord(t, h.Hub, id).Checkpoint; string(stored) != string(prior) {
					t.Errorf("the stored checkpoint = %s; want it unchanged, %s", stored, prior)
				}
				for _, c := range h.rt.Calls()[calls:] {
					if strings.HasPrefix(c.Target, id) {
						t.Errorf("the runtime was called: %+v", c)
					}
				}
				if n := h.count(t, `SELECT count(*) FROM hub_events WHERE resource_id = ?`, id); n != 0 {
					t.Errorf("%d events about the sandbox; want none", n)
				}
			})
		}
	})

	t.Run("records a failure and answers it as a runtime error", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			set  func() func()
			// text is what the answer and the stored error hold besides.
			text string
		}{
			{name: "the runtime cannot be reached", set: func() func() {
				h.rt.SetUnreachable(true)
				return func() { h.rt.SetUnreachable(false) }
			}},
			{name: "git exits with 128 in the sandbox", text: "fatal: bad object", set: func() func() {
				h.hooks.setExec(func([]string) (int, string, error) { return 128, "fatal: bad object", nil })
				return func() { h.hooks.setExec(nil) }
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				id, dir := h.workSandbox(t, nil)
				h.cloneWork(t, dir)
				t.Cleanup(tc.set())

				a := h.call(t, http.MethodPost, path(id), h.key, nil)
				if a.status != http.StatusBadGateway || a.err.ErrorType != "sandbox_runtime_error" {
					t.Fatalf("POST checkpoint = %d %s; want 502 sandbox_runtime_error", a.status, a.body)
				}
				rec := storedRecord(t, h.Hub, id)
				cp := h.storedCheckpoint(t, id)
				if cp == nil || cp.OK || cp.Error == nil || rec.State != sandbox.StateReady {
					t.Fatalf("the sandbox is %s with the checkpoint %+v; want it ready with a failed checkpoint", rec.State, cp)
				}
				if !strings.Contains(a.err.Message, *cp.Error) || !strings.Contains(*cp.Error, tc.text) {
					t.Errorf("the answer's message %q, the stored error %q; want the message to hold the error, which holds %q",
						a.err.Message, *cp.Error, tc.text)
				}
			})
		}
	})
}
