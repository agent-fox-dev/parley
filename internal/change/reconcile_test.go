package change_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// task opens in demo, by mia, a change from the new branch of demo--t1, a
// draft when asked, and returns its number.
func (m *miaWorld) task(t *testing.T, branch string, draft bool) int64 {
	t.Helper()
	commit(t, m.t1, branch, m.base, branch+"\n\nWhy.\n", map[string]string{branch + ".txt": "1\n"})
	return m.openBranch(t, branch, func(in *change.OpenInput) {
		in.SourceWorkspace, in.Draft = "demo--t1", draft
		in.Caller = m.caller()
	}).Number
}

// row is what a reconciliation may change of the change number of the
// workspace: its state, version, head and updated_at as stored.
func (w *world) row(t *testing.T, workspace string, number int64) string {
	t.Helper()
	var state, head string
	var version, updated int64
	if err := w.Store.Main().QueryRow(`SELECT state, version, head_sha, updated_at FROM changes WHERE workspace_slug = ? AND number = ?`,
		workspace, number).Scan(&state, &version, &head, &updated); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s v%d %s %d", state, version, head, updated)
}

func TestReconcile_RecordsTheHeadOfAMovedSourceBranchAsAVersionByTheHub(t *testing.T) {
	t.Parallel()
	m := newMiaWorld(t)
	numbers := map[string]int64{"open": m.task(t, "t", false), "draft": m.task(t, "d", true)}
	heads := map[string]string{}
	for name, n := range numbers {
		ch := m.settled(t, n)
		heads[name] = commit(t, m.t1, ch.Source.Branch, ch.HeadSHA, "Moved\n", map[string]string{name + ".txt": "2\n"})
	}
	seq := m.seq(t)

	m.Restart(t)
	for name, n := range numbers {
		path := "/workspaces/demo/changes/" + strconv.FormatInt(n, 10)
		if status, got, _ := m.get(t, path, m.ownerKey); status != http.StatusOK || got["version"] != float64(2) || got["head_sha"] != heads[name] {
			t.Errorf("%s: the first GET after the start = %d version %v head %v, want version 2 at %s", name, status, got["version"], got["head_sha"], heads[name])
		}
		if ref := gitrepotest.Refs(t, m.demo, "refs/parley/changes/")[fmt.Sprintf("%d/2", n)]; ref != heads[name] {
			t.Errorf("%s: version ref 2 = %q, want %s", name, ref, heads[name])
		}
		vs := m.versions(t, "demo", n)
		if last := vs[len(vs)-1]; last["kind"] != change.KindRef || last["created_by"].(map[string]any)["actor_type"] != change.ActorSystem {
			t.Errorf("%s: version %v, want kind ref by the system", name, last)
		}
		var credential sql.NullString
		if err := m.Store.Main().QueryRow(`SELECT credential_type FROM change_versions WHERE workspace_slug = 'demo' AND number = ? AND version = 2`,
			n).Scan(&credential); err != nil || credential.Valid {
			t.Errorf("%s: credential of version 2 = %v (%v), want none", name, credential, err)
		}
		events := m.changeEvents(t, seq, change.EventVersion, n)
		if len(events) != 1 || metadata(t, events[0])["version"] != float64(2) || metadata(t, events[0])["kind"] != change.KindRef {
			t.Errorf("%s: version events %v, want one of version 2 and kind ref", name, events)
		}
		wantSystemEvents(t, events)
		if ch := m.settled(t, n); ch.Mergeable != change.MergeableClean {
			t.Errorf("%s: mergeable %s after the start, want clean", name, ch.Mergeable)
		}
	}

	seq = m.seq(t)
	m.Restart(t)
	for name, n := range numbers {
		if v := m.read(t, n).Version; v != 2 {
			t.Errorf("%s: version %d after one more start, want 2", name, v)
		}
	}
	if got := m.seq(t); got != seq {
		t.Errorf("one more start wrote events up to %d, want none after %d", got, seq)
	}
}

func TestReconcile_ChangesNothingWhileEverySourceBranchIsAtItsVersion(t *testing.T) {
	t.Parallel()
	m := newMiaWorld(t)
	numbers := []int64{m.task(t, "t", false), m.task(t, "d", true)}
	m.branch(t, "own", "Own\n\nWhy.\n")
	numbers = append(numbers, m.openBranch(t, "own", nil).Number)
	var before []string
	for _, n := range numbers {
		m.settled(t, n)
		before = append(before, m.row(t, "demo", n))
	}
	seq := m.seq(t)

	for start := range 2 {
		m.Restart(t)
		for i, n := range numbers {
			if got := m.row(t, "demo", n); got != before[i] {
				t.Errorf("start %d: demo#%d is %s, want %s", start+1, n, got, before[i])
			}
		}
		if got := m.seq(t); got != seq {
			t.Errorf("start %d wrote events up to %d, want none after %d", start+1, got, seq)
		}
	}
}

func TestReconcile_AbandonsAChangeWhoseSourceBranchIsGone(t *testing.T) {
	t.Parallel()
	m := newMiaWorld(t)
	open := m.task(t, "t", false)
	abandoned := m.task(t, "a", false)
	if status, out := m.abandon(t, abandoned, m.miaKey, nil); status != http.StatusOK {
		t.Fatalf("abandon = %d %v", status, out)
	}
	landed := m.task(t, "l", false)
	m.settled(t, open)
	m.settled(t, landed)
	head := m.read(t, landed).HeadSHA
	m.write(t, func(ctx context.Context, tx *sql.Tx) error {
		return m.changes.SetLanded(ctx, tx, "demo", landed, change.Landed{SHA: head, At: time.Now(), LandID: "l1",
			By: change.Actor{ActorType: change.CredentialAPIKey, ActorID: &m.mia}, Commits: []change.LandedCommit{{VersionSHA: head, LandedSHA: head}}})
	})
	closed := map[int64]string{abandoned: m.row(t, "demo", abandoned), landed: m.row(t, "demo", landed)}
	for _, b := range []string{"t", "a", "l"} {
		gitrepotest.Git(t, m.t1, "update-ref", "-d", "refs/heads/"+b)
	}
	seq := m.seq(t)

	m.Restart(t)
	if _, got, _ := m.get(t, "/workspaces/demo/changes/"+strconv.FormatInt(open, 10), m.ownerKey); got["state"] != change.StateAbandoned {
		t.Errorf("the first GET after the start = %v, want state abandoned", got)
	}
	events := m.changeEvents(t, seq, change.EventAbandon, open)
	if len(events) != 1 || metadata(t, events[0])["reason"] != change.ReasonSourceDeleted {
		t.Errorf("abandon events %v, want one with reason source_deleted", events)
	}
	wantSystemEvents(t, events)
	if got := m.abandonedChanges(t, seq); len(got) != 1 {
		t.Errorf("changes abandoned at the start: %v, want only demo#%d", got, open)
	}
	if ref := gitrepotest.Refs(t, m.demo, "refs/parley/changes/")[fmt.Sprintf("%d/1", open)]; ref == "" {
		t.Error("the version ref of the abandoned change is gone")
	}
	for n, want := range closed {
		if got := m.row(t, "demo", n); got != want {
			t.Errorf("demo#%d is %s after the start, want %s", n, got, want)
		}
	}
}

func TestReconcile_SchedulesTheCheckOfEveryChangeLeftUnknown(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.branch(t, "a", "A\n\nWhy.\n")
	n := w.openBranch(t, "a", nil).Number
	w.settled(t, n)
	w.write(t, func(ctx context.Context, tx *sql.Tx) error {
		return w.changes.SetMergeable(ctx, tx, "demo", n, change.MergeableUnknown, nil)
	})

	w.Restart(t)
	if ch := w.settled(t, n); ch.Mergeable != change.MergeableClean {
		t.Errorf("mergeable %s after the start, want clean", ch.Mergeable)
	}
}

func TestReconcile_LogsAChangeItCannotReconcileAndStartsAnyway(t *testing.T) {
	t.Parallel()
	m := newMiaWorld(t)
	broken := m.addWorkspace(t, "broken")
	commit(t, broken, "x", "main", "X\n\nWhy.\n", map[string]string{"x.txt": "x\n"})
	brokenChange := m.openBranch(t, "x", func(in *change.OpenInput) { in.Workspace = "broken" }).Number
	m.addFork(t, "demo--t2", "main", nil, nil)
	commit(t, m.workspaces.RepoPath("demo--t2"), "y", m.base, "Y\n\nWhy.\n", map[string]string{"y.txt": "y\n"})
	orphan := m.openBranch(t, "y", func(in *change.OpenInput) { in.SourceWorkspace = "demo--t2" }).Number
	moved := m.task(t, "t", false)
	head := commit(t, m.t1, "t", m.settled(t, moved).HeadSHA, "Moved\n", map[string]string{"t.txt": "2\n"})
	m.settled(t, orphan)
	m.quiet(t)
	before := map[string]string{"broken": m.row(t, "broken", brokenChange), "demo": m.row(t, "demo", orphan)}
	if err := os.WriteFile(filepath.Join(broken, "HEAD"), []byte("garbage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.write(t, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE slug = 'demo--t2'`)
		return err
	})

	m.Restart(t)
	resp, err := http.Get(m.URL("/readyz"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /readyz = %d, want 200", resp.StatusCode)
	}
	if ch := m.read(t, moved); ch.Version != 2 || ch.HeadSHA != head {
		t.Errorf("demo#%d is at version %d %s, want 2 at %s", moved, ch.Version, ch.HeadSHA, head)
	}
	for workspace, n := range map[string]int64{"broken": brokenChange, "demo": orphan} {
		if got := m.row(t, workspace, n); got != before[workspace] {
			t.Errorf("%s#%d is %s after the start, want %s", workspace, n, got, before[workspace])
		}
		entries := 0
		for line := range strings.Lines(m.Log.String()) {
			var e map[string]any
			if json.Unmarshal([]byte(line), &e) == nil && e["workspace"] == workspace && e["number"] == float64(n) {
				entries++
			}
		}
		if entries != 1 {
			t.Errorf("%d log entries name %s#%d, want 1:\n%s", entries, workspace, n, m.Log.String())
		}
	}
}
