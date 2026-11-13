package cli

import (
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// The tests in this file set environment variables, so they must not run in
// parallel.

func TestLoginConfig_CreatesAPrivateFileWithTheLogin(t *testing.T) {
	for _, home := range []bool{false, true} {
		t.Run(map[bool]string{false: "PARLEY_CONFIG_DIR", true: "home directory"}[home], func(t *testing.T) {
			loginEnv(t)
			tmp := t.TempDir()
			path := filepath.Join(tmp, "cfg", "config.toml")
			t.Setenv("PARLEY_CONFIG_DIR", filepath.Join(tmp, "cfg"))
			if home {
				path = filepath.Join(tmp, ".parley", "config.toml")
				t.Setenv("PARLEY_CONFIG_DIR", "")
				t.Setenv("HOME", tmp)
			}
			hub := userHub(t, testUser)

			code, _, stderr := runCLI(t, "login", "--api-key", testAPIKey, "--endpoint-url", hub.URL+"/")

			if code != 0 {
				t.Fatalf("exit status = %d, want 0; stderr %q", code, stderr)
			}
			if got := modeOf(t, filepath.Dir(path)); got != 0o700 {
				t.Errorf("directory mode = %v, want 0700", got)
			}
			if got := modeOf(t, path); got != 0o600 {
				t.Errorf("file mode = %v, want 0600", got)
			}
			want := map[string]any{"endpoint_url": hub.URL, "api_key": testAPIKey, "user_id": "u-1"}
			if got := readTOML(t, path); !maps.Equal(got, want) {
				t.Errorf("config = %v, want %v", got, want)
			}
		})
	}
}

func TestLoginConfig_ReplacesTheFileAndKeepsTheKeysItDoesNotKnow(t *testing.T) {
	dir := loginEnv(t)
	path := filepath.Join(dir, "config.toml")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(path, []byte("endpoint_url = \"http://old\"\napi_key = \"old\"\ntheme = \"dark\"\n\n[editor]\ntab = 4\n"), 0o644)
	os.Chmod(path, 0o644)
	hub := userHub(t, testUser)

	code, _, stderr := runCLI(t, "login", "--api-key", testAPIKey, "--endpoint-url", hub.URL)

	if code != 0 {
		t.Fatalf("exit status = %d, want 0; stderr %q", code, stderr)
	}
	if got := modeOf(t, path); got != 0o600 {
		t.Errorf("file mode = %v, want 0600", got)
	}
	want := map[string]any{
		"endpoint_url": hub.URL, "api_key": testAPIKey, "user_id": "u-1",
		"theme": "dark", "editor": map[string]any{"tab": int64(4)},
	}
	if got := readTOML(t, path); !reflect.DeepEqual(got, want) {
		t.Errorf("config = %v, want %v", got, want)
	}
	if got := namesIn(t, dir); !slices.Equal(got, []string{"config.toml"}) {
		t.Errorf("config directory holds %v, want only config.toml", got)
	}
}

func TestLoginConfig_LeavesAnUnparseableFileAloneAndSendsNothing(t *testing.T) {
	for _, flow := range [][]string{{"--api-key", testAPIKey}, nil} {
		t.Run(strings.Join(append([]string{"login"}, flow...), " "), func(t *testing.T) {
			dir := loginEnv(t)
			path := filepath.Join(dir, "config.toml")
			os.MkdirAll(dir, 0o700)
			os.WriteFile(path, []byte("not = [toml"), 0o600)
			hub := userHub(t, testUser)

			code, stdout, stderr := runCLI(t, append(append([]string{"login"}, flow...), "--endpoint-url", hub.URL)...)

			wantClientFailure(t, code, stdout, stderr, "client_error")
			if n := len(hub.recorded()); n != 0 {
				t.Errorf("the hub got %d requests, want none", n)
			}
			if got, _ := os.ReadFile(path); string(got) != "not = [toml" {
				t.Errorf("config file = %q, want it unchanged", got)
			}
		})
	}
}

func TestLoginConfig_SaysTheCredentialWasNotSavedWhenTheFileCannotBeWritten(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write into a read-only directory")
	}
	dir := loginEnv(t)
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	hub := userHub(t, testUser)

	code, stdout, stderr := runCLI(t, "login", "--api-key", testAPIKey, "--endpoint-url", hub.URL)

	if message := wantClientFailure(t, code, stdout, stderr, "client_error"); !strings.Contains(message, "not saved") {
		t.Errorf("message = %q, want it to say the credential was not saved", message)
	}
	if got := namesIn(t, dir); len(got) != 0 {
		t.Errorf("config directory holds %v, want nothing", got)
	}
	if reqs := hub.recorded(); len(reqs) != 1 || reqs[0].method != http.MethodGet {
		t.Errorf("the hub got %v, want the one GET that accepted the credential", reqs)
	}
}

func TestConfigPath_IsInTheConfigDirOrUnderTheHomeDirectory(t *testing.T) {
	unset := "<unset>"
	for _, tc := range []struct {
		name, configDir, home, want string
	}{
		{"PARLEY_CONFIG_DIR set", "/tmp/a", "/home/u", "/tmp/a/config.toml"},
		{"PARLEY_CONFIG_DIR unset", unset, "/home/u", "/home/u/.parley/config.toml"},
		{"PARLEY_CONFIG_DIR empty", "", "/home/u", "/home/u/.parley/config.toml"},
		{"no home directory", unset, unset, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for key, value := range map[string]string{"PARLEY_CONFIG_DIR": tc.configDir, "HOME": tc.home} {
				if value == unset {
					unsetEnv(t, key)
				} else {
					t.Setenv(key, value)
				}
			}

			got, err := configPath()

			if tc.want == "" {
				if err == nil {
					t.Errorf("configPath() = %q, want an error saying there is no home directory", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("configPath() = %q, %v, want %q", got, err, tc.want)
			}
		})
	}
}

func TestReadConfig_GivesTheKnownKeysOfTheFileOrNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string // "" leaves the file missing
		noHome  bool
		want    configSettings
	}{
		{
			name:    "the file under the home directory",
			content: "endpoint_url = \"http://hub.example\"\napi_key = \"k\"\nuser_id = \"u1\"\ncolor = 5\n",
			want:    configSettings{EndpointURL: "http://hub.example", APIKey: "k", UserID: "u1"},
		},
		{name: "a missing file"},
		{name: "no home directory", noHome: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			unsetEnv(t, "PARLEY_CONFIG_DIR")
			t.Setenv("HOME", home)
			if tc.noHome {
				unsetEnv(t, "HOME")
			}
			dir := filepath.Join(home, ".parley")
			if tc.content != "" {
				os.Mkdir(dir, 0o700)
				os.WriteFile(filepath.Join(dir, "config.toml"), []byte(tc.content), 0o600)
			}

			got, err := readConfig()

			if err != nil || got != tc.want {
				t.Errorf("readConfig() = %+v, %v, want %+v and no error", got, err, tc.want)
			}
			if tc.content == "" {
				wantAbsent(t, dir)
			}
		})
	}
}

func TestReadConfig_RefusesAFileItCannotParseAndNamesIt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{"not TOML", writeFile("api_key = \"secret\"\nnot = [toml")},
		{"endpoint_url not a string", writeFile("endpoint_url = 5")},
		{"api_key not a string", writeFile("api_key = [\"a\"]")},
		{"user_id not a string", writeFile("user_id = 1")},
		{"unreadable", func(t *testing.T, path string) {
			if os.Geteuid() == 0 {
				t.Skip("root reads a file of mode 0000")
			}
			writeFile("api_key = \"k\"")(t, path)
			os.Chmod(path, 0o000)
		}},
		{"a directory", func(t *testing.T, path string) { os.Mkdir(path, 0o700) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("PARLEY_CONFIG_DIR", dir)
			path := filepath.Join(dir, "config.toml")
			tc.setup(t, path)

			_, err := readConfig()

			message := wantClientError(t, err, path)
			if strings.Contains(message, "secret") {
				t.Errorf("message = %q, want it not to quote the file", message)
			}
		})
	}
}

func TestSaveConfig_CreatesAPrivateDirectoryAndFile(t *testing.T) {
	defer syscall.Umask(syscall.Umask(0o022))
	dir := filepath.Join(t.TempDir(), "new", "dir")
	t.Setenv("PARLEY_CONFIG_DIR", dir)
	saved := configSettings{EndpointURL: "http://hub.example", APIKey: "k", UserID: "u1"}

	if err := saveConfig(saved); err != nil {
		t.Fatalf("saveConfig() = %v, want no error", err)
	}

	path := filepath.Join(dir, "config.toml")
	if got := modeOf(t, dir); got != 0o700 {
		t.Errorf("directory mode = %v, want 0700", got)
	}
	if got := modeOf(t, path); got != 0o600 {
		t.Errorf("file mode = %v, want 0600", got)
	}
	want := map[string]any{"endpoint_url": "http://hub.example", "api_key": "k", "user_id": "u1"}
	if got := readTOML(t, path); !maps.Equal(got, want) {
		t.Errorf("config = %v, want %v", got, want)
	}
	if got, err := readConfig(); err != nil || got != saved {
		t.Errorf("readConfig() after the save = %+v, %v, want %+v", got, err, saved)
	}
}

func TestSaveConfig_ReplacesTheFileAndKeepsTheKeysItDoesNotKnow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PARLEY_CONFIG_DIR", dir)
	path := filepath.Join(dir, "config.toml")
	os.WriteFile(path, []byte("endpoint_url = \"http://old\"\napi_key = \"old\"\ncolor = \"blue\"\nretries = 3\n\n[profile]\nname = \"x\"\n"), 0o644)
	os.Chmod(path, 0o644)
	before := inodeOf(t, path)

	if err := saveConfig(configSettings{EndpointURL: "http://new", APIKey: "new", UserID: "u2"}); err != nil {
		t.Fatalf("saveConfig() = %v, want no error", err)
	}

	want := map[string]any{
		"endpoint_url": "http://new", "api_key": "new", "user_id": "u2",
		"color": "blue", "retries": int64(3), "profile": map[string]any{"name": "x"},
	}
	if got := readTOML(t, path); !reflect.DeepEqual(got, want) {
		t.Errorf("config = %v, want %v", got, want)
	}
	if got := modeOf(t, path); got != 0o600 {
		t.Errorf("file mode = %v, want 0600", got)
	}
	if inodeOf(t, path) == before {
		t.Error("the file kept its inode, want it replaced by a rename")
	}
	if got := namesIn(t, dir); !slices.Equal(got, []string{"config.toml"}) {
		t.Errorf("config directory holds %v, want only config.toml", got)
	}
}

func TestSaveConfig_FailsAndLeavesWhatWasThere(t *testing.T) {
	for _, tc := range []struct {
		name   string
		noHome bool
		setup  func(t *testing.T, dir, path string)
	}{
		{name: "directory not writable", setup: func(t *testing.T, dir, path string) {
			if os.Geteuid() == 0 {
				t.Skip("root writes into a read-only directory")
			}
			writeFile("endpoint_url = \"http://old\"\n")(t, path)
			os.Chmod(dir, 0o500)
			t.Cleanup(func() { os.Chmod(dir, 0o700) })
		}},
		{name: "config.toml is a directory", setup: func(t *testing.T, dir, path string) {
			os.Mkdir(path, 0o700)
			writeFile("x")(t, filepath.Join(path, "inside"))
		}},
		{name: "the file is not TOML", setup: func(t *testing.T, dir, path string) {
			writeFile("not = [toml")(t, path)
		}},
		{name: "no home directory", noHome: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			t.Setenv("PARLEY_CONFIG_DIR", dir)
			if tc.noHome {
				unsetEnv(t, "PARLEY_CONFIG_DIR")
				unsetEnv(t, "HOME")
				path = ""
			}
			if tc.setup != nil {
				tc.setup(t, dir, path)
			}
			before := snapshot(t, dir)

			err := saveConfig(configSettings{EndpointURL: "http://new", APIKey: "new", UserID: "u2"})

			wantClientError(t, err, path)
			if after := snapshot(t, dir); !maps.Equal(after, before) {
				t.Errorf("the config directory changed from %v to %v", before, after)
			}
		})
	}
}
