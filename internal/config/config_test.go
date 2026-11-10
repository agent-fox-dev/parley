package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dirWith returns a temporary working directory holding config.toml with
// text, or no file when text is nil.
func dirWith(t *testing.T, text *string) string {
	t.Helper()
	dir := t.TempDir()
	if text != nil {
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(*text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func file(s string) *string { return &s }

var noEnv = MapEnv(nil)

func mustLoad(t *testing.T, env LookupEnv, dir string) *Config {
	t.Helper()
	cfg, err := Load(env, dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestLoad_ReadsTheFileFromXDGConfigHomeOrTheWorkingDirectory(t *testing.T) {
	tests := []struct {
		name     string
		xdg      *string // nil: unset
		inXDG    bool
		wantXDG  bool
		wantPort int
	}{
		{"unset", nil, false, false, 1111},
		{"empty", file(""), false, false, 1111},
		{"set with a file there", file("x"), true, true, 2222},
		{"set without a file there", file("x"), false, true, 8080},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			work := dirWith(t, file("[server]\nport = 1111\n"))
			xdg := t.TempDir()
			env := map[string]string{}
			if tt.xdg != nil {
				env["XDG_CONFIG_HOME"] = *tt.xdg
				if *tt.xdg != "" {
					env["XDG_CONFIG_HOME"] = xdg
				}
			}
			if tt.inXDG {
				if err := os.WriteFile(filepath.Join(xdg, "config.toml"), []byte("[server]\nport = 2222\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg := mustLoad(t, MapEnv(env), work)
			want := filepath.Join(work, "config.toml")
			if tt.wantXDG {
				want = filepath.Join(xdg, "config.toml")
			}
			if cfg.Path != want || cfg.Dir != filepath.Dir(want) {
				t.Errorf("Path %q Dir %q, want %q", cfg.Path, cfg.Dir, want)
			}
			if cfg.Server.Port != tt.wantPort {
				t.Errorf("port = %d, want %d", cfg.Server.Port, tt.wantPort)
			}
		})
	}
}

func TestLoad_AMissingOrEmptyFileMeansEveryDefault(t *testing.T) {
	for _, text := range []*string{nil, file(""), file("# only a comment\n")} {
		dir := dirWith(t, text)
		cfg := mustLoad(t, noEnv, dir)
		if cfg.Server.Bind != "0.0.0.0" || cfg.Server.Port != 8080 || cfg.Server.ExternalURL != "" || cfg.Server.MaxBodySize != 1048576 {
			t.Errorf("server = %+v", cfg.Server)
		}
		if cfg.Workspace.Workers != 4 || cfg.Logging.Level != "info" || cfg.Logging.LogHealthProbes || cfg.Limits.RequestsPerMinute != 600 {
			t.Errorf("workspace %+v logging %+v limits %+v", cfg.Workspace, cfg.Logging, cfg.Limits)
		}
		if len(cfg.OAuth.Providers) != 0 {
			t.Errorf("providers = %v", cfg.OAuth.Providers)
		}
		if cfg.Database.Path != filepath.Join(dir, "data", "parley.db") || cfg.Workspace.Path != filepath.Join(dir, "data", "workspaces") {
			t.Errorf("paths = %q %q", cfg.Database.Path, cfg.Workspace.Path)
		}
	}
}

func TestLoad_ReturnsValidValuesOfTheFile(t *testing.T) {
	tests := []struct {
		text string
		get  func(*Config) any
		want any
	}{
		{"[server]\nport = 0", func(c *Config) any { return c.Server.Port }, 0},
		{"[server]\nport = 65535", func(c *Config) any { return c.Server.Port }, 65535},
		{"[server]\nbind = \"127.0.0.1\"", func(c *Config) any { return c.Server.Bind }, "127.0.0.1"},
		{"[server]\nexternal_url = \"https://hub.example\"", func(c *Config) any { return c.Server.ExternalURL }, "https://hub.example"},
		{"[server]\nmax_body_size = \"1KB\"", func(c *Config) any { return c.Server.MaxBodySize }, int64(1024)},
		{"[server]\nmax_body_size = \"512kb\"", func(c *Config) any { return c.Server.MaxBodySize }, int64(524288)},
		{"[server]\nmax_body_size = \"2Mb\"", func(c *Config) any { return c.Server.MaxBodySize }, int64(2097152)},
		{"[server]\nmax_body_size = \"1GB\"", func(c *Config) any { return c.Server.MaxBodySize }, int64(1073741824)},
		{"[workspace]\nworkers = 1", func(c *Config) any { return c.Workspace.Workers }, 1},
		{"[workspace]\nworkers = 64", func(c *Config) any { return c.Workspace.Workers }, 64},
		{"[logging]\nlevel = \"TRACE\"", func(c *Config) any { return c.Logging.Level }, "trace"},
		{"[logging]\nlevel = \"Debug\"", func(c *Config) any { return c.Logging.Level }, "debug"},
		{"[logging]\nlevel = \"warn\"", func(c *Config) any { return c.Logging.Level }, "warn"},
		{"[logging]\nlevel = \"ERROR\"", func(c *Config) any { return c.Logging.Level }, "error"},
		{"[logging]\nlog_health_probes = true", func(c *Config) any { return c.Logging.LogHealthProbes }, true},
		{"[limits]\nrequests_per_minute = 0", func(c *Config) any { return c.Limits.RequestsPerMinute }, 0},
		{"[limits]\nrequests_per_minute = 7", func(c *Config) any { return c.Limits.RequestsPerMinute }, 7},
	}
	for _, tt := range tests {
		cfg := mustLoad(t, noEnv, dirWith(t, file(tt.text)))
		if got := tt.get(cfg); got != tt.want {
			t.Errorf("%q: got %v (%T), want %v", tt.text, got, got, tt.want)
		}
	}
}

const provider = "[[oauth.providers]]\nname = \"github\"\nclient_id = \"a\"\nclient_secret = \"b\"\n"

func TestLoad_RefusesTheWholeFileNamingTheOffendingKey(t *testing.T) {
	tests := []struct{ text, want string }{
		{"[server\nport = 1", "line 1"},
		{"[nosuch]\nx = 1", "nosuch"},
		{"[server]\nprot = 1", "server.prot"},
		{"top = 1", "top"},
		{"[server]\nport = \"80\"", "server.port"},
		{"[server]\nport = -1", "server.port"},
		{"[server]\nport = 65536", "server.port"},
		{"[server]\nbind = \"\"", "server.bind"},
		{"[server]\nexternal_url = \"hub.example\"", "server.external_url"},
		{"[workspace]\nworkers = 0", "workspace.workers"},
		{"[database]\npath = \"\"", "database.path"},
		{"[logging]\nlevel = \"loud\"", "logging.level"},
		{"[logging]\nlog_health_probes = \"yes\"", "logging.log_health_probes"},
		{"[limits]\nrequests_per_minute = -1", "limits.requests_per_minute"},
		{"[maintenance]\ninterval = \"1m\"", "maintenance"},
		{"[[oauth.providers]]\nname = \"gitlab\"\nclient_id = \"a\"\nclient_secret = \"b\"", "oauth.providers"},
		{"[[oauth.providers]]\nname = \"github\"\nclient_secret = \"b\"", "client_id"},
		{"[[oauth.providers]]\nname = \"github\"\nclient_id = \"a\"\nclient_secret = \"\"", "client_secret"},
		{provider + provider, "oauth.providers"},
		{provider + "token_url = \"ftp://x\"", "token_url"},
		{provider + "tenant = \"t\"", "tenant"},
	}
	for _, size := range []string{"0MB", "-1MB", "1.5MB", "1TB", "1 MB", "MB", "1024", ""} {
		tests = append(tests, struct{ text, want string }{"[server]\nmax_body_size = \"" + size + "\"", "server.max_body_size"})
	}
	for _, tt := range tests {
		cfg, err := Load(noEnv, dirWith(t, file(tt.text)))
		if cfg != nil || err == nil {
			t.Errorf("%q: got a configuration, want an error", tt.text)
			continue
		}
		if !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%q: error %q does not contain %q", tt.text, err, tt.want)
		}
	}
}

func TestLoad_ReturnsOAuthProvidersWithTheirEndpoints(t *testing.T) {
	gh := [3]string{"https://github.com/login/oauth/authorize", "https://github.com/login/oauth/access_token", "https://api.github.com/user"}
	gg := [3]string{"https://accounts.google.com/o/oauth2/v2/auth", "https://oauth2.googleapis.com/token", "https://openidconnect.googleapis.com/v1/userinfo"}
	ghe := [3]string{"https://ghe.example/authorize", "https://ghe.example/token", "https://ghe.example/user"}
	google := "[[oauth.providers]]\nname = \"google\"\nclient_id = \"g-id\"\nclient_secret = \"g-secret\"\n"
	github := "[[oauth.providers]]\nname = \"github\"\nclient_id = \"h-id\"\nclient_secret = \"h-secret\"\n"
	tests := []struct {
		text  string
		names []string
		urls  [][3]string
	}{
		{github, []string{"github"}, [][3]string{gh}},
		{google, []string{"google"}, [][3]string{gg}},
		{google + github, []string{"google", "github"}, [][3]string{gg, gh}},
		{github + "authorize_url = \"" + ghe[0] + "\"\ntoken_url = \"" + ghe[1] + "\"\nuserinfo_url = \"" + ghe[2] + "\"\n", []string{"github"}, [][3]string{ghe}},
	}
	for _, tt := range tests {
		cfg := mustLoad(t, noEnv, dirWith(t, file(tt.text)))
		if len(cfg.OAuth.Providers) != len(tt.names) {
			t.Fatalf("providers = %+v", cfg.OAuth.Providers)
		}
		for i, p := range cfg.OAuth.Providers {
			if p.Name != tt.names[i] || p.ClientID != map[string]string{"github": "h-id", "google": "g-id"}[p.Name] ||
				p.ClientSecret != map[string]string{"github": "h-secret", "google": "g-secret"}[p.Name] {
				t.Errorf("provider %d = %+v", i, p)
			}
			if got := [3]string{p.AuthorizeURL, p.TokenURL, p.UserinfoURL}; got != tt.urls[i] {
				t.Errorf("provider %s urls = %v, want %v", p.Name, got, tt.urls[i])
			}
		}
	}
}

func atLeastOne(v any) error {
	if v.(int) < 1 {
		return errors.New("must be at least 1")
	}
	return nil
}

func TestRegistry_AcceptsKeysOtherPackagesRegister(t *testing.T) {
	tests := []struct {
		text      *string
		forks     int
		things    int
		perMinute int
		wantErr   string
	}{
		{nil, 200, 5, 600, ""},
		{file("[forks]\nmax_per_workspace = 9"), 9, 5, 600, ""},
		{file("[limits]\nmax_things = 2\nrequests_per_minute = 3"), 200, 2, 3, ""},
		{file("[forks]\nmax_per_workspace = 0"), 0, 0, 0, "forks.max_per_workspace"},
		{file("[forks]\nother = 1"), 0, 0, 0, "forks.other"},
	}
	for _, tt := range tests {
		reg := NewRegistry()
		reg.Register(Key{Section: "forks", Name: "max_per_workspace", Type: Int, Default: 200, Validate: atLeastOne})
		reg.Register(Key{Section: "limits", Name: "max_things", Type: Int, Default: 5})
		cfg, err := reg.Load(noEnv, dirWith(t, tt.text))
		if tt.wantErr != "" {
			if cfg != nil || err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("got %v, %v; want an error containing %q", cfg, err, tt.wantErr)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Value("forks", "max_per_workspace") != tt.forks || cfg.Value("limits", "max_things") != tt.things || cfg.Limits.RequestsPerMinute != tt.perMinute {
			t.Errorf("got %v %v %d", cfg.Value("forks", "max_per_workspace"), cfg.Value("limits", "max_things"), cfg.Limits.RequestsPerMinute)
		}
	}
}

func TestRegistry_RefusesADuplicateKey(t *testing.T) {
	for _, k := range []Key{{Section: "forks", Name: "default_ttl", Type: String, Default: "1d"}, {Section: "server", Name: "port", Type: Int, Default: 1}} {
		reg := NewRegistry()
		reg.Register(k)
		if k.Section == "forks" {
			reg.Register(k)
		}
		cfg, err := reg.Load(noEnv, dirWith(t, nil))
		if cfg != nil || err == nil || !strings.Contains(err.Error(), k.Section+"."+k.Name) {
			t.Errorf("duplicate %s.%s: got %v, %v", k.Section, k.Name, cfg, err)
		}
	}
}

func TestLoad_AFileThatCannotBeReadFailsTheLoad(t *testing.T) {
	asDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(asDir, "config.toml"), 0o700); err != nil {
		t.Fatal(err)
	}
	dirs := []string{asDir}
	if os.Geteuid() != 0 {
		unreadable := dirWith(t, file("[server]\nport = 1"))
		if err := os.Chmod(filepath.Join(unreadable, "config.toml"), 0); err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, unreadable)
	}
	for _, dir := range dirs {
		cfg, err := Load(noEnv, dir)
		if cfg != nil || err == nil || !strings.Contains(err.Error(), filepath.Join(dir, "config.toml")) {
			t.Errorf("got %v, %v; want an error naming the file", cfg, err)
		}
	}
}

func TestLoad_ResolvesTheDataPaths(t *testing.T) {
	work := t.TempDir()
	xdg := t.TempDir()
	tests := []struct {
		xdg            *string
		db, ws         string // in the file; "" means not set
		wantDB, wantWS string
	}{
		{nil, "", "", filepath.Join(work, "data/parley.db"), filepath.Join(work, "data/workspaces")},
		{file(""), "", "", filepath.Join(work, "data/parley.db"), filepath.Join(work, "data/workspaces")},
		{file(xdg), "", "", filepath.Join(xdg, "parley.db"), filepath.Join(xdg, "workspaces")},
		{file(xdg), "hub.db", "repos", filepath.Join(xdg, "hub.db"), filepath.Join(xdg, "repos")},
		{nil, "hub.db", "repos", filepath.Join(work, "hub.db"), filepath.Join(work, "repos")},
		{file(xdg), "var/hub.db", "var/repos", filepath.Join(work, "var/hub.db"), filepath.Join(work, "var/repos")},
		{file(xdg), "/abs/hub.db", "/abs/repos", "/abs/hub.db", "/abs/repos"},
	}
	for _, tt := range tests {
		text := ""
		if tt.db != "" {
			text += "[database]\npath = \"" + tt.db + "\"\n[workspace]\npath = \"" + tt.ws + "\"\n"
		}
		if err := os.WriteFile(filepath.Join(work, "config.toml"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		env := map[string]string{}
		if tt.xdg != nil {
			env["XDG_DATA_HOME"] = *tt.xdg
		}
		cfg := mustLoad(t, MapEnv(env), work)
		if cfg.Database.Path != tt.wantDB || cfg.Workspace.Path != tt.wantWS {
			t.Errorf("%+v: got %q %q, want %q %q", tt, cfg.Database.Path, cfg.Workspace.Path, tt.wantDB, tt.wantWS)
		}
		if cfg.DataDir() != filepath.Dir(tt.wantDB) || cfg.AnalyticsPath() != filepath.Join(filepath.Dir(tt.wantDB), "analytics.db") {
			t.Errorf("data dir %q analytics %q", cfg.DataDir(), cfg.AnalyticsPath())
		}
	}
}
