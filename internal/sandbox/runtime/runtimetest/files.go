package runtimetest

import (
	"bytes"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

// file is a file or directory of a sandbox held in memory.
type file struct {
	dir  bool
	data []byte
	mode fs.FileMode
}

// cleanPath is the clean form of an absolute sandbox path.
func cleanPath(p string) string {
	return path.Clean("/" + p)
}

// checkPath refuses a path that is not absolute.
func checkPath(p string) (string, error) {
	if !strings.HasPrefix(p, "/") {
		return "", invalid("path %q is not absolute", p)
	}
	return cleanPath(p), nil
}

// hostError turns an error of the host's file system into the runtime's
// kinds, without the host path.
func hostError(p string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return notFound("no such path %q", p)
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	return invalid("%s: %v", p, err)
}

// makeRoot gives a new sandbox its file system with its working directory.
// The caller holds f.mu.
func (f *Fake) makeRoot(ws string, sb *sandbox) error {
	work, err := checkPath(workDirOf(sb))
	if err != nil {
		return err
	}
	if f.hostRoot == "" {
		return f.makeDir(sb, work, 0o755)
	}
	sb.host = filepath.Join(f.hostRoot, ws, sb.spec.Name)
	if err := os.RemoveAll(sb.host); err != nil {
		return hostError(work, err)
	}
	if err := os.MkdirAll(hostPath(sb.host, work), 0o755); err != nil {
		return hostError(work, err)
	}
	return nil
}

// makeDirs makes dir and its missing parents in memory, dir itself with
// mode.
func (sb *sandbox) makeDirs(dir string, mode fs.FileMode) error {
	if dir == "/" {
		return nil
	}
	if err := sb.makeDirs(path.Dir(dir), 0o755); err != nil {
		return err
	}
	switch e := sb.files[dir]; {
	case e == nil:
		sb.files[dir] = &file{dir: true, mode: mode.Perm()}
	case !e.dir:
		return invalid("%s is not a directory", dir)
	}
	return nil
}

func (f *Fake) makeDir(sb *sandbox, p string, mode fs.FileMode) error {
	p, err := checkPath(p)
	if err != nil {
		return err
	}
	if sb.host == "" {
		return sb.makeDirs(p, mode)
	}
	hp := hostPath(sb.host, p)
	if err := os.MkdirAll(hp, 0o755); err != nil {
		return hostError(p, err)
	}
	if err := os.Chmod(hp, mode.Perm()); err != nil {
		return hostError(p, err)
	}
	return nil
}

func (f *Fake) writeFile(sb *sandbox, p string, mode fs.FileMode, data []byte) error {
	p, err := checkPath(p)
	if err != nil {
		return err
	}
	if p == "/" {
		return invalid("/ is a directory")
	}
	if sb.host == "" {
		if err := sb.makeDirs(path.Dir(p), 0o755); err != nil {
			return err
		}
		if e := sb.files[p]; e != nil && e.dir {
			return invalid("%s is a directory", p)
		}
		sb.files[p] = &file{data: bytes.Clone(data), mode: mode.Perm()}
		return nil
	}
	hp := hostPath(sb.host, p)
	if err := os.MkdirAll(filepath.Dir(hp), 0o755); err != nil {
		return hostError(p, err)
	}
	if err := os.WriteFile(hp, data, mode.Perm()); err != nil {
		return hostError(p, err)
	}
	if err := os.Chmod(hp, mode.Perm()); err != nil {
		return hostError(p, err)
	}
	return nil
}

func (f *Fake) stat(sb *sandbox, p string) (runtime.FileInfo, error) {
	p, err := checkPath(p)
	if err != nil {
		return runtime.FileInfo{}, err
	}
	if sb.host != "" {
		fi, err := os.Stat(hostPath(sb.host, p))
		if err != nil {
			return runtime.FileInfo{}, hostError(p, err)
		}
		return hostInfo(p, fi), nil
	}
	if p == "/" {
		return runtime.FileInfo{Path: p, Dir: true, Mode: 0o755}, nil
	}
	e := sb.files[p]
	if e == nil {
		return runtime.FileInfo{}, notFound("no such path %q", p)
	}
	return e.info(p), nil
}

func (e *file) info(p string) runtime.FileInfo {
	return runtime.FileInfo{Path: p, Dir: e.dir, Size: int64(len(e.data)), Mode: e.mode}
}

func hostInfo(p string, fi fs.FileInfo) runtime.FileInfo {
	info := runtime.FileInfo{Path: p, Dir: fi.IsDir(), Mode: fi.Mode().Perm()}
	if !info.Dir {
		info.Size = fi.Size()
	}
	return info
}

func (f *Fake) readFile(sb *sandbox, p string) ([]byte, error) {
	p, err := checkPath(p)
	if err != nil {
		return nil, err
	}
	if sb.host != "" {
		data, err := os.ReadFile(hostPath(sb.host, p))
		if err != nil {
			return nil, hostError(p, err)
		}
		return data, nil
	}
	e := sb.files[p]
	switch {
	case p == "/" || (e != nil && e.dir):
		return nil, invalid("%s is a directory", p)
	case e == nil:
		return nil, notFound("no such path %q", p)
	}
	return bytes.Clone(e.data), nil
}

func (f *Fake) walk(sb *sandbox, dir string) ([]runtime.FileInfo, error) {
	top, err := f.stat(sb, dir)
	if err != nil {
		return nil, err
	}
	if !top.Dir {
		return nil, invalid("%s is not a directory", top.Path)
	}
	dir = top.Path
	var out []runtime.FileInfo
	if sb.host == "" {
		for p, e := range sb.files {
			if below(p, dir) {
				out = append(out, e.info(p))
			}
		}
	} else {
		root := hostPath(sb.host, dir)
		err := filepath.WalkDir(root, func(hp string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if hp == root {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, hp)
			if err != nil {
				return err
			}
			out = append(out, hostInfo(path.Join(dir, filepath.ToSlash(rel)), fi))
			return nil
		})
		if err != nil {
			return nil, hostError(dir, err)
		}
	}
	slices.SortFunc(out, func(a, b runtime.FileInfo) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// below reports whether p is below the directory dir.
func below(p, dir string) bool {
	if dir == "/" {
		return p != "/"
	}
	return strings.HasPrefix(p, dir+"/")
}

// removeTree removes a directory of a sandbox and everything below it; the
// sandbox's root stays. The caller holds f.mu.
func (f *Fake) removeTree(sb *sandbox, dir string) {
	dir = cleanPath(dir)
	if sb.host == "" {
		for p := range maps.Clone(sb.files) {
			if p == dir || below(p, dir) {
				delete(sb.files, p)
			}
		}
		return
	}
	if err := os.RemoveAll(hostPath(sb.host, dir)); err != nil {
		f.t.Errorf("runtimetest: removing %s of sandbox %q: %v", dir, sb.spec.Name, err)
	}
	if err := os.MkdirAll(sb.host, 0o755); err != nil {
		f.t.Errorf("runtimetest: keeping the root of sandbox %q: %v", sb.spec.Name, err)
	}
}
