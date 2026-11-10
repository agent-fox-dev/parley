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

	"github.com/spf13/cobra"
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

func TestSettings_FlagThenEnvironmentThenFile(t *testing.T) {
	a, b, c := newRecordingHub(t, 200, `{}`), newRecordingHub(t, 200, `{}`), newRecordingHub(t, 200, `{}`)
	const absent, unset = "\x00absent", "\x00unset"
	tests := []struct {
		flagURL, envURL, fileURL, flagKey, envKey, fileKey string
		hub                                                *recordingHub
		key                                                string
	}{
		{a.URL, b.URL, c.URL, "k1", "k2", "k3", a, "k1"},
		{absent, b.URL, c.URL, absent, "k2", "k3", b, "k2"},
		{absent, unset, c.URL, absent, unset, "k3", c, "k3"},
		{"", b.URL, c.URL, "", "k2", "k3", b, "k2"},
		{absent, "", c.URL, absent, "", "k3", c, "k3"},
		{a.URL, unset, c.URL, absent, unset, "k3", a, "k3"},
	}
	for i, tt := range tests {
		dir := cliEnv(t)
		os.MkdirAll(dir, 0o700)
		os.WriteFile(filepath.Join(dir, "config.toml"), []byte(fmt.Sprintf("endpoint_url = %q\napi_key = %q\n", tt.fileURL, tt.fileKey)), 0o600)
		args := []string{"probe", "get", "x"}
		for _, f := range []struct{ name, val string }{{"--endpoint-url", tt.flagURL}, {"--api-key", tt.flagKey}} {
			if f.val != absent {
				args = append(args, f.name, f.val)
			}
		}
		for _, e := range []struct{ name, val string }{{"PARLEY_ENDPOINT_URL", tt.envURL}, {"PARLEY_API_KEY", tt.envKey}} {
			if e.val == unset {
				os.Unsetenv(e.name)
			} else {
				t.Setenv(e.name, e.val)
			}
		}
		before := map[*recordingHub]int{a: len(a.all()), b: len(b.all()), c: len(c.all())}
		if r := runProbe(t, []*cobra.Command{probeGet()}, args...); r.code != 0 {
			t.Fatalf("case %d: exit %d %s", i, r.code, r.stderr)
		}
		for _, h := range []*recordingHub{a, b, c} {
			got := h.all()[before[h]:]
			if h == tt.hub && (len(got) != 1 || got[0].header.Get("Authorization") != "Bearer "+tt.key) {
				t.Errorf("case %d: wanted hub got %+v", i, got)
			} else if h != tt.hub && len(got) != 0 {
				t.Errorf("case %d: another hub got %d requests", i, len(got))
			}
		}
	}
}

func TestSettings_AMissingSettingFailsBeforeAnyRequest(t *testing.T) {
	hub := newRecordingHub(t, 200, `{}`)
	for _, withFile := range []bool{false, true} {
		for _, missing := range []string{"url", "key", "both"} {
			dir := cliEnv(t)
			if withFile {
				os.MkdirAll(dir, 0o700)
				os.WriteFile(filepath.Join(dir, "config.toml"), []byte("user_id = \"u1\"\n"), 0o600)
			}
			args := []string{"probe", "get", "x"}
			if missing != "url" && missing != "both" {
				args = append(args, "--endpoint-url", hub.URL)
			}
			if missing != "key" && missing != "both" {
				args = append(args, "--api-key", "k")
			}
			r := runProbe(t, []*cobra.Command{probeGet()}, args...)
			if r.code != 2 || r.stdout != "" {
				t.Fatalf("%s: exit %d stdout %q", missing, r.code, r.stdout)
			}
			e := r.envelope(t)
			msg, _ := e["message"].(string)
			path := filepath.Join(dir, "config.toml")
			if e["code"] != float64(0) || e["error_type"] != "client_error" || !strings.Contains(msg, path) {
				t.Errorf("%s: %v", missing, e)
			}
			urlNamed := strings.Contains(msg, "--endpoint-url") && strings.Contains(msg, "PARLEY_ENDPOINT_URL")
			keyNamed := strings.Contains(msg, "--api-key") && strings.Contains(msg, "PARLEY_API_KEY")
			if urlNamed != (missing != "key") || keyNamed != (missing != "url") {
				t.Errorf("%s: message %q", missing, msg)
			}
		}
	}
	if n := len(hub.all()); n != 0 {
		t.Errorf("%d requests", n)
	}
}

func TestSettings_AHubURLMustBeAnAbsoluteHTTPURL(t *testing.T) {
	for _, u := range []string{"localhost:8080", "ftp://host", "http://", "/api", "host"} {
		cliEnv(t)
		r := runProbe(t, []*cobra.Command{probeGet()}, "probe", "get", "x", "--endpoint-url", u, "--api-key", "k")
		e := r.envelope(t)
		if r.code != 2 || r.stdout != "" || e["error_type"] != "client_error" || !strings.Contains(e["message"].(string), u) {
			t.Errorf("%q: exit %d %v", u, r.code, e)
		}
	}
}

func TestSettings_FlagsAndEnvironmentAloneNeverReadTheFile(t *testing.T) {
	hub := newRecordingHub(t, 200, `{}`)
	for _, how := range []string{"flags", "variables", "mixed"} {
		dir := cliEnv(t)
		os.MkdirAll(dir, 0o700)
		os.WriteFile(filepath.Join(dir, "config.toml"), []byte("not = [toml"), 0o600)
		args := []string{"probe", "get", "x"}
		switch how {
		case "flags":
			args = append(args, "--endpoint-url", hub.URL, "--api-key", "k")
		case "variables":
			t.Setenv("PARLEY_ENDPOINT_URL", hub.URL)
			t.Setenv("PARLEY_API_KEY", "k")
		case "mixed":
			args = append(args, "--endpoint-url", hub.URL)
			t.Setenv("PARLEY_API_KEY", "k")
		}
		before := len(hub.all())
		if r := runProbe(t, []*cobra.Command{probeGet()}, args...); r.code != 0 || r.stderr != "" || len(hub.all()) != before+1 {
			t.Errorf("%s: exit %d stderr %q", how, r.code, r.stderr)
		}
	}
}

func TestSettings_WithoutAFileOrHomeNothingIsCreated(t *testing.T) {
	hub := newRecordingHub(t, 200, `{}`)
	tmp := t.TempDir()
	for _, c := range []struct {
		dir, home string
		unsetDir  bool
		unsetHome bool
	}{
		{filepath.Join(tmp, "missing"), "", false, false},
		{"", filepath.Join(tmp, "nohome"), true, false},
		{"", "", true, true},
	} {
		cliEnv(t)
		t.Setenv("PARLEY_CONFIG_DIR", c.dir)
		t.Setenv("HOME", c.home)
		if c.unsetDir {
			os.Unsetenv("PARLEY_CONFIG_DIR")
		}
		if c.unsetHome {
			os.Unsetenv("HOME")
		}
		t.Setenv("PARLEY_ENDPOINT_URL", hub.URL)
		t.Setenv("PARLEY_API_KEY", "k")
		if r := runProbe(t, []*cobra.Command{probeGet()}, "probe", "get", "x"); r.code != 0 {
			t.Errorf("%+v: exit %d %s", c, r.code, r.stderr)
		}
		os.Unsetenv("PARLEY_API_KEY")
		r := runProbe(t, []*cobra.Command{probeGet()}, "probe", "get", "x")
		if r.code != 2 || r.envelope(t)["error_type"] != "client_error" {
			t.Errorf("%+v: missing key: exit %d %s", c, r.code, r.stderr)
		}
	}
	for _, p := range []string{filepath.Join(tmp, "missing"), filepath.Join(tmp, "nohome", ".parley")} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s was created", p)
		}
	}
}

func TestSettings_AnUnparseableFileIsAClientErrorWhenNeeded(t *testing.T) {
	hub := newRecordingHub(t, 200, `{}`)
	states := []func(dir string){
		func(dir string) { os.WriteFile(filepath.Join(dir, "config.toml"), []byte("not = [toml"), 0o600) },
		func(dir string) { os.WriteFile(filepath.Join(dir, "config.toml"), []byte("endpoint_url = 5"), 0o600) },
		func(dir string) { os.WriteFile(filepath.Join(dir, "config.toml"), []byte(`api_key = ["a"]`), 0o600) },
		func(dir string) { os.MkdirAll(filepath.Join(dir, "config.toml"), 0o700) },
	}
	if os.Geteuid() != 0 {
		states = append(states, func(dir string) {
			os.WriteFile(filepath.Join(dir, "config.toml"), []byte(`api_key = "k"`), 0o000)
		})
	}
	for i, arrange := range states {
		dir := cliEnv(t)
		os.MkdirAll(dir, 0o700)
		arrange(dir)
		t.Setenv("PARLEY_ENDPOINT_URL", hub.URL)
		r := runProbe(t, []*cobra.Command{probeGet()}, "probe", "get", "x")
		e := r.envelope(t)
		if r.code != 2 || r.stdout != "" || e["code"] != float64(0) || e["error_type"] != "client_error" || !strings.Contains(e["message"].(string), filepath.Join(dir, "config.toml")) {
			t.Errorf("state %d: exit %d %v", i, r.code, e)
		}
	}
	if n := len(hub.all()); n != 0 {
		t.Errorf("%d requests", n)
	}
}

func TestSettings_AFileUnderHomeAndASavedFileAreUsed(t *testing.T) {
	hub := newRecordingHub(t, 200, `{}`)
	home := t.TempDir()
	cliEnv(t)
	os.Unsetenv("PARLEY_CONFIG_DIR")
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".parley"), 0o700)
	os.WriteFile(filepath.Join(home, ".parley", "config.toml"), []byte(fmt.Sprintf("endpoint_url = %q\napi_key = \"home-key\"\n", hub.URL)), 0o600)
	if r := runProbe(t, []*cobra.Command{probeGet()}, "probe", "get", "x"); r.code != 0 || hub.all()[0].header.Get("Authorization") != "Bearer home-key" {
		t.Fatalf("file under HOME: exit %d %s", r.code, r.stderr)
	}
	t.Setenv("PARLEY_CONFIG_DIR", filepath.Join(t.TempDir(), "new", "dir"))
	if err := saveSettings(Settings{EndpointURL: hub.URL, APIKey: "saved", UserID: "u1"}); err != nil {
		t.Fatal(err)
	}
	if r := runProbe(t, []*cobra.Command{probeGet()}, "probe", "get", "x"); r.code != 0 || hub.all()[1].header.Get("Authorization") != "Bearer saved" {
		t.Fatalf("saved file: exit %d %s", r.code, r.stderr)
	}
}
