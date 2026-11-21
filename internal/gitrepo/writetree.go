package gitrepo

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// git's modes of a tree's entries that WriteTree tells apart.
const (
	modeExecutable = "100755"
	modeLink       = "120000"
)

// maxLinkTarget is the longest target of a symbolic link WriteTree writes;
// a longer one is no path a file system takes.
const maxLinkTarget = 4096

// WriteTree writes the files of the commit of the full id commit into the
// new directory dir, from the repository's objects alone, and returns how
// many entries it wrote. It is no checkout: there is no .git, no index, no
// hook runs and no attribute, filter or line-ending conversion applies, so
// each file holds its blob's bytes. An executable file is executable, a
// symbolic link is written as a link with the target the commit holds and
// never followed, and a submodule entry is left out.
//
// Nothing is written outside dir, whatever the commit's tree holds: an
// entry whose path is not a plain relative path, or lies under a .git, is
// left out, and so is an entry whose path is taken on this file system, as
// two names that differ only by case are on some, so that no file is ever
// written through a link the tree itself made.
func (r *Repo) WriteTree(ctx context.Context, commit, dir string) (written int, err error) {
	out, err := r.git(ctx, "ls-tree", "-r", "-z", "--full-tree", strings.ToLower(commit))
	if err != nil {
		return 0, err
	}
	type entry struct{ mode, id, path string }
	var entries []entry
	for line := range bytes.SplitSeq(out, []byte{0}) {
		meta, name, ok := strings.Cut(string(line), "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			continue
		}
		if fields[1] != ObjectBlob || !plainPath(name) {
			continue
		}
		entries = append(entries, entry{mode: fields[0], id: fields[2], path: name})
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		return 0, fmt.Errorf("write the tree of %s: %w", commit, err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0, fmt.Errorf("write the tree of %s: %w", commit, err)
	}
	defer root.Close()
	if len(entries) == 0 {
		return 0, nil
	}

	private, err := privateDir(false)
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.RemoveAll(private) }()
	cmd := r.g.command(ctx, private, access{}, []string{"--git-dir=" + r.dir, "cat-file", "--batch"})
	errOut := &limitedBuffer{limit: stderrLimit}
	cmd.Stderr = errOut
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 0, fmt.Errorf("git cat-file: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, fmt.Errorf("git cat-file: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("git cat-file: %w", err)
	}
	// The ids are written beside the reading of the answers: git answers
	// each as it reads it, and neither pipe may fill while the other waits.
	go func() {
		w := bufio.NewWriter(stdin)
		for _, e := range entries {
			if _, err := w.WriteString(e.id + "\n"); err != nil {
				break
			}
		}
		_ = w.Flush()
		_ = stdin.Close()
	}()
	in := bufio.NewReaderSize(stdout, 1<<16)
	var writeErr error
	for _, e := range entries {
		size, err := blobHeader(in, e.id)
		if err != nil {
			writeErr = err
			break
		}
		blob := &io.LimitedReader{R: in, N: size}
		wrote, err := writeEntry(root, e.mode, e.path, blob)
		if err == nil {
			// The rest of a blob that was left out, and the line end git
			// writes after every blob.
			_, err = io.Copy(io.Discard, io.LimitReader(in, blob.N+1))
		}
		if err != nil {
			writeErr = fmt.Errorf("write %q of %s: %w", e.path, commit, err)
			break
		}
		if wrote {
			written++
		}
	}
	if writeErr != nil {
		// git stops at its closed output; its own error says less than ours.
		_ = stdout.Close()
		_ = cmd.Wait()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return written, fmt.Errorf("git cat-file: %w", ctxErr)
		}
		return written, writeErr
	}
	if err := cmd.Wait(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return written, fmt.Errorf("git cat-file: %w", ctxErr)
		}
		return written, fmt.Errorf("git cat-file: %w: %s", err, strings.TrimSpace(errOut.String()))
	}
	return written, nil
}

// plainPath reports whether name, a path of a tree, is one WriteTree
// writes: relative, without an empty, . or .. segment, and not under a
// .git in any letter case.
func plainPath(name string) bool {
	if name == "" || strings.ContainsRune(name, 0) || !filepath.IsLocal(filepath.FromSlash(name)) || path.Clean(name) != name {
		return false
	}
	for seg := range strings.SplitSeq(name, "/") {
		if strings.EqualFold(seg, ".git") {
			return false
		}
	}
	return true
}

// blobHeader reads the header git cat-file --batch writes before the blob
// id and returns the blob's size.
func blobHeader(in *bufio.Reader, id string) (int64, error) {
	line, err := in.ReadString('\n')
	if err != nil {
		return 0, fmt.Errorf("git cat-file: read the header of %s: %w", id, err)
	}
	fields := strings.Fields(line)
	if len(fields) != 3 || fields[1] != ObjectBlob {
		return 0, fmt.Errorf("git cat-file: %s is no blob: %s", id, strings.TrimSpace(line))
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 {
		return 0, fmt.Errorf("git cat-file: the size of %s: %s", id, strings.TrimSpace(line))
	}
	return size, nil
}

// writeEntry writes the entry name of mode with content below root and
// reports whether it did: an entry whose name is taken, or whose directory
// is not one, is left out. It may read less than all of content.
func writeEntry(root *os.Root, mode, name string, content io.Reader) (bool, error) {
	local := filepath.FromSlash(name)
	if parent := filepath.Dir(local); parent != "." {
		if err := root.MkdirAll(parent, 0o755); err != nil {
			return false, taken(err)
		}
	}
	if mode == modeLink {
		target, err := io.ReadAll(io.LimitReader(content, maxLinkTarget+1))
		if err != nil {
			return false, err
		}
		if len(target) == 0 || len(target) > maxLinkTarget || bytes.IndexByte(target, 0) >= 0 {
			return false, nil
		}
		if err := root.Symlink(string(target), local); err != nil {
			return false, taken(err)
		}
		return true, nil
	}
	perm := fs.FileMode(0o644)
	if mode == modeExecutable {
		perm = 0o755
	}
	f, err := root.OpenFile(local, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return false, taken(err)
	}
	_, err = io.Copy(f, content)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err == nil, err
}

// taken is nil for the error of an entry that is left out: its name is
// taken, a part of its path is no directory, or the tree's own links lead
// its path out of the directory. Any other error, such as a full disk, is
// returned.
func taken(err error) error {
	if errors.Is(err, fs.ErrExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP) ||
		strings.Contains(err.Error(), "path escapes") {
		return nil
	}
	return err
}
