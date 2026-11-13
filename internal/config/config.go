// Package config loads the hub's configuration file, config.toml, and checks
// it strictly against the keys the hub's packages register: a section, a key
// or a value nobody accepts stops the load instead of being ignored.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// Env looks up an environment variable, in the shape of os.LookupEnv. The
// loader takes one instead of reading the process environment, so that tests
// load in parallel with environments of their own.
type Env func(name string) (string, bool)

// MapEnv returns an Env that looks names up in m; a nil m is an empty
// environment.
func MapEnv(m map[string]string) Env {
	return func(name string) (string, bool) {
		v, ok := m[name]
		return v, ok
	}
}

// FileName is the name of the configuration file.
const FileName = "config.toml"

// Config is a loaded configuration: the values of the file, or the default of
// every key the file does not set.
type Config struct {
	// Path is the absolute path of the file that was looked for, whether or
	// not it exists.
	Path string
	// Dir is the directory of Path, the config dir.
	Dir string

	Server    Server
	Database  Database
	Workspace Workspace
	Logging   Logging
	Limits    Limits
	OAuth     OAuth

	values map[string]map[string]any
	// sections holds the content of each section registered as a whole that
	// the file holds.
	sections map[string]map[string]any
}

// Server holds the keys of [server].
type Server struct {
	// Bind is the address the hub listens on.
	Bind string
	// Port is the TCP port the hub listens on; 0 picks a free port.
	Port int
	// ExternalURL is the URL clients reach the hub at; empty when not set.
	ExternalURL string
	// MaxBodySize is the largest REST request body, in bytes.
	MaxBodySize int64
}

// Database holds the keys of [database].
type Database struct {
	// Path is the absolute path of the transactional store's file.
	Path string
}

// Workspace holds the keys of [workspace].
type Workspace struct {
	// Path is the absolute path of the workspace root.
	Path string
	// Workers is how many workspace jobs run at the same time.
	Workers int
}

// Logging holds the keys of [logging].
type Logging struct {
	// Level is trace, debug, info, warn or error, in lower case.
	Level string
	// LogHealthProbes writes an access-log line for /healthz and /readyz.
	LogHealthProbes bool
}

// Limits holds the keys of [limits] the loader owns.
type Limits struct {
	// RequestsPerMinute is the REST rate limit per caller; 0 turns it off.
	RequestsPerMinute int
}

// OAuth holds the [[oauth.providers]] tables.
type OAuth struct {
	// Providers are in file order.
	Providers []Provider
}

// Provider is one [[oauth.providers]] table.
type Provider struct {
	// Name is github or google.
	Name         string
	ClientID     string
	ClientSecret string
	// AuthorizeURL, TokenURL and UserinfoURL are the file's, or the
	// provider's public endpoints when the file does not give them.
	AuthorizeURL string
	TokenURL     string
	UserinfoURL  string
}

// DataDir returns the data directory: the directory of the database file.
func (c *Config) DataDir() string { return filepath.Dir(c.Database.Path) }

// AnalyticsPath returns the path of the analytical store's file,
// analytics.db in the data directory.
func (c *Config) AnalyticsPath() string { return filepath.Join(c.DataDir(), "analytics.db") }

// Section returns the content of a section registered as a whole, as its
// validator was given it, and whether the file holds the section.
func (c *Config) Section(name string) (map[string]any, bool) {
	s, ok := c.sections[name]
	return s, ok
}

// Value returns the value of a registered key, the file's or its default, and
// whether the key is registered.
func (c *Config) Value(section, name string) (any, bool) {
	v, ok := c.values[section][name]
	return v, ok
}

// String returns the value of a registered String key, or "" for a key that
// is not one.
func (c *Config) String(section, name string) string {
	v, _ := c.values[section][name].(string)
	return v
}

// Int returns the value of a registered Int key, or 0 for a key that is not
// one.
func (c *Config) Int(section, name string) int {
	v, _ := c.values[section][name].(int)
	return v
}

// Bool returns the value of a registered Bool key, or false for a key that is
// not one.
func (c *Config) Bool(section, name string) bool {
	v, _ := c.values[section][name].(bool)
	return v
}

// Load loads the configuration with the keys the loader owns and no others.
func Load(env Env, workDir string) (*Config, error) {
	return NewRegistry().Load(env, workDir)
}

// Load reads the configuration file once, replaces the environment
// references in its string values and checks it against the registered keys
// and sections. A string value's ${NAME} becomes the value of the variable
// NAME, which must be set, and $$ becomes one $. The file is $XDG_CONFIG_HOME/config.toml when
// XDG_CONFIG_HOME is set and non-empty, and config.toml in workDir otherwise;
// a missing file means every default. Any error refuses the whole file: Load
// returns a configuration only when every value is valid.
func (r *Registry) Load(env Env, workDir string) (*Config, error) {
	if len(r.errs) > 0 {
		return nil, errors.Join(r.errs...)
	}
	if env == nil {
		env = MapEnv(nil)
	}
	work, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("working directory %s: %w", workDir, err)
	}
	path := filepath.Join(work, FileName)
	if dir := lookup(env, "XDG_CONFIG_HOME"); dir != "" {
		path = filepath.Join(under(work, dir), FileName)
	}
	tree, err := readTree(path)
	if err != nil {
		return nil, err
	}
	if err := expandTree(tree, env); err != nil {
		return nil, fmt.Errorf("configuration file %s: %w", path, err)
	}
	values, providers, sections, err := r.check(tree)
	if err != nil {
		return nil, fmt.Errorf("configuration file %s: %w", path, err)
	}
	c := build(values, providers, path, work, lookup(env, "XDG_DATA_HOME"))
	c.sections = sections
	return c, nil
}

// lookup returns the variable's value, "" when it is unset.
func lookup(env Env, name string) string {
	v, _ := env(name)
	return v
}

// under resolves p against dir unless it is absolute.
func under(dir, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(dir, p)
}

// readTree reads and decodes the file; a file that does not exist decodes to
// nothing.
func readTree(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return nil, fmt.Errorf("configuration file %s cannot be read: %w", path, err)
	}
	var tree map[string]any
	if _, err := toml.Decode(string(data), &tree); err != nil {
		var pe toml.ParseError
		if errors.As(err, &pe) {
			// The parser counts a line break that ends a line as the start of
			// the next one; the byte offset names the line the mistake is on.
			line := 1 + strings.Count(string(data[:min(max(pe.Position.Start, 0), len(data))]), "\n")
			return nil, fmt.Errorf("configuration file %s: line %d: %s", path, line, pe.Message)
		}
		return nil, fmt.Errorf("configuration file %s: %w", path, err)
	}
	return tree, nil
}

// check walks the decoded file against the registry and returns the value of
// every registered key, the OAuth providers and the content of the sections
// registered as a whole. It stops at the first key it refuses, in the order of
// section and key names; the validators of whole sections run only once every
// key is accepted.
func (r *Registry) check(tree map[string]any) (map[string]map[string]any, []Provider, map[string]map[string]any, error) {
	values := map[string]map[string]any{}
	for section, keys := range r.keys {
		values[section] = map[string]any{}
		for name, k := range keys {
			values[section][name] = k.Default
		}
	}
	var providers []Provider
	sections := map[string]map[string]any{}
	for _, section := range sortedKeys(tree) {
		table, isTable := tree[section].(map[string]any)
		if _, whole := r.sections[section]; whole {
			if !isTable {
				return nil, nil, nil, fmt.Errorf("%s: must be a [%s] table", section, section)
			}
			sections[section] = table
			continue
		}
		if section == oauthSection {
			p, err := checkOAuth(tree[section])
			if err != nil {
				return nil, nil, nil, err
			}
			providers = p
			continue
		}
		keys, known := r.keys[section]
		switch {
		case !isTable && !known:
			return nil, nil, nil, fmt.Errorf("%s: unknown key: every key belongs in a [section]", section)
		case !isTable:
			return nil, nil, nil, fmt.Errorf("%s: must be a [%s] table", section, section)
		case !known:
			return nil, nil, nil, fmt.Errorf("[%s]: unknown section", section)
		}
		for _, name := range sortedKeys(table) {
			k, ok := keys[name]
			if !ok {
				return nil, nil, nil, fmt.Errorf("%s.%s: unknown key", section, name)
			}
			v, err := coerce(k.Type, table[name])
			if err == nil && k.Validate != nil {
				err = k.Validate(v)
			}
			if err != nil {
				return nil, nil, nil, fmt.Errorf("%s: %w", k.id(), err)
			}
			values[section][name] = v
		}
	}
	for _, section := range sortedKeys(sections) {
		if err := r.sections[section](sections[section]); err != nil {
			return nil, nil, nil, err
		}
	}
	return values, providers, sections, nil
}

// The keys of a [[oauth.providers]] table and the endpoints of each provider
// the hub can log in with.
var (
	providerKeys     = []string{"name", "client_id", "client_secret", "authorize_url", "token_url", "userinfo_url"}
	defaultEndpoints = map[string][3]string{
		"github": {
			"https://github.com/login/oauth/authorize",
			"https://github.com/login/oauth/access_token",
			"https://api.github.com/user",
		},
		"google": {
			"https://accounts.google.com/o/oauth2/v2/auth",
			"https://oauth2.googleapis.com/token",
			"https://openidconnect.googleapis.com/v1/userinfo",
		},
	}
)

// checkOAuth reads the [oauth] section, which holds nothing but the
// repeatable [[oauth.providers]] table. Errors name the key, never its value,
// so that a client secret does not reach a log.
func checkOAuth(raw any) ([]Provider, error) {
	table, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: must be a table", oauthSection)
	}
	for _, name := range sortedKeys(table) {
		if name != "providers" {
			return nil, fmt.Errorf("%s.%s: unknown key", oauthSection, name)
		}
	}
	list, ok := tables(table["providers"])
	if !ok {
		return nil, fmt.Errorf("%s.providers: must be [[%s.providers]] tables", oauthSection, oauthSection)
	}
	var providers []Provider
	for i, t := range list {
		prefix := fmt.Sprintf("%s.providers[%d]", oauthSection, i)
		fields := map[string]string{}
		for _, name := range sortedKeys(t) {
			if !slices.Contains(providerKeys, name) {
				return nil, fmt.Errorf("%s.%s: unknown key", prefix, name)
			}
			s, ok := t[name].(string)
			if !ok {
				return nil, fmt.Errorf("%s.%s: must be a string", prefix, name)
			}
			fields[name] = s
		}
		name := fields["name"]
		endpoints, ok := defaultEndpoints[name]
		switch {
		case !ok:
			return nil, fmt.Errorf("%s.name: must be github or google", prefix)
		case slices.ContainsFunc(providers, func(p Provider) bool { return p.Name == name }):
			return nil, fmt.Errorf("%s.name: provider %s is configured twice", prefix, name)
		case fields["client_id"] == "":
			return nil, fmt.Errorf("%s.client_id: must not be empty", prefix)
		case fields["client_secret"] == "":
			return nil, fmt.Errorf("%s.client_secret: must not be empty", prefix)
		}
		for j, key := range providerKeys[3:] {
			s, given := fields[key]
			if !given {
				fields[key] = endpoints[j]
			} else if err := httpURL(s); err != nil {
				return nil, fmt.Errorf("%s.%s: %w", prefix, key, err)
			}
		}
		providers = append(providers, Provider{
			Name:         name,
			ClientID:     fields["client_id"],
			ClientSecret: fields["client_secret"],
			AuthorizeURL: fields["authorize_url"],
			TokenURL:     fields["token_url"],
			UserinfoURL:  fields["userinfo_url"],
		})
	}
	return providers, nil
}

// tables returns v as a list of tables: [[name]] tables decode as one Go
// type and an inline array of tables as another. Nothing is an empty list.
func tables(v any) ([]map[string]any, bool) {
	switch x := v.(type) {
	case nil:
		return nil, true
	case []map[string]any:
		return x, true
	case []any:
		list := make([]map[string]any, 0, len(x))
		for _, e := range x {
			t, ok := e.(map[string]any)
			if !ok {
				return nil, false
			}
			list = append(list, t)
		}
		return list, true
	}
	return nil, false
}

// build turns checked values into a Config, resolving the data paths.
func build(values map[string]map[string]any, providers []Provider, path, work, dataHome string) *Config {
	if dataHome != "" {
		dataHome = under(work, dataHome)
	}
	values["database"]["path"] = dataPath(values["database"]["path"].(string), "parley.db", work, dataHome)
	values["workspace"]["path"] = dataPath(values["workspace"]["path"].(string), "workspaces", work, dataHome)
	values["logging"]["level"] = strings.ToLower(values["logging"]["level"].(string))
	maxBody, _ := parseSize(values["server"]["max_body_size"].(string)) // checked by its key's rule

	c := &Config{Path: path, Dir: filepath.Dir(path), values: values}
	c.Server = Server{
		Bind:        c.String("server", "bind"),
		Port:        c.Int("server", "port"),
		ExternalURL: c.String("server", "external_url"),
		MaxBodySize: maxBody,
	}
	c.Database = Database{Path: c.String("database", "path")}
	c.Workspace = Workspace{Path: c.String("workspace", "path"), Workers: c.Int("workspace", "workers")}
	c.Logging = Logging{Level: c.String("logging", "level"), LogHealthProbes: c.Bool("logging", "log_health_probes")}
	c.Limits = Limits{RequestsPerMinute: c.Int("limits", "requests_per_minute")}
	c.OAuth = OAuth{Providers: providers}
	return c
}

// dataPath resolves database.path or workspace.path. Not set (""), it is name
// under $XDG_DATA_HOME, or under data/ in the working directory. A bare file
// name goes under $XDG_DATA_HOME when that is set; any other relative path is
// resolved against the working directory.
func dataPath(p, name, work, dataHome string) string {
	switch {
	case p == "" && dataHome != "":
		return filepath.Join(dataHome, name)
	case p == "":
		return filepath.Join(work, "data", name)
	case filepath.IsAbs(p):
		return filepath.Clean(p)
	case dataHome != "" && filepath.Base(p) == p:
		return filepath.Join(dataHome, p)
	}
	return filepath.Join(work, p)
}

func sortedKeys[V any](m map[string]V) []string { return slices.Sorted(maps.Keys(m)) }
