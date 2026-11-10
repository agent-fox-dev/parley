package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
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
