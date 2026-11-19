package sandbox_test

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/sandbox"
)

// The restore's behaviours share one hub whose provisioner runs and whose
// sandboxes run the real git in directories of the host. A row may set the
// hub's exec hook for the time it runs, so none runs in parallel.
func TestRestore(t *testing.T) {
	t.Parallel()
	h := startRestoreHub(t, agentProfile)
	ctx := context.Background()
	// restored checks the working directory of the ready sandbox id: its
	// branches at the commits of heads, the branch head checked out, the
	// status lines status and an index that equals the head.
	restored := func(t *testing.T, id string, heads map[string]string, head string, status []string) {
		t.Helper()
		if rec := storedRecord(t, h.Hub, id); rec.State != sandbox.StateReady {
			t.Fatalf("sandbox = %s %q; want ready", rec.State, rec.StateReason)
		}
		dir := h.workDir(id)
		for _, branch := range slices.Sorted(maps.Keys(heads)) {
			if got := workGit(t, dir, "rev-parse", "refs/heads/"+branch); got != heads[branch] {
				t.Errorf("%s = %s; want %s", branch, got, heads[branch])
			}
		}
		if got := workGit(t, dir, "symbolic-ref", "--short", "HEAD"); got != head {
			t.Errorf("checked out %s; want %s", got, head)
		}
		if got := workStatus(t, dir); !slices.Equal(got, status) {
			t.Errorf("status = %q; want %q", got, status)
		}
		if staged := workGit(t, dir, "diff", "--cached", "--name-only"); staged != "" {
			t.Errorf("staged %q; want an index equal to the head", staged)
		}
		if left, _ := filepath.Glob(filepath.Join(dir, ".git", "parley-checkpoint*")); len(left) != 0 {
			t.Errorf("the restore left %v", left)
		}
	}
	start := func(t *testing.T, id string) {
		t.Helper()
		if a := h.call(t, http.MethodPost, sandboxPath("demo--t1", id)+"/start", h.key, nil); a.status != http.StatusOK {
			t.Fatalf("POST start = %d %s; want 200", a.status, a.body)
		}
		h.settled(t, id)
	}

	t.Run("seeds a new sandbox with the branches and the uncommitted changes of a checkpoint", func(t *testing.T) {
		src, dir := h.workSandbox(t, nil)
		h.cloneWork(t, dir)
		c1 := commitWork(t, dir, "c1", map[string]string{"a.txt": "a\n", "old.txt": "old\n"})
		workGit(t, dir, "checkout", "--quiet", "-b", "x")
		c2 := commitWork(t, dir, "c2", map[string]string{"x.txt": "x\n"})
		workGit(t, dir, "checkout", "--quiet", "t1")
		writeWork(t, dir, map[string]string{"a.txt": "a changed\n", "new.txt": "new\n"})
		if err := os.Remove(filepath.Join(dir, "old.txt")); err != nil {
			t.Fatal(err)
		}
		if _, err := h.svc.Checkpoint(ctx, storedRecord(t, h.Hub, src), sandbox.SystemActor); err != nil {
			t.Fatal(err)
		}
		// The source is gone, at the runtime too, before a sandbox is
		// restored from it.
		h.exec(t, `UPDATE sandboxes SET state = 'deleted' WHERE id = ?`, src)
		if err := h.rt.Client("parley", nil).DeleteSandbox(ctx, src); err != nil {
			t.Fatal(err)
		}
		source, refs := storedRecord(t, h.Hub, src), h.checkpointRefs(t, src)
		if len(refs) != 2 {
			t.Fatalf("checkpoint refs %v; want t1 and x", refs)
		}

		for _, tc := range []struct {
			name     string
			checkout map[string]any
			head     string
			status   []string
		}{
			{"with the dirty branch checked out", nil, "t1", []string{" D old.txt", " M a.txt", "?? new.txt"}},
			{"with another branch checked out", map[string]any{"ref": "main", "branch": "other"}, "other", nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				body := map[string]any{"restore_from": src}
				if tc.checkout != nil {
					body["checkout"] = tc.checkout
				}
				_, rec := h.provisioned(t, "demo--t1", "", body)
				restored(t, rec.ID, map[string]string{"t1": c1, "x": c2}, tc.head, tc.status)
				if after := storedRecord(t, h.Hub, src); !reflect.DeepEqual(after, source) {
					t.Errorf("the source's record = %+v; want %+v", after, source)
				}
				if after := h.checkpointRefs(t, src); !maps.Equal(after, refs) {
					t.Errorf("the source's checkpoint refs = %v; want %v", after, refs)
				}
			})
		}
	})

	t.Run("rebuilds the working directory of a start from its own checkpoint when the runtime lost it", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			drop bool
		}{
			{"the runtime dropped the working directory", true},
			{"the runtime kept the working directory", false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				id, dir := h.workSandbox(t, nil)
				h.cloneWork(t, dir)
				c1 := commitWork(t, dir, "c1", map[string]string{"a.txt": tc.name + "\n"})
				writeWork(t, dir, map[string]string{"new.txt": "new\n"})
				h.hubCredential(t, id)
				if _, err := h.svc.Stop(ctx, storedRecord(t, h.Hub, id), sandbox.StopAPI, sandbox.SystemActor); err != nil {
					t.Fatal(err)
				}
				if tc.drop {
					h.rt.DropWorkDir("parley", id)
				} else {
					writeWork(t, dir, map[string]string{".gitignore": "kept.ign\n", "kept.ign": "kept\n"})
				}
				calls := len(h.rt.Calls())

				start(t, id)
				if tc.drop {
					restored(t, id, map[string]string{"t1": c1}, "t1", []string{"?? new.txt"})
					return
				}
				if rec := storedRecord(t, h.Hub, id); rec.State != sandbox.StateReady {
					t.Fatalf("sandbox = %s %q; want ready", rec.State, rec.StateReason)
				}
				if _, err := os.Stat(filepath.Join(dir, "kept.ign")); err != nil {
					t.Errorf("kept.ign after the start: %v", err)
				}
				if n := h.uploads(id, calls); n != 0 {
					t.Errorf("%d uploads into the sandbox; want none", n)
				}
			})
		}
	})

	t.Run("leaves the checkout as it is for a checkpoint without refs", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			// sandbox returns the sandbox to wait for, ready, and the
			// runtime's calls before it was asked for.
			sandbox func(t *testing.T) (id string, calls int)
		}{
			{"a new sandbox restored from a checkpoint without refs", func(t *testing.T) (string, int) {
				src := h.seedRecord(t, "demo--t1", sandbox.StateDeleted, func(r *sandbox.Record) {
					r.Checkpoint = json.RawMessage(`{"ok":true,"error":null,"refs":[]}`)
				})
				calls := len(h.rt.Calls())
				_, rec := h.provisioned(t, "demo--t1", "", map[string]any{"restore_from": src})
				return rec.ID, calls
			}},
			{"a start whose own failed checkpoint kept no refs", func(t *testing.T) (string, int) {
				id, _ := h.workSandbox(t, func(r *sandbox.Record) {
					r.State = sandbox.StateStopped
					r.Checkpoint = json.RawMessage(`{"ok":false,"error":"the gateway is restarting","refs":[]}`)
				})
				h.rt.DropWorkDir("parley", id)
				calls := len(h.rt.Calls())
				start(t, id)
				return id, calls
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				id, calls := tc.sandbox(t)
				restored(t, id, map[string]string{"t1": h.base}, "t1", nil)
				if n := h.uploads(id, calls); n != 0 {
					t.Errorf("%d uploads into the sandbox; want none", n)
				}
			})
		}
	})

	t.Run("fails a new sandbox whose checkpoint ref is gone or whose restore command fails", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			stored bool
			fault  func(argv []string) (int, string, error)
			// named is what the fail event's error must name.
			named string
		}{
			{name: "the ref is gone from the workspace's repository", named: sandbox.CheckpointRefPrefix},
			{name: "the fetch from the bundle exits with 1", stored: true, named: "git fetch",
				fault: func(argv []string) (int, string, error) {
					if slices.Contains(argv, "fetch") {
						return 1, "fatal: the bundle is damaged", nil
					}
					return 0, "", nil
				}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				src := h.dirtySource(t, tc.stored)
				source := storedRecord(t, h.Hub, src)
				if tc.fault != nil {
					h.hooks.setExec(tc.fault)
					t.Cleanup(func() { h.hooks.setExec(nil) })
				}

				_, rec := h.provisioned(t, "demo--t1", "", map[string]any{"restore_from": src})
				if rec.State != sandbox.StateFailed || rec.StateReason != sandbox.ReasonCheckoutFailed {
					t.Errorf("sandbox = %s %q; want failed checkout_failed", rec.State, rec.StateReason)
				}
				ev := h.sandboxEvents(t, sandbox.EventFail, rec.ID)
				if len(ev) != 1 || ev[0].metadata["reason"] != sandbox.ReasonCheckoutFailed {
					t.Fatalf("fail events %+v; want one with reason checkout_failed", ev)
				}
				if text, _ := ev[0].metadata["error"].(string); len(text) > 1024 || !strings.Contains(text, tc.named) {
					t.Errorf("the fail event's error = %q; want at most 1,024 bytes naming %q", text, tc.named)
				}
				if left := h.runtimeLeft(t, rec.ID); len(left) != 0 {
					t.Errorf("the runtime holds %v; want nothing of the sandbox", left)
				}
				if after := storedRecord(t, h.Hub, src); !reflect.DeepEqual(after, source) {
					t.Errorf("the source's record = %+v; want %+v", after, source)
				}
			})
		}
	})
}
