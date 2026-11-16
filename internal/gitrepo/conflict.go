package gitrepo

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
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

// Resolution is what a path that does not merge holds in a resolving
// rebase (RebaseFromResolving): Content, or no file when Delete is set.
type Resolution struct {
	Content []byte
	Delete  bool
}

// RebaseFromResolving is RebaseFrom with a resolution for some paths, by
// path. At a commit that does not apply, when every conflicted path of the
// commit has a resolution, the commit is replayed as the tree git merged
// for it, each conflicted path replaced by its resolution's content, or
// with no file there for one that deletes, and the rebase goes on; a path
// that conflicts at several commits gets its resolution at each. A
// resolution whose path does not conflict at a commit changes nothing
// there. The first commit with a conflicted path that has no resolution
// ends the rebase with its report, as RebaseFrom does.
//
// A resolved file has the mode it has in the commit replayed, else the
// mode of the tree it is replayed onto, else ModeFile. It takes the place
// of whatever is at its path, a directory too, of a side git moved aside
// to <path>~<commit>, and of the paths the conflict's other sides name,
// such as both paths a file was renamed to; a deleted one leaves a
// directory at its path in place.
//
// Like the rebase, the resolution happens in the object store alone: the
// trees and blobs it writes are new objects, and no index is used.
func (r *Repo) RebaseFromResolving(ctx context.Context, base, head, onto string, at time.Time, resolutions map[string]Resolution) (string, *ConflictReport, error) {
	newHead, _, report, err := r.rebase(ctx, base, head, onto, HubSignature(at), resolutions)
	if report != nil {
		report.BaseSHA = base
	}
	return newHead, report, err
}

// resolveFunc is given, for the commit theirs, which does not apply on top
// of the commit ours, the tree git merged for it and its conflicted files,
// and returns the tree the commit is replayed as, with ok set, or ok unset
// to leave the conflict.
type resolveFunc func(ours, theirs, tree string, files []ConflictFile) (resolved string, ok bool, err error)

// resolver is the resolveFunc of resolutions, as RebaseFromResolving
// says. The blob of each resolution is written once, when it is first
// used.
func (r *Repo) resolver(ctx context.Context, resolutions map[string]Resolution) resolveFunc {
	blobs := map[string]string{}
	return func(ours, theirs, tree string, files []ConflictFile) (string, bool, error) {
		edits := map[string]*treeEntry{}
		for _, f := range files {
			if _, ok := resolutions[f.Path]; !ok {
				return "", false, nil
			}
			edits[f.Path+"~"+ours], edits[f.Path+"~"+theirs] = nil, nil
			// The resolution stands in for the whole conflict: the paths
			// its other sides name, such as where the change renamed a
			// file the target renamed too, go.
			for _, side := range []*ConflictSide{f.Base, f.Ours, f.Theirs} {
				if side != nil && side.Path != f.Path {
					edits[side.Path] = nil
				}
			}
		}
		for _, f := range files {
			res := resolutions[f.Path]
			if res.Delete {
				edits[f.Path] = nil
				continue
			}
			blob, ok := blobs[f.Path]
			if !ok {
				var err error
				if blob, err = r.writeBlob(ctx, res.Content); err != nil {
					return "", false, err
				}
				blobs[f.Path] = blob
			}
			edits[f.Path] = &treeEntry{mode: resolvedMode(f), typ: ObjectBlob, id: blob}
		}
		resolved, _, err := r.editTree(ctx, tree, edits)
		return resolved, err == nil, err
	}
}

// resolvedMode is the mode of the resolution of the conflicted file f:
// that of its side in the change, theirs, else in the target, ours, when
// that side is a file, and ModeFile otherwise.
func resolvedMode(f ConflictFile) string {
	for _, side := range []*ConflictSide{f.Theirs, f.Ours} {
		if side != nil && (side.Mode == ModeFile || side.Mode == ModeExecutable || side.Mode == ModeSymlink) {
			return side.Mode
		}
	}
	return ModeFile
}

// writeBlob writes content as a blob, byte for byte, and returns its id.
func (r *Repo) writeBlob(ctx context.Context, content []byte) (string, error) {
	out, err := r.g.runInput(ctx, access{}, content, "--git-dir="+r.dir, "hash-object", "-t", ObjectBlob, "-w", "--stdin")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// treeEntry is one entry of a tree: its mode, its object's type and id.
type treeEntry struct {
	mode, typ, id string
}

// modeTree is the mode git writes for a directory's entry.
const modeTree = "040000"

// editTree writes the tree that is tree ("" for an empty one) with edits,
// by path relative to it, applied: an entry is put at its path, in place of
// whatever is there, and a nil one removes the entry at its path unless
// it is a directory; the directories of a path are made as needed, in place
// of a file, and a directory left empty goes. It returns the new tree's id
// and whether it is empty. Each directory an edit reaches is read and
// written again with git ls-tree and git mktree, in the object store.
func (r *Repo) editTree(ctx context.Context, tree string, edits map[string]*treeEntry) (string, bool, error) {
	entries := map[string]treeEntry{}
	if tree != "" {
		out, err := r.git(ctx, "ls-tree", "-z", tree)
		if err != nil {
			return "", false, err
		}
		for rec := range strings.SplitSeq(strings.TrimSuffix(string(out), "\x00"), "\x00") {
			if rec == "" {
				continue
			}
			meta, name, ok := strings.Cut(rec, "\t")
			f := strings.Fields(meta)
			if !ok || len(f) != 3 {
				return "", false, fmt.Errorf("ls-tree answered %q", rec)
			}
			entries[name] = treeEntry{mode: f[0], typ: f[1], id: f[2]}
		}
	}
	below := map[string]map[string]*treeEntry{}
	for path, e := range edits {
		name, rest, deeper := strings.Cut(path, "/")
		switch {
		case deeper:
			if below[name] == nil {
				below[name] = map[string]*treeEntry{}
			}
			below[name][rest] = e
		case e != nil:
			entries[name] = *e
		case entries[name].typ != ObjectTree:
			delete(entries, name)
		}
	}
	for name, sub := range below {
		var subtree string
		if cur, ok := entries[name]; ok && cur.typ == ObjectTree {
			subtree = cur.id
		} else if removesOnly(sub) {
			// Nothing to remove below what is no directory.
			continue
		}
		id, empty, err := r.editTree(ctx, subtree, sub)
		switch {
		case err != nil:
			return "", false, err
		case empty:
			delete(entries, name)
		default:
			entries[name] = treeEntry{mode: modeTree, typ: ObjectTree, id: id}
		}
	}
	var in strings.Builder
	for name, e := range entries {
		in.WriteString(e.mode + " " + e.typ + " " + e.id + "\t" + name + "\x00")
	}
	// mktree orders the entries itself; --missing lets a submodule's
	// commit, which the repository does not hold, stay.
	out, err := r.g.runInput(ctx, access{}, []byte(in.String()), "--git-dir="+r.dir, "mktree", "-z", "--missing")
	if err != nil {
		return "", false, err
	}
	return strings.TrimSpace(string(out)), len(entries) == 0, nil
}

// removesOnly reports whether every edit of edits removes an entry.
func removesOnly(edits map[string]*treeEntry) bool {
	for _, e := range edits {
		if e != nil {
			return false
		}
	}
	return true
}
