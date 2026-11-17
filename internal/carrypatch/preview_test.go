package carrypatch_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/jobs"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// previewAnswer is the decoded answer of a preview.
type previewAnswer struct {
	UpstreamHeadSHA string          `json:"upstream_head_sha"`
	PatchResults    []previewResult `json:"patch_results"`
}

// previewResult is one entry of a preview's patch_results.
type previewResult struct {
	PatchID       string          `json:"patch_id"`
	BranchName    string          `json:"branch_name"`
	Position      int             `json:"position"`
	Status        string          `json:"status"`
	ConflictFiles []string        `json:"conflict_files"`
	Conflict      json.RawMessage `json:"conflict"`
}

// previewConflict is the conflict report of a would_conflict entry.
type previewConflict struct {
	PatchID   string `json:"patch_id"`
	TargetSHA string `json:"target_sha"`
	HeadSHA   string `json:"head_sha"`
	BaseSHA   string `json:"base_sha"`
	CommitSHA string `json:"commit_sha"`
	Files     []struct {
		Path     string                `json:"path"`
		Kind     string                `json:"kind"`
		Base     *gitrepo.ConflictSide `json:"base"`
		Ours     *gitrepo.ConflictSide `json:"ours"`
		Theirs   *gitrepo.ConflictSide `json:"theirs"`
		LandedBy []any                 `json:"landed_by"`
	} `json:"files"`
	LandedChanges []any           `json:"landed_changes"`
	Change        json.RawMessage `json:"change"`
}

// isNull tells a JSON member that is null.
func isNull(raw json.RawMessage) bool { return len(raw) == 0 || string(raw) == "null" }

// decodePreview decodes the 200 answer of a preview, failing the test on
// another answer or an entry that lacks a member.
func decodePreview(t *testing.T, a answer) previewAnswer {
	t.Helper()
	if a.status != http.StatusOK {
		t.Fatalf("preview = %d %s, want 200", a.status, a.raw)
	}
	var p previewAnswer
	var members struct {
		PatchResults []map[string]json.RawMessage `json:"patch_results"`
	}
	if err := json.Unmarshal(a.raw, &p); err != nil {
		t.Fatalf("decode the preview %s: %v", a.raw, err)
	}
	_ = json.Unmarshal(a.raw, &members)
	want := []string{"branch_name", "conflict", "conflict_files", "patch_id", "position", "status"}
	for _, m := range members.PatchResults {
		if got := slices.Sorted(maps.Keys(m)); !slices.Equal(got, want) {
			t.Errorf("an entry has the members %v, want %v", got, want)
		}
	}
	return p
}

// conflictOf decodes the conflict report of the entry r.
func conflictOf(t *testing.T, r previewResult) previewConflict {
	t.Helper()
	var c previewConflict
	if err := json.Unmarshal(r.Conflict, &c); err != nil {
		t.Fatalf("decode the conflict of %s %s: %v", r.BranchName, r.Conflict, err)
	}
	return c
}

// previewPatch is a patch of a preview case: its branch patch/<name> ends
// at the commit of the history at index tip, or does not exist when tip is
// -1.
type previewPatch struct {
	name, status string
	tip          int
}

// previewCase is a carry-patch workspace whose copy holds history, the
// upstream branch main at the commit of index upstream, and patches.
type previewCase struct {
	name     string
	history  []histCommit
	upstream int
	patches  []previewPatch
	// want are the entries, each "<branch> <position> <status>".
	want  []string
	check func(t *testing.T, f previewFixture, p previewAnswer, a answer)
}

// previewFixture is what previewCarry made of a case: the copy's
// directory, the ids of the history and the stored patches by name.
type previewFixture struct {
	dir    string
	ids    []string
	stored map[string]carrypatch.Patch
}

// lines is content of the lines given, each ended with a newline.
func lines(ls ...string) string { return strings.Join(ls, "\n") + "\n" }

func TestPreview_PredictsTheOutcomeOfEachPatch(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	file := func(name, content string) map[string]string { return map[string]string{name: content} }
	cases := []previewCase{{
		name: "the patches neither disabled nor merged upstream, in position order",
		history: []histCommit{
			{from: -1, files: file("base.txt", "base\n")},
			{from: 0, files: file("a.txt", "a\n")}, {from: 0, files: file("d.txt", "d\n")}, {from: 0, files: file("b.txt", "b\n")},
			{from: 0, files: file("m.txt", "m\n")}, {from: 0, files: file("x.txt", "x\n")},
		},
		patches: []previewPatch{
			{"a", carrypatch.StatusActive, 1}, {"d", carrypatch.StatusDisabled, 2}, {"b", carrypatch.StatusConflict, 3},
			{"m", carrypatch.StatusMergedUpstream, 4}, {"x", carrypatch.StatusDeleted, 5},
		},
		want: []string{"patch/a 1 would_succeed", "patch/b 3 would_succeed"},
	}, {
		name:    "no patch to test",
		history: []histCommit{{from: -1, files: file("base.txt", "base\n")}, {from: 0, files: file("d.txt", "d\n")}},
		patches: []previewPatch{{"d", carrypatch.StatusDisabled, 1}, {"m", carrypatch.StatusMergedUpstream, 1}, {"x", carrypatch.StatusDeleted, 1}},
		want:    []string{},
		check: func(t *testing.T, _ previewFixture, _ previewAnswer, a answer) {
			if !bytes.Contains(a.raw, []byte(`"patch_results":[]`)) {
				t.Errorf("the preview answered %s, want patch_results []", a.raw)
			}
		},
	}, {
		name: "a missing branch is skipped and leaves the state",
		history: []histCommit{
			{from: -1, files: file("base.txt", "base\n")}, {from: 0, files: file("a.txt", "a\n")}, {from: 0, files: file("b.txt", "b\n")},
		},
		patches: []previewPatch{{"a", carrypatch.StatusActive, 1}, {"gone", carrypatch.StatusActive, -1}, {"b", carrypatch.StatusActive, 2}},
		want:    []string{"patch/a 1 would_succeed", "patch/gone 2 would_skip", "patch/b 3 would_succeed"},
	}, {
		name: "a clean patch joins the state the later ones are tested against",
		history: []histCommit{
			{from: -1, files: file("f.txt", "base\n")}, {from: 0, files: file("f.txt", "a\n")}, {from: 0, files: file("f.txt", "b\n")},
		},
		patches: []previewPatch{{"a", carrypatch.StatusActive, 1}, {"b", carrypatch.StatusActive, 2}},
		want:    []string{"patch/a 1 would_succeed", "patch/b 2 would_conflict"},
		check: func(t *testing.T, f previewFixture, p previewAnswer, _ answer) {
			ids := f.ids
			c := conflictOf(t, p.PatchResults[1])
			if c.TargetSHA == ids[0] || len(c.Files) != 1 || c.Files[0].Ours == nil || c.Files[0].Ours.SHA != blobID("a\n") {
				t.Fatalf("the conflict of patch/b = %s, want it tested against a state that holds patch/a", p.PatchResults[1].Conflict)
			}
			// The state is a merge of patch/a's tip by the landing identity,
			// which no ref points at.
			want := []string{ids[0] + " " + ids[1], "Parley <parley@localhost>", "Parley <parley@localhost>"}
			if got := strings.Split(show(t, f.dir, c.TargetSHA, "%P|%an <%ae>|%cn <%ce>"), "|"); !slices.Equal(got, want) {
				t.Errorf("parents|author|committer of the state = %q, want %q", got, want)
			}
			if refs := gitrepotest.Git(t, f.dir, "for-each-ref", "--points-at", c.TargetSHA); refs != "" {
				t.Errorf("the refs %q point at the predicted state", refs)
			}
		},
	}, {
		name:    "a tip the state already holds would succeed",
		history: []histCommit{{from: -1, files: file("f.txt", "base\n")}},
		patches: []previewPatch{{"c", carrypatch.StatusActive, 0}},
		want:    []string{"patch/c 1 would_succeed"},
	}, {
		name: "a conflicting patch is left out of the state",
		history: []histCommit{
			{from: -1, files: map[string]string{"f.txt": lines("1", "2", "3"), "g.txt": lines("1", "2", "3")}},
			{from: 0, files: map[string]string{"f.txt": lines("u", "2", "3"), "g.txt": lines("u", "2", "3")}},
			{from: 0, files: map[string]string{"g.txt": lines("b", "2", "3"), "f.txt": lines("b", "2", "3")}},
			{from: 1, files: file("g.txt", lines("c", "2", "3"))},
		},
		upstream: 1,
		patches:  []previewPatch{{"b", carrypatch.StatusActive, 2}, {"c", carrypatch.StatusActive, 3}},
		want:     []string{"patch/b 1 would_conflict", "patch/c 2 would_succeed"},
		check: func(t *testing.T, _ previewFixture, p previewAnswer, _ answer) {
			if got := p.PatchResults[0].ConflictFiles; !slices.Equal(got, []string{"f.txt", "g.txt"}) {
				t.Errorf("the conflict files of patch/b = %q, want f.txt and g.txt", got)
			}
		},
	}, {
		name: "the conflict report of a rebuild",
		history: []histCommit{
			{from: -1, files: map[string]string{"f.txt": "base\n", "del.txt": "d\n"}},
			{from: 0, files: map[string]string{"f.txt": "upstream\n", "del.txt": "d2\n", "new.txt": "u\n"}},
			{from: 0, files: map[string]string{"f.txt": "patch\n", "new.txt": "p\n"}, deleted: []string{"del.txt"}},
		},
		upstream: 1,
		patches:  []previewPatch{{"b", carrypatch.StatusActive, 2}},
		want:     []string{"patch/b 1 would_conflict"},
		check: func(t *testing.T, f previewFixture, p previewAnswer, _ answer) {
			ids, stored := f.ids, f.stored
			r := p.PatchResults[0]
			if want := []string{"del.txt", "f.txt", "new.txt"}; !slices.Equal(r.ConflictFiles, want) {
				t.Errorf("conflict_files = %q, want %q", r.ConflictFiles, want)
			}
			c := conflictOf(t, r)
			if c.PatchID != stored["b"].ID || c.TargetSHA != ids[1] || c.HeadSHA != ids[2] || c.CommitSHA != ids[2] || c.BaseSHA != ids[0] {
				t.Errorf("the conflict of patch/b = %s, want patch %s, target U %s, head and commit %s, base %s",
					r.Conflict, stored["b"].ID, ids[1], ids[2], ids[0])
			}
			if c.LandedChanges == nil || len(c.LandedChanges) != 0 || !isNull(c.Change) || !bytes.Contains(r.Conflict, []byte(`"change":null`)) {
				t.Errorf("the conflict of patch/b = %s, want landed_changes [] and change null", r.Conflict)
			}
			side := func(content, path string) *gitrepo.ConflictSide {
				return &gitrepo.ConflictSide{SHA: blobID(content), Mode: "100644", Path: path}
			}
			type file struct {
				kind               string
				base, ours, theirs *gitrepo.ConflictSide
			}
			want := map[string]file{
				"del.txt": {gitrepo.ConflictModifyDelete, side("d\n", "del.txt"), side("d2\n", "del.txt"), nil},
				"f.txt":   {gitrepo.ConflictContent, side("base\n", "f.txt"), side("upstream\n", "f.txt"), side("patch\n", "f.txt")},
				"new.txt": {gitrepo.ConflictAddAdd, nil, side("u\n", "new.txt"), side("p\n", "new.txt")},
			}
			var paths []string
			for _, f := range c.Files {
				paths = append(paths, f.Path)
				if got := (file{f.Kind, f.Base, f.Ours, f.Theirs}); !reflect.DeepEqual(got, want[f.Path]) {
					t.Errorf("the file entry of %s = %+v, want %+v", f.Path, got, want[f.Path])
				}
				if f.LandedBy == nil || len(f.LandedBy) != 0 {
					t.Errorf("the file entry of %s has landed_by %v, want []", f.Path, f.LandedBy)
				}
			}
			if !slices.Equal(paths, r.ConflictFiles) {
				t.Errorf("the report's files are %q, want the conflict files %q", paths, r.ConflictFiles)
			}
		},
	}}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			slug := fmt.Sprintf("w%d", i)
			f := h.previewCarry(t, slug, c)
			ids, stored := f.ids, f.stored
			a := h.preview(t, slug)
			p := decodePreview(t, a)
			if p.UpstreamHeadSHA != ids[c.upstream] {
				t.Errorf("upstream_head_sha = %q, want %s", p.UpstreamHeadSHA, ids[c.upstream])
			}
			got := []string{}
			for _, r := range p.PatchResults {
				got = append(got, fmt.Sprintf("%s %d %s", r.BranchName, r.Position, r.Status))
				name := strings.TrimPrefix(r.BranchName, "patch/")
				if r.PatchID != stored[name].ID {
					t.Errorf("the entry of %s has patch_id %q, want %q", r.BranchName, r.PatchID, stored[name].ID)
				}
				if r.Status != "would_conflict" {
					if r.ConflictFiles == nil || len(r.ConflictFiles) != 0 || !isNull(r.Conflict) {
						t.Errorf("the entry of %s has conflict_files %q and conflict %s, want [] and null", r.BranchName, r.ConflictFiles, r.Conflict)
					}
					continue
				}
				if len(r.ConflictFiles) == 0 || !slices.IsSorted(r.ConflictFiles) || isNull(r.Conflict) {
					t.Errorf("the entry of %s has conflict_files %q and conflict %s, want sorted paths and a report", r.BranchName, r.ConflictFiles, r.Conflict)
				}
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("patch_results = %q, want %q", got, c.want)
			}
			if c.check != nil {
				c.check(t, f, p, a)
			}
		})
	}
}

// previewCarry stores the ready carry-patch workspace slug of the case c,
// with its copy and its patches.
func (h *rebuildHub) previewCarry(t *testing.T, slug string, c previewCase) previewFixture {
	t.Helper()
	h.carryWith(t, slug, nil)
	history := slices.Clone(c.history)
	history[c.upstream].refs = append(slices.Clone(history[c.upstream].refs), "refs/remotes/upstream/main")
	stored := map[string]carrypatch.Patch{}
	var ps []carrypatch.Patch
	pos := 0
	for _, pp := range c.patches {
		branch := "patch/" + pp.name
		if pp.tip >= 0 {
			history[pp.tip].refs = append(slices.Clone(history[pp.tip].refs), "refs/heads/"+branch)
		}
		p := deletedPatch(slug, branch, at(10, 0))
		if pp.status != carrypatch.StatusDeleted {
			pos++
			p = livePatch(slug, branch, pos)
			p.Status = pp.status
		}
		stored[pp.name] = p
		ps = append(ps, p)
	}
	h.put(t, ps...)
	dir, ids := h.previewCopy(t, slug, history)
	return previewFixture{dir: dir, ids: ids, stored: stored}
}

func TestPreview_TakesTheUpstreamBaseFromTheLastFetch(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	gone := "file://" + filepath.Join(t.TempDir(), "gone.git")
	history := func(upstream ...string) []histCommit {
		return []histCommit{
			{from: -1, files: map[string]string{"u.txt": "u\n"}, refs: upstream[:1]},
			{from: -1, files: map[string]string{"u2.txt": "u2\n"}, refs: upstream[1:]},
			{from: 0, files: map[string]string{"a.txt": "a\n"}, refs: []string{"refs/heads/patch/a"}},
		}
	}
	for i, c := range []struct {
		name string
		// main and trunk are the refs the upstream commits U and U2 have.
		main, trunk string
		// recorded is the upstream's default branch the last fetch recorded.
		recorded string
		// base is the index of the commit of the base, -1 for none.
		base int
	}{
		{"the upstream's default branch the last fetch recorded", "refs/remotes/upstream/main", "refs/remotes/upstream/trunk", "main", 0},
		{"the upstream branch named like the default branch", "refs/remotes/upstream/main", "refs/remotes/upstream/trunk", "", 1},
		{"no upstream branch to start from", "refs/heads/main", "refs/heads/trunk", "main", -1},
	} {
		t.Run(c.name, func(t *testing.T) {
			slug := fmt.Sprintf("w%d", i)
			h.add(t, slug, workspace.ModeCarryPatch, workspace.StatusReady, workspace.MarkerNone, func(w *workspace.Workspace) {
				w.IntegrationBranch, w.DefaultBranch, w.UpstreamURL, w.GitURL = new("deploy"), new("trunk"), new(gone), new(gone)
			})
			h.put(t, livePatch(slug, "patch/a", 1))
			dir, ids := h.previewCopy(t, slug, history(c.main, c.trunk))
			if c.recorded != "" {
				gitrepotest.Git(t, dir, "config", "parley.upstreamDefaultBranch", c.recorded)
			}
			gits := len(h.gitCommands())

			a := h.preview(t, slug)
			if c.base < 0 {
				if a.status != http.StatusConflict || a.Error.ErrorType != "upstream_unknown" {
					t.Errorf("preview = %d %s, want 409 upstream_unknown", a.status, a.raw)
				}
				if refs := gitrepotest.Refs(t, dir, "refs/remotes/upstream/"); len(refs) != 0 {
					t.Errorf("the copy has the upstream refs %v after the preview, want none", refs)
				}
			} else if p := decodePreview(t, a); p.UpstreamHeadSHA != ids[c.base] || len(p.PatchResults) != 1 {
				t.Errorf("preview answered %s, want the base %s and one entry", a.raw, ids[c.base])
			}
			for _, cmd := range h.gitCommands()[gits:] {
				if strings.Contains(cmd, " fetch") || strings.Contains(cmd, "ls-remote") || strings.Contains(cmd, "gone.git") {
					t.Errorf("the preview contacted a remote: %s", cmd)
				}
			}
		})
	}
}

// previewState is what a preview leaves as it found it: the refs of the
// copy, the patch rows and job rows of the workspace, and the number of
// events in the hub's log.
type previewState struct {
	Refs    string
	Patches []carrypatch.Patch
	Jobs    []jobs.Job
	Events  int
}

func (h *rebuildHub) previewState(t *testing.T, slug string) previewState {
	t.Helper()
	ctx := context.Background()
	live, err := carrypatch.ListLivePatches(ctx, h.Store.Main(), slug)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := carrypatch.ListDeletedPatches(ctx, h.Store.Main(), slug)
	if err != nil {
		t.Fatal(err)
	}
	return previewState{Refs: gitrepotest.Git(t, h.repoPath(slug), "for-each-ref"), Patches: append(live, deleted...), Jobs: h.jobsOf(t, slug), Events: h.eventCount(t)}
}

// leftAsItWas fails the test when the preview state of slug differs from
// before.
func (h *rebuildHub) leftAsItWas(t *testing.T, slug string, before previewState) {
	t.Helper()
	if after := h.previewState(t, slug); !reflect.DeepEqual(after, before) {
		t.Errorf("the preview of %s changed its refs, patches, jobs or the event log:\n got %+v\nwant %+v", slug, after, before)
	}
}

// outcomes are the entries of p, each "<branch> <status> <conflict files>".
func outcomes(p previewAnswer) []string {
	out := []string{}
	for _, r := range p.PatchResults {
		out = append(out, fmt.Sprintf("%s %s %q", r.BranchName, r.Status, r.ConflictFiles))
	}
	return out
}

func TestPreview_TestsAMergeWhateverTheStrategy(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	// The first commit of patch/b conflicts with the upstream when it is
	// replayed alone and its second restores the upstream's content, so a
	// replay commit by commit conflicts and a merge of the tip does not.
	h.previewCarry(t, "w", previewCase{
		history: []histCommit{
			{from: -1, files: map[string]string{"f.txt": lines("1", "2", "3")}},
			{from: 0, files: map[string]string{"f.txt": lines("u", "2", "3")}},
			{from: 0, files: map[string]string{"f.txt": lines("p", "2", "3")}},
			{from: 2, files: map[string]string{"f.txt": lines("u", "2", "3")}},
			{from: 0, files: map[string]string{"f.txt": lines("c", "2", "3")}},
		},
		upstream: 1,
		patches:  []previewPatch{{"b", carrypatch.StatusActive, 3}, {"c", carrypatch.StatusActive, 4}},
	})
	want := []string{`patch/b would_succeed []`, `patch/c would_conflict ["f.txt"]`}
	// The rows change the setting of w in turn, so they run one after the
	// other.
	for _, strategy := range []string{"rebase", "merge"} {
		t.Run(strategy, func(t *testing.T) {
			h.configure(t, "w", map[string]string{"rebuild_strategy": strategy})
			if got := outcomes(decodePreview(t, h.preview(t, "w"))); !slices.Equal(got, want) {
				t.Errorf("with rebuild_strategy %s the preview predicted %q, want %q", strategy, got, want)
			}
		})
	}
}

func TestPreview_IgnoresRememberedResolutions(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	f := h.previewCarry(t, "w", previewCase{
		history: []histCommit{
			{from: -1, files: map[string]string{"f.txt": lines("1", "2", "3")}},
			{from: 0, files: map[string]string{"f.txt": lines("u", "2", "3")}},
			{from: 0, files: map[string]string{"f.txt": lines("b", "2", "3")}},
		},
		upstream: 1,
		patches:  []previewPatch{{"b", carrypatch.StatusActive, 2}},
	})
	// The copy remembers resolutions, as its set-up leaves it, and holds
	// one for exactly the conflict of patch/b with the upstream.
	gitrepotest.Git(t, f.dir, "config", "rerere.enabled", "true")
	seedResolution(t, f.dir, f.ids[1], f.ids[2], map[string]string{"f.txt": lines("resolved", "2", "3")})
	before := rrCache(t, f.dir)
	if !slices.ContainsFunc(slices.Collect(maps.Keys(before)), func(p string) bool { return strings.HasSuffix(p, "/postimage") }) {
		t.Fatalf("the rr-cache holds %q, want a recorded resolution", slices.Sorted(maps.Keys(before)))
	}

	p := decodePreview(t, h.preview(t, "w"))
	if got, want := outcomes(p), []string{`patch/b would_conflict ["f.txt"]`}; !slices.Equal(got, want) {
		t.Errorf("the preview predicted %q, want %q", got, want)
	}
	if after := rrCache(t, f.dir); !reflect.DeepEqual(after, before) {
		t.Errorf("the rr-cache holds %q after the preview, want %q as before", slices.Sorted(maps.Keys(after)), slices.Sorted(maps.Keys(before)))
	}
}

// rrCache is the content of every file in the rr-cache of the repository
// dir, by its path there.
func rrCache(t *testing.T, dir string) map[string]string {
	t.Helper()
	root := filepath.Join(dir, "rr-cache")
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		content, err := os.ReadFile(path)
		rel, _ := filepath.Rel(root, path)
		files[filepath.ToSlash(rel)] = string(content)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestPreview_ChangesNothing(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	f := h.previewCarry(t, "w", previewCase{
		history: []histCommit{
			{from: -1, files: map[string]string{"f.txt": lines("1", "2", "3")}},
			{from: 0, files: map[string]string{"f.txt": lines("u", "2", "3")}},
			{from: 0, files: map[string]string{"a.txt": "a\n"}},
			{from: 0, files: map[string]string{"f.txt": lines("b", "2", "3")}},
		},
		upstream: 1,
		patches:  []previewPatch{{"a", carrypatch.StatusActive, 2}, {"b", carrypatch.StatusActive, 3}},
	})
	h.settledRebuild(t, "w", rebuiltFrom(f.ids[0], f.ids[1]), nil)

	before := h.previewState(t, "w")
	p := decodePreview(t, h.preview(t, "w"))
	if got, want := outcomes(p), []string{`patch/a would_succeed []`, `patch/b would_conflict ["f.txt"]`}; !slices.Equal(got, want) {
		t.Fatalf("the preview predicted %q, want %q", got, want)
	}
	h.leftAsItWas(t, "w", before)
	for _, s := range before.Patches {
		if s.Status != carrypatch.StatusActive || len(s.ConflictFiles) != 0 {
			t.Errorf("the stored %s has status %s and conflict files %q, want active and none", s.BranchName, s.Status, s.ConflictFiles)
		}
	}

	gitrepotest.Git(t, f.dir, "update-ref", "-d", "refs/remotes/upstream/main")
	before = h.previewState(t, "w")
	if a := h.preview(t, "w"); a.status != http.StatusConflict || a.Error.ErrorType != "upstream_unknown" {
		t.Fatalf("preview without an upstream base = %d %s, want 409 upstream_unknown", a.status, a.raw)
	}
	h.leftAsItWas(t, "w", before)
}

func TestPreview_AnswersBesideARebuild(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	// The cases pass the queue in turn, so they run one after the other.
	for _, status := range []string{jobs.StatusQueued, jobs.StatusRunning, jobs.StatusFailed} {
		t.Run(status, func(t *testing.T) {
			slug := "w-" + status
			h.previewCarry(t, slug, previewCase{
				history: []histCommit{{from: -1, files: map[string]string{"base.txt": "base\n"}}, {from: 0, files: map[string]string{"a.txt": "a\n"}}},
				patches: []previewPatch{{"a", carrypatch.StatusActive, 1}},
			})
			job := h.queueRebuild(t, slug)
			if status != jobs.StatusQueued {
				h.drive(t, job, status)
			}
			before := h.previewState(t, slug)
			p := decodePreview(t, h.preview(t, slug))
			if got, want := outcomes(p), []string{`patch/a would_succeed []`}; !slices.Equal(got, want) {
				t.Errorf("the preview beside a %s rebuild predicted %q, want %q", status, got, want)
			}
			h.leftAsItWas(t, slug, before)
		})
	}
}

func TestPreview_GitFailureIsInternalError(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	// The cases count the hub's events in turn, so they run one after the
	// other.
	for i, c := range []struct {
		name string
		err  error
	}{
		{"a git failure that is no conflict", errors.New("fatal: unable to read tree of https://user:s3cret@upstream.example/r.git")},
		{"a merge stopped by the deadline", fmt.Errorf("git merge-tree: %w", gitrepo.ErrDeadline)},
	} {
		t.Run(c.name, func(t *testing.T) {
			slug := fmt.Sprintf("w%d", i)
			f := h.previewCarry(t, slug, previewCase{
				history: []histCommit{
					{from: -1, files: map[string]string{"base.txt": "base\n"}},
					{from: 0, files: map[string]string{"a.txt": "a\n"}},
					{from: 0, files: map[string]string{"b.txt": "b\n"}},
				},
				patches: []previewPatch{{"a", carrypatch.StatusActive, 1}, {"b", carrypatch.StatusActive, 2}},
			})
			// The merge of the second patch fails.
			var failed atomic.Int32
			h.onTrialMerge(f.dir, func(_, theirs string) error {
				if theirs != f.ids[2] {
					return nil
				}
				failed.Add(1)
				return c.err
			})
			before := h.previewState(t, slug)
			a := h.preview(t, slug)
			if a.status != http.StatusInternalServerError || a.Error.ErrorType != "internal" {
				t.Errorf("preview = %d %s, want 500 internal", a.status, a.raw)
			}
			if bytes.Contains(a.raw, []byte("patch_results")) || bytes.Contains(a.raw, []byte("s3cret")) {
				t.Errorf("preview answered %s, want no patch_results and no git text", a.raw)
			}
			if n := failed.Load(); n != 1 {
				t.Errorf("the merge of patch/b failed %d times, want once", n)
			}
			h.leftAsItWas(t, slug, before)
		})
	}
}
