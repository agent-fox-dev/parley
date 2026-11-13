package cli

import (
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The configured API key of the key commands' tests, its id, and the key a
// refresh answers in its place.
const (
	configuredKey   = "parley_Ab3dE6gH_0123456789abcdefghijABCDEFGHIJ01"
	configuredKeyID = "Ab3dE6gH"
	refreshedKey    = "parley_Ab3dE6gH_ZYXWVUTSRQponmlkjihgfedcba987654"
	refreshAnswer   = `{"key":"` + refreshedKey + `","key_id":"` + configuredKeyID + `","expires_at":"2027-02-11T10:00:00Z"}`
	revokedKey      = `{"key_id":"` + configuredKeyID + `","user_id":"u-1","expires_at":null,"revoked_at":"2026-11-13T10:00:00Z","created_at":"2026-11-01T10:00:00Z"}`
)

// keysEnv writes a config file in a new config directory that holds hub's
// URL, credential, the user id u-1 and the key parley does not know extra,
// unsets PARLEY_ENDPOINT_URL and PARLEY_API_KEY, and returns the file's
// path. A test that calls it must not run in parallel.
func keysEnv(t *testing.T, hub *stubHub, credential string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cfg")
	t.Setenv("PARLEY_CONFIG_DIR", dir)
	unsetEnv(t, "PARLEY_ENDPOINT_URL")
	unsetEnv(t, "PARLEY_API_KEY")
	return writeConfigFile(t, dir, "endpoint_url = '"+hub.URL+"'\napi_key = '"+credential+"'\nuser_id = 'u-1'\nextra = 'x'\n")
}

// noConfigEnv points PARLEY_CONFIG_DIR at an empty directory and gives hub's
// URL and the configured key through PARLEY_ENDPOINT_URL and PARLEY_API_KEY,
// and returns the path the config file would have. A test that calls it
// must not run in parallel.
func noConfigEnv(t *testing.T, hub *stubHub) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PARLEY_CONFIG_DIR", dir)
	t.Setenv("PARLEY_ENDPOINT_URL", hub.URL)
	t.Setenv("PARLEY_API_KEY", configuredKey)
	return filepath.Join(dir, "config.toml")
}

// wantOneRequest fails the test unless the hub got exactly one request,
// method to rawPath with the configured key.
func wantOneRequest(t *testing.T, hub *stubHub, method, rawPath string) {
	t.Helper()
	sent := hub.recorded()
	if len(sent) != 1 {
		t.Fatalf("the hub got %d requests, want 1", len(sent))
	}
	if sent[0].method != method || sent[0].rawPath != rawPath {
		t.Errorf("sent %s %s, want %s %s", sent[0].method, sent[0].rawPath, method, rawPath)
	}
	if auth := sent[0].header.Get("Authorization"); auth != "Bearer "+configuredKey {
		t.Errorf("Authorization = %q, want the configured key", auth)
	}
}

// The test sets environment variables, so it must not run in parallel.
func TestKeysList_SendsOneRequestAndPrintsTheAnswer(t *testing.T) {
	wantSends(t, []string{"keys", "list"}, sentRequest{method: http.MethodGet, rawPath: "/api/v1/user/keys"})
}

// The test sets environment variables, so it must not run in parallel.
func TestKeysRefresh_PrintsTheNewKeyAndSavesItInTheConfigFile(t *testing.T) {
	hub := newStubHub(t).answer(http.MethodPost, "/api/v1/user/keys/"+configuredKeyID+"/refresh", http.StatusOK, refreshAnswer)
	path := keysEnv(t, hub, configuredKey)

	code, stdout, stderr := runCLI(t, "keys", "refresh")

	if code != 0 {
		t.Errorf("exit status = %d, want 0 (stderr %q)", code, stderr)
	}
	wantOneRequest(t, hub, http.MethodPost, "/api/v1/user/keys/"+configuredKeyID+"/refresh")
	if want := indented(t, refreshAnswer); stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if !strings.Contains(stderr, showOnceWarning) {
		t.Errorf("stderr = %q, want the show-once warning", stderr)
	}
	want := map[string]any{"endpoint_url": hub.URL, "api_key": refreshedKey, "user_id": "u-1", "extra": "x"}
	if got := readTOML(t, path); !reflect.DeepEqual(got, want) {
		t.Errorf("config file = %v, want %v", got, want)
	}
	if mode := modeOf(t, path); mode != 0o600 {
		t.Errorf("config file mode = %v, want 0600", mode)
	}
}

// The test sets environment variables, so it must not run in parallel.
func TestKeysRefresh_ReportsANewKeyItCannotSave(t *testing.T) {
	tests := []struct {
		name string
		// setup prepares the environment and returns the config file's
		// path.
		setup func(t *testing.T, hub *stubHub) string
	}{
		{"the config directory is read-only", func(t *testing.T, hub *stubHub) string {
			if os.Geteuid() == 0 {
				t.Skip("root writes into a read-only directory")
			}
			path := keysEnv(t, hub, configuredKey)
			dir := filepath.Dir(path)
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(dir, 0o700) })
			return path
		}},
		{"there is no config file", noConfigEnv},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := newStubHub(t).answer(http.MethodPost, "/api/v1/user/keys/"+configuredKeyID+"/refresh", http.StatusOK, refreshAnswer)
			path := tt.setup(t, hub)
			before := snapshot(t, filepath.Dir(path))

			code, stdout, stderr := runCLI(t, "keys", "refresh")

			if code != 2 {
				t.Errorf("exit status = %d, want 2", code)
			}
			if want := indented(t, refreshAnswer); stdout != want {
				t.Errorf("stdout = %q, want %q", stdout, want)
			}
			if !strings.Contains(stderr, "the new key was not saved") {
				t.Errorf("stderr = %q, want it to say that the new key was not saved", stderr)
			}
			if !strings.Contains(stderr, `"error_type": "client_error"`) {
				t.Errorf("stderr = %q, want a client_error envelope", stderr)
			}
			if after := snapshot(t, filepath.Dir(path)); !reflect.DeepEqual(after, before) {
				t.Errorf("the config directory changed:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}

// The test sets environment variables, so it must not run in parallel.
func TestKeysCommands_RefuseACredentialThatIsNotAnAPIKey(t *testing.T) {
	credentials := map[string]string{
		"a PAT":           "parley_pat_k3x9q2mz_0123456789abcdefghijklmnopqrstuv",
		"the admin token": "parley_admin_" + strings.Repeat("0123456789abcdef", 4),
		"garbage":         "garbage",
	}
	for _, command := range []string{"refresh", "revoke"} {
		for name, credential := range credentials {
			t.Run(command+" with "+name, func(t *testing.T) {
				hub := newStubHub(t)
				path := keysEnv(t, hub, credential)
				before := snapshot(t, filepath.Dir(path))

				code, stdout, stderr := runCLI(t, "keys", command)

				message := wantClientFailure(t, code, stdout, stderr, clientErrorType)
				if strings.Contains(message, credential) {
					t.Errorf("message = %q, want it not to quote the credential", message)
				}
				if n := len(hub.recorded()); n != 0 {
					t.Errorf("the hub got %d requests, want none", n)
				}
				if after := snapshot(t, filepath.Dir(path)); !reflect.DeepEqual(after, before) {
					t.Errorf("the config directory changed:\nbefore %v\nafter  %v", before, after)
				}
			})
		}
	}
}

// The test sets environment variables, so it must not run in parallel.
func TestKeysRevoke_RevokesTheConfiguredKeyAndClearsItFromTheConfigFile(t *testing.T) {
	const notFound = `{"error":{"code":404,"message":"API key not found","error_type":"not_found"}}`
	tests := []struct {
		name   string
		status int
		answer string
		// noFile gives the settings through the environment, without a
		// config file.
		noFile bool
	}{
		{name: "with a config file", status: http.StatusOK, answer: revokedKey},
		{name: "without a config file", status: http.StatusOK, answer: revokedKey, noFile: true},
		{name: "the hub answers an error", status: http.StatusNotFound, answer: notFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := newStubHub(t).answer(http.MethodDelete, "/api/v1/user/keys/"+configuredKeyID, tt.status, tt.answer)
			var path string
			if tt.noFile {
				path = noConfigEnv(t, hub)
			} else {
				path = keysEnv(t, hub, configuredKey)
			}
			before := snapshot(t, filepath.Dir(path))

			code, stdout, stderr := runCLI(t, "keys", "revoke")

			wantOneRequest(t, hub, http.MethodDelete, "/api/v1/user/keys/"+configuredKeyID)
			if tt.status != http.StatusOK {
				if code != 1 {
					t.Errorf("exit status = %d, want 1", code)
				}
				if want := indented(t, tt.answer); stderr != want {
					t.Errorf("stderr = %q, want %q", stderr, want)
				}
				if stdout != "" {
					t.Errorf("stdout = %q, want it empty", stdout)
				}
				if after := snapshot(t, filepath.Dir(path)); !reflect.DeepEqual(after, before) {
					t.Errorf("the config directory changed:\nbefore %v\nafter  %v", before, after)
				}
				return
			}
			if code != 0 {
				t.Errorf("exit status = %d, want 0 (stderr %q)", code, stderr)
			}
			if want := indented(t, tt.answer); stdout != want {
				t.Errorf("stdout = %q, want %q", stdout, want)
			}
			if !strings.Contains(stderr, "parley login") {
				t.Errorf("stderr = %q, want it to tell the user to run parley login", stderr)
			}
			if tt.noFile {
				wantAbsent(t, path)
				return
			}
			want := map[string]any{"endpoint_url": hub.URL, "api_key": "", "user_id": "", "extra": "x"}
			if got := readTOML(t, path); !reflect.DeepEqual(got, want) {
				t.Errorf("config file = %v, want %v", got, want)
			}
			if mode := modeOf(t, path); mode != 0o600 {
				t.Errorf("config file mode = %v, want 0600", mode)
			}
		})
	}
}
