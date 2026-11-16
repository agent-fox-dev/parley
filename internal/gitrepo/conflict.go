package gitrepo

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// The labels of the conflict markers of ConflictText: the target is ours,
// the change theirs.
const (
	labelTarget = "target"
	labelBase   = "base"
	labelChange = "change"
)

// ConflictText is the conflicted file f merged as text: every line both
// sides keep from f's base, or that one side alone changed, appears once,
// and each region both sides changed is written as a line
// "<<<<<<< target", the lines of f's ours side, a line "=======", the
// lines of its theirs side and a line ">>>>>>> change". There is never a
// "|||||||" section of the base, whatever merge.conflictStyle a git
// configuration sets. A side f does not have, such as the base of a file
// both sides added, is an empty file.
//
// The three blobs are written to files of a temporary directory, which is
// removed on every return, and merged there by git merge-file outside any
// repository.
func (r *Repo) ConflictText(ctx context.Context, f ConflictFile) ([]byte, error) {
	dir, err := os.MkdirTemp("", "parley-merge-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	args := []string{"merge-file", "-p", "--no-diff3", "-L", labelTarget, "-L", labelBase, "-L", labelChange}
	for _, side := range []struct {
		label string
		blob  *ConflictSide
	}{{labelTarget, f.Ours}, {labelBase, f.Base}, {labelChange, f.Theirs}} {
		path := filepath.Join(dir, side.label)
		if err := r.writeSide(ctx, side.blob, path); err != nil {
			return nil, fmt.Errorf("conflict text of %s: %w", f.Path, err)
		}
		args = append(args, path)
	}
	// merge-file exits with the number of conflicts it wrote, at most 127,
	// and with a status above that when it could not merge.
	out, _, err := r.g.runOutput(ctx, access{}, nil, args...)
	if code := exitCode(err); err != nil && (code < 1 || code > 127) {
		return nil, fmt.Errorf("conflict text of %s: %w", f.Path, err)
	}
	return out, nil
}

// writeSide writes the blob of side to a new file at path, an empty file
// for a nil side.
func (r *Repo) writeSide(ctx context.Context, side *ConflictSide, path string) (err error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := file.Close(); err == nil {
			err = cerr
		}
	}()
	if side == nil {
		return nil
	}
	return r.WriteBlob(ctx, side.SHA, file)
}
