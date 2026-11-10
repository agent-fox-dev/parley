// Package config loads parleyd's configuration file: where it is, the TOML
// it holds, the keys every package registers and their validation, and the
// keys of the server, the data and the logs that this package owns.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// FileName is the configuration file's name.
const FileName = "config.toml"

// LookupEnv looks up an environment variable, as os.LookupEnv does.
type LookupEnv func(name string) (string, bool)

// MapEnv is a LookupEnv over a map.
func MapEnv(m map[string]string) LookupEnv {
	return func(name string) (string, bool) {
		v, ok := m[name]
		return v, ok
	}
}

// Provider is a configured OAuth provider.
type Provider struct {
	Name         string
	ClientID     string
	ClientSecret string
	AuthorizeURL string
	TokenURL     string
	UserinfoURL  string
}

// providerEndpoints are the public endpoints of each provider: authorize,
// token and userinfo.
var providerEndpoints = map[string][3]string{
	"github": {"https://github.com/login/oauth/authorize", "https://github.com/login/oauth/access_token", "https://api.github.com/user"},
	"google": {"https://accounts.google.com/o/oauth2/v2/auth", "https://oauth2.googleapis.com/token", "https://openidconnect.googleapis.com/v1/userinfo"},
}

// Config is a loaded and validated configuration.
type Config struct {
	// Path is the absolute path of the file that was looked for, whether or
	// not it exists; Dir is its directory, the config dir.
	Path string
	Dir  string

	Server struct {
		Bind        string
		Port        int
		ExternalURL string
		// MaxBodySize is in bytes.
		MaxBodySize int64
	}
	Database struct {
		// Path is the absolute path of the transactional store's file.
		Path string
	}
	Workspace struct {
		// Path is the absolute path of the workspace root.
		Path    string
		Workers int
	}
	Logging struct {
		// Level is in lower case.
		Level           string
		LogHealthProbes bool
	}
	Limits struct {
		RequestsPerMinute int
	}
	OAuth struct {
		Providers []Provider
	}

	values   map[string]any
	sections map[string]map[string]any
}

// Load reads the configuration with the keys of this package only.
func Load(env LookupEnv, workDir string) (*Config, error) {
	return NewRegistry().Load(env, workDir)
}

// DataDir is the data directory: the directory of the database file.
func (c *Config) DataDir() string { return filepath.Dir(c.Database.Path) }

// AnalyticsPath is the analytical store's file, analytics.db in the data
// directory.
func (c *Config) AnalyticsPath() string { return filepath.Join(c.DataDir(), "analytics.db") }

// Value returns the value of a registered key (string, int or bool), the
// file's or the default; nil for a key nobody registered.
func (c *Config) Value(section, name string) any { return c.values[section+"."+name] }

// String returns a registered string key's value.
func (c *Config) String(section, name string) string {
	s, _ := c.Value(section, name).(string)
	return s
}

// Int returns a registered integer key's value.
func (c *Config) Int(section, name string) int {
	n, _ := c.Value(section, name).(int)
	return n
}

// Bool returns a registered boolean key's value.
func (c *Config) Bool(section, name string) bool {
	b, _ := c.Value(section, name).(bool)
	return b
}

// lookup returns a non-empty variable.
func nonEmptyEnv(env LookupEnv, name string) string {
	if env == nil {
		return ""
	}
	v, _ := env(name)
	return v
}

// configPath is $XDG_CONFIG_HOME/config.toml when the variable is set and
// non-empty, otherwise config.toml in the working directory.
func configPath(env LookupEnv, workDir string) string {
	dir := workDir
	if xdg := nonEmptyEnv(env, "XDG_CONFIG_HOME"); xdg != "" {
		dir = xdg
	}
	return absUnder(workDir, filepath.Join(dir, FileName))
}

func absUnder(workDir, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(workDir, p)
}

// Load reads the configuration file once: the environment gives
// XDG_CONFIG_HOME and XDG_DATA_HOME, and workDir resolves relative paths.
// It returns a configuration only when the whole file is valid; otherwise
// the error names the offending key as section.key, or the line of a syntax
// error.
func (r *Registry) Load(env LookupEnv, workDir string) (*Config, error) {
	if len(r.errs) > 0 {
		return nil, errors.Join(r.errs...)
	}
	path := configPath(env, workDir)
	raw := map[string]any{}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("cannot read configuration file %s: %w", path, err)
	default:
		if _, err := toml.Decode(string(data), &raw); err != nil {
			return nil, syntaxError(path, data, err)
		}
	}

	cfg := &Config{Path: path, Dir: filepath.Dir(path), values: map[string]any{}, sections: map[string]map[string]any{}}
	for _, id := range r.order {
		cfg.values[id] = r.keys[id].Default
	}
	if err := r.apply(raw, cfg, env); err != nil {
		return nil, err
	}
	if err := r.applyWhole(raw, cfg, env); err != nil {
		return nil, err
	}
	cfg.fill(env, workDir)
	return cfg, nil
}

// syntaxError names the line that holds the character the TOML parser
// stopped at; the parser counts a newline that ends a line as the next one.
func syntaxError(path string, data []byte, err error) error {
	var pe toml.ParseError
	if errors.As(err, &pe) {
		start := min(max(pe.Position.Start, 0), len(data))
		line := strings.Count(string(data[:start]), "\n") + 1
		return fmt.Errorf("configuration file %s: invalid TOML at line %d: %s", path, line, pe.Message)
	}
	return fmt.Errorf("configuration file %s: %w", path, err)
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// apply checks every key of the parsed file against the registry, replaces
// environment references in string values, and sets the values; the first
// problem, in name order, is the error. Sections registered as a whole are
// left to applyWhole.
func (r *Registry) apply(raw map[string]any, cfg *Config, env LookupEnv) error {
	for _, section := range sortedKeys(raw) {
		table, isTable := raw[section].(map[string]any)
		if !isTable {
			return fmt.Errorf("configuration key %s is not in a section", section)
		}
		if r.whole[section] != nil {
			continue
		}
		if section == "oauth" {
			providers, err := parseOAuth(table, env)
			if err != nil {
				return err
			}
			cfg.OAuth.Providers = providers
			continue
		}
		if !r.sections[section] {
			return fmt.Errorf("unknown configuration section [%s]", section)
		}
		for _, name := range sortedKeys(table) {
			id := section + "." + name
			k, ok := r.keys[id]
			if !ok {
				return fmt.Errorf("unknown configuration key %s", id)
			}
			v, err := typed(k.Type, table[name])
			if err != nil {
				return fmt.Errorf("%s: %w", id, err)
			}
			if str, ok := v.(string); ok {
				if v, err = expand(str, env); err != nil {
					return fmt.Errorf("%s: %w", id, err)
				}
			}
			if k.Validate != nil {
				if err := k.Validate(v); err != nil {
					return fmt.Errorf("%s: %w", id, err)
				}
			}
			cfg.values[id] = v
		}
	}
	return nil
}

// applyWhole hands every section registered as a whole that the file holds
// to its validator, once, after replacing the references in its strings.
func (r *Registry) applyWhole(raw map[string]any, cfg *Config, env LookupEnv) error {
	for _, section := range sortedKeys(raw) {
		validate := r.whole[section]
		if validate == nil {
			continue
		}
		expanded, err := expandDeep(raw[section], section, env)
		if err != nil {
			return fmt.Errorf("[%s]: %w", section, err)
		}
		content := expanded.(map[string]any)
		if err := validate(content); err != nil {
			return fmt.Errorf("[%s]: %w", section, err)
		}
		cfg.sections[section] = content
	}
	return nil
}

// typed converts a parsed TOML value to the key's type.
func typed(t Type, v any) (any, error) {
	switch t {
	case String:
		if s, ok := v.(string); ok {
			return s, nil
		}
	case Int:
		if n, ok := v.(int64); ok {
			if int64(int(n)) != n {
				return nil, errors.New("is out of range")
			}
			return int(n), nil
		}
	case Bool:
		if b, ok := v.(bool); ok {
			return b, nil
		}
	}
	return nil, fmt.Errorf("must be %s", t)
}

var providerKeys = map[string]bool{"name": true, "client_id": true, "client_secret": true, "authorize_url": true, "token_url": true, "userinfo_url": true}

// parseOAuth validates the [oauth] table, which holds only the repeatable
// [[oauth.providers]] table.
func parseOAuth(table map[string]any, env LookupEnv) ([]Provider, error) {
	var out []Provider
	seen := map[string]bool{}
	for _, name := range sortedKeys(table) {
		if name != "providers" {
			return nil, fmt.Errorf("unknown configuration key oauth.%s", name)
		}
	}
	list, ok := table["providers"].([]map[string]any)
	if !ok && table["providers"] != nil {
		return nil, errors.New("oauth.providers: must be a list of [[oauth.providers]] tables")
	}
	for _, p := range list {
		for _, k := range sortedKeys(p) {
			if !providerKeys[k] {
				return nil, fmt.Errorf("unknown configuration key oauth.providers.%s", k)
			}
			str, ok := p[k].(string)
			if !ok {
				return nil, fmt.Errorf("oauth.providers.%s: must be a string", k)
			}
			expanded, err := expand(str, env)
			if err != nil {
				return nil, fmt.Errorf("oauth.providers.%s: %w", k, err)
			}
			p[k] = expanded
		}
		get := func(k string) string { s, _ := p[k].(string); return s }
		prov := Provider{Name: get("name"), ClientID: get("client_id"), ClientSecret: get("client_secret")}
		defaults, known := providerEndpoints[prov.Name]
		if !known {
			return nil, errors.New("oauth.providers.name: must be github or google")
		}
		if seen[prov.Name] {
			return nil, fmt.Errorf("oauth.providers: provider %s is configured more than once", prov.Name)
		}
		seen[prov.Name] = true
		for _, k := range []string{"client_id", "client_secret"} {
			if get(k) == "" {
				return nil, fmt.Errorf("oauth.providers.%s: must not be empty (provider %s)", k, prov.Name)
			}
		}
		urls := [3]string{}
		for i, k := range []string{"authorize_url", "token_url", "userinfo_url"} {
			urls[i] = defaults[i]
			if _, set := p[k]; set {
				if !httpURL(get(k)) {
					return nil, fmt.Errorf("oauth.providers.%s: must be an absolute http or https URL (provider %s)", k, prov.Name)
				}
				urls[i] = get(k)
			}
		}
		prov.AuthorizeURL, prov.TokenURL, prov.UserinfoURL = urls[0], urls[1], urls[2]
		out = append(out, prov)
	}
	return out, nil
}

// fill sets the typed fields from the validated values and resolves the
// data paths.
func (c *Config) fill(env LookupEnv, workDir string) {
	c.Server.Bind = c.String("server", "bind")
	c.Server.Port = c.Int("server", "port")
	c.Server.ExternalURL = c.String("server", "external_url")
	c.Server.MaxBodySize, _ = ParseSize(c.String("server", "max_body_size"))
	c.Workspace.Workers = c.Int("workspace", "workers")
	c.Logging.Level = strings.ToLower(c.String("logging", "level"))
	c.Logging.LogHealthProbes = c.Bool("logging", "log_health_probes")
	c.Limits.RequestsPerMinute = c.Int("limits", "requests_per_minute")

	xdg := nonEmptyEnv(env, "XDG_DATA_HOME")
	c.Database.Path = dataPath(c.String("database", "path"), "parley.db", xdg, workDir)
	c.Workspace.Path = dataPath(c.String("workspace", "path"), "workspaces", xdg, workDir)
}

// dataPath resolves a data path: unset, the default name under
// $XDG_DATA_HOME or under data/ in the working directory; a bare file name
// under $XDG_DATA_HOME when that is set; any other relative path against
// the working directory; an absolute path as it is.
func dataPath(configured, defaultName, xdg, workDir string) string {
	switch {
	case configured == "" && xdg != "":
		return absUnder(workDir, filepath.Join(xdg, defaultName))
	case configured == "":
		return filepath.Join(workDir, "data", defaultName)
	case filepath.IsAbs(configured):
		return filepath.Clean(configured)
	case xdg != "" && !strings.ContainsRune(configured, filepath.Separator) && !strings.Contains(configured, "/"):
		return absUnder(workDir, filepath.Join(xdg, configured))
	}
	return filepath.Join(workDir, configured)
}

// Section returns a section registered as a whole: its content as its
// validator received it, and whether the file held it.
func (c *Config) Section(name string) (map[string]any, bool) {
	s, ok := c.sections[name]
	return s, ok
}
