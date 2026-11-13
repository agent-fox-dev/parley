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

	t.Run("a command finds the hub through the file under the home directory", func(t *testing.T) {
		loginEnv(t)
		unsetEnv(t, "PARLEY_CONFIG_DIR")
		home := t.TempDir()
		t.Setenv("HOME", home)
		hub := thingHub(t, `{}`)
		writeConfigFile(t, filepath.Join(home, ".parley"), "endpoint_url = \""+hub.URL+"\"\napi_key = \"k\"\n")

		code, _, stderr := runProbes(t, "probe", "get", "x")

		if code != 0 {
			t.Errorf("exit status = %d, want 0; stderr %q", code, stderr)
		}
		if got := hub.recorded(); len(got) != 1 || got[0].header.Get("Authorization") != "Bearer k" {
			t.Errorf("the hub got %v, want one request with Authorization Bearer k", got)
		}
	})
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
	loginEnv(t)
	dir := filepath.Join(t.TempDir(), "new", "dir")
	t.Setenv("PARLEY_CONFIG_DIR", dir)
	hub := thingHub(t, `{}`)
	saved := configSettings{EndpointURL: hub.URL, APIKey: "k", UserID: "u1"}

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
	want := map[string]any{"endpoint_url": hub.URL, "api_key": "k", "user_id": "u1"}
	if got := readTOML(t, path); !maps.Equal(got, want) {
		t.Errorf("config = %v, want %v", got, want)
	}
	if got, err := readConfig(); err != nil || got != saved {
		t.Errorf("readConfig() after the save = %+v, %v, want %+v", got, err, saved)
	}

	code, _, stderr := runProbes(t, "probe", "get", "x")

	if code != 0 {
		t.Errorf("a command after the save: exit status = %d, want 0; stderr %q", code, stderr)
	}
	if got := hub.recorded(); len(got) != 1 || got[0].header.Get("Authorization") != "Bearer k" {
		t.Errorf("the hub got %v, want one request with Authorization Bearer k", got)
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

func TestSettings_TakesEachFromTheFirstNonEmptySource(t *testing.T) {
	// A nil flag is not given and a nil variable is unset; "A", "B" and "C"
	// stand for the URLs of three hubs.
	tests := []struct {
		name                             string
		flagURL, envURL, flagKey, envKey *string
		wantHub, wantKey                 string
	}{
		{"flags first", new("A"), new("B"), new("k1"), new("k2"), "A", "k1"},
		{"variables next", nil, new("B"), nil, new("k2"), "B", "k2"},
		{"the file last", nil, nil, nil, nil, "C", "k3"},
		{"empty flags are passed over", new(""), new("B"), new(""), new("k2"), "B", "k2"},
		{"empty variables are passed over", nil, new(""), nil, new(""), "C", "k3"},
		{"each setting on its own", new("A"), nil, nil, nil, "A", "k3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := loginEnv(t)
			hubs := map[string]*stubHub{"A": thingHub(t, `{}`), "B": thingHub(t, `{}`), "C": thingHub(t, `{}`)}
			writeConfigFile(t, dir, "endpoint_url = \""+hubs["C"].URL+"\"\napi_key = \"k3\"\n")
			hubURL := func(s string) string {
				if hub, ok := hubs[s]; ok {
					return hub.URL
				}
				return s
			}
			args := []string{"probe", "get", "x"}
			if tt.flagURL != nil {
				args = append(args, "--endpoint-url", hubURL(*tt.flagURL))
			}
			if tt.flagKey != nil {
				args = append(args, "--api-key", *tt.flagKey)
			}
			if tt.envURL != nil {
				t.Setenv("PARLEY_ENDPOINT_URL", hubURL(*tt.envURL))
			}
			if tt.envKey != nil {
				t.Setenv("PARLEY_API_KEY", *tt.envKey)
			}

			code, _, stderr := runProbes(t, args...)

			if code != 0 {
				t.Fatalf("exit status = %d, want 0; stderr %q", code, stderr)
			}
			for name, hub := range hubs {
				got := hub.recorded()
				if name != tt.wantHub {
					if len(got) != 0 {
						t.Errorf("hub %s got %d requests, want none", name, len(got))
					}
					continue
				}
				if len(got) != 1 {
					t.Fatalf("hub %s got %d requests, want 1", name, len(got))
				}
				if auth := got[0].header.Get("Authorization"); auth != "Bearer "+tt.wantKey {
					t.Errorf("Authorization = %q, want %q", auth, "Bearer "+tt.wantKey)
				}
			}
		})
	}
}

func TestSettings_RefusesToRunWithoutAHubURLOrACredential(t *testing.T) {
	for _, missing := range []string{"hub URL", "credential", "both"} {
		for _, file := range []string{"", "user_id = \"u1\"\n"} {
			t.Run(missing+map[string]string{"": ", no file"}[file], func(t *testing.T) {
				dir := loginEnv(t)
				path := filepath.Join(dir, "config.toml")
				if file != "" {
					writeConfigFile(t, dir, file)
				}
				hub := thingHub(t, `{}`)
				args := []string{"probe", "get", "x"}
				wantNamed := []string{path, "--endpoint-url", "PARLEY_ENDPOINT_URL", "--api-key", "PARLEY_API_KEY"}
				var wantNotNamed string
				switch missing {
				case "hub URL":
					args = append(args, "--api-key", "k")
					wantNamed, wantNotNamed = []string{path, "--endpoint-url", "PARLEY_ENDPOINT_URL"}, "--api-key"
				case "credential":
					args = append(args, "--endpoint-url", hub.URL)
					wantNamed, wantNotNamed = []string{path, "--api-key", "PARLEY_API_KEY"}, "--endpoint-url"
				}

				code, stdout, stderr := runProbes(t, args...)

				message := wantClientFailure(t, code, stdout, stderr, "client_error")
				for _, name := range wantNamed {
					if !strings.Contains(message, name) {
						t.Errorf("message %q does not name %s", message, name)
					}
				}
				if wantNotNamed != "" && strings.Contains(message, wantNotNamed) {
					t.Errorf("message %q names %s, which was given", message, wantNotNamed)
				}
				if n := len(hub.recorded()); n != 0 {
					t.Errorf("the hub got %d requests, want none", n)
				}
			})
		}
	}
}

func TestSettings_RefusesAHubURLThatIsNotAnAbsoluteHTTPURL(t *testing.T) {
	for _, hubURL := range []string{"localhost:8080", "ftp://host", "http://", "/api", "host"} {
		t.Run(hubURL, func(t *testing.T) {
			loginEnv(t)

			code, stdout, stderr := runProbes(t, "probe", "get", "x", "--endpoint-url", hubURL, "--api-key", "k")

			if message := wantClientFailure(t, code, stdout, stderr, "client_error"); !strings.Contains(message, hubURL) {
				t.Errorf("message %q does not name %q", message, hubURL)
			}
		})
	}
}

func TestSettings_LeaveTheFileUnreadWhenFlagsAndVariablesGiveBoth(t *testing.T) {
	for _, tc := range []struct {
		name           string
		envURL, envKey bool
	}{
		{name: "both by flag"},
		{name: "both by variable", envURL: true, envKey: true},
		{name: "URL by flag, key by variable", envKey: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := loginEnv(t)
			writeConfigFile(t, dir, "not = [toml")
			hub := thingHub(t, `{}`)
			args := []string{"probe", "get", "x"}
			if tc.envURL {
				t.Setenv("PARLEY_ENDPOINT_URL", hub.URL)
			} else {
				args = append(args, "--endpoint-url", hub.URL)
			}
			if tc.envKey {
				t.Setenv("PARLEY_API_KEY", "k")
			} else {
				args = append(args, "--api-key", "k")
			}

			code, _, stderr := runProbes(t, args...)

			if code != 0 || stderr != "" {
				t.Errorf("exit status = %d, stderr %q, want 0 and nothing", code, stderr)
			}
			if n := len(hub.recorded()); n != 1 {
				t.Errorf("the hub got %d requests, want 1", n)
			}
		})
	}
}

func TestSettings_CreateNothingWithoutAConfigFile(t *testing.T) {
	for _, tc := range []string{"PARLEY_CONFIG_DIR names a missing directory", "home directory without .parley", "no home directory"} {
		t.Run(tc, func(t *testing.T) {
			tmp := t.TempDir()
			loginEnv(t)
			switch tc {
			case "PARLEY_CONFIG_DIR names a missing directory":
				t.Setenv("PARLEY_CONFIG_DIR", filepath.Join(tmp, "missing"))
			case "home directory without .parley":
				unsetEnv(t, "PARLEY_CONFIG_DIR")
				t.Setenv("HOME", filepath.Join(tmp, "nohome"))
			default:
				unsetEnv(t, "PARLEY_CONFIG_DIR")
				unsetEnv(t, "HOME")
			}
			hub := thingHub(t, `{}`)
			t.Setenv("PARLEY_ENDPOINT_URL", hub.URL)
			t.Setenv("PARLEY_API_KEY", "k")

			if code, _, stderr := runProbes(t, "probe", "get", "x"); code != 0 {
				t.Errorf("with both variables: exit status = %d, want 0; stderr %q", code, stderr)
			}
			unsetEnv(t, "PARLEY_API_KEY")
			code, stdout, stderr := runProbes(t, "probe", "get", "x")
			wantClientFailure(t, code, stdout, stderr, "client_error")

			wantAbsent(t, filepath.Join(tmp, "missing"))
			wantAbsent(t, filepath.Join(tmp, "nohome", ".parley"))
		})
	}
}

func TestSettings_RefuseAConfigFileItCannotParseBeforeAnyRequest(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{"not TOML", writeFile("not = [toml")},
		{"endpoint_url not a string", writeFile("endpoint_url = 5")},
		{"api_key not a string", writeFile("api_key = [\"a\"]")},
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
			dir := loginEnv(t)
			os.MkdirAll(dir, 0o700)
			path := filepath.Join(dir, "config.toml")
			tc.setup(t, path)
			hub := thingHub(t, `{}`)
			t.Setenv("PARLEY_ENDPOINT_URL", hub.URL)

			code, stdout, stderr := runProbes(t, "probe", "get", "x")

			if message := wantClientFailure(t, code, stdout, stderr, "client_error"); !strings.Contains(message, path) {
				t.Errorf("message %q does not name %s", message, path)
			}
			if n := len(hub.recorded()); n != 0 {
				t.Errorf("the hub got %d requests, want none", n)
			}
		})
	}
}
