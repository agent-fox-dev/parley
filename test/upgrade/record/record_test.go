package main

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/store"
)

// unpack extracts the archive at path into a new directory with stock tar,
// which also shows the archive is readable without the recorder.
func unpack(t *testing.T, path string) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("tar", "-xzf", path, "-C", dir).CombinedOutput(); err != nil {
		t.Fatalf("tar -xzf %s: %v\n%s", path, err, out)
	}
	return dir
}

// listDir returns the names in dir and the bytes of each file.
func listDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := make(map[string]string, len(entries))
	for _, e := range entries {
		data, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		files[e.Name()] = string(data)
	}
	return files
}

var hex40 = regexp.MustCompile(`^[0-9a-f]{40}$`)

// The test sets TMPDIR, where the recorder makes its temporary directory, to
// see that it is removed; so it does not run in parallel.
func TestRun_RecordsABootstrappedHubWithOneRepository(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	out := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"r1"}, out, &stdout, &stderr); code != 0 {
		t.Fatalf("Run returned %d, want 0; stderr = %q", code, stderr.String())
	}
	archive := filepath.Join(out, "r1.tar.gz")
	if got := stdout.String(); got != archive+"\n" {
		t.Errorf("stdout = %q, want %q", got, archive+"\n")
	}
	if left := listDir(t, tmp); len(left) != 0 {
		t.Errorf("the temporary directory is left behind: %v", slices.Collect(maps.Keys(left)))
	}

	d := unpack(t, archive)
	data, err := os.ReadFile(filepath.Join(d, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("manifest.json: %v\n%s", err, data)
	}
	if m.Release != "r1" {
		t.Errorf("release = %q, want r1", m.Release)
	}
	if _, err := time.Parse(time.RFC3339, m.RecordedAt); err != nil {
		t.Errorf("recorded_at = %q: %v", m.RecordedAt, err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(m.AdminTokenSHA256) {
		t.Errorf("admin_token_sha256 = %q, want 64 lowercase hex characters", m.AdminTokenSHA256)
	}

	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{
		DatabasePath:  filepath.Join(d, "parley.db"),
		WorkspaceRoot: filepath.Join(d, "workspaces"),
	}, nil)
	if err != nil {
		t.Fatalf("open the recorded stores: %v", err)
	}
	defer st.Close()
	for _, s := range []struct {
		name     string
		target   store.Target
		recorded []string
	}{
		{"transactional", store.Main, m.Steps.Transactional},
		{"analytical", store.Analytics, m.Steps.Analytical},
	} {
		applied, err := st.AppliedSteps(ctx, s.target)
		if err != nil {
			t.Fatal(err)
		}
		if len(applied) == 0 || !slices.Equal(s.recorded, applied) {
			t.Errorf("steps.%s = %v, want the store's applied steps %v", s.name, s.recorded, applied)
		}
	}
	var hash string
	if err := st.Main().QueryRowContext(ctx, `SELECT token_hash FROM admin_settings`).Scan(&hash); err != nil || hash != m.AdminTokenSHA256 {
		t.Errorf("stored admin token hash = %q, %v; want the manifest's %q", hash, err, m.AdminTokenSHA256)
	}

	if len(m.Repositories) != 1 || m.Repositories[0].Path != "fixture.git" {
		t.Fatalf("repositories = %+v, want fixture.git alone", m.Repositories)
	}
	refs := m.Repositories[0].Refs
	if len(refs) != 1 || !hex40.MatchString(refs["refs/heads/main"]) {
		t.Fatalf("refs of fixture.git = %v, want refs/heads/main with a commit id", refs)
	}
	repo := filepath.Join(d, "workspaces", "fixture.git")
	got, err := exec.Command("git", "-C", repo, "rev-parse", "refs/heads/main").Output()
	if err != nil || strings.TrimSpace(string(got)) != refs["refs/heads/main"] {
		t.Errorf("git rev-parse refs/heads/main = %q, %v; want %s", got, err, refs["refs/heads/main"])
	}
}

func TestRun_RefusesABadNameOrAnExistingArchiveWithoutWritingAnything(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		// existing is written to out/r1.tar.gz before the run.
		existing bool
		want     string
	}{
		{"no name", nil, false, ""},
		{"two names", []string{"a", "b"}, false, ""},
		{"an upper-case letter", []string{"Upper"}, false, "Upper"},
		{"a leading dash", []string{"-x"}, false, "-x"},
		{"a slash", []string{"a/b"}, false, "a/b"},
		{"65 characters", []string{strings.Repeat("a", 65)}, false, strings.Repeat("a", 65)},
		{"an existing archive", []string{"r1"}, true, "r1.tar.gz"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out := t.TempDir()
			if tt.existing {
				if err := os.WriteFile(filepath.Join(out, "r1.tar.gz"), []byte("old"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			before := listDir(t, out)
			var stdout, stderr bytes.Buffer
			if code := Run(context.Background(), tt.args, out, &stdout, &stderr); code != 1 {
				t.Errorf("Run(%q) returned %d, want 1", tt.args, code)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want nothing", stdout.String())
			}
			lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
			if len(lines) != 1 || lines[0] == "" {
				t.Errorf("stderr = %q, want one line", stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Errorf("stderr = %q, want it to name %q", stderr.String(), tt.want)
			}
			if tt.want != "" && !tt.existing && !strings.Contains(stderr.String(), namePattern) {
				t.Errorf("stderr = %q, want it to give the pattern %s", stderr.String(), namePattern)
			}
			if after := listDir(t, out); !maps.Equal(before, after) {
				t.Errorf("the output directory changed: %v, was %v", after, before)
			}
		})
	}
}
