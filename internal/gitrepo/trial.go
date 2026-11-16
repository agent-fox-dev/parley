package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// ErrMergeCommit is the error of a trial rebase whose commits to replay
// include a merge commit, which a rebase that keeps no merges cannot
// replay.
var ErrMergeCommit = errors.New("a merge commit is among the commits to replay")

// TrialRebase replays the commits that the commit of the full id head
// reaches and that of onto does not, oldest first, on top of onto, and
// reports the first that does not apply, nil when every one applies. A
// merge commit among them is ErrMergeCommit, before anything is replayed.
//
// The replay happens in the object store alone: no work tree, no index and
// no ref is used or changed. It may leave objects no ref reaches, which
// git's garbage collection removes.
func (r *Repo) TrialRebase(ctx context.Context, onto, head string) (*ConflictReport, error) {
	return r.trial(ctx, onto, head, onto)
}

// TrialRebaseFrom is TrialRebase of the commits that head reaches and base
// does not, the commits of a branch made on base, on top of onto, as
// RebaseFrom replays them: base's own commits are not replayed, also when
// onto does not have them. A base of "" replays every commit of head. The
// report of a commit that does not apply names base as its base.
func (r *Repo) TrialRebaseFrom(ctx context.Context, base, head, onto string) (*ConflictReport, error) {
	report, err := r.trial(ctx, base, head, onto)
	if report != nil {
		report.BaseSHA = base
	}
	return report, err
}

// trial replays the commits from..head, every commit of head for a from
// of "", on top of onto, as TrialRebase says.
func (r *Repo) trial(ctx context.Context, from, head, onto string) (*ConflictReport, error) {
	onto, head, err := rangeEnds(onto, head)
	if err != nil {
		return nil, err
	}
	commitRange := head
	if from != "" {
		if from, _, err = rangeEnds(from, head); err != nil {
			return nil, err
		}
		commitRange = from + ".." + head
	}
	commits, err := r.replayed(ctx, commitRange)
	if err != nil || len(commits) == 0 {
		return nil, err
	}

	// The commit itself holds the tree its replay makes when that is its
	// own tree; otherwise only the tree is known, and the next merge that
	// needs it as a commit writes one (trialCommit).
	_, report, err := r.replay(ctx, onto, head, commits, func(c Commit, _, _, tree string) (string, error) {
		if tree == c.Tree {
			return c.ID, nil
		}
		return "", nil
	})
	return report, err
}

// replayed lists the commits of the rev-list range commitRange, such as
// onto..head, oldest first, each with its tree and parents alone: their
// messages are not read. A merge commit among them is ErrMergeCommit.
func (r *Repo) replayed(ctx context.Context, commitRange string) ([]Commit, error) {
	out, err := r.git(ctx, "rev-list", "--reverse", "--no-commit-header", "--format=%H %T %P", commitRange)
	if err != nil {
		return nil, err
	}
	var commits []Commit
	for line := range strings.Lines(string(out)) {
		f := strings.Fields(line)
		switch {
		case len(f) < 2:
			return nil, fmt.Errorf("rev-list answered %q", line)
		case len(f) > 3:
			return nil, ErrMergeCommit
		}
		commits = append(commits, Commit{ID: f[0], Tree: f[1], Parents: f[2:]})
	}
	return commits, nil
}

// trialStart is the tree of the commit onto, and the parent of the commit
// first with its tree: for a root commit, a commit of the empty tree, the
// side a three-way merge starts from for everything first adds.
func (r *Repo) trialStart(ctx context.Context, onto string, first Commit) (ontoTree, parent, parentTree string, err error) {
	names := []string{onto + "^{tree}"}
	if len(first.Parents) > 0 {
		parent = first.Parents[0]
		names = append(names, parent+"^{tree}")
	}
	trees, err := r.batchCheck(ctx, "%(objectname)", names)
	if err != nil {
		return "", "", "", err
	}
	if len(trees) != len(names) || slices.ContainsFunc(trees, func(l string) bool { return strings.HasSuffix(l, " missing") }) {
		return "", "", "", fmt.Errorf("trial rebase of %s: cat-file answered %q", first.ID, trees)
	}
	if parent != "" {
		return trees[0], parent, trees[1], nil
	}
	if parentTree, err = r.emptyTree(ctx); err != nil {
		return "", "", "", err
	}
	parent, err = r.trialCommit(ctx, parentTree)
	return trees[0], parent, parentTree, err
}

// trialCommit writes a commit of tree with no parent and returns its id,
// for a merge that takes commits and not trees: git merge-tree before 2.45
// takes no tree.
func (r *Repo) trialCommit(ctx context.Context, tree string) (string, error) {
	obj := "tree " + tree + "\nauthor " + HubName + " <" + HubEmail + "> 0 +0000\ncommitter " + HubName + " <" + HubEmail + "> 0 +0000\n\ntrial rebase\n"
	out, err := r.g.runInput(ctx, access{}, []byte(obj), "--git-dir="+r.dir, "hash-object", "-t", "commit", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// mergeTree merges the commit theirs into ours with the commit base as the
// merge base, writing the result's objects, and returns the merged tree,
// or the conflicted files, in byte order of path, when it does not merge
// cleanly.
func (r *Repo) mergeTree(ctx context.Context, base, ours, theirs string) (string, []ConflictFile, error) {
	out, _, err := r.g.runOutput(ctx, access{}, nil, "--git-dir="+r.dir, "merge-tree", "--write-tree", "-z",
		"--messages", "--merge-base="+base, ours, theirs)
	switch {
	case err == nil:
		tree, _, _ := strings.Cut(string(out), "\x00")
		return tree, nil, nil
	case exitCode(err) == 1 && ctx.Err() == nil:
		// merge-tree exits with 1 when the merge has conflicts.
		files, perr := parseConflicts(out)
		for i := range files {
			files[i].Path = asidePath(files[i].Path, ours, theirs)
			for _, side := range []*ConflictSide{files[i].Base, files[i].Ours, files[i].Theirs} {
				if side != nil {
					side.Path = asidePath(side.Path, ours, theirs)
				}
			}
		}
		slices.SortFunc(files, func(a, b ConflictFile) int { return strings.Compare(a.Path, b.Path) })
		return "", files, perr
	}
	return "", nil, err
}

// asidePath is the path of a side of a conflict before git moved it aside.
// To record both sides of a path the sides give different types, such as
// a file and a symlink, or a file and a directory, git renames one of
// them to <path>~<label>, the label being the commit merge-tree was given
// for that side; that path exists on no side, and the side's own path is
// <path>.
func asidePath(path, ours, theirs string) string {
	for _, label := range []string{ours, theirs} {
		if p, ok := strings.CutSuffix(path, "~"+label); ok && p != "" {
			return p
		}
	}
	return path
}

// parseConflicts reads what git merge-tree -z writes for a merge with
// conflicts: the tree's id; the conflicted file entries, "<mode> <id>
// <stage>\t<path>", stage 1 the merge base, 2 ours and 3 theirs; an empty
// field; then the messages, each the number of paths it names, the paths,
// a type such as "CONFLICT (contents)" and the text. Each conflict message
// makes one conflicted file of the paths it names; a stage entry no
// message names makes one of its own path.
func parseConflicts(out []byte) ([]ConflictFile, error) {
	fields := strings.Split(string(out), "\x00")
	if len(fields) < 2 {
		return nil, fmt.Errorf("merge-tree answered %q", out)
	}
	fields = fields[1:]
	// The sides of each path, by stage.
	stages := map[string]*[4]*ConflictSide{}
	var staged []string
	for len(fields) > 0 && fields[0] != "" {
		info, path, ok := strings.Cut(fields[0], "\t")
		f := strings.Fields(info)
		if !ok || len(f) != 3 || len(f[2]) != 1 || f[2][0] < '1' || f[2][0] > '3' {
			return nil, fmt.Errorf("merge-tree answered the entry %q", fields[0])
		}
		if stages[path] == nil {
			stages[path] = &[4]*ConflictSide{}
			staged = append(staged, path)
		}
		stages[path][f[2][0]-'0'] = &ConflictSide{SHA: f[1], Mode: f[0], Path: path}
		fields = fields[1:]
	}
	if len(fields) > 0 {
		fields = fields[1:]
	}

	type conflict struct {
		paths []string
		types []string
	}
	var conflicts []*conflict
	owner := map[string]*conflict{}
	for len(fields) > 0 && fields[0] != "" {
		n, err := strconv.Atoi(fields[0])
		if err != nil || n < 0 || len(fields) < n+3 {
			return nil, fmt.Errorf("merge-tree answered the message %q", fields)
		}
		paths, typ := fields[1:1+n], fields[1+n]
		fields = fields[n+3:]
		if !strings.HasPrefix(typ, "CONFLICT") || n == 0 {
			continue
		}
		var c *conflict
		for _, p := range paths {
			if c = owner[p]; c != nil {
				break
			}
		}
		if c == nil {
			c = &conflict{}
			conflicts = append(conflicts, c)
		}
		for _, p := range paths {
			if owner[p] == nil {
				owner[p], c.paths = c, append(c.paths, p)
			}
		}
		c.types = append(c.types, typ)
	}
	for _, p := range staged {
		if owner[p] == nil {
			c := &conflict{paths: []string{p}}
			owner[p], conflicts = c, append(conflicts, c)
		}
	}

	files := make([]ConflictFile, 0, len(conflicts))
	for _, c := range conflicts {
		f := ConflictFile{Path: c.paths[0]}
		var sides [4]*ConflictSide
		for stage := 1; stage <= 3; stage++ {
			for _, p := range c.paths {
				if s := stages[p]; s != nil && s[stage] != nil {
					sides[stage] = s[stage]
					break
				}
			}
		}
		f.Base, f.Ours, f.Theirs = sides[1], sides[2], sides[3]
		f.Kind = conflictKind(c.types, sides)
		files = append(files, f)
	}
	return files, nil
}

// conflictKind is the kind of a conflicted file whose messages are of the
// types types and whose sides, by stage, are sides: the first that applies
// of submodule, binary and rename, as git names the conflict; add_add for
// a file both sides added with different content, or a file one side put
// where the other put a directory; modify_delete and delete_modify for a
// file one side deleted, the target being ours and the change theirs; mode
// for sides that hold the file with different modes, such as a file and a
// symlink, or the same content added with two modes; content otherwise.
// One kind for each path, in an order that goes from what a text
// resolution can least express to what it can.
func conflictKind(types []string, sides [4]*ConflictSide) string {
	has := func(match func(string) bool) bool { return slices.ContainsFunc(types, match) }
	submodule := slices.ContainsFunc(sides[1:], func(s *ConflictSide) bool { return s != nil && s.Mode == "160000" })
	base, ours, theirs := sides[1], sides[2], sides[3]
	switch {
	case submodule || has(func(t string) bool { return strings.Contains(t, "submodule") }):
		return ConflictSubmodule
	case has(func(t string) bool { return t == "CONFLICT (binary)" }):
		return ConflictBinary
	case has(func(t string) bool { return strings.Contains(t, "rename") }):
		return ConflictRename
	case has(func(t string) bool { return t == "CONFLICT (file/directory)" }),
		base == nil && ours != nil && theirs != nil && ours.SHA != theirs.SHA:
		return ConflictAddAdd
	case base != nil && ours != nil && theirs == nil:
		return ConflictModifyDelete
	case base != nil && ours == nil && theirs != nil:
		return ConflictDeleteModify
	case has(func(t string) bool { return t == "CONFLICT (distinct modes)" }),
		ours != nil && theirs != nil && ours.Mode != theirs.Mode:
		return ConflictMode
	}
	return ConflictContent
}
