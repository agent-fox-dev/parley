package carrypatch_test

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// recordingDescriber is a conflict-file describer that records every call
// and returns, for each path it is given, a content entry whose ours and
// theirs are the path's blobs in the two commits it is given.
type recordingDescriber struct {
	mu    sync.Mutex
	calls []describeCall
}

// describeCall is one call of a conflict-file describer.
type describeCall struct {
	target, commit string
	paths          []string
}

func (d *recordingDescriber) ConflictFiles(_ context.Context, repo *gitrepo.Repo, target, commit string, paths []string) ([]gitrepo.ConflictFile, error) {
	d.mu.Lock()
	d.calls = append(d.calls, describeCall{target, commit, slices.Clone(paths)})
	d.mu.Unlock()
	files := make([]gitrepo.ConflictFile, 0, len(paths))
	for _, p := range paths {
		files = append(files, describedFile(repo.Dir(), target, commit, p))
	}
	return files, nil
}

// recorded are the calls the describer had.
func (d *recordingDescriber) recorded() []describeCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.calls)
}

// describedFile is the entry the recording describer gives for path.
func describedFile(dir, target, commit, path string) gitrepo.ConflictFile {
	return gitrepo.ConflictFile{Path: path, Kind: gitrepo.ConflictContent, Ours: blobSide(dir, target, path), Theirs: blobSide(dir, commit, path)}
}

// blobSide is the file path of the commit rev of the repository dir as a
// side of a conflict, nil when rev holds none.
func blobSide(dir, rev, path string) *gitrepo.ConflictSide {
	out, err := gitrepotest.Run(dir, "", "ls-tree", rev, "--", path)
	if fields := strings.Fields(out); err == nil && len(fields) == 4 {
		return &gitrepo.ConflictSide{SHA: fields[2], Mode: fields[0], Path: path}
	}
	return nil
}

// conflictFixture is the rebuild fixture with the recording describer,
// whose upstream's main holds a.txt and b.txt of three lines each; cut is
// that commit, in the copy too, which conflicting patches are cut from.
type conflictFixture struct {
	*rebuildFixture
	describer *recordingDescriber
	cut       string
}

// startConflictFixture starts the conflict fixture with the carry-patch
// component's dependencies changed by deps, when it is not nil.
func startConflictFixture(t *testing.T, deps func(d *carrypatch.Deps)) *conflictFixture {
	t.Helper()
	desc := &recordingDescriber{}
	f := startRebuildFixtureWith(t, func(d *carrypatch.Deps) {
		d.ConflictFiles = desc
		if deps != nil {
			deps(d)
		}
	})
	f.moveUpstream(t, "U2", map[string]string{"a.txt": "1\n2\n3\n", "b.txt": "1\n2\n3\n"})
	if err := f.comp.EnsureSetup(context.Background(), f.record(t)); err != nil {
		t.Fatal(err)
	}
	return &conflictFixture{rebuildFixture: f, describer: desc, cut: refIn(f.copy, "refs/remotes/upstream/main")}
}

// conflictingPatch stores an active patch of branch, cut from the upstream's
// main, of two commits: the first adds a file of its own, the second
// changes line 3 of each of files, as moveOn does on upstream. It returns
// the patch and its two commits.
func (f *conflictFixture) conflictingPatch(t *testing.T, branch string, files ...string) (carrypatch.Patch, [2]string) {
	t.Helper()
	name := strings.ReplaceAll(branch, "/", "-")
	first := commitFiles(t, f.copy, f.cut, name+" adds", map[string]string{name + ".txt": branch + "\n"})
	changes := map[string]string{}
	for _, file := range files {
		changes[file] = "1\n2\n3 " + branch + "\n"
	}
	second := commitFiles(t, f.copy, first, name+" changes", changes)
	return f.putPatch(t, branch, second), [2]string{first, second}
}

// moveOn gives the upstream's main a commit that changes line 3 of a.txt
// and b.txt, and returns it: the upstream base of the next attempt.
func (f *conflictFixture) moveOn(t *testing.T) string {
	t.Helper()
	return f.moveUpstream(t, "U3", map[string]string{"a.txt": "1\n2\n3 upstream\n", "b.txt": "1\n2\n3 upstream\n"})
}

// continueRebase are the options of a rebuild by rebase that goes on past
// a conflicting patch.
var continueRebase = carrypatch.RebuildOptions{Strategy: new("rebase"), FailMode: new("continue")}

func TestRebuild_ConflictUndoesThePatchAndMarksIt(t *testing.T) {
	t.Parallel()
	f := startConflictFixture(t, nil)
	p1, commits := f.conflictingPatch(t, "p1", "b.txt", "a.txt")
	base := f.moveOn(t)

	out, id := f.attemptWith(t, continueRebase, nil)
	if out.Kind != attemptCompleted {
		t.Fatalf("the attempt = %+v, want completed", out)
	}
	files := []string{"a.txt", "b.txt"}
	if got := f.stored(t, p1.ID); got.Status != carrypatch.StatusConflict || !slices.Equal(got.ConflictFiles, files) {
		t.Errorf("p1 in the store has status %s and conflict_files %q, want conflict and %q", got.Status, got.ConflictFiles, files)
	}
	results := f.patchResults(t, id)
	if len(results) != 1 {
		t.Fatalf("patch_results = %v, want one", results)
	}
	r := results[0]
	if r["status"] != "conflict" || !reflect.DeepEqual(r["conflict_files"], []any{"a.txt", "b.txt"}) || r["new_head_sha"] != nil || r["conflict"] == nil {
		t.Errorf("p1's result = %v, want conflict with conflict_files %q, new_head_sha null and a conflict report", r, files)
	}
	updates := f.patchEvents(t, carrypatch.EventPatchUpdate)
	if len(updates) != 1 {
		t.Fatalf("parley.patch.update events = %+v, want one", updates)
	}
	e := updates[0]
	want := map[string]any{"branch_name": "p1", "changed": []any{"status"}, "from": "active", "to": "conflict", "reason": "rebuild_conflict"}
	if e.ResourceType != "patch" || e.ResourceID != p1.ID || e.ActorType != event.ActorSystem || e.ActorID != nil || !reflect.DeepEqual(e.Metadata, want) {
		t.Errorf("the parley.patch.update = %+v, want resource patch %s, actor system and the metadata %v", e, p1.ID, want)
	}
	if deploy := refIn(f.copy, "refs/heads/deploy"); deploy != base {
		t.Errorf("deploy = %s, want the upstream base %s", deploy, base)
	}
	if _, err := gitrepotest.Run(f.copy, "", "merge-base", "--is-ancestor", commits[0], "refs/heads/deploy"); err == nil {
		t.Errorf("the first commit of p1, %s, is in deploy; want the patch undone", commits[0])
	}

	// The patch is conflict already: a later conflict changes no status.
	if out, _ := f.attemptWith(t, continueRebase, nil); out.Kind != attemptCompleted {
		t.Fatalf("the second attempt = %+v, want completed", out)
	}
	if got := f.patchEvents(t, carrypatch.EventPatchUpdate); len(got) != 1 {
		t.Errorf("parley.patch.update events after a second conflict = %+v, want the first alone", got)
	}
}

func TestRebuild_ConflictReportMembers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		strategy string
		// first stores an applying patch before the conflicting one.
		first bool
	}{
		{"rebase, the conflicting patch first", "rebase", false},
		{"rebase, the conflicting patch after an applied patch", "rebase", true},
		{"merge", "merge", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := startConflictFixture(t, nil)
			if tc.first {
				f.addPatch(t, "p0")
			}
			p1, commits := f.conflictingPatch(t, "p1", "a.txt")
			f.moveOn(t)

			out, id := f.attemptWith(t, carrypatch.RebuildOptions{Strategy: &tc.strategy, FailMode: new("continue")}, nil)
			if out.Kind != attemptCompleted {
				t.Fatalf("the attempt = %+v, want completed", out)
			}
			results := f.patchResults(t, id)
			target, _ := f.result(t, id)["upstream_head_sha"].(string)
			if tc.first {
				target, _ = results[0]["new_head_sha"].(string)
			}
			source := commits[1]
			commit := commits[1]
			if tc.strategy == "merge" {
				commit = source
			}
			rep, ok := results[len(results)-1]["conflict"].(map[string]any)
			if !ok {
				t.Fatalf("p1's result = %v, want a conflict report", results[len(results)-1])
			}
			if keys := slices.Sorted(maps.Keys(rep)); !slices.Equal(keys, []string{"base_sha", "change", "commit_sha", "files", "head_sha", "landed_changes", "patch_id", "target_sha"}) {
				t.Errorf("the members of the report = %q, want the eight of a rebuild's conflict report", keys)
			}
			mergeBase := gitrepotest.Git(t, f.copy, "merge-base", source, target)
			wantMembers := map[string]any{"patch_id": p1.ID, "target_sha": target, "head_sha": source, "base_sha": mergeBase,
				"commit_sha": commit, "landed_changes": []any{}, "change": nil}
			for k, v := range wantMembers {
				if !reflect.DeepEqual(rep[k], v) {
					t.Errorf("%s of the report = %v, want %v", k, rep[k], v)
				}
			}
			if got, want := f.describer.recorded(), []describeCall{{target, commit, []string{"a.txt"}}}; !reflect.DeepEqual(got, want) {
				t.Errorf("the describer's calls = %+v, want %+v", got, want)
			}
			raw, err := json.Marshal([]gitrepo.ConflictFile{describedFile(f.copy, target, commit, "a.txt")})
			if err != nil {
				t.Fatal(err)
			}
			var wantFiles []any
			if err := json.Unmarshal(raw, &wantFiles); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(rep["files"], wantFiles) {
				t.Errorf("files of the report = %v, want the describer's entries with landed_by []: %v", rep["files"], wantFiles)
			}
		})
	}
}

func TestRebuild_FailFastStopsAndPublishesNothing(t *testing.T) {
	t.Parallel()
	f := startConflictFixture(t, nil)
	f.addPatch(t, "p1")
	f.conflictingPatch(t, "feature/b", "a.txt")
	p3 := f.addPatch(t, "p3")
	f.moveOn(t)
	before := refIn(f.copy, "refs/heads/deploy")
	stored := f.stored(t, p3.ID)

	out, id := f.attemptWith(t, carrypatch.RebuildOptions{FailMode: new("fail_fast")}, nil)
	if out.Kind != attemptPermanent || out.Reason != "conflict" {
		t.Fatalf("the attempt = %+v, want permanent with reason conflict", out)
	}
	if !strings.Contains(out.Text, "feature/b") {
		t.Errorf("the attempt's text %q does not name the branch feature/b", out.Text)
	}
	results := f.patchResults(t, id)
	if len(results) != 2 {
		t.Fatalf("patch_results = %v, want the results of p1 and feature/b", results)
	}
	var conflict any
	if err := json.Unmarshal(out.Conflict, &conflict); err != nil || conflict == nil || !reflect.DeepEqual(conflict, results[1]["conflict"]) {
		t.Errorf("the attempt's conflict = %s (%v), want the report of feature/b's result, %v", out.Conflict, err, results[1]["conflict"])
	}
	res := f.result(t, id)
	for _, k := range []string{"patches_applied", "patches_skipped", "patches_conflicted", "patches_removed"} {
		if res[k] != float64(0) {
			t.Errorf("%s = %v, want 0", k, res[k])
		}
	}
	if res["integration_head_sha"] != nil {
		t.Errorf("integration_head_sha = %v, want null", res["integration_head_sha"])
	}
	if got := refIn(f.copy, "refs/heads/deploy"); got != before {
		t.Errorf("deploy = %s, want it where it was, %s", got, before)
	}
	if got := f.stored(t, p3.ID); !reflect.DeepEqual(got, stored) {
		t.Errorf("p3 after the attempt = %+v, want it unchanged, %+v", got, stored)
	}
}

func TestRebuild_ContinuePublishesWithoutTheConflictingPatch(t *testing.T) {
	t.Parallel()
	f := startConflictFixture(t, nil)
	f.addPatch(t, "p1")
	p2, _ := f.conflictingPatch(t, "p2", "a.txt")
	f.addPatch(t, "p3")
	base := f.moveOn(t)

	out, id := f.attemptWith(t, continueRebase, nil)
	if out.Kind != attemptCompleted {
		t.Fatalf("the attempt = %+v, want completed", out)
	}
	results := f.patchResults(t, id)
	var statuses []string
	for _, r := range results {
		s, _ := r["status"].(string)
		statuses = append(statuses, s)
	}
	if want := []string{"success", "conflict", "success"}; !slices.Equal(statuses, want) {
		t.Fatalf("the statuses of patch_results = %q, want %q", statuses, want)
	}
	log := commitsIn(t, f.copy, base, refIn(f.copy, "refs/heads/deploy"), "%P %s")
	if want := []string{base + " p1", results[0]["new_head_sha"].(string) + " p3"}; !slices.Equal(log, want) {
		t.Errorf("base..deploy (parent and subject) = %q, want p1 on the base and p3 on p1's new head: %q", log, want)
	}
	if got := f.stored(t, p2.ID); got.Status != carrypatch.StatusConflict {
		t.Errorf("p2 in the store has status %s, want conflict", got.Status)
	}
}

func TestRebuild_NonConflictFailuresAreRetryable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// prepare makes the replay of p2, the second patch, fail; deps
		// changes the component's dependencies.
		prepare func(t *testing.T, f *rebuildFixture, p2 string)
		deps    func(t *testing.T, d *carrypatch.Deps)
		reason  string
	}{
		{"a commit of the second patch is missing", func(t *testing.T, f *rebuildFixture, p2 string) {
			f.hooks.beforePatch(func(i int) {
				if i == 1 {
					if err := os.Remove(filepath.Join(f.copy, "objects", p2[:2], p2[2:])); err != nil {
						t.Errorf("remove the commit of p2: %v", err)
					}
				}
			})
		}, nil, "git_error"},
		{"the replay of the second patch outlasts the limit", nil, func(t *testing.T, d *carrypatch.Deps) {
			d.RebuildGitDeadline = slowGitLimit
		}, "timeout"},
		{"the hub stops before the second patch", func(t *testing.T, f *rebuildFixture, _ string) {
			f.hooks.beforePatch(func(i int) {
				if i == 1 {
					f.runner.cancelAttempt()
				}
			})
		}, nil, "shutdown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := startRebuildFixtureWith(t, func(d *carrypatch.Deps) {
				if tc.deps != nil {
					tc.deps(t, d)
				}
			})
			f.addPatch(t, "p1")
			var pid string
			var p2 carrypatch.Patch
			if tc.reason == "timeout" {
				// The replay of p2 checks out slow.txt, whose filter sleeps.
				pid = slowFilter(t, f.copy)
				p2 = f.putPatch(t, "p2", commitFiles(t, f.copy, f.base, "p2", map[string]string{"slow.txt": "slow\n"}))
			} else {
				p2 = f.addPatch(t, "p2")
			}
			if tc.prepare != nil {
				tc.prepare(t, f, refIn(f.copy, "refs/heads/p2"))
			}
			before := refIn(f.copy, "refs/heads/deploy")

			out, id := f.attempt(t, nil)
			if out.Kind != attemptRetryable || out.Reason != tc.reason {
				t.Errorf("the attempt = %+v, want retryable with reason %s", out, tc.reason)
			}
			if results := f.patchResults(t, id); len(results) != 1 {
				t.Errorf("patch_results = %v, want p1's result alone", results)
			}
			if got := f.stored(t, p2.ID); got.Status != carrypatch.StatusActive || len(got.ConflictFiles) != 0 {
				t.Errorf("p2 in the store has status %s and conflict_files %q, want active and none", got.Status, got.ConflictFiles)
			}
			if got := refIn(f.copy, "refs/heads/deploy"); got != before {
				t.Errorf("deploy = %s, want it where it was, %s", got, before)
			}
			if pid != "" {
				waitGone(t, pid)
			}
		})
	}
}

// slowGitLimit is the per-operation limit of the attempt whose replay
// sleeps: far below the filter's sleep, and above what any other git
// operation of the attempt takes under load.
const slowGitLimit = 5 * time.Second

// slowFilter gives slow.txt, in the repository dir's own configuration, a
// smudge filter that writes its process id to a file and then sleeps for a
// minute, and returns that file.
func slowFilter(t *testing.T, dir string) string {
	t.Helper()
	tmp := t.TempDir()
	pid, script, attrs := filepath.Join(tmp, "pid"), filepath.Join(tmp, "slow.sh"), filepath.Join(tmp, "attributes")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho $$ > "+pid+"\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(attrs, []byte("slow.txt filter=slow\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitrepotest.Git(t, dir, "config", "core.attributesFile", attrs)
	gitrepotest.Git(t, dir, "config", "filter.slow.smudge", script)
	return pid
}

// waitGone waits for the process whose id the file pidFile holds to be
// gone, and fails the test when it is still there after ten seconds: the
// process sleeps for a minute unless it is killed.
func waitGone(t *testing.T, pidFile string) {
	t.Helper()
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the filter of slow.txt did not run: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if !alive(pid) {
			return
		}
	}
	t.Errorf("the git child process %d is still alive after the attempt", pid)
}

// alive reports whether the process pid runs: one that exited and is not
// yet reaped by its parent does not.
func alive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	stat := strings.TrimSpace(string(out))
	return err == nil && stat != "" && !strings.HasPrefix(stat, "Z")
}
