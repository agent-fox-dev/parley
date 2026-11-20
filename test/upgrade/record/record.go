// Command record writes the data directory of the current release to an
// archive the upgrade test opens with every later release.
//
// Run it from the repository root with the release's name:
//
//	go run ./test/upgrade/record <name>
//
// It bootstraps a hub in a temporary directory, serves it, creates the bare
// repository fixture.git with one commit on main under its workspace root,
// stops it, and writes test/upgrade/testdata/<name>.tar.gz: the data
// directory (both stores and the workspace root) and manifest.json, which
// says what the data holds. It never replaces an archive.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/store"
)

// namePattern is what a release name must match: it becomes a file name.
const namePattern = `^[a-z0-9][a-z0-9._-]{0,62}$`

var validName = regexp.MustCompile(namePattern)

// fixture is the repository the recording holds, under the workspace root.
const fixture = "fixture.git"

// manifest is manifest.json of an archive.
type manifest struct {
	Release    string `json:"release"`
	RecordedAt string `json:"recorded_at"`
	Steps      struct {
		Transactional []string `json:"transactional"`
		Analytical    []string `json:"analytical"`
	} `json:"steps"`
	// AdminTokenSHA256 is the admin token hash the transactional store holds.
	AdminTokenSHA256 string       `json:"admin_token_sha256"`
	Repositories     []repository `json:"repositories"`
}

// repository is a repository of the workspace root, by its path in the
// root, with the commit id of each of its refs.
type repository struct {
	Path string            `json:"path"`
	Refs map[string]string `json:"refs"`
}

// Run records the release named by args, its one argument, into
// outDir/<name>.tar.gz, writes the archive's path to stdout and returns 0. A
// bad name or an archive that exists is refused before anything is started
// or written, with one line on stderr and status 1.
func Run(ctx context.Context, args []string, outDir string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintf(stderr, "record: want one release name matching %s, got %d arguments\n", namePattern, len(args))
		return 1
	}
	name := args[0]
	if !validName.MatchString(name) {
		fmt.Fprintf(stderr, "record: release name %q does not match %s\n", name, namePattern)
		return 1
	}
	archive := filepath.Join(outDir, name+".tar.gz")
	switch _, err := os.Lstat(archive); {
	case err == nil:
		fmt.Fprintf(stderr, "record: archive %s already exists\n", archive)
		return 1
	case !errors.Is(err, fs.ErrNotExist):
		fmt.Fprintf(stderr, "record: %v\n", err)
		return 1
	}

	data, err := record(ctx, name)
	if err == nil {
		err = writeNew(archive, data)
	}
	if err != nil {
		fmt.Fprintf(stderr, "record: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, archive)
	return 0
}

// record runs a hub in a temporary directory, which it removes, and returns
// the archive of its data directory.
func record(ctx context.Context, name string) ([]byte, error) {
	tmp, err := os.MkdirTemp("", "parley-record-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	cfgDir, dataDir := filepath.Join(tmp, "config"), filepath.Join(tmp, "data")
	if err := os.Mkdir(cfgDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"),
		[]byte("[server]\nbind = \"127.0.0.1\"\nport = 0\n"), 0o600); err != nil {
		return nil, err
	}
	// The hub reads only these: database.path and workspace.path default to
	// parley.db and workspaces in the data directory.
	env := config.MapEnv(map[string]string{"XDG_CONFIG_HOME": cfgDir, "XDG_DATA_HOME": dataDir})

	var bootLog logWatch
	if code := hub.Run(ctx, hub.Options{
		Args: []string{"--admin-email", "admin@example.com"}, Env: env, WorkDir: cfgDir, Stderr: &bootLog,
	}); code != 0 {
		return nil, fmt.Errorf("bootstrap exited with %d: %s", code, bootLog.last())
	}
	// A bootstrapped hub serves only once its token file is gone.
	if err := os.Remove(filepath.Join(cfgDir, "admin_token")); err != nil {
		return nil, err
	}

	m := manifest{Release: name}
	if err := serve(ctx, hub.Options{Env: env, WorkDir: cfgDir}, &m, filepath.Join(dataDir, "workspaces", fixture)); err != nil {
		return nil, err
	}
	m.RecordedAt = time.Now().UTC().Format(time.RFC3339)
	doc, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return archiveDir(dataDir, append(doc, '\n'))
}

// serve runs the hub until it listens, fills m from its stores, creates the
// fixture repository at repo and stops the hub as an operator does.
func serve(ctx context.Context, opts hub.Options, m *manifest, repo string) error {
	opts.Compose = func(c *hub.Composition) {
		c.AddStartHook("record-manifest", func(ctx context.Context, rt *hub.Runtime) error {
			var err error
			if m.Steps.Transactional, err = rt.Store.AppliedSteps(ctx, store.Main); err != nil {
				return err
			}
			if m.Steps.Analytical, err = rt.Store.AppliedSteps(ctx, store.Analytics); err != nil {
				return err
			}
			admin, err := identity.AdminSettings(ctx, rt.Store)
			m.AdminTokenSHA256 = admin.TokenHash
			return err
		})
	}
	log := &logWatch{listening: make(chan struct{})}
	opts.Stderr = log
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	exited := make(chan int, 1)
	go func() { exited <- hub.Run(runCtx, opts) }()

	select {
	case code := <-exited:
		return fmt.Errorf("the hub exited with %d before listening: %s", code, log.last())
	case <-log.listening:
	}
	refs, err := createFixture(ctx, repo)
	stop()
	if code := <-exited; code != 0 && err == nil {
		err = fmt.Errorf("the hub exited with %d: %s", code, log.last())
	}
	if err != nil {
		return err
	}
	m.Repositories = []repository{{Path: fixture, Refs: refs}}
	return nil
}

// createFixture creates the bare repository repo with stock git, with one
// commit on main, and returns its refs.
func createFixture(ctx context.Context, repo string) (map[string]string, error) {
	// No template keeps the repository to what git needs: no sample hooks.
	if _, err := git(ctx, "", "init", "--quiet", "--bare", "--template=", "--object-format=sha1",
		"--initial-branch=main", repo); err != nil {
		return nil, err
	}
	blob, err := git(ctx, "fixture\n", "--git-dir="+repo, "hash-object", "-w", "--stdin")
	if err != nil {
		return nil, err
	}
	tree, err := git(ctx, "100644 blob "+blob+"\tREADME\n", "--git-dir="+repo, "mktree")
	if err != nil {
		return nil, err
	}
	commit, err := git(ctx, "", "--git-dir="+repo, "commit-tree", tree, "-m", "fixture")
	if err != nil {
		return nil, err
	}
	if _, err := git(ctx, "", "--git-dir="+repo, "update-ref", "refs/heads/main", commit); err != nil {
		return nil, err
	}
	out, err := git(ctx, "", "--git-dir="+repo, "for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for line := range strings.Lines(out) {
		if ref, id, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			refs[ref] = id
		}
	}
	return refs, nil
}

// fixedDate makes the fixture's commit the same in every recording.
const fixedDate = "2026-01-01T00:00:00Z"

// git runs stock git with stdin and returns its trimmed output. It runs with
// no user or system configuration and no GIT_ variable of the caller, so
// that the recording does not depend on who makes it.
func git(ctx context.Context, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=Parley Recorder", "GIT_AUTHOR_EMAIL=recorder@example.com", "GIT_AUTHOR_DATE="+fixedDate,
		"GIT_COMMITTER_NAME=Parley Recorder", "GIT_COMMITTER_EMAIL=recorder@example.com", "GIT_COMMITTER_DATE="+fixedDate)
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// archiveDir returns a gzipped tar of manifest.json, holding doc, and of
// everything under dir, empty directories included, since a bare repository
// needs some that git does not track. Owners are left out.
func archiveDir(dir string, doc []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "manifest.json", Mode: 0o644,
		Size: int64(len(doc)), ModTime: time.Now()})
	if err == nil {
		_, err = tw.Write(doc)
	}
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == dir {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			return fmt.Errorf("%s is neither a file nor a directory", path)
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(hdr); err != nil || !info.Mode().IsRegular() {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("archive %s: %w", dir, err)
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeNew writes data to a new file at path, never over one that exists,
// and removes what it wrote when the write fails.
func writeNew(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}

// logWatch keeps a hub's log lines. When listening is not nil it is closed
// at the first line whose msg is "listening".
type logWatch struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	listening chan struct{}
	seen      bool
}

func (w *logWatch) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	if w.listening != nil && !w.seen {
		for line := range strings.Lines(w.buf.String()) {
			var rec struct{ Msg string }
			if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "listening" {
				w.seen = true
				close(w.listening)
				break
			}
		}
	}
	return len(p), nil
}

// last returns the last line logged, which explains a failed run.
func (w *logWatch) last() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	lines := strings.Split(strings.TrimSpace(w.buf.String()), "\n")
	return lines[len(lines)-1]
}
