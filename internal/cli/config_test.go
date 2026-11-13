package cli

import (
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
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
