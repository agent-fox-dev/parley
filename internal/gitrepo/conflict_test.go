package gitrepo_test

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// conflictRepo is a bare repository made with git init whose main is the
// commit base; target is one commit on base and work the commits of some
// work made on base, the last one the head.
type conflictRepo struct {
	repo *gitrepo.Repo
	dir  string
	base string
	// target is the onto commit, work the work's commits, oldest first.
	target string
	work   []string
}

// edit is one change of a commit's tree, as fast-import writes it: a
// file (mode, content) at path, a gitlink (mode 160000, content its
// commit id), or the removal of path for a mode of "".
type edit struct{ mode, path, content string }

func file(path, content string) edit { return edit{"100644", path, content} }
func remove(path string) edit        { return edit{"", path, ""} }

// newConflictRepo makes the repository: base holds base's edits on top of
// x.txt, target target's edits on base, and each commit of work the edits
// given for it on the one before, the first on base.
func newConflictRepo(t *testing.T, base, target []edit, work ...[]edit) conflictRepo {
	t.Helper()
	dir := t.TempDir()
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", dir)
	commit := func(ref, mark, from string, edits []edit) string {
		s := "commit refs/heads/" + ref + "\nmark :" + mark + "\ncommitter T <t@example.invalid> 1700000000 +0000\ndata 1\n" + mark + "\n"
		if from != "" {
			s += "from :" + from + "\n"
		}
		for _, e := range edits {
			switch {
			case e.mode == "":
				s += "D " + e.path + "\n"
			case e.mode == "160000":
				s += "M 160000 " + e.content + " " + e.path + "\n"
			default:
				s += "M " + e.mode + " inline " + e.path + "\ndata " + strconv.Itoa(len(e.content)) + "\n" + e.content + "\n"
			}
		}
		return s
	}
	stream := commit("main", "1", "", append([]edit{file("x.txt", "x\n")}, base...)) + commit("target", "2", "1", target)
	// Each commit of work is on the one before it, the first on base.
	for i, edits := range work {
		from := "1"
		if i > 0 {
			from = strconv.Itoa(i + 2)
		}
		stream += commit("work", strconv.Itoa(i+3), from, edits)
	}
	if _, err := gitrepotest.Run(dir, stream, "fast-import", "--quiet"); err != nil {
		t.Fatal(err)
	}
	repo, err := (gitrepo.Git{}).Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	r := conflictRepo{repo: repo, dir: dir, base: rev(t, dir, "main"), target: rev(t, dir, "target")}
	for i := range work {
		r.work = append(r.work, rev(t, dir, "work~"+strconv.Itoa(len(work)-1-i)))
	}
	return r
}

// head is the last commit of the work.
func (r conflictRepo) head() string { return r.work[len(r.work)-1] }

// rev is the full id of the revision of the repository at dir.
func rev(t *testing.T, dir, revision string) string {
	t.Helper()
	return gitrepotest.Git(t, dir, "rev-parse", revision)
}

// blob is the file path of the commit at as a side of a conflict, nil
// when that commit holds no file there.
func (r conflictRepo) blob(t *testing.T, at, path string) *gitrepo.ConflictSide {
	t.Helper()
	fields := strings.Fields(gitrepotest.Git(t, r.dir, "ls-tree", at, "--", path))
	if len(fields) != 4 || fields[1] == "tree" {
		return nil
	}
	return &gitrepo.ConflictSide{SHA: fields[2], Mode: fields[0], Path: path}
}

var fullID = regexp.MustCompile(`^[0-9a-f]{40}$`)

func TestTrialRebaseFrom_ReportsTheFirstCommitOfTheWorkThatDoesNotApply(t *testing.T) {
	t.Parallel()
	r := newConflictRepo(t,
		[]edit{file("a.txt", "a1\na2\na3\n"), file("b.txt", "b1\nb2\nb3\n")},
		[]edit{file("a.txt", "target\na2\na3\n"), file("b.txt", "target\nb2\nb3\n")},
		[]edit{file("c.txt", "c\n")},
		[]edit{file("b.txt", "work\nb2\nb3\n"), file("a.txt", "work\na2\na3\n")},
		[]edit{file("d.txt", "d\n")},
	)

	report, err := r.repo.TrialRebaseFrom(context.Background(), r.base, r.head(), r.target)

	if err != nil || report == nil {
		t.Fatalf("TrialRebaseFrom = %+v, %v; want a conflict", report, err)
	}
	if report.TargetSHA != r.target || report.HeadSHA != r.head() || report.BaseSHA != r.base || report.CommitSHA != r.work[1] {
		t.Errorf("the report names target %s, head %s, base %s, commit %s; want %s, %s, %s, %s", report.TargetSHA,
			report.HeadSHA, report.BaseSHA, report.CommitSHA, r.target, r.head(), r.base, r.work[1])
	}
	var paths []string
	for _, f := range report.Files {
		paths = append(paths, f.Path)
	}
	if want := []string{"a.txt", "b.txt"}; !slices.Equal(paths, want) {
		t.Errorf("the report's files are %q, want %q", paths, want)
	}

	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	members := func(m map[string]any) []string { return slices.Sorted(maps.Keys(m)) }
	if want := []string{"base_sha", "change", "commit_sha", "files", "head_sha", "landed_changes", "target_sha"}; !slices.Equal(members(got), want) {
		t.Errorf("the report's members are %q, want %q", members(got), want)
	}
	if got["change"] != nil || !reflect.DeepEqual(got["landed_changes"], []any{}) {
		t.Errorf("the report's change is %v and landed_changes %v, want null and []", got["change"], got["landed_changes"])
	}
	shas := []any{got["target_sha"], got["head_sha"], got["base_sha"], got["commit_sha"]}
	files, _ := got["files"].([]any)
	for _, f := range files {
		entry, _ := f.(map[string]any)
		if want := []string{"base", "kind", "landed_by", "ours", "path", "theirs"}; !slices.Equal(members(entry), want) {
			t.Errorf("a file entry's members are %q, want %q", members(entry), want)
		}
		for _, side := range []string{"base", "ours", "theirs"} {
			s, _ := entry[side].(map[string]any)
			shas = append(shas, s["sha"])
		}
	}
	for _, sha := range shas {
		if s, _ := sha.(string); !fullID.MatchString(s) {
			t.Errorf("the report holds the id %v, want 40 lowercase hexadecimal characters", sha)
		}
	}

	clean := newConflictRepo(t, []edit{file("a.txt", "a\n")}, []edit{file("a.txt", "target\n")},
		[]edit{file("c.txt", "c\n")}, []edit{file("d.txt", "d\n")})
	if report, err := clean.repo.TrialRebaseFrom(context.Background(), clean.base, clean.head(), clean.target); report != nil || err != nil {
		t.Errorf("TrialRebaseFrom of work that applies = %+v, %v; want no report", report, err)
	}
}

// conflictCase is a repository whose work's one commit conflicts on path
// with the target, the kind it is reported as, and the paths of the base,
// ours and theirs sides, "" for a side the file does not exist on.
type conflictCase struct {
	name         string
	base, target []edit
	work         []edit
	path, kind   string
	sides        [3]string
}

// gitlink is the commit id made of c, which a submodule entry points at.
func gitlink(c byte) string { return strings.Repeat(string(c), 40) }

var conflictCases = []conflictCase{
	{name: "both edit a line", base: []edit{file("a.txt", "l1\nl2\n")},
		target: []edit{file("a.txt", "target\nl2\n")}, work: []edit{file("a.txt", "work\nl2\n")},
		path: "a.txt", kind: gitrepo.ConflictContent, sides: [3]string{"a.txt", "a.txt", "a.txt"}},
	{name: "both add a file with different text",
		target: []edit{file("f.txt", "target\n")}, work: []edit{file("f.txt", "work\n")},
		path: "f.txt", kind: gitrepo.ConflictAddAdd, sides: [3]string{"", "f.txt", "f.txt"}},
	{name: "the target edits a file the work deletes", base: []edit{file("f.txt", "f\n")},
		target: []edit{file("f.txt", "target\n")}, work: []edit{remove("f.txt")},
		path: "f.txt", kind: gitrepo.ConflictModifyDelete, sides: [3]string{"f.txt", "f.txt", ""}},
	{name: "the target deletes a file the work edits", base: []edit{file("f.txt", "f\n")},
		target: []edit{remove("f.txt")}, work: []edit{file("f.txt", "work\n")},
		path: "f.txt", kind: gitrepo.ConflictDeleteModify, sides: [3]string{"f.txt", "", "f.txt"}},
	{name: "both rename a file elsewhere", base: []edit{file("f.txt", "1\n2\n3\n4\n5\n6\n7\n8\n")},
		target: []edit{remove("f.txt"), file("g.txt", "1\n2\n3\n4\n5\n6\n7\n8\n")},
		work:   []edit{remove("f.txt"), file("h.txt", "1\n2\n3\n4\n5\n6\n7\n8\n")},
		path:   "f.txt", kind: gitrepo.ConflictRename, sides: [3]string{"f.txt", "g.txt", "h.txt"}},
	{name: "both add a file with the same text and different modes",
		target: []edit{{"100755", "f.sh", "s\n"}}, work: []edit{file("f.sh", "s\n")},
		path: "f.sh", kind: gitrepo.ConflictMode, sides: [3]string{"", "f.sh", "f.sh"}},
	{name: "both change a binary file", base: []edit{file("bin", "\x00base")},
		target: []edit{file("bin", "\x00target")}, work: []edit{file("bin", "\x00work")},
		path: "bin", kind: gitrepo.ConflictBinary, sides: [3]string{"bin", "bin", "bin"}},
	{name: "both move a submodule", base: []edit{{"160000", "sub", gitlink('5')}},
		target: []edit{{"160000", "sub", gitlink('6')}}, work: []edit{{"160000", "sub", gitlink('7')}},
		path: "sub", kind: gitrepo.ConflictSubmodule, sides: [3]string{"sub", "sub", "sub"}},
	{name: "the target adds a file where the work adds a directory",
		target: []edit{file("d", "d\n")}, work: []edit{file("d/x", "x\n")},
		path: "d", kind: gitrepo.ConflictAddAdd, sides: [3]string{"", "d", ""}},
}

func TestTrialRebaseFrom_ReportsEachConflictedPathOnceWithItsKindAndSides(t *testing.T) {
	t.Parallel()
	for _, c := range conflictCases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newConflictRepo(t, c.base, c.target, c.work)

			report, err := r.repo.TrialRebaseFrom(context.Background(), r.base, r.head(), r.target)

			if err != nil || report == nil {
				t.Fatalf("TrialRebaseFrom = %+v, %v; want a conflict", report, err)
			}
			if len(report.Files) != 1 || report.Files[0].Path != c.path || report.Files[0].Kind != c.kind {
				t.Fatalf("the report's files are %+v; want %s alone, as %s", report.Files, c.path, c.kind)
			}
			f := report.Files[0]
			for i, s := range []struct {
				name, at string
				side     *gitrepo.ConflictSide
			}{{"base", r.base, f.Base}, {"ours", r.target, f.Ours}, {"theirs", r.head(), f.Theirs}} {
				var want *gitrepo.ConflictSide
				if c.sides[i] != "" {
					if want = r.blob(t, s.at, c.sides[i]); want == nil {
						t.Fatalf("the %s side's commit holds no file %s", s.name, c.sides[i])
					}
				}
				if !reflect.DeepEqual(s.side, want) {
					t.Errorf("%s is %+v, want %+v", s.name, s.side, want)
				}
			}
		})
	}
}

// Not parallel: it points the process's temporary directory at a
// directory of its own, to see what a replay leaves there.
func TestReplay_LeavesEveryRefAndNoTemporaryFileWhateverItEndsIn(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	ctx := context.Background()
	// path is the path the replay's conflict is in, which the resolving
	// rebase is given a resolution for.
	type replay struct {
		name string
		r    conflictRepo
		head string
		path string
	}
	var replays []replay
	for _, c := range conflictCases {
		r := newConflictRepo(t, c.base, c.target, c.work)
		replays = append(replays, replay{c.name, r, r.head(), c.path})
	}
	clean := newConflictRepo(t, []edit{file("a.txt", "a\n")}, []edit{file("a.txt", "target\n")}, []edit{file("c.txt", "c\n")})
	replays = append(replays, replay{"work that applies", clean, clean.head(), "a.txt"},
		replay{"a head that is no commit", clean, strings.Repeat("0", 40), "a.txt"})

	for _, p := range replays {
		for _, run := range []struct {
			name string
			call func() error
		}{
			{"TrialRebaseFrom", func() error { _, err := p.r.repo.TrialRebaseFrom(ctx, p.r.base, p.head, p.r.target); return err }},
			{"RebaseFrom", func() error {
				_, _, err := p.r.repo.RebaseFrom(ctx, p.r.base, p.head, p.r.target, rebaseAt)
				return err
			}},
			{"RebaseFromResolving", func() error {
				_, _, err := p.r.repo.RebaseFromResolving(ctx, p.r.base, p.head, p.r.target, rebaseAt,
					map[string]gitrepo.Resolution{p.path: {Content: []byte("resolved\n")}})
				return err
			}},
		} {
			refs, files := gitrepotest.Git(t, p.r.dir, "for-each-ref"), repoFiles(t, p.r.dir)
			before := entries(t, scratch)

			err := run.call()

			if wantErr := p.name == "a head that is no commit"; (err != nil) != wantErr {
				t.Errorf("%s, %s: the error is %v, want one: %v", p.name, run.name, err, wantErr)
			}
			if after := gitrepotest.Git(t, p.r.dir, "for-each-ref"); after != refs {
				t.Errorf("%s, %s: the refs changed from\n%s\nto\n%s", p.name, run.name, refs, after)
			}
			if after := repoFiles(t, p.r.dir); !slices.Equal(after, files) {
				t.Errorf("%s, %s: the repository's files outside objects/ were %q, then %q", p.name, run.name, files, after)
			}
			if after := entries(t, scratch); !slices.Equal(after, before) {
				t.Errorf("%s, %s: the temporary directory held %v, then %v", p.name, run.name, before, after)
			}
		}
	}
}

// entries are the names of the entries of the directory dir.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(list))
	for i, e := range list {
		names[i] = e.Name()
	}
	return names
}

// Not parallel: it points the process's temporary directory at a
// directory of its own, to see what the merge leaves there.
func TestConflictText_MarksEachOverlapAsTheTargetsLinesThenTheChangesWhateverTheGitConfig(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	ctx := context.Background()
	for _, c := range []struct {
		name               string
		base, target, work []edit
		want               string
	}{
		{"both change a line and the change adds one",
			[]edit{file("app.txt", "one\ntwo\nthree\n")},
			[]edit{file("app.txt", "one\nTARGET\nthree\n")},
			[]edit{file("app.txt", "one\nCHANGE\nthree\nfour\n")},
			"one\n<<<<<<< target\nTARGET\n=======\nCHANGE\n>>>>>>> change\nthree\nfour\n"},
		{"both add the file, from an empty base",
			nil,
			[]edit{file("app.txt", "one\nTARGET\n")},
			[]edit{file("app.txt", "one\nCHANGE\n")},
			"one\n<<<<<<< target\nTARGET\n=======\nCHANGE\n>>>>>>> change\n"},
	} {
		r := newConflictRepo(t, c.base, c.target, c.work)
		// The repository's own config and the hub's environment both ask
		// for a base section, which the text never has.
		gitrepotest.Git(t, r.dir, "config", "merge.conflictStyle", "zdiff3")
		repo, err := (gitrepo.Git{Env: []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=merge.conflictStyle",
			"GIT_CONFIG_VALUE_0=diff3"}}).Open(ctx, r.dir)
		if err != nil {
			t.Fatal(err)
		}
		report, err := repo.TrialRebaseFrom(ctx, r.base, r.head(), r.target)
		if err != nil || report == nil || len(report.Files) != 1 {
			t.Fatalf("%s: TrialRebaseFrom = %+v, %v; want a conflict in app.txt", c.name, report, err)
		}
		before := entries(t, scratch)

		text, err := repo.ConflictText(ctx, report.Files[0])

		if err != nil || string(text) != c.want {
			t.Errorf("%s: ConflictText = %q, %v; want %q", c.name, text, err, c.want)
		}
		if after := entries(t, scratch); !slices.Equal(after, before) {
			t.Errorf("%s: the temporary directory held %v, then %v", c.name, before, after)
		}
	}

	r := newConflictRepo(t, nil, []edit{file("app.txt", "target\n")}, []edit{file("app.txt", "work\n")})
	before := entries(t, scratch)
	missing := &gitrepo.ConflictSide{SHA: strings.Repeat("0", 40), Mode: "100644", Path: "app.txt"}
	if text, err := r.repo.ConflictText(ctx, gitrepo.ConflictFile{Path: "app.txt", Kind: gitrepo.ConflictContent,
		Ours: missing, Theirs: missing}); err == nil {
		t.Errorf("ConflictText of blobs the repository does not hold = %q, want an error", text)
	}
	if after := entries(t, scratch); !slices.Equal(after, before) {
		t.Errorf("a failed merge: the temporary directory held %v, then %v", before, after)
	}
}

// files are the entries other than directories of the tree of the commit
// at of the repository at dir, each as its mode, a space and its content,
// by path.
func files(t *testing.T, dir, at string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for line := range strings.SplitSeq(gitrepotest.Git(t, dir, "ls-tree", "-r", "--format=%(objectmode) %(objectname) %(path)", at), "\n") {
		f := strings.SplitN(line, " ", 3)
		if len(f) != 3 {
			t.Fatalf("ls-tree answered %q", line)
		}
		out[f[2]] = f[0] + " " + string(gitrepotest.Blob(t, dir, f[1]))
	}
	return out
}

func TestRebaseFromResolving_PutsEachResolutionInPlaceOfItsConflictedPath(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name               string
		base, target, work []edit
		resolutions        map[string]gitrepo.Resolution
		want               map[string]string
	}{
		{"a file in a directory, with the mode the change gives it",
			[]edit{file("dir/sub/a.sh", "l1\nl2\n"), file("dir/keep.txt", "k\n")},
			[]edit{file("dir/sub/a.sh", "target\nl2\n")},
			[]edit{{"100755", "dir/sub/a.sh", "work\nl2\n"}},
			map[string]gitrepo.Resolution{"dir/sub/a.sh": {Content: []byte("both\nl2\n")}},
			map[string]string{"x.txt": "100644 x\n", "dir/keep.txt": "100644 k\n", "dir/sub/a.sh": "100755 both\nl2\n"}},
		{"a file the change deletes, with the mode the target gives it",
			[]edit{{"100755", "f.sh", "f\n"}},
			[]edit{{"100755", "f.sh", "target\n"}},
			[]edit{remove("f.sh")},
			map[string]gitrepo.Resolution{"f.sh": {Content: []byte("kept\n")}},
			map[string]string{"x.txt": "100644 x\n", "f.sh": "100755 kept\n"}},
		{"a file the target deletes, deleted",
			[]edit{file("f.txt", "f\n")},
			[]edit{remove("f.txt")},
			[]edit{file("f.txt", "work\n")},
			map[string]gitrepo.Resolution{"f.txt": {Delete: true}},
			map[string]string{"x.txt": "100644 x\n"}},
		{"a file of the target where the change adds a directory, kept as a file",
			nil,
			[]edit{file("d", "d\n")},
			[]edit{file("d/x", "x\n")},
			map[string]gitrepo.Resolution{"d": {Content: []byte("file\n")}},
			map[string]string{"x.txt": "100644 x\n", "d": "100644 file\n"}},
		{"a file of the target where the change adds a directory, deleted",
			nil,
			[]edit{file("d", "d\n")},
			[]edit{file("d/x", "x\n")},
			map[string]gitrepo.Resolution{"d": {Delete: true}},
			map[string]string{"x.txt": "100644 x\n", "d/x": "100644 x\n"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newConflictRepo(t, c.base, c.target, c.work)

			head, report, err := r.repo.RebaseFromResolving(context.Background(), r.base, r.head(), r.target, rebaseAt, c.resolutions)

			if err != nil || report != nil || head == "" {
				t.Fatalf("RebaseFromResolving = %q, %+v, %v; want a head", head, report, err)
			}
			if _, err := gitrepotest.Run(r.dir, "", "merge-base", "--is-ancestor", r.target, head); err != nil {
				t.Errorf("the head %s is not built on the target %s: %v", head, r.target, err)
			}
			if got := files(t, r.dir, head); !reflect.DeepEqual(got, c.want) {
				t.Errorf("the head holds %q, want %q", got, c.want)
			}
		})
	}
}
