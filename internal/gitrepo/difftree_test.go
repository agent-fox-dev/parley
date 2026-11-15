package gitrepo_test

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// diffFixture is main with c1, which adds src.txt (the lines 1 to 10),
// kind.txt and the binary bin.dat, and c2, which changes line 5 of src.txt,
// copies src.txt as it was to dup.txt, turns kind.txt into a symlink and
// changes bin.dat; side, on top of c1, adding s.txt; and orphan, a root
// commit of its own. id maps these names and the blob paths (as c1:src.txt)
// to their ids.
type diffFixture struct {
	repo *gitrepo.Repo
	id   map[string]string
}

func newDiffFixture(t *testing.T) diffFixture {
	t.Helper()
	dir := t.TempDir()
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", dir)
	data := func(s string) string { return "data " + strconv.Itoa(len(s)) + "\n" + s + "\n" }
	const who = "committer T <t@example.invalid> 1700000000 +0000\n"
	stream := "commit refs/heads/main\nmark :1\n" + who + data("c1") +
		"M 100644 inline src.txt\n" + data("1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n") + "M 100644 inline kind.txt\n" + data("k\n") +
		"M 100644 inline bin.dat\n" + data("\x00a\n") +
		"\ncommit refs/heads/main\n" + who + data("c2") +
		"M 100644 inline src.txt\n" + data("1\n2\n3\n4\nfive\n6\n7\n8\n9\n10\n") + "M 100644 inline dup.txt\n" + data("1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n") +
		"M 120000 inline kind.txt\n" + data("src.txt") + "M 100644 inline bin.dat\n" + data("\x00b\n") +
		"\ncommit refs/heads/side\n" + who + data("side") + "from :1\nM 100644 inline s.txt\n" + data("s\n") +
		"\ncommit refs/heads/orphan\n" + who + data("orphan") + "M 100644 inline o.txt\n" + data("o\n") + "\n"
	if _, err := gitrepotest.Run(dir, stream, "fast-import", "--quiet"); err != nil {
		t.Fatal(err)
	}
	f := diffFixture{id: map[string]string{}}
	names := []string{"c1", "c2", "side", "orphan", "c1:src.txt", "c2:src.txt", "c1:kind.txt", "c2:kind.txt", "c1:bin.dat", "c2:bin.dat"}
	revs := []string{"main~1", "main", "side", "orphan", "main~1:src.txt", "main:src.txt", "main~1:kind.txt", "main:kind.txt", "main~1:bin.dat", "main:bin.dat"}
	for i, id := range strings.Fields(gitrepotest.Git(t, dir, append([]string{"rev-parse"}, revs...)...)) {
		f.id[names[i]] = id
	}
	var err error
	if f.repo, err = (gitrepo.Git{}).Open(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestMergeBase_IsTheCommonAncestorOfTwoCommitsOrNone(t *testing.T) {
	t.Parallel()
	f := newDiffFixture(t)
	for _, c := range []struct{ a, b, want string }{
		{"c2", "side", "c1"},
		{"side", "c2", "c1"},
		{"c2", "c1", "c1"},
		{"c2", "orphan", ""},
	} {
		got, err := f.repo.MergeBase(context.Background(), f.id[c.a], f.id[c.b])
		if err != nil || got != f.id[c.want] {
			t.Errorf("MergeBase(%s, %s) = %q, %v; want %q (%s)", c.a, c.b, got, err, f.id[c.want], c.want)
		}
	}
	if _, err := f.repo.MergeBase(context.Background(), "main", f.id["c1"]); !errors.Is(err, gitrepo.ErrNotFound) {
		t.Errorf("MergeBase(main, c1) error = %v, want ErrNotFound", err)
	}
}

func TestDiffChanges_ListsTheFilesThatDifferWithTheirNumbers(t *testing.T) {
	t.Parallel()
	h, d := newHistoryFixture(t), newDiffFixture(t)
	for _, c := range []struct {
		repo     *gitrepo.Repo
		from, to string
		want     []gitrepo.FileChange
	}{
		{h.repo, "", h.id["c1"], []gitrepo.FileChange{
			{Path: "a.txt", Status: gitrepo.ChangeAdded, Insertions: 1, NewID: h.id["c1:a.txt"]},
			{Path: "bin.dat", Status: gitrepo.ChangeAdded, Binary: true, NewID: h.id["c1:bin.dat"]},
			{Path: "old.md", Status: gitrepo.ChangeAdded, Insertions: 1, NewID: h.id["c1:old.md"]},
		}},
		{h.repo, h.id["c1"], h.id["c2"], []gitrepo.FileChange{
			{Path: "a.txt", Status: gitrepo.ChangeModified, Insertions: 2, Deletions: 1, OldID: h.id["c1:a.txt"], NewID: h.id["c2:a.txt"]},
			{Path: "new.md", OldPath: "old.md", Status: gitrepo.ChangeRenamed, OldID: h.id["c1:old.md"], NewID: h.id["c1:old.md"]},
		}},
		{h.repo, h.id["c2"], h.id["c1"], []gitrepo.FileChange{
			{Path: "a.txt", Status: gitrepo.ChangeModified, Insertions: 1, Deletions: 2, OldID: h.id["c2:a.txt"], NewID: h.id["c1:a.txt"]},
			{Path: "old.md", OldPath: "new.md", Status: gitrepo.ChangeRenamed, OldID: h.id["c1:old.md"], NewID: h.id["c1:old.md"]},
		}},
		{h.repo, h.id["c2"], h.id["c3"], nil},
		{d.repo, d.id["c1"], d.id["c2"], []gitrepo.FileChange{
			{Path: "bin.dat", Status: gitrepo.ChangeModified, Binary: true, OldID: d.id["c1:bin.dat"], NewID: d.id["c2:bin.dat"]},
			{Path: "dup.txt", OldPath: "src.txt", Status: gitrepo.ChangeCopied, OldID: d.id["c1:src.txt"], NewID: d.id["c1:src.txt"]},
			{Path: "kind.txt", Status: gitrepo.ChangeTypeChanged, Insertions: 1, Deletions: 1, OldID: d.id["c1:kind.txt"], NewID: d.id["c2:kind.txt"]},
			{Path: "src.txt", Status: gitrepo.ChangeModified, Insertions: 1, Deletions: 1, OldID: d.id["c1:src.txt"], NewID: d.id["c2:src.txt"]},
		}},
	} {
		got, err := c.repo.DiffChanges(context.Background(), c.from, c.to)
		if err != nil || len(got) != len(c.want) || (len(got) > 0 && !reflect.DeepEqual(got, c.want)) {
			t.Errorf("DiffChanges(%s, %s) = %+v, %v; want %+v", c.from, c.to, got, err, c.want)
		}
	}
}

func TestDiffPatches_ReadsThePatchOfEachFileInTheOrderOfItsChanges(t *testing.T) {
	t.Parallel()
	f := newDiffFixture(t)
	ctx := context.Background()
	read := func(from string, opt gitrepo.PatchOptions) []string {
		t.Helper()
		var patches []string
		err := f.repo.DiffPatches(ctx, from, f.id["c2"], opt, func(i int, patch []byte) bool {
			if i != len(patches) {
				t.Errorf("DiffPatches(%+v) passed the change %d after %d patches", opt, i, len(patches))
			}
			if patch == nil {
				patches = append(patches, "<too long>")
			} else {
				patches = append(patches, string(patch))
			}
			return true
		})
		if err != nil {
			t.Fatalf("DiffPatches(%+v): %v", opt, err)
		}
		return patches
	}
	const most = 1 << 20

	p0 := read(f.id["c1"], gitrepo.PatchOptions{Context: 0, Max: most})
	if whole := gitrepotest.Git(t, f.repo.Dir(), "diff-tree", "-r", "-p", "-M", "-C", "-U0", f.id["c1"], f.id["c2"]); len(p0) != 4 || strings.TrimSpace(strings.Join(p0, "")) != strings.TrimSpace(whole) {
		t.Fatalf("the patches with no context = %q, want the four files' parts of %q", p0, whole)
	}
	for i, want := range []string{"Binary files a/bin.dat and b/bin.dat differ", "diff --git a/src.txt b/dup.txt\nsimilarity index 100%\ncopy from src.txt", "new file mode 120000", "@@ -5 +5 @@\n-5\n+five\n"} {
		if !strings.Contains(p0[i], want) {
			t.Errorf("patch %d = %q, want it to hold %q", i, p0[i], want)
		}
	}
	if n := strings.Count(p0[2], "diff --git "); n != 2 || !strings.HasPrefix(p0[2], "diff --git a/kind.txt b/kind.txt\ndeleted file mode 100644") {
		t.Errorf("the type change's patch = %q, want the deletion of the file and the creation of the symlink", p0[2])
	}
	for line := range strings.Lines(p0[3]) {
		if strings.HasPrefix(line, " ") {
			t.Errorf("the patch of src.txt with no context has the context line %q", line)
		}
	}

	if p3 := read(f.id["c1"], gitrepo.PatchOptions{Context: 3, Max: most}); !strings.Contains(p3[3], "@@ -2,7 +2,7 @@\n 2\n 3\n 4\n-5\n+five\n 6\n") {
		t.Errorf("the patch of src.txt with three lines of context = %q", p3[3])
	}
	if pb := read(f.id["c1"], gitrepo.PatchOptions{Context: 3, Binary: true, Max: most}); !strings.Contains(pb[0], "GIT binary patch") {
		t.Errorf("the patch of bin.dat with binary patches = %q, want a binary patch", pb[0])
	}
	if pe := read("", gitrepo.PatchOptions{Context: 3, Max: most}); len(pe) != 4 || !strings.Contains(pe[1], "new file mode 100644") {
		t.Errorf("the patches from the empty tree = %q, want four new files", pe)
	}

	limit := len(p0[2]) - 1
	short := read(f.id["c1"], gitrepo.PatchOptions{Context: 0, Max: limit})
	for i, p := range p0 {
		want := p
		if len(p) > limit {
			want = "<too long>"
		}
		if short[i] != want {
			t.Errorf("with a limit of %d bytes, patch %d = %q, want %q", limit, i, short[i], want)
		}
	}

	calls := 0
	err := f.repo.DiffPatches(ctx, f.id["c1"], f.id["c2"], gitrepo.PatchOptions{Max: most}, func(i int, _ []byte) bool {
		calls++
		return i < 1
	})
	if err != nil || calls != 2 {
		t.Errorf("DiffPatches stopped after the second patch: %d calls, %v; want 2 calls and no error", calls, err)
	}
}

// side adds s.txt on top of c1; moved adds the same s.txt on top of c2, as
// a rebase of side would, and other adds another file there.
func TestPatchID_IsEqualForTheSameChangeOnAnotherBase(t *testing.T) {
	t.Parallel()
	f := newDiffFixture(t)
	ctx := context.Background()
	dir := f.repo.Dir()
	data := func(s string) string { return "data " + strconv.Itoa(len(s)) + "\n" + s + "\n" }
	const who = "committer T <t@example.invalid> 1700000001 +0000\n"
	stream := "commit refs/heads/moved\n" + who + data("moved") + "from " + f.id["c2"] + "\nM 100644 inline s.txt\n" + data("s\n") +
		"\ncommit refs/heads/other\n" + who + data("other") + "from " + f.id["c2"] + "\nM 100644 inline t.txt\n" + data("t\n") + "\n"
	if _, err := gitrepotest.Run(dir, stream, "fast-import", "--quiet"); err != nil {
		t.Fatal(err)
	}
	ids := strings.Fields(gitrepotest.Git(t, dir, "rev-parse", "moved", "other"))
	patchID := func(base, head string) string {
		t.Helper()
		id, err := f.repo.PatchID(ctx, base, head)
		if err != nil || id == "" {
			t.Fatalf("PatchID(%s, %s) = %q, %v", base, head, id, err)
		}
		return id
	}
	side, moved, other := patchID(f.id["c1"], f.id["side"]), patchID(f.id["c2"], ids[0]), patchID(f.id["c2"], ids[1])
	if side != moved {
		t.Errorf("the same change on another base: %q and %q, want equal", side, moved)
	}
	if other == side {
		t.Errorf("another change has the patch id %q of side", other)
	}
	emptyOne, emptyTwo := patchID(f.id["c1"], f.id["c1"]), patchID(f.id["c2"], f.id["c2"])
	if emptyOne != emptyTwo || emptyOne == side {
		t.Errorf("empty diffs = %q and %q, want equal and unlike %q", emptyOne, emptyTwo, side)
	}
}
