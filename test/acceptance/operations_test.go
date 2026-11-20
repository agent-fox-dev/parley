package acceptance

import (
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/agent-fox-dev/parley/test/acceptance/harness"
)

// writers keeps eight writers busy on demo until stop is called: four
// commit to branches of their own through the source control API, four
// create claims. It counts the writes answered 2xx and records every other
// answer, and every request that got none, as a fault: no write of theirs
// may be refused. A writer whose request got no answer stops, so that a
// hub that went away is not called in a loop.
type writers struct {
	acked  atomic.Int64
	mu     sync.Mutex
	faults []string
	done   chan struct{}
	wg     sync.WaitGroup
	once   sync.Once
}

// startWriters starts the writers; they are stopped when the test ends at
// the latest.
func startWriters(t *testing.T, hub *harness.Hub, credential string) *writers {
	w := &writers{done: make(chan struct{})}
	t.Cleanup(func() { w.stop() })
	for i := range 8 {
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			branch, head := fmt.Sprint("writer-", i), ""
			for n := 0; ; n++ {
				select {
				case <-w.done:
					return
				default:
				}
				var r harness.Response
				if i%2 == 0 {
					r = hub.Call(http.MethodPost, claims, credential, map[string]any{
						"paths": []string{fmt.Sprintf("w%d/%d.txt", i, n)}, "intent": "write while the backup runs",
					})
				} else {
					body := map[string]any{"branch": branch, "message": fmt.Sprint("Write ", n),
						"operations": []any{put(branch+".txt", fmt.Sprint(n, "\n"))}}
					if head == "" {
						body["base_ref"] = "main"
					} else {
						body["expected_head_sha"] = head
					}
					r = hub.Call(http.MethodPost, scm+"/commits", credential, body)
					if r.Status == http.StatusCreated {
						head = harness.Str(harness.Field(r.Body, "commit.sha"))
					}
				}
				if r.Err == nil && r.Status/100 == 2 {
					w.acked.Add(1)
					continue
				}
				w.mu.Lock()
				w.faults = append(w.faults, fmt.Sprintf("writer %d: %d %v %s", i, r.Status, r.Err, r.Raw))
				w.mu.Unlock()
				if r.Err != nil {
					return
				}
			}
		}()
	}
	return w
}

// stop stops the writers, once, and returns the faults they met.
func (w *writers) stop() []string {
	w.once.Do(func() { close(w.done) })
	w.wg.Wait()
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.faults
}

func TestOperations_BackupWhileRunningRestoresToACleanStart(t *testing.T) {
	t.Parallel()
	// Four writers of claims pass the default limit of 1000 active claims
	// of a workspace within seconds, and the limit refuses the rest.
	hub := harness.Start(t, harness.Options{TOML: "[awareness]\nmax_claims_per_workspace = 1000000\n"})
	key := hub.Workspace(hub.User("alice"), "demo").Owner.Key
	var open []int
	for i := range 8 {
		number := openChange(t, hub, key, fmt.Sprint("change-", i), map[string]string{fmt.Sprintf("c%d.txt", i): "change\n"})
		if i < 3 {
			hub.Land(key, "demo", number)
		} else {
			open = append(open, number)
		}
	}

	w := startWriters(t, hub, key)
	harness.Eventually(t, "the writers to be writing", func() bool { return w.acked.Load() >= 8 })
	snap := filepath.Join(t.TempDir(), "snapshot")
	ackedBefore := w.acked.Load()
	backup := hub.Parleyd("backup", snap)
	ackedDuring := w.acked.Load() - ackedBefore
	faults := w.stop()
	if backup.Code != 0 {
		t.Fatalf("parleyd backup exited %d, want 0:\n%s%s", backup.Code, backup.Stdout, backup.Stderr)
	}
	if len(faults) > 0 {
		t.Errorf("the writers met %d answers other than 2xx, or none, while the backup ran: %v", len(faults), faults)
	}
	if ackedDuring == 0 {
		t.Error("no write was answered 2xx while the backup ran, want the hub to keep writing")
	}
	t.Logf("%d writes answered 2xx while the backup ran", ackedDuring)

	hub2 := hub.StartOn(snap)
	hub2.Get("", "/readyz")
	for _, ws := range hub2.Get(key, "/api/v1/workspaces").Items() {
		if ws["status"] != "ready" {
			t.Errorf("workspace %v on the snapshot is %v, want ready", ws["slug"], ws["status"])
		}
	}
	clone := hub2.Clone(harness.Str(hub2.Get(key, "/api/v1/workspaces/demo").Body["hub_url"]), key)
	stillOpen := hub2.Get(key, changes+"?state=open").Items()
	for _, c := range stillOpen {
		ref := fmt.Sprintf("refs/parley/changes/%d/%d", harness.Num(c["number"]), harness.Num(c["version"]))
		if f := hub2.Git(clone, "", "fetch", "-q", "origin", ref); f.Code != 0 {
			t.Errorf("git fetch %s from the snapshot exited %d: %s", ref, f.Code, f.Stderr)
		}
	}
	if len(stillOpen) < len(open) {
		t.Errorf("the snapshot holds %d open changes, want at least the %d opened before the backup", len(stillOpen), len(open))
	}
	if fsck := hub2.Git(clone, "", "fsck", "--full"); fsck.Code != 0 {
		t.Errorf("git fsck of a clone of the snapshot exited %d:\n%s%s", fsck.Code, fsck.Stdout, fsck.Stderr)
	}
	hub2.Land(key, "demo", open[0])
	var seqs []int64
	for _, e := range eventLog(t, hub2, key, "", 0) {
		seqs = append(seqs, seqOf(e))
	}
	if len(seqs) == 0 || !ascending(seqs) {
		t.Errorf("the snapshot's event log has the seqs %v, want them strictly ascending", seqs)
	}

	if again := hub.Parleyd("backup", snap); again.Code != 1 {
		t.Errorf("parleyd backup into the non-empty snapshot exited %d, want 1:\n%s", again.Code, again.Stderr)
	}
}
