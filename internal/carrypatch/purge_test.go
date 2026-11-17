package carrypatch_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestPurgeDeletedPatches_RemovesThePatchesDeletedMoreThanSevenDaysAgo(t *testing.T) {
	t.Parallel()
	h := startPurgeHub(t)
	now := h.clock()
	h.add(t, "a", workspace.ModeCarryPatch, workspace.StatusReady, workspace.MarkerNone)
	h.add(t, "b", workspace.ModeCarryPatch, workspace.StatusArchived, workspace.MarkerNone)
	day := 24 * time.Hour
	withStatus := func(p carrypatch.Patch, status string, added time.Time) carrypatch.Patch {
		p.Status, p.AddedAt = status, added
		return p
	}
	rows := []struct {
		name  string
		patch carrypatch.Patch
		gone  bool
	}{
		{"deleted 7 days and 1 second ago", deletedPatch("a", "d-7d1s", now.Add(-7*day-time.Second)), true},
		{"deleted 30 days ago", deletedPatch("b", "d-30d", now.Add(-30*day)), true},
		{"deleted exactly 7 days ago", deletedPatch("b", "d-7d", now.Add(-7*day)), false},
		{"deleted 1 day ago", deletedPatch("a", "d-1d", now.Add(-day)), false},
		{"active, added 60 days ago", withStatus(livePatch("a", "live-active", 1), carrypatch.StatusActive, now.Add(-60*day)), false},
		{"disabled", withStatus(livePatch("a", "live-disabled", 2), carrypatch.StatusDisabled, at(9, 0)), false},
		{"merged upstream", withStatus(livePatch("a", "live-merged", 3), carrypatch.StatusMergedUpstream, at(9, 0)), false},
	}
	for _, r := range rows {
		h.put(t, r.patch)
	}
	events := h.eventCount(t)

	if err := h.comp.PurgeDeletedPatches(context.Background(), now); err != nil {
		t.Fatalf("PurgeDeletedPatches = %v, want nil", err)
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			p := r.patch
			_, found, err := carrypatch.ReadPatch(context.Background(), h.Store.Main(), p.Workspace, p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if found == r.gone {
				t.Errorf("the patch of %s in %s is stored: %v, want %v", p.BranchName, p.Workspace, found, !r.gone)
			}
			if !r.gone {
				return
			}
			if a := h.get(t, "/workspaces/"+p.Workspace+"/patches/"+p.ID); a.status != 404 {
				t.Errorf("GET the purged patch of %s = %d %s, want 404", p.BranchName, a.status, a.raw)
			}
			if ids := h.listedIDs(t, p.Workspace); slices.Contains(ids, p.ID) {
				t.Errorf("the list of %s with include_deleted=true holds the purged patch of %s", p.Workspace, p.BranchName)
			}
		})
	}
	live, err := carrypatch.ListLivePatches(context.Background(), h.Store.Main(), "a")
	if err != nil {
		t.Fatal(err)
	}
	var places []string
	for _, p := range live {
		places = append(places, fmt.Sprintf("%s@%d", p.BranchName, *p.Position))
	}
	if want := []string{"live-active@1", "live-disabled@2", "live-merged@3"}; !slices.Equal(places, want) {
		t.Errorf("the patches of a that are not deleted = %v, want %v", places, want)
	}

	before := h.storedIDs(t)
	if err := h.comp.PurgeDeletedPatches(context.Background(), now); err != nil {
		t.Fatalf("a second PurgeDeletedPatches = %v, want nil", err)
	}
	if after := h.storedIDs(t); !slices.Equal(after, before) {
		t.Errorf("the second run left the patches %v, want %v as after the first", after, before)
	}
	if got := h.eventCount(t); got != events {
		t.Errorf("the purge wrote %d events, want none", got-events)
	}
}

func TestPurgeDeletedPatches_DeletesTheBackupRefWhenNoLivePatchUsesTheBranch(t *testing.T) {
	t.Parallel()
	h := startPurgeHub(t)
	now := h.clock()
	h.add(t, "demo", workspace.ModeCarryPatch, workspace.StatusReady, workspace.MarkerNone)
	dir := h.purgeCopy(t, "demo", "p-old", "p-reused", "p-live")
	before := gitrepotest.Refs(t, dir, "")
	old := deletedPatch("demo", "p-old", now.Add(-8*24*time.Hour))
	reused := deletedPatch("demo", "p-reused", now.Add(-8*24*time.Hour))
	h.put(t, old, reused, livePatch("demo", "p-reused", 1), livePatch("demo", "p-live", 2))

	if err := h.comp.PurgeDeletedPatches(context.Background(), now); err != nil {
		t.Fatalf("PurgeDeletedPatches = %v, want nil", err)
	}
	after := gitrepotest.Refs(t, dir, "")
	want := maps.Clone(before)
	delete(want, carrypatch.ReplacedRefPrefix+"p-old")
	if !maps.Equal(after, want) {
		t.Errorf("the refs after the purge =\n%v\nwant those before without the backup ref of p-old:\n%v", after, want)
	}
	for _, p := range []carrypatch.Patch{old, reused} {
		if _, found, err := carrypatch.ReadPatch(context.Background(), h.Store.Main(), "demo", p.ID); err != nil || found {
			t.Errorf("the deleted patch of %s is stored after the purge: %v, %v", p.BranchName, found, err)
		}
	}
}

func TestPurgeDeletedPatches_RemovesTheRecordsOfAWorkspaceWithoutACopy(t *testing.T) {
	t.Parallel()
	h := startPurgeHub(t)
	now := h.clock()
	rows := []struct {
		slug   string
		status workspace.Status
	}{
		{"failed", workspace.StatusFailed},
		{"pending", workspace.StatusPending},
	}
	due := map[string]carrypatch.Patch{}
	for _, r := range rows {
		h.add(t, r.slug, workspace.ModeCarryPatch, r.status, workspace.MarkerNone)
		due[r.slug] = deletedPatch(r.slug, "p-old", now.Add(-8*24*time.Hour))
		h.put(t, due[r.slug])
	}

	if err := h.comp.PurgeDeletedPatches(context.Background(), now); err != nil {
		t.Fatalf("PurgeDeletedPatches = %v, want nil", err)
	}
	for _, r := range rows {
		t.Run(string(r.status)+" without a copy", func(t *testing.T) {
			if _, found, err := carrypatch.ReadPatch(context.Background(), h.Store.Main(), r.slug, due[r.slug].ID); err != nil || found {
				t.Errorf("the patch deleted 8 days ago is stored: %v, %v", found, err)
			}
			if calls := h.gitCallsIn(r.slug); len(calls) != 0 {
				t.Errorf("git ran on the workspace without a copy: %v", calls)
			}
		})
	}
}

func TestPurgeDeletedPatches_LetsARestoreAtTheSameMomentWin(t *testing.T) {
	t.Parallel()
	h := startPurgeHub(t)
	now := h.clock()
	h.add(t, "demo", workspace.ModeCarryPatch, workspace.StatusReady, workspace.MarkerNone)
	dir := h.purgeCopy(t, "demo", "p-old")
	backup := carrypatch.ReplacedRefPrefix + "p-old"
	saved := refIn(dir, backup)
	deletedAt := now.Add(-8 * 24 * time.Hour)
	p := deletedPatch("demo", "p-old", deletedAt)
	h.put(t, p)

	// A restore that commits after the purge chose the patch and before the
	// purge removes it.
	h.beforePurge(func(slug, id string) {
		if id != p.ID {
			return
		}
		if restored, err := h.restoreInStore(slug, id); err != nil || !restored {
			t.Errorf("restore the patch in the store = %v, %v; want it restored", restored, err)
		}
	})
	if err := h.comp.PurgeDeletedPatches(context.Background(), now); err != nil {
		t.Fatalf("PurgeDeletedPatches = %v, want nil", err)
	}
	h.beforePurge(nil)
	if got := h.storedPatch(t, "demo", p.ID); got.Status != carrypatch.StatusActive || got.DeletedAt != nil {
		t.Errorf("the restored patch is %s, deleted at %v; want active and never deleted", got.Status, got.DeletedAt)
	}
	if got := refIn(dir, backup); got != saved {
		t.Errorf("%s = %q after the purge, want %s", backup, got, saved)
	}

	// The same race without a pause: whichever of the two commits first,
	// a restored patch keeps its backup ref.
	for i := range 50 {
		h.redelete(t, p, deletedAt)
		if refIn(dir, backup) == "" {
			gitrepotest.SetRef(t, dir, backup, saved)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		var restored bool
		var purgeErr, restoreErr error
		wg.Go(func() {
			<-start
			purgeErr = h.comp.PurgeDeletedPatches(context.Background(), now)
		})
		wg.Go(func() {
			<-start
			restored, restoreErr = h.restoreInStore("demo", p.ID)
		})
		close(start)
		wg.Wait()
		if purgeErr != nil || restoreErr != nil {
			t.Fatalf("run %d: purge = %v, restore = %v; want no error", i, purgeErr, restoreErr)
		}
		got, found, err := carrypatch.ReadPatch(context.Background(), h.Store.Main(), "demo", p.ID)
		if err != nil {
			t.Fatal(err)
		}
		ref := refIn(dir, backup)
		switch {
		case restored && (!found || got.Status != carrypatch.StatusActive || ref != saved):
			t.Fatalf("run %d: the restore won, and the patch is stored %v as %s with %s = %q; want active with %s",
				i, found, got.Status, backup, ref, saved)
		case !restored && (found || ref != ""):
			t.Fatalf("run %d: the purge won, and the patch is stored %v with %s = %q; want both gone", i, found, backup, ref)
		}
	}
}

func TestPurgeDeletedPatches_KeepsAPatchItCannotRemoveAndPurgesTheOthers(t *testing.T) {
	t.Parallel()
	h := startPurgeHub(t)
	now := h.clock()
	cases := []struct {
		name string
		// fault makes the removal of the patch p of the workspace whose copy
		// is dir fail, and returns what removes the fault.
		fault func(t *testing.T, dir string, p carrypatch.Patch) (remove func())
	}{
		{"the backup ref cannot be deleted", func(t *testing.T, dir string, p carrypatch.Patch) func() {
			// git takes the ref's lock file to delete it, and fails when
			// another writer holds it.
			lock := filepath.Join(dir, filepath.FromSlash(carrypatch.ReplacedRefPrefix+p.BranchName)+".lock")
			if err := os.WriteFile(lock, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			return func() {
				if err := os.Remove(lock); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"the transaction fails after the backup ref is deleted", func(_ *testing.T, _ string, p carrypatch.Patch) func() {
			h.refFault.Store(p.ID, errors.New("the transaction fails after the ref's deletion"))
			return func() { h.refFault.Delete(p.ID) }
		}},
		{"the store refuses the delete", func(t *testing.T, _ string, p carrypatch.Patch) func() {
			h.exec(t, fmt.Sprintf(`CREATE TRIGGER refuse_purge BEFORE DELETE ON patches WHEN OLD.id = '%s'
BEGIN SELECT RAISE(ABORT, 'the store refuses the delete'); END`, p.ID))
			return func() { h.exec(t, `DROP TRIGGER refuse_purge`) }
		}},
	}
	// The cases run one after the other: each run of the purge goes over
	// every workspace of the hub.
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			slug := fmt.Sprintf("demo%d", i)
			h.add(t, slug, workspace.ModeCarryPatch, workspace.StatusReady, workspace.MarkerNone)
			dir := h.purgeCopy(t, slug, "p1", "p2", "p3")
			saved := gitrepotest.Refs(t, dir, "")
			ps := map[string]carrypatch.Patch{}
			for _, b := range []string{"p1", "p2", "p3"} {
				ps[b] = deletedPatch(slug, b, now.Add(-8*24*time.Hour))
				h.put(t, ps[b])
			}
			remove := tc.fault(t, dir, ps["p2"])

			if err := h.comp.PurgeDeletedPatches(context.Background(), now); err == nil {
				t.Errorf("PurgeDeletedPatches = nil, want an error")
			}
			if got := h.storedPatch(t, slug, ps["p2"].ID); got.Status != carrypatch.StatusDeleted {
				t.Errorf("the patch of p2 is %s, want deleted", got.Status)
			}
			if got, want := refIn(dir, carrypatch.ReplacedRefPrefix+"p2"), saved[carrypatch.ReplacedRefPrefix+"p2"]; got != want {
				t.Errorf("the backup ref of p2 = %q, want %s", got, want)
			}
			for _, b := range []string{"p1", "p3"} {
				if _, found, err := carrypatch.ReadPatch(context.Background(), h.Store.Main(), slug, ps[b].ID); err != nil || found {
					t.Errorf("the patch of %s is stored: %v, %v; want it purged", b, found, err)
				}
				if got := refIn(dir, carrypatch.ReplacedRefPrefix+b); got != "" {
					t.Errorf("the backup ref of %s = %s, want it deleted", b, got)
				}
			}
			if !h.loggedPatch(slug, ps["p2"].ID) {
				t.Errorf("no log line names %s and the patch %s; log = %q", slug, ps["p2"].ID, h.Log.String())
			}

			remove()
			if err := h.comp.PurgeDeletedPatches(context.Background(), now); err != nil {
				t.Fatalf("PurgeDeletedPatches without the fault = %v, want nil", err)
			}
			if _, found, err := carrypatch.ReadPatch(context.Background(), h.Store.Main(), slug, ps["p2"].ID); err != nil || found {
				t.Errorf("the patch of p2 is stored without the fault: %v, %v; want it purged", found, err)
			}
			if got := refIn(dir, carrypatch.ReplacedRefPrefix+"p2"); got != "" {
				t.Errorf("the backup ref of p2 = %s without the fault, want it deleted", got)
			}
		})
	}
}

// purgeHub is a hub whose clock is the test's, whose first maintenance
// pass has ended before the test goes on, and whose carry-patch component
// records the git directory of every git command it runs and calls the
// hook beforePurge gave it before the purge removes a patch.
type purgeHub struct {
	*addHub
	comp *carrypatch.Component
	hook atomic.Pointer[func(slug, id string)]
	// refFault is the error the purge's transaction of the patch it names
	// fails with once its backup ref is deleted.
	refFault sync.Map

	gitMu   sync.Mutex
	gitDirs []string
}

func startPurgeHub(t *testing.T) *purgeHub {
	t.Helper()
	h := &purgeHub{addHub: &addHub{readHub: &readHub{}, now: time.Now().UTC().Truncate(time.Second).Add(700 * time.Millisecond)}}
	passed := make(chan struct{})
	var once sync.Once
	h.Hub = hubtest.StartWith(t, hubtest.Config{
		Now: h.clock,
		Compose: func(c *hub.Composition) {
			h.comp = c.CarryPatch()
			// Tasks of names the maintenance loop does not know run last in a
			// pass.
			c.AddMaintenanceTask("test-purge-pass-end", func(context.Context, *hub.Runtime) error {
				once.Do(func() { close(passed) })
				return nil
			})
			c.AdjustCarryPatch(func(d *carrypatch.Deps) {
				h.repoPath = d.RepoPath
				d.BeforePurgePatch = func(slug, id string) {
					if fn := h.hook.Load(); fn != nil {
						(*fn)(slug, id)
					}
				}
				d.AfterPurgeRef = func(_, id string) error {
					if err, ok := h.refFault.Load(id); ok {
						return err.(error)
					}
					return nil
				}
				trace := d.Git.Trace
				d.Git.Trace = func(args []string) {
					if trace != nil {
						trace(args)
					}
					for _, a := range args {
						if dir, ok := strings.CutPrefix(a, "--git-dir="); ok {
							h.gitMu.Lock()
							h.gitDirs = append(h.gitDirs, dir)
							h.gitMu.Unlock()
						}
					}
				}
			})
		},
	})
	hubtest.WaitClosed(t, passed, "the first maintenance pass")
	h.owner = h.CreateUser(t, "owner")
	h.key = h.IssueKey(t, h.owner)
	h.org = h.CreateOrg(t, "acme")
	h.AddMember(t, h.org, h.owner)
	return h
}

// beforePurge makes fn run before the purge removes each patch it chose,
// nil for nothing.
func (h *purgeHub) beforePurge(fn func(slug, id string)) {
	if fn == nil {
		h.hook.Store(nil)
		return
	}
	h.hook.Store(&fn)
}

// purgeCopy makes the copy of slug a bare repository holding, for each of
// branches, the branch and its backup ref, each at a commit of its own, and
// returns its directory.
func (h *purgeHub) purgeCopy(t *testing.T, slug string, branches ...string) string {
	t.Helper()
	dir := h.repoPath(slug)
	gitrepotest.InitBare(t, dir, "")
	msgs := map[string]string{}
	for _, b := range branches {
		msgs["refs/heads/"+b] = b
		msgs[carrypatch.ReplacedRefPrefix+b] = "replaced " + b
	}
	rootsAt(t, dir, msgs)
	return dir
}

// gitCallsIn are the git commands of the carry-patch component that ran on
// the copy of slug.
func (h *purgeHub) gitCallsIn(slug string) []string {
	want, _ := filepath.Abs(h.repoPath(slug))
	h.gitMu.Lock()
	defer h.gitMu.Unlock()
	var out []string
	for _, dir := range h.gitDirs {
		if dir == want {
			out = append(out, dir)
		}
	}
	return out
}

// restoreInStore brings the deleted patch id of slug back in the store as a
// restore does, active at the end of the list, and reports whether there
// was a deleted patch to bring back.
func (h *purgeHub) restoreInStore(slug, id string) (bool, error) {
	ctx := context.Background()
	var restored bool
	err := h.Store.Tx(ctx, func(tx *sql.Tx) error {
		n, err := carrypatch.CountLivePatches(ctx, tx, slug)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE patches SET status = ?, position = ?, deleted_at = NULL
WHERE workspace_slug = ? AND id = ? AND status = ?`, carrypatch.StatusActive, n+1, slug, id, carrypatch.StatusDeleted)
		if err != nil {
			return err
		}
		k, err := res.RowsAffected()
		restored = k == 1
		return err
	})
	return restored, err
}

// redelete stores p as deleted at deletedAt again: restored, it is marked
// deleted, and purged, it is stored again.
func (h *purgeHub) redelete(t *testing.T, p carrypatch.Patch, deletedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := h.Store.Tx(ctx, func(tx *sql.Tx) error {
		if err := carrypatch.DeletePatch(ctx, tx, p.Workspace, p.ID); err != nil {
			return err
		}
		again := deletedPatch(p.Workspace, p.BranchName, deletedAt)
		again.ID = p.ID
		return carrypatch.InsertPatch(ctx, tx, again)
	}); err != nil {
		t.Fatalf("store the patch of %s deleted again: %v", p.BranchName, err)
	}
}

// exec runs the statement stmt on the hub's store.
func (h *purgeHub) exec(t *testing.T, stmt string) {
	t.Helper()
	ctx := context.Background()
	if err := h.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, stmt)
		return err
	}); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}

// storedIDs are the ids of every stored patch, sorted.
func (h *purgeHub) storedIDs(t *testing.T) []string {
	t.Helper()
	rows, err := h.Store.Main().Query(`SELECT id FROM patches ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

// listedIDs are the ids of the patch list of slug with include_deleted=true.
func (h *purgeHub) listedIDs(t *testing.T, slug string) []string {
	t.Helper()
	a := h.get(t, "/workspaces/"+slug+"/patches?include_deleted=true&limit=100")
	if a.status != 200 {
		t.Fatalf("GET the patches of %s = %d %s", slug, a.status, a.raw)
	}
	var ids []string
	for _, it := range a.Items {
		ids = append(ids, fmt.Sprint(it["id"]))
	}
	return ids
}

// loggedPatch reports whether a line of the hub's JSON log names the
// workspace slug and the patch id.
func (h *purgeHub) loggedPatch(slug, id string) bool {
	for line := range strings.Lines(h.Log.String()) {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["workspace"] == slug && rec["patch_id"] == id {
			return true
		}
	}
	return false
}

// storedPatch is the stored patch id of slug.
func (h *purgeHub) storedPatch(t *testing.T, slug, id string) carrypatch.Patch {
	t.Helper()
	p, found, err := carrypatch.ReadPatch(context.Background(), h.Store.Main(), slug, id)
	if err != nil || !found {
		t.Fatalf("read the patch %s of %s: found %v, %v", id, slug, found, err)
	}
	return p
}
