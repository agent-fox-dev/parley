package gitrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// rerereConfig turns git's rerere on for one replay or merge, on the
// command line and not in the repository's configuration file: a conflict
// is recorded in the repository's rr-cache, a resolution recorded there
// earlier is replayed and, resolving a path, staged.
var rerereConfig = []string{"-c", "rerere.enabled=true", "-c", "rerere.autoUpdate=true"}

// rerereCache is the directory of a repository's git directory where git's
// rerere keeps the conflicts it recorded, one directory per conflict id.
const rerereCache = "rr-cache"

// RerereConflict is a conflict git's rerere recorded at a path of a scratch
// working tree. ID is the id git gave the conflict, which is the name of its
// directory in the repository's rr-cache: the same conflict met again, at
// any path and by any replay or merge in the repository, has the same id.
type RerereConflict struct {
	ID   string
	Path string
}

// RerereResolved reports whether the repository holds a resolution for the
// conflict id, that is a resolution git's rerere replays when it meets the
// conflict again. Git may keep several variants of one conflict; one
// resolved variant is enough.
func (r *Repo) RerereResolved(id string) (bool, error) {
	if !isHexID(id, idLen) {
		return false, fmt.Errorf("remembered resolution %q: not an id", id)
	}
	entries, err := os.ReadDir(filepath.Join(r.dir, rerereCache, strings.ToLower(id)))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("remembered resolution %s: %w", id, err)
	}
	for _, e := range entries {
		if name := e.Name(); name == "postimage" || strings.HasPrefix(name, "postimage.") {
			return true, nil
		}
	}
	return false, nil
}

// Learn redoes, in the scratch working tree, the merge commit of the full
// id merge, which has two parents, with rerere on, so that git records how
// the commit resolved what conflicted: it checks out the first parent and
// merges the second; at each path that conflicts and that merge holds, the
// file becomes the merge commit's, and git records that content as the
// resolution of the conflict. It returns the conflicts it recorded a
// resolution for, in path order; a merge that merges cleanly teaches
// nothing. The scratch working tree is left clean at the first parent;
// HEAD, the scratch working tree's own, is the one ref it moves.
func (s *Scratch) Learn(ctx context.Context, merge string) ([]RerereConflict, error) {
	cs, err := s.r.ReadCommits(ctx, []string{merge})
	if err != nil {
		return nil, err
	}
	m := cs[0]
	if len(m.Parents) != 2 {
		return nil, fmt.Errorf("learn from %s: not a merge of two parents", m.ID)
	}
	if _, err := s.git(ctx, "checkout", "--quiet", "--force", "--detach", "--no-recurse-submodules", m.Parents[0]); err != nil {
		return nil, err
	}
	conflicted, err := s.mergeNoCommit(ctx, m.Parents[1])
	if err != nil || !conflicted {
		return nil, errors.Join(err, s.Reset(ctx, m.Parents[0]))
	}
	learned, err := s.resolveAs(ctx, m.ID)
	return learned, errors.Join(err, s.Reset(ctx, m.Parents[0]))
}

// resolveAs gives each path left in conflict the content the commit holds
// there, when it holds one, and has git's rerere record the resolutions. It
// returns the conflicts git holds a resolution for then.
func (s *Scratch) resolveAs(ctx context.Context, commit string) ([]RerereConflict, error) {
	recorded, err := s.recorded(ctx)
	if err != nil {
		return nil, err
	}
	paths, err := s.unmerged(ctx)
	if err != nil {
		return nil, err
	}
	held, err := s.held(ctx, commit, paths)
	if err != nil {
		return nil, err
	}
	if len(held) > 0 {
		args := append([]string{"--literal-pathspecs", "checkout", "--quiet", commit, "--"}, held...)
		if _, err := s.git(ctx, args...); err != nil {
			return nil, err
		}
	}
	if _, err := s.git(ctx, append(slices.Clone(rerereConfig), "rerere")...); err != nil {
		return nil, err
	}
	var learned []RerereConflict
	for _, c := range recorded {
		ok, err := s.r.RerereResolved(c.ID)
		if err != nil {
			return nil, err
		}
		if ok {
			learned = append(learned, c)
		}
	}
	return learned, nil
}

// held are those of paths the commit holds a file at.
func (s *Scratch) held(ctx context.Context, commit string, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	args := append([]string{"--literal-pathspecs", "ls-tree", "-r", "-z", "--name-only", commit, "--"}, paths...)
	out, err := s.git(ctx, args...)
	if err != nil {
		return nil, err
	}
	in := map[string]bool{}
	for p := range strings.SplitSeq(string(out), "\x00") {
		in[p] = true
	}
	var held []string
	for _, p := range paths {
		if in[p] {
			held = append(held, p)
		}
	}
	return held, nil
}

// mergeNoCommit merges the commit into HEAD, stopping before the merge
// commit, with rerere on, and reports whether it stopped at a conflict,
// which may have left every path resolved by a recorded resolution. Any
// other failure of git is an error.
func (s *Scratch) mergeNoCommit(ctx context.Context, commit string) (bool, error) {
	args := append(slices.Clone(rerereConfig), "merge", "--no-commit", "--no-ff", "--no-squash", "--no-verify-signatures",
		"--no-log", "--no-stat", "--no-autostash", "--quiet", commit)
	return s.stopped(ctx, args...)
}

// stopped runs git with args, a replay or a merge with rerere on, and
// reports whether it stopped at a conflict: git's rerere then ran, which it
// does at a conflict alone. Any other failure of git is an error.
func (s *Scratch) stopped(ctx context.Context, args ...string) (bool, error) {
	_, err := s.git(ctx, args...)
	if err == nil {
		return false, nil
	}
	if ctx.Err() != nil {
		return false, err
	}
	admin, aerr := s.adminDir(ctx)
	if aerr != nil {
		return false, errors.Join(err, aerr)
	}
	if _, serr := os.Stat(filepath.Join(admin, "MERGE_RR")); serr != nil {
		return false, err
	}
	return true, nil
}

// recorded are the conflicts git's rerere recorded at the last replay or
// merge of the scratch working tree and holds no resolution for, in path
// order: its MERGE_RR, which a reset deletes, so it is read before one.
func (s *Scratch) recorded(ctx context.Context) ([]RerereConflict, error) {
	admin, err := s.adminDir(ctx)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(admin, "MERGE_RR"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the conflicts rerere recorded: %w", err)
	}
	var out []RerereConflict
	for rec := range bytes.SplitSeq(b, []byte{0}) {
		id, path, ok := strings.Cut(string(rec), "\t")
		if !ok || path == "" {
			continue
		}
		// A variant of a conflict is <id>.<n>: its id is the directory's.
		id, _, _ = strings.Cut(id, ".")
		if !isHexID(id, idLen) {
			continue
		}
		out = append(out, RerereConflict{ID: strings.ToLower(id), Path: path})
	}
	slices.SortFunc(out, func(a, b RerereConflict) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// adminDir is the scratch working tree's own git directory, where git
// keeps its HEAD, its index and the state of an operation in progress.
func (s *Scratch) adminDir(ctx context.Context) (string, error) {
	out, err := s.git(ctx, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
