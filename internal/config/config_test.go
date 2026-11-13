package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/config"
)

// dirWith returns a new directory holding a config.toml with text.
func dirWith(t *testing.T, text string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.toml"), text)
	return dir
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func noEnv() config.Env { return config.MapEnv(nil) }

func TestLoad_ReadsTheFileFromXDGConfigHomeOrTheWorkingDirectory(t *testing.T) {
	cases := []struct {
		name     string
		xdg      *string // nil: unset; points at "" or "xdg"
		xdgFile  bool
		wantXDG  bool
		wantPort int
	}{
		{name: "unset", wantPort: 1111},
		{name: "empty", xdg: ptr(""), wantPort: 1111},
		{name: "set with both files", xdg: ptr("xdg"), xdgFile: true, wantXDG: true, wantPort: 2222},
		{name: "set with only the working directory's file", xdg: ptr("xdg"), wantXDG: true, wantPort: 8080},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			work := dirWith(t, "[server]\nport = 1111\n")
			xdg := t.TempDir()
			if tc.xdgFile {
				writeFile(t, filepath.Join(xdg, "config.toml"), "[server]\nport = 2222\n")
			}
			env := map[string]string{}
			if tc.xdg != nil {
				env["XDG_CONFIG_HOME"] = ""
				if *tc.xdg != "" {
					env["XDG_CONFIG_HOME"] = xdg
				}
			}
			cfg, err := config.Load(config.MapEnv(env), work)
			if err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(work, "config.toml")
			if tc.wantXDG {
				want = filepath.Join(xdg, "config.toml")
			}
			if cfg.Path != want {
				t.Errorf("Path = %q, want %q", cfg.Path, want)
			}
			if cfg.Dir != filepath.Dir(want) {
				t.Errorf("Dir = %q, want %q", cfg.Dir, filepath.Dir(want))
			}
			if cfg.Server.Port != tc.wantPort {
				t.Errorf("port = %d, want %d", cfg.Server.Port, tc.wantPort)
			}
		})
	}
}

func ptr(s string) *string { return &s }

func TestLoad_UsesTheDefaultOfEveryKeyTheFileDoesNotSet(t *testing.T) {
	cases := map[string]*string{
		"no file":             nil,
		"an empty file":       ptr(""),
		"only a comment line": ptr("# nothing to see\n"),
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if text != nil {
				writeFile(t, filepath.Join(dir, "config.toml"), *text)
			}
			cfg, err := config.Load(noEnv(), dir)
			if err != nil {
				t.Fatal(err)
			}
			checks := []struct {
				key       string
				got, want any
			}{
				{"server.bind", cfg.Server.Bind, "0.0.0.0"},
				{"server.port", cfg.Server.Port, 8080},
				{"server.external_url", cfg.Server.ExternalURL, ""},
				{"server.max_body_size", cfg.Server.MaxBodySize, int64(1048576)},
				{"workspace.workers", cfg.Workspace.Workers, 4},
				{"logging.level", cfg.Logging.Level, "info"},
				{"logging.log_health_probes", cfg.Logging.LogHealthProbes, false},
				{"limits.requests_per_minute", cfg.Limits.RequestsPerMinute, 600},
				{"oauth providers", len(cfg.OAuth.Providers), 0},
				{"database.path", cfg.Database.Path, filepath.Join(dir, "data", "parley.db")},
				{"workspace.path", cfg.Workspace.Path, filepath.Join(dir, "data", "workspaces")},
			}
			for _, c := range checks {
				if c.got != c.want {
					t.Errorf("%s = %v, want %v", c.key, c.got, c.want)
				}
			}
		})
	}
}

func TestLoad_ReturnsTheValuesTheFileSets(t *testing.T) {
	port := func(c *config.Config) any { return c.Server.Port }
	bodySize := func(c *config.Config) any { return c.Server.MaxBodySize }
	level := func(c *config.Config) any { return c.Logging.Level }
	workers := func(c *config.Config) any { return c.Workspace.Workers }
	rpm := func(c *config.Config) any { return c.Limits.RequestsPerMinute }
	cases := []struct {
		file string
		get  func(*config.Config) any
		want any
	}{
		{"[server]\nport = 0", port, 0},
		{"[server]\nport = 65535", port, 65535},
		{"[server]\nbind = '127.0.0.1'", func(c *config.Config) any { return c.Server.Bind }, "127.0.0.1"},
		{"[server]\nexternal_url = 'https://hub.example'", func(c *config.Config) any { return c.Server.ExternalURL }, "https://hub.example"},
		{"[server]\nmax_body_size = '1KB'", bodySize, int64(1024)},
		{"[server]\nmax_body_size = '512kb'", bodySize, int64(524288)},
		{"[server]\nmax_body_size = '2Mb'", bodySize, int64(2097152)},
		{"[server]\nmax_body_size = '1GB'", bodySize, int64(1073741824)},
		{"[workspace]\nworkers = 1", workers, 1},
		{"[workspace]\nworkers = 64", workers, 64},
		{"[logging]\nlevel = 'TRACE'", level, "trace"},
		{"[logging]\nlevel = 'Debug'", level, "debug"},
		{"[logging]\nlevel = 'warn'", level, "warn"},
		{"[logging]\nlevel = 'ERROR'", level, "error"},
		{"[logging]\nlog_health_probes = true", func(c *config.Config) any { return c.Logging.LogHealthProbes }, true},
		{"[limits]\nrequests_per_minute = 0", rpm, 0},
		{"[limits]\nrequests_per_minute = 7", rpm, 7},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			cfg, err := config.Load(noEnv(), dirWith(t, tc.file))
			if err != nil {
				t.Fatal(err)
			}
			if got := tc.get(cfg); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLoad_RefusesTheWholeFileAndNamesTheOffendingKey(t *testing.T) {
	const provider = "[[oauth.providers]]\nname = 'github'\nclient_id = 'a'\nclient_secret = 'b'\n"
	cases := []struct{ file, want string }{
		{"[server\nport = 1", "line 1"},
		{"[nosuch]\nx = 1", "nosuch"},
		{"[maintenance]\ninterval = 1", "maintenance"},
		{"[server]\nprot = 1", "server.prot"},
		{"top = 1", "top"},
		{"[server]\nport = \"80\"", "server.port"},
		{"[server]\nport = 80.0", "server.port"},
		{"[server]\nport = -1", "server.port"},
		{"[server]\nport = 65536", "server.port"},
		{"[server]\nbind = \"\"", "server.bind"},
		{"[server]\nexternal_url = \"hub.example\"", "server.external_url"},
		{"[server]\nmax_body_size = '0MB'", "server.max_body_size"},
		{"[server]\nmax_body_size = '-1MB'", "server.max_body_size"},
		{"[server]\nmax_body_size = '1.5MB'", "server.max_body_size"},
		{"[server]\nmax_body_size = '1TB'", "server.max_body_size"},
		{"[server]\nmax_body_size = '1 MB'", "server.max_body_size"},
		{"[server]\nmax_body_size = 'MB'", "server.max_body_size"},
		{"[server]\nmax_body_size = '1024'", "server.max_body_size"},
		{"[server]\nmax_body_size = ''", "server.max_body_size"},
		{"[workspace]\nworkers = 0", "workspace.workers"},
		{"[database]\npath = \"\"", "database.path"},
		{"[logging]\nlevel = \"loud\"", "logging.level"},
		{"[logging]\nlog_health_probes = \"yes\"", "logging.log_health_probes"},
		{"[limits]\nrequests_per_minute = -1", "limits.requests_per_minute"},
		{"[[oauth.providers]]\nname = \"gitlab\"\nclient_id = \"a\"\nclient_secret = \"b\"", "oauth.providers"},
		{"[[oauth.providers]]\nname = 'github'\nclient_secret = 'b'", "client_id"},
		{"[[oauth.providers]]\nname = 'github'\nclient_id = 'a'\nclient_secret = ''", "client_secret"},
		{provider + provider, "oauth.providers"},
		{provider + "token_url = 'ftp://x'", "token_url"},
		{provider + "tenant = 'x'", "tenant"},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			cfg, err := config.Load(noEnv(), dirWith(t, tc.file))
			if cfg != nil {
				t.Errorf("returned a configuration: %+v", cfg)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestLoad_ReturnsTheOAuthProvidersInFileOrderWithTheirEndpoints(t *testing.T) {
	github := config.Provider{
		Name: "github", ClientID: "gh-id", ClientSecret: "gh-secret",
		AuthorizeURL: "https://github.com/login/oauth/authorize",
		TokenURL:     "https://github.com/login/oauth/access_token",
		UserinfoURL:  "https://api.github.com/user",
	}
	google := config.Provider{
		Name: "google", ClientID: "go-id", ClientSecret: "go-secret",
		AuthorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL:     "https://oauth2.googleapis.com/token",
		UserinfoURL:  "https://openidconnect.googleapis.com/v1/userinfo",
	}
	const githubTable = "[[oauth.providers]]\nname = 'github'\nclient_id = 'gh-id'\nclient_secret = 'gh-secret'\n"
	const googleTable = "[[oauth.providers]]\nname = 'google'\nclient_id = 'go-id'\nclient_secret = 'go-secret'\n"
	enterprise := github
	enterprise.AuthorizeURL = "https://ghe.example/login/oauth/authorize"
	enterprise.TokenURL = "https://ghe.example/login/oauth/access_token"
	enterprise.UserinfoURL = "https://ghe.example/api/v3/user"
	cases := []struct {
		name string
		file string
		want []config.Provider
	}{
		{"github alone", githubTable, []config.Provider{github}},
		{"google alone", googleTable, []config.Provider{google}},
		{"google then github", googleTable + githubTable, []config.Provider{google, github}},
		{"github with its own endpoints", githubTable +
			"authorize_url = 'https://ghe.example/login/oauth/authorize'\n" +
			"token_url = 'https://ghe.example/login/oauth/access_token'\n" +
			"userinfo_url = 'https://ghe.example/api/v3/user'\n",
			[]config.Provider{enterprise}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Load(noEnv(), dirWith(t, tc.file))
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.OAuth.Providers) != len(tc.want) {
				t.Fatalf("providers = %+v, want %+v", cfg.OAuth.Providers, tc.want)
			}
			for i, p := range cfg.OAuth.Providers {
				if p != tc.want[i] {
					t.Errorf("provider %d = %+v, want %+v", i, p, tc.want[i])
				}
			}
		})
	}
}

func atLeast(n int) func(any) error {
	return func(v any) error {
		if v.(int) < n {
			return errors.New("too small")
		}
		return nil
	}
}

func TestRegistry_LoadsTheKeysOtherPackagesRegister(t *testing.T) {
	cases := []struct {
		name      string
		file      *string
		wantForks int
		wantThing int
		wantRPM   int
		wantErr   string
	}{
		{name: "no file", wantForks: 200, wantThing: 5, wantRPM: 600},
		{name: "own section", file: ptr("[forks]\nmax_per_workspace = 9"), wantForks: 9, wantThing: 5, wantRPM: 600},
		{name: "shared section", file: ptr("[limits]\nmax_things = 2\nrequests_per_minute = 3"), wantForks: 200, wantThing: 2, wantRPM: 3},
		{name: "refused by its rule", file: ptr("[forks]\nmax_per_workspace = 0"), wantErr: "forks.max_per_workspace"},
		{name: "unregistered key in a registered section", file: ptr("[forks]\nother = 1"), wantErr: "forks.other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := config.NewRegistry()
			if err := reg.Register(config.Key{Section: "forks", Name: "max_per_workspace", Type: config.Int, Default: 200, Validate: atLeast(1)}); err != nil {
				t.Fatal(err)
			}
			if err := reg.Register(config.Key{Section: "limits", Name: "max_things", Type: config.Int, Default: 5}); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if tc.file != nil {
				dir = dirWith(t, *tc.file)
			}
			cfg, err := reg.Load(noEnv(), dir)
			if tc.wantErr != "" {
				if cfg != nil || err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("cfg = %v, err = %v, want no configuration and an error containing %q", cfg, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.Int("forks", "max_per_workspace"); got != tc.wantForks {
				t.Errorf("forks.max_per_workspace = %d, want %d", got, tc.wantForks)
			}
			if got := cfg.Int("limits", "max_things"); got != tc.wantThing {
				t.Errorf("limits.max_things = %d, want %d", got, tc.wantThing)
			}
			if got := cfg.Limits.RequestsPerMinute; got != tc.wantRPM {
				t.Errorf("limits.requests_per_minute = %d, want %d", got, tc.wantRPM)
			}
		})
	}
}

func TestRegistry_RefusesAKeyRegisteredTwice(t *testing.T) {
	cases := []struct {
		name  string
		key   config.Key
		twice bool
	}{
		{"registered twice by one package", config.Key{Section: "forks", Name: "default_ttl", Type: config.String, Default: "7d"}, true},
		{"owned by the loader", config.Key{Section: "server", Name: "port", Type: config.Int, Default: 1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := config.NewRegistry()
			if tc.twice {
				if err := reg.Register(tc.key); err != nil {
					t.Fatal(err)
				}
			}
			want := tc.key.Section + "." + tc.key.Name
			if err := reg.Register(tc.key); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("Register error = %v, want one containing %q", err, want)
			}
			cfg, err := reg.Load(noEnv(), t.TempDir())
			if cfg != nil || err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("cfg = %v, err = %v, want no configuration and an error containing %q", cfg, err, want)
			}
		})
	}
}

func TestLoad_FailsWhenTheFileExistsButCannotBeRead(t *testing.T) {
	cases := map[string]func(t *testing.T, path string){
		"a directory": func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"a file without read permission": func(t *testing.T, path string) {
			if os.Geteuid() == 0 {
				t.Skip("root reads a file of mode 0000")
			}
			writeFile(t, path, "[server]\nport = 1\n")
			if err := os.Chmod(path, 0); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			setup(t, path)
			cfg, err := config.Load(noEnv(), dir)
			if cfg != nil || err == nil || !strings.Contains(err.Error(), path) {
				t.Errorf("cfg = %v, err = %v, want no configuration and an error containing %q", cfg, err, path)
			}
		})
	}
}

func TestLoad_ResolvesTheDatabaseAndWorkspacePaths(t *testing.T) {
	cases := []struct {
		name          string
		xdg           *string // nil: unset; "xdg": the test's directory
		db, ws        string
		wantDB, wantW string // relative to work, or to xdg with a leading "xdg/", or absolute
	}{
		{name: "defaults without XDG_DATA_HOME", wantDB: "data/parley.db", wantW: "data/workspaces"},
		{name: "defaults with an empty XDG_DATA_HOME", xdg: ptr(""), wantDB: "data/parley.db", wantW: "data/workspaces"},
		{name: "defaults under XDG_DATA_HOME", xdg: ptr("xdg"), wantDB: "xdg/parley.db", wantW: "xdg/workspaces"},
		{name: "bare names under XDG_DATA_HOME", xdg: ptr("xdg"), db: "hub.db", ws: "repos", wantDB: "xdg/hub.db", wantW: "xdg/repos"},
		{name: "bare names without XDG_DATA_HOME", db: "hub.db", ws: "repos", wantDB: "hub.db", wantW: "repos"},
		{name: "relative paths with a directory", xdg: ptr("xdg"), db: "var/hub.db", ws: "var/repos", wantDB: "var/hub.db", wantW: "var/repos"},
		{name: "absolute paths", xdg: ptr("xdg"), db: "/abs/hub.db", ws: "/abs/repos", wantDB: "/abs/hub.db", wantW: "/abs/repos"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var file strings.Builder
			if tc.db != "" {
				file.WriteString("[database]\npath = '" + tc.db + "'\n")
			}
			if tc.ws != "" {
				file.WriteString("[workspace]\npath = '" + tc.ws + "'\n")
			}
			work := dirWith(t, file.String())
			xdg := t.TempDir()
			env := map[string]string{}
			if tc.xdg != nil {
				env["XDG_DATA_HOME"] = ""
				if *tc.xdg != "" {
					env["XDG_DATA_HOME"] = xdg
				}
			}
			resolve := func(p string) string {
				switch {
				case filepath.IsAbs(p):
					return p
				case strings.HasPrefix(p, "xdg/"):
					return filepath.Join(xdg, strings.TrimPrefix(p, "xdg/"))
				default:
					return filepath.Join(work, p)
				}
			}
			cfg, err := config.Load(config.MapEnv(env), work)
			if err != nil {
				t.Fatal(err)
			}
			wantDB, wantW := resolve(tc.wantDB), resolve(tc.wantW)
			if cfg.Database.Path != wantDB {
				t.Errorf("database path = %q, want %q", cfg.Database.Path, wantDB)
			}
			if cfg.Workspace.Path != wantW {
				t.Errorf("workspace path = %q, want %q", cfg.Workspace.Path, wantW)
			}
			if cfg.DataDir() != filepath.Dir(wantDB) {
				t.Errorf("data dir = %q, want %q", cfg.DataDir(), filepath.Dir(wantDB))
			}
			if want := filepath.Join(filepath.Dir(wantDB), "analytics.db"); cfg.AnalyticsPath() != want {
				t.Errorf("analytics path = %q, want %q", cfg.AnalyticsPath(), want)
			}
		})
	}
}
