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

// ScratchParent is the directory of a repository's git directory under
// which its scratch working trees live, one per name. Every scratch working
// tree has this one place, so that whoever finds one left over by a work
// that ended without its cleanup can discard it.
const ScratchParent = "parley-rebuild"

// emptyTree is the id of the tree with no entry, which git knows without
// holding it.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// scratchConfig is the configuration of every git process that makes or
// works in a scratch working tree, given on its command line, where it
// overrides the repository's. Hooks are off. The attributes are read from
// the empty tree instead of the files being worked on, so that the content
// chooses no filter, merge driver or line-ending conversion. No automatic
// housekeeping runs in the background of a command.
var scratchConfig = []string{
	"-c", "core.hooksPath=" + os.DevNull,
	"-c", "attr.tree=" + emptyTree,
	"-c", "gc.auto=0",
	"-c", "maintenance.auto=false",
}

// Scratch is a scratch working tree of a bare repository: a detached
// working tree, with an index and a HEAD of its own, in which a commit is
// built where no client sees it. No ref points to what it holds; only its
// own HEAD, which no fetch is offered, does. Make it with Repo.AddScratch
// and remove it with Remove.
type Scratch struct {
	r   *Repo
	dir string
}

// PickOutcome is what the replay of one commit in a scratch working tree
// did.
type PickOutcome string

// The outcomes of a replay.
const (
	// PickApplied made a new commit on top of HEAD and moved HEAD to it.
	PickApplied PickOutcome = "applied"
	// PickDropped made no commit: HEAD already holds the commit's change.
	PickDropped PickOutcome = "dropped"
	// PickConflicted made no commit: the change does not apply to HEAD.
	PickConflicted PickOutcome = "conflicted"
)

// Picked is the result of the replay of one commit in a scratch working
// tree.
type Picked struct {
	Outcome PickOutcome
	// Commit is the new commit of PickApplied, "" otherwise.
	Commit string
	// Conflicts are the paths that did not merge, in path order, for
	// PickConflicted; nil otherwise.
	Conflicts []string
}

// AddScratch makes the scratch working tree name of the repository, at
// <git directory>/parley-rebuild/<name>, with HEAD detached at commit, a
// full id, and its files checked out. It adds or moves no ref. name is one
// path segment; a scratch working tree of that name that exists already is
// an error.
func (r *Repo) AddScratch(ctx context.Context, name, commit string) (*Scratch, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return nil, fmt.Errorf("scratch working tree %q: not a name", name)
	}
	if !isHexID(commit, idLen) {
		return nil, fmt.Errorf("scratch working tree at %q: %w", commit, ErrNotFound)
	}
	parent := filepath.Join(r.dir, ScratchParent)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, fmt.Errorf("scratch working tree %s: %w", name, err)
	}
	s := &Scratch{r: r, dir: filepath.Join(parent, name)}
	args := append(slices.Clone(scratchConfig), "--git-dir="+r.dir, "worktree", "add", "--quiet", "--detach", "--", s.dir, strings.ToLower(commit))
	if _, err := r.g.run(ctx, access{}, args...); err != nil {
		return nil, err
	}
	return s, nil
}

// Dir is the scratch working tree's directory.
func (s *Scratch) Dir() string { return s.dir }

// Head is the commit the scratch working tree's HEAD is at.
func (s *Scratch) Head(ctx context.Context) (string, error) {
	out, err := s.git(ctx, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Pick replays the commit of the full id commit, which has at most one
// parent, onto HEAD: its change against its parent is applied to HEAD's
// files and, unless HEAD already holds it, committed on top of HEAD with
// the commit's author, author date and message and with committer, its
// name, e-mail address and time written as given and never taken from the
// environment or a configuration. HEAD then moves to the new commit.
//
// A change that does not apply is PickConflicted with the paths that did
// not merge; the scratch working tree is left in that state, its files
// holding git's conflict markers, for the caller to resolve or undo, and
// HEAD does not move. A change HEAD already holds is PickDropped, with the
// files as HEAD has them. An error is a failure of git.
func (s *Scratch) Pick(ctx context.Context, commit string, committer Signature) (Picked, error) {
	orig, err := s.r.ReadCommits(ctx, []string{commit})
	if err != nil {
		return Picked{}, err
	}
	c := orig[0]
	if len(c.Parents) > 1 {
		return Picked{}, fmt.Errorf("replay %s: a merge commit is not replayed", c.ID)
	}
	head, err := s.Head(ctx)
	if err != nil {
		return Picked{}, err
	}
	if _, err := s.git(ctx, "cherry-pick", "--no-commit", c.ID); err != nil {
		if ctx.Err() != nil {
			return Picked{}, err
		}
		paths, uerr := s.unmerged(ctx)
		if uerr != nil {
			return Picked{}, errors.Join(err, uerr)
		}
		if len(paths) == 0 {
			return Picked{}, err
		}
		return Picked{Outcome: PickConflicted, Conflicts: paths}, nil
	}
	tree, err := s.git(ctx, "write-tree")
	if err != nil {
		return Picked{}, err
	}
	headTree, err := s.git(ctx, "rev-parse", "--verify", head+"^{tree}")
	if err != nil {
		return Picked{}, err
	}
	if bytes.Equal(tree, headTree) {
		return Picked{Outcome: PickDropped}, nil
	}
	author := c.Author
	zone, err := s.r.AuthorZone(ctx, c.ID)
	if err != nil {
		return Picked{}, err
	}
	author.Time = author.Time.In(zone)
	id, err := s.r.CreateCommit(ctx, CommitSpec{Tree: strings.TrimSpace(string(tree)), Parents: []string{head},
		Message: c.Message, Author: author, Committer: committer})
	if err != nil {
		return Picked{}, err
	}
	// The index and the files hold the new commit's tree already: only
	// HEAD, the scratch working tree's own, moves.
	if _, err := s.git(ctx, "update-ref", "--no-deref", "HEAD", id, head); err != nil {
		return Picked{}, err
	}
	return Picked{Outcome: PickApplied, Commit: id}, nil
}

// unmerged are the paths of the scratch working tree's index that did not
// merge, in path order.
func (s *Scratch) unmerged(ctx context.Context) ([]string, error) {
	out, err := s.git(ctx, "diff", "--name-only", "--diff-filter=U", "-z")
	if err != nil {
		return nil, err
	}
	var paths []string
	for p := range strings.SplitSeq(string(out), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)
	return slices.Compact(paths), nil
}

// Remove removes the scratch working tree, its files and its
// administrative entry in the repository, whatever state it is in. One
// that is gone already is no error.
func (s *Scratch) Remove(ctx context.Context) error {
	if err := os.RemoveAll(s.dir); err != nil {
		return fmt.Errorf("remove the scratch working tree %s: %w", s.dir, err)
	}
	// git drops the entry of every working tree whose directory is gone.
	_, err := s.r.g.run(ctx, access{}, append(slices.Clone(scratchConfig), "--git-dir="+s.r.dir, "worktree", "prune")...)
	return err
}

// git runs git with args in the scratch working tree.
func (s *Scratch) git(ctx context.Context, args ...string) ([]byte, error) {
	return s.r.g.runIn(ctx, s.dir, append(slices.Clone(scratchConfig), args...)...)
}

// runIn is run with no credential and with dir, a working tree, as git's
// working directory, so that git works on that working tree and its
// repository.
func (g Git) runIn(ctx context.Context, dir string, args ...string) ([]byte, error) {
	home, err := privateDir(false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(home) }()
	cmd := g.command(ctx, home, access{}, args)
	cmd.Dir = dir
	var out bytes.Buffer
	errOut := &limitedBuffer{limit: stderrLimit}
	cmd.Stdout, cmd.Stderr = &out, errOut
	if err := cmd.Run(); err != nil {
		stderr := strings.TrimSpace(errOut.String())
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("git %s: %w", subcommand(args), ctxErr)
		}
		return nil, fmt.Errorf("git %s: %w: %s", subcommand(args), err, stderr)
	}
	return out.Bytes(), nil
}
