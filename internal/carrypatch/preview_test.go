package carrypatch_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
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
