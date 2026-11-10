package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestConfig_ALoginCreatesThePrivateFile(t *testing.T) {
	for _, viaHome := range []bool{false, true} {
		tmp := t.TempDir()
		cliEnv(t)
		dir := filepath.Join(tmp, "cfg")
		if viaHome {
			t.Setenv("PARLEY_CONFIG_DIR", "")
			t.Setenv("HOME", tmp)
			dir = filepath.Join(tmp, ".parley")
		} else {
			t.Setenv("PARLEY_CONFIG_DIR", dir)
		}
		hub := newStubHub(t)
		if r := runCLI(t, "", "login", "--api-key", apiKey, "--endpoint-url", hub.URL+"/"); r.code != 0 {
			t.Fatalf("exit %d: %s", r.code, r.stderr)
		}
		path := filepath.Join(dir, "config.toml")
		if mode(t, dir) != 0o700 || mode(t, path) != 0o600 {
			t.Errorf("modes %o %o", mode(t, dir), mode(t, path))
		}
		got := readTOML(t, path)
		if got["endpoint_url"] != hub.URL || got["api_key"] != apiKey || got["user_id"] != "u-1" || len(got) != 3 {
			t.Errorf("file = %v", got)
		}
	}
}

func TestConfig_ALoginReplacesTheFileKeepingUnknownKeys(t *testing.T) {
	dir := cliEnv(t)
	os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, "config.toml")
	os.WriteFile(path, []byte("endpoint_url = \"http://old\"\napi_key = \"old\"\ntheme = \"dark\"\n\n[editor]\ntab = 4\n"), 0o644)
	hub := newStubHub(t)
	newKey := "parley_zzzz9999_" + strings.Repeat("Q", 32)
	if r := runCLI(t, "", "login", "--api-key", newKey, "--endpoint-url", hub.URL); r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if mode(t, path) != 0o600 {
		t.Errorf("mode %o", mode(t, path))
	}
	got := readTOML(t, path)
	editor, _ := got["editor"].(map[string]any)
	if got["endpoint_url"] != hub.URL || got["api_key"] != newKey || got["user_id"] != "u-1" || got["theme"] != "dark" || editor["tab"] != int64(4) {
		t.Errorf("file = %v", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("dir holds %d entries", len(entries))
	}
}

func TestConfig_AnUnparseableFileFailsBeforeAnyRequest(t *testing.T) {
	for _, args := range [][]string{{"--api-key", apiKey}, nil} {
		dir := cliEnv(t)
		os.MkdirAll(dir, 0o700)
		path := filepath.Join(dir, "config.toml")
		os.WriteFile(path, []byte("not = [toml"), 0o600)
		hub := newStubHub(t)
		r := runCLI(t, "", append([]string{"login", "--endpoint-url", hub.URL}, args...)...)
		if r.code != 2 {
			t.Fatalf("exit %d: %s", r.code, r.stderr)
		}
		if e := r.envelope(t); e["code"] != float64(0) || e["error_type"] != "client_error" {
			t.Errorf("envelope %v", e)
		}
		if r.stdout != "" || len(hub.requests()) != 0 {
			t.Errorf("stdout %q, %d requests", r.stdout, len(hub.requests()))
		}
		if b, _ := os.ReadFile(path); !bytes.Equal(b, []byte("not = [toml")) {
			t.Error("the file changed")
		}
	}
}

func TestConfig_AFileThatCannotBeWrittenSaysTheKeyWasNotSaved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	dir := cliEnv(t)
	os.MkdirAll(dir, 0o700)
	os.Chmod(dir, 0o500)
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	hub := newStubHub(t)
	r := runCLI(t, "", "login", "--api-key", apiKey, "--endpoint-url", hub.URL)
	if r.code != 2 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	e := r.envelope(t)
	if e["code"] != float64(0) || e["error_type"] != "client_error" || !strings.Contains(e["message"].(string), "not saved") {
		t.Errorf("envelope %v", e)
	}
	if r.stdout != "" {
		t.Errorf("stdout %q", r.stdout)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("dir holds %d entries", len(entries))
	}
}

func TestConfigPath_IsTheConfigDirOrDotParleyUnderHome(t *testing.T) {
	tests := []struct {
		dir, home string
		unsetDir  bool
		want      string
	}{
		{"/tmp/a", "/home/u", false, "/tmp/a/config.toml"},
		{"", "/home/u", true, "/home/u/.parley/config.toml"},
		{"", "/home/u", false, "/home/u/.parley/config.toml"},
	}
	for _, tt := range tests {
		t.Setenv("HOME", tt.home)
		t.Setenv("PARLEY_CONFIG_DIR", tt.dir)
		if tt.unsetDir {
			os.Unsetenv("PARLEY_CONFIG_DIR")
		}
		if got, ok := configPath(); !ok || got != tt.want {
			t.Errorf("%+v: configPath() = %q, %v", tt, got, ok)
		}
	}
}

func TestSaveSettings_CreatesThePrivateDirectoryAndFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new", "dir")
	t.Setenv("PARLEY_CONFIG_DIR", dir)
	if err := saveSettings(Settings{EndpointURL: "http://hub", APIKey: "k", UserID: "u1"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	if mode(t, dir) != 0o700 || mode(t, path) != 0o600 {
		t.Errorf("modes %o %o", mode(t, dir), mode(t, path))
	}
	got := readTOML(t, path)
	if len(got) != 3 || got["endpoint_url"] != "http://hub" || got["api_key"] != "k" || got["user_id"] != "u1" {
		t.Errorf("file = %v", got)
	}
}

func inode(t *testing.T, p string) uint64 {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Ino
}

func TestSaveSettings_ReplacesTheFileKeepingUnknownKeys(t *testing.T) {
	dir := cliEnv(t)
	os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, "config.toml")
	os.WriteFile(path, []byte("endpoint_url = \"http://old\"\napi_key = \"old\"\ncolor = \"blue\"\nretries = 3\n\n[profile]\nname = \"x\"\n"), 0o644)
	before := inode(t, path)
	if err := saveSettings(Settings{EndpointURL: "http://new", APIKey: "new", UserID: "u2"}); err != nil {
		t.Fatal(err)
	}
	got := readTOML(t, path)
	profile, _ := got["profile"].(map[string]any)
	if got["endpoint_url"] != "http://new" || got["api_key"] != "new" || got["user_id"] != "u2" || got["color"] != "blue" || got["retries"] != int64(3) || profile["name"] != "x" {
		t.Errorf("file = %v", got)
	}
	if mode(t, path) != 0o600 || inode(t, path) == before {
		t.Errorf("mode %o, same inode %v", mode(t, path), inode(t, path) == before)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("dir holds %d entries", len(entries))
	}
}

// snapshot is a directory's entries and a file's bytes and mode.
func snapshot(t *testing.T, dir, path string) string {
	t.Helper()
	var b strings.Builder
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		b.WriteString(e.Name() + ";")
	}
	if data, err := os.ReadFile(path); err == nil {
		fi, _ := os.Stat(path)
		b.WriteString(fmt.Sprintf("%o:%s", fi.Mode().Perm(), data))
	}
	return b.String()
}

func TestSaveSettings_AFailedSaveChangesNothing(t *testing.T) {
	type arrange func(t *testing.T, dir string) (path string)
	cases := []struct {
		name    string
		arrange arrange
	}{
		{"unparseable file", func(t *testing.T, dir string) string {
			os.MkdirAll(dir, 0o700)
			os.WriteFile(filepath.Join(dir, "config.toml"), []byte("not = [toml"), 0o600)
			return filepath.Join(dir, "config.toml")
		}},
		{"config.toml is a non-empty directory", func(t *testing.T, dir string) string {
			os.MkdirAll(filepath.Join(dir, "config.toml", "inside"), 0o700)
			return filepath.Join(dir, "config.toml")
		}},
		{"no home directory", func(t *testing.T, dir string) string {
			os.Unsetenv("PARLEY_CONFIG_DIR")
			os.Unsetenv("HOME")
			return ""
		}},
	}
	if os.Geteuid() != 0 {
		cases = append(cases, struct {
			name    string
			arrange arrange
		}{"read-only directory", func(t *testing.T, dir string) string {
			os.MkdirAll(dir, 0o700)
			os.WriteFile(filepath.Join(dir, "config.toml"), []byte("api_key = \"keep\"\n"), 0o600)
			os.Chmod(dir, 0o500)
			t.Cleanup(func() { os.Chmod(dir, 0o700) })
			return filepath.Join(dir, "config.toml")
		}})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := cliEnv(t)
			t.Setenv("HOME", t.TempDir())
			path := c.arrange(t, dir)
			before := snapshot(t, dir, path)
			err := saveSettings(Settings{EndpointURL: "http://new", APIKey: "new", UserID: "u"})
			var ce *Error
			if !errors.As(err, &ce) || ce.Type != TypeClient {
				t.Fatalf("err = %v, want a client error", err)
			}
			if path != "" && !strings.Contains(ce.Message, path) {
				t.Errorf("message %q does not name %s", ce.Message, path)
			}
			if after := snapshot(t, dir, path); after != before {
				t.Errorf("changed: %q -> %q", before, after)
			}
		})
	}
}
