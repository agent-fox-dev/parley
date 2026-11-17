package gitrepotest

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// templateFile is a file or directory of a repository git init made, by its
// path relative to the repository.
type templateFile struct {
	path    string
	mode    fs.FileMode
	dir     bool
	content []byte
}

// templates holds, by initial branch, the files of a bare repository git
// init made in this test process.
var templates struct {
	mu    sync.Mutex
	files map[string][]templateFile
}

// InitBare makes dir a bare repository whose HEAD names branch, git's
// default branch when branch is "", as git init --quiet --bare
// --initial-branch=<branch> makes it, without the sample hooks. It copies a
// repository git init made once per test process and branch rather than
// start git for every repository, since a test process pays for every
// process it forks. A dir that exists and is not empty is git's to
// reinitialise, so it runs git init then.
func InitBare(t testing.TB, dir, branch string) {
	t.Helper()
	args := []string{"init", "--quiet", "--bare"}
	if branch != "" {
		args = append(args, "--initial-branch="+branch)
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		Git(t, dir, append(args, dir)...)
		return
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("init %s: %v", dir, err)
	}
	for _, f := range template(t, args, branch) {
		path := filepath.Join(dir, filepath.FromSlash(f.path))
		var err error
		if f.dir {
			err = os.MkdirAll(path, f.mode)
		} else {
			err = os.WriteFile(path, f.content, f.mode)
		}
		if err != nil {
			t.Fatalf("init %s: %v", dir, err)
		}
	}
}

// template is the files, parents first, of a bare repository git init with
// args makes, made once per branch.
func template(t testing.TB, args []string, branch string) []templateFile {
	t.Helper()
	templates.mu.Lock()
	defer templates.mu.Unlock()
	if files, ok := templates.files[branch]; ok {
		return files
	}
	dir := filepath.Join(t.TempDir(), "template.git")
	Git(t, dir, append(args, dir)...)
	var files []templateFile
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		f := templateFile{path: filepath.ToSlash(rel), mode: info.Mode().Perm(), dir: d.IsDir()}
		switch {
		case f.dir:
		case filepath.Base(filepath.Dir(path)) == "hooks" && strings.HasSuffix(d.Name(), ".sample"):
			return nil
		default:
			if f.content, err = os.ReadFile(path); err != nil {
				return err
			}
		}
		files = append(files, f)
		return nil
	})
	if err != nil {
		t.Fatalf("read the repository git init made: %v", err)
	}
	if templates.files == nil {
		templates.files = map[string][]templateFile{}
	}
	templates.files[branch] = files
	return files
}
