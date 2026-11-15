package gitrepo

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// findRenames are the options with which every comparison of two trees
// finds renamed and copied files, so that the file changes, their patches
// and a commit's numbers agree on what a file change is. A copy is found
// when its source changed in the same comparison.
var findRenames = []string{"-M", "-C"}

// The statuses of a FileChange.
const (
	ChangeAdded       = "added"
	ChangeModified    = "modified"
	ChangeDeleted     = "deleted"
	ChangeRenamed     = "renamed"
	ChangeCopied      = "copied"
	ChangeTypeChanged = "type_changed"
)

// FileChange is how one file differs between two trees.
type FileChange struct {
	// Path is the file's path on the new side, and on the old side for a
	// deleted file; OldPath is the path on the old side of a renamed or
	// copied file, "" otherwise.
	Path, OldPath string
	// Status is one of ChangeAdded, ChangeModified, ChangeDeleted,
	// ChangeRenamed, ChangeCopied and ChangeTypeChanged.
	Status string
	// Insertions and Deletions count lines, both 0 for a binary file.
	Insertions, Deletions int
	Binary                bool
	// OldID and NewID are the file's blob ids on each side, "" on the side
	// it is missing from.
	OldID, NewID string
}

// nullID is the id git writes for the side of a change a file is missing
// from.
var nullID = strings.Repeat("0", idLen)

// DiffChanges are the file changes from the tree of from to that of to,
// each a full id of a commit or a tree, from "" being the empty tree, in
// byte order of path, the order git writes them in. Renamed and copied
// files are found.
func (r *Repo) DiffChanges(ctx context.Context, from, to string) ([]FileChange, error) {
	from, to, err := r.diffSides(ctx, from, to)
	if err != nil {
		return nil, err
	}
	args := append(append([]string{"diff-tree", "-r", "-z"}, findRenames...), "--raw", "--numstat", from, to)
	out, err := r.git(ctx, args...)
	if err != nil {
		return nil, err
	}
	// The raw entries come first, each ":<modes> <ids> <status>" and its
	// paths, then a numstat entry for each, in the same order.
	fields := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	var changes []FileChange
	for len(fields) > 0 && strings.HasPrefix(fields[0], ":") {
		f := strings.Fields(fields[0])
		if len(f) != 5 || len(fields) < 2 {
			return nil, fmt.Errorf("diff: diff-tree answered %q", fields[0])
		}
		c := FileChange{Path: fields[1], OldID: blobID(f[2]), NewID: blobID(f[3])}
		fields = fields[2:]
		switch f[4][0] {
		case 'A':
			c.Status = ChangeAdded
		case 'D':
			c.Status = ChangeDeleted
		case 'M':
			c.Status = ChangeModified
		case 'T':
			c.Status = ChangeTypeChanged
		case 'R', 'C':
			if len(fields) == 0 {
				return nil, fmt.Errorf("diff: diff-tree answered %q without its new path", f[4])
			}
			c.Status = map[byte]string{'R': ChangeRenamed, 'C': ChangeCopied}[f[4][0]]
			c.OldPath, c.Path = c.Path, fields[0]
			fields = fields[1:]
		default:
			return nil, fmt.Errorf("diff: diff-tree answered the status %q", f[4])
		}
		changes = append(changes, c)
	}
	for i := range changes {
		n, rest, err := parseNumstat(fields)
		if err != nil {
			return nil, err
		}
		changes[i].Insertions, changes[i].Deletions, changes[i].Binary = n.Insertions, n.Deletions, n.binary
		fields = rest
	}
	return changes, nil
}

// blobID is the id git writes for one side of a change, "" for the side a
// file is missing from.
func blobID(id string) string {
	if id == nullID {
		return ""
	}
	return id
}

// emptyTree is the id of the tree that holds nothing, in the repository's
// object format; git knows it whether or not the repository stores it.
func (r *Repo) emptyTree(ctx context.Context) (string, error) {
	out, err := r.g.runInput(ctx, access{}, []byte{}, "--git-dir="+r.dir, "hash-object", "-t", "tree", "--stdin")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Stats are the numbers of changes taken together.
func Stats(changes []FileChange) DiffStats {
	var s DiffStats
	for _, c := range changes {
		s.add(numstat{Insertions: c.Insertions, Deletions: c.Deletions})
	}
	return s
}

// diffSides are the sides of a comparison of from with to, each a full id
// of a commit or a tree, from "" being the empty tree, as git takes them.
func (r *Repo) diffSides(ctx context.Context, from, to string) (string, string, error) {
	if from == "" {
		empty, err := r.emptyTree(ctx)
		if err != nil {
			return "", "", err
		}
		from = empty
	}
	for _, id := range []string{from, to} {
		if !isHexID(id, idLen) {
			return "", "", fmt.Errorf("diff of %q: %w", id, ErrNotFound)
		}
	}
	return strings.ToLower(from), strings.ToLower(to), nil
}

// MergeBase is the full id of the newest common ancestor of the commits of
// the full ids a and b, "" when they have none, which is a result and not
// an error. Of several equally good ancestors it is the one git picks.
func (r *Repo) MergeBase(ctx context.Context, a, b string) (string, error) {
	for _, id := range []string{a, b} {
		if !isHexID(id, idLen) {
			return "", fmt.Errorf("merge base of %q: %w", id, ErrNotFound)
		}
	}
	out, err := r.git(ctx, "merge-base", strings.ToLower(a), strings.ToLower(b))
	if err != nil {
		// merge-base exits with 1, saying nothing, when the commits share no
		// ancestor.
		if exitCode(err) == 1 && ctx.Err() == nil {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// PatchOptions choose how DiffPatches writes a file's patch.
type PatchOptions struct {
	// Context is the number of unchanged lines around each change.
	Context int
	// Binary writes a binary file as a patch git apply can apply, rather
	// than as a line saying that it differs.
	Binary bool
	// Max is the length in bytes of the longest patch read; a longer one is
	// passed as nil, and only Max bytes of it are ever held.
	Max int
}

// DiffPatches reads the unified patch of each file change DiffChanges(from,
// to) lists, in its order, and calls each with the change's index and the
// patch, until each returns false or the patches end. The patches are read
// as git writes them, one at a time, and git is stopped as soon as each
// returns false.
func (r *Repo) DiffPatches(ctx context.Context, from, to string, opt PatchOptions, each func(i int, patch []byte) bool) error {
	from, to, err := r.diffSides(ctx, from, to)
	if err != nil {
		return err
	}
	args := append(append([]string{"--git-dir=" + r.dir, "diff-tree", "-r", "-p"}, findRenames...), "-U"+strconv.Itoa(max(opt.Context, 0)))
	if opt.Binary {
		args = append(args, "--binary")
	}
	args = append(args, from, to)

	dir, err := privateDir(false)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	gitCtx, stop := context.WithCancel(ctx)
	defer stop()
	cmd := r.g.command(gitCtx, dir, access{}, args)
	errOut := &limitedBuffer{limit: stderrLimit}
	cmd.Stderr = errOut
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("git diff-tree: %w", err)
	}
	stopped, readErr := splitPatches(out, opt.Max, each)
	if stopped || readErr != nil {
		stop()
	}
	waitErr := cmd.Wait()
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("git diff-tree: %w", ctx.Err())
	case readErr != nil:
		return fmt.Errorf("git diff-tree: read the patches: %w", readErr)
	case waitErr != nil && !stopped:
		return fmt.Errorf("git diff-tree: %w: %s", waitErr, strings.TrimSpace(errOut.String()))
	}
	return nil
}

// patchHeader begins the part of a patch that describes one file. No other
// line of a patch begins with it: a line of a hunk begins with a space, +,
// - or \.
var patchHeader = []byte("diff --git ")

// splitPatches reads the patch in, the part of each file in turn, and calls
// each with the file's index and its part, nil for a part longer than max
// bytes, until each returns false; stopped says that it did. A file that
// changes between a regular file and a symlink or a submodule is written
// by git as a deletion and a creation under the same header line, which
// make up the one part of that file.
func splitPatches(in io.Reader, max int, each func(i int, patch []byte) bool) (stopped bool, err error) {
	br := bufio.NewReaderSize(in, 64<<10)
	var part, header []byte
	i, over, lineStart := -1, false, true
	emit := func() bool {
		if i < 0 {
			return true
		}
		if over {
			return each(i, nil)
		}
		return each(i, part)
	}
	for {
		line, err := br.ReadSlice('\n')
		if len(line) > 0 {
			if lineStart && bytes.HasPrefix(line, patchHeader) && !(err == nil && bytes.Equal(line, header)) {
				if !emit() {
					return true, nil
				}
				i, part, over = i+1, nil, false
				header = append(header[:0], line...)
			}
			if !over {
				if len(part)+len(line) > max {
					part, over = nil, true
				} else {
					part = append(part, line...)
				}
			}
			lineStart = err == nil
		}
		switch {
		case err == nil, errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return !emit(), nil
		default:
			return false, err
		}
	}
}
