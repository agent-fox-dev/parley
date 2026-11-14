package cli

import (
	"os"
	"testing"
)

// The tests of this file set environment variables, so they must not run in
// parallel.

// helperEnv gives the test an empty config directory of its own, which it
// returns, and sets the environment variables of env.
func helperEnv(t *testing.T, env map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(configDirEnv, dir)
	for k, v := range env {
		t.Setenv(k, v)
	}
	return dir
}

// onHubEnv is the environment of a hub at hubURL and the credential key.
func onHubEnv(hubURL, key string) map[string]string {
	return map[string]string{endpointURLEnv: hubURL, apiKeyEnv: key}
}

// helperAnswer is what the helper prints for protocol, host and password.
func helperAnswer(protocol, host, password string) string {
	return "protocol=" + protocol + "\nhost=" + host + "\nusername=x-token-auth\npassword=" + password + "\n"
}

// checkHelperRun fails the test unless the helper exited 0 with want on
// stdout and nothing on stderr.
func checkHelperRun(t *testing.T, code int, stdout, stderr, want string) {
	t.Helper()
	if code != 0 || stdout != want || stderr != "" {
		t.Errorf("exit %d, stdout %q, stderr %q; want 0, %q and nothing", code, stdout, stderr, want)
	}
}

func TestCredentialHelper_PrintsTheCredentialForTheHubWithoutCallingIt(t *testing.T) {
	t.Run("a hub nothing listens at", func(t *testing.T) {
		helperEnv(t, onHubEnv("https://hub.example:8443", "parley_k_secret"))
		code, stdout, stderr := runWithStdin(t, "protocol=https\nhost=hub.example:8443\npath=git/acme/demo.git\n\n", "credential-helper", "get")
		checkHelperRun(t, code, stdout, stderr, helperAnswer("https", "hub.example:8443", "parley_k_secret"))
	})
	t.Run("a hub that records every request", func(t *testing.T) {
		hub := newStubHub(t)
		helperEnv(t, onHubEnv(hub.URL, "k"))
		host := hub.Listener.Addr().String()
		code, stdout, stderr := runWithStdin(t, "protocol=http\nhost="+host+"\n\n", "credential-helper", "get")
		checkHelperRun(t, code, stdout, stderr, helperAnswer("http", host, "k"))
		if n := len(hub.recorded()); n != 0 {
			t.Errorf("the hub got %d requests, want none", n)
		}
	})
}

func TestCredentialHelper_AnswersOnlyForTheHubsSchemeHostAndPort(t *testing.T) {
	tests := []struct {
		hub, protocol, host string
		answers             bool
	}{
		{"https://hub", "https", "HUB:443", true},
		{"https://hub", "https", "hub", true},
		{"http://hub:80/base", "http", "hub", true},
		{"https://hub:8443", "https", "hub:8443", true},
		{"https://hub", "http", "hub", false},
		{"https://hub", "https", "hub:444", false},
		{"https://hub:8443", "https", "hub", false},
		{"https://hub", "https", "other", false},
		{"https://hub", "https", "hub.evil", false},
	}
	for _, tt := range tests {
		t.Run(tt.hub+" asked for "+tt.protocol+" "+tt.host, func(t *testing.T) {
			helperEnv(t, onHubEnv(tt.hub, "k"))
			code, stdout, stderr := runWithStdin(t, "protocol="+tt.protocol+"\nhost="+tt.host+"\npath=base/git/acme/demo.git\n\n", "credential-helper", "get")
			want := ""
			if tt.answers {
				want = helperAnswer(tt.protocol, tt.host, "k")
			}
			checkHelperRun(t, code, stdout, stderr, want)
		})
	}
}

func TestCredentialHelper_ReadsTheRequestUpToTheFirstEmptyLine(t *testing.T) {
	answer := helperAnswer("https", "hub", "k")
	tests := []struct{ stdin, want string }{
		{"host=hub\nusername=bob\nwwwauth[]=Basic realm=\"parley\"\ngarbage\nprotocol=https\n\nhost=other\n", answer},
		{"protocol=https\nhost=hub", answer},
		{"host=hub\n\n", ""},
		{"protocol=https\n\n", ""},
		{"", ""},
		{"\nprotocol=https\nhost=hub\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.stdin, func(t *testing.T) {
			helperEnv(t, onHubEnv("https://hub", "k"))
			code, stdout, stderr := runWithStdin(t, tt.stdin, "credential-helper", "get")
			checkHelperRun(t, code, stdout, stderr, tt.want)
		})
	}
}

func TestCredentialHelper_IgnoresEveryVerbButGet(t *testing.T) {
	for _, verb := range []string{"store", "erase", "capability", "GET", ""} {
		t.Run(verb, func(t *testing.T) {
			dir := helperEnv(t, onHubEnv("https://hub", "k"))
			code, stdout, stderr := runWithStdin(t, "protocol=https\nhost=hub\nusername=x\npassword=y\n\n", "credential-helper", verb)
			checkHelperRun(t, code, stdout, stderr, "")
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
				t.Errorf("the config directory holds %v (%v), want it empty", entries, err)
			}
		})
	}
}

func TestCredentialHelper_AnswersNothingWithoutUsableSettings(t *testing.T) {
	tests := []struct {
		name   string
		env    map[string]string
		config string
	}{
		{"no hub URL", map[string]string{apiKeyEnv: "k"}, ""},
		{"no credential", map[string]string{endpointURLEnv: "https://hub"}, ""},
		{"neither", nil, ""},
		{"a hub URL without a host", onHubEnv("https://", "k"), ""},
		{"a hub URL without a scheme", onHubEnv("hub.example", "k"), ""},
		{"a hub URL of another scheme", onHubEnv("ssh://hub", "k"), ""},
		{"a config file that is not TOML", nil, "not = [toml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := helperEnv(t, tt.env)
			if tt.config != "" {
				writeConfigFile(t, dir, tt.config)
			}
			code, stdout, stderr := runWithStdin(t, "protocol=https\nhost=hub\n\n", "credential-helper", "get")
			checkHelperRun(t, code, stdout, stderr, "")
		})
	}
}

func TestCredentialHelper_TakesEachSettingFromTheFlagOrEnvironmentBeforeTheConfigFile(t *testing.T) {
	tests := []struct {
		name  string
		env   map[string]string
		flags []string
		host  string
		want  string
	}{
		{name: "the file alone", host: "file.example", want: helperAnswer("https", "file.example", "filekey")},
		{name: "the credential from the environment", env: map[string]string{apiKeyEnv: "envkey"}, host: "file.example", want: helperAnswer("https", "file.example", "envkey")},
		{name: "the hub URL from the environment", env: map[string]string{endpointURLEnv: "https://env.example"}, host: "env.example", want: helperAnswer("https", "env.example", "filekey")},
		{name: "the file's hub URL overridden by the environment", env: map[string]string{endpointURLEnv: "https://env.example"}, host: "file.example"},
		{name: "the credential from the flag", env: map[string]string{apiKeyEnv: "envkey"}, flags: []string{"--api-key", "flagkey"}, host: "file.example", want: helperAnswer("https", "file.example", "flagkey")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := helperEnv(t, tt.env)
			writeConfigFile(t, dir, "endpoint_url = \"https://file.example\"\napi_key = \"filekey\"\n")
			code, stdout, stderr := runWithStdin(t, "protocol=https\nhost="+tt.host+"\n\n", append([]string{"credential-helper", "get"}, tt.flags...)...)
			checkHelperRun(t, code, stdout, stderr, tt.want)
		})
	}
}

func TestCredentialHelper_RefusesAWrongArgumentCount(t *testing.T) {
	for _, args := range [][]string{{"credential-helper"}, {"credential-helper", "get", "extra"}} {
		t.Run(args[len(args)-1], func(t *testing.T) {
			helperEnv(t, onHubEnv("https://hub", "k"))
			code, stdout, stderr := runWithStdin(t, "protocol=https\nhost=hub\n\n", args...)
			if code != 2 || stdout != "" {
				t.Errorf("exit %d, stdout %q; want 2 and nothing", code, stdout)
			}
			if errCode, _, errorType := clientEnvelope(t, stderr); errCode != 0 || errorType != "usage_error" {
				t.Errorf("stderr = %q, want a usage_error envelope with code 0", stderr)
			}
		})
	}
}
