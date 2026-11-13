package config

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Type is the TOML type a configuration key takes.
type Type int

// The types a key can have. An Int key's value is an int, a String key's a
// string and a Bool key's a bool: in its default, in what Validate receives
// and in what Config returns for it.
const (
	String Type = iota + 1
	Int
	Bool
)

func (t Type) String() string {
	switch t {
	case String:
		return "a string"
	case Int:
		return "an integer"
	case Bool:
		return "a boolean"
	}
	return fmt.Sprintf("type %d", int(t))
}

// Key describes one configuration key a package owns.
type Key struct {
	// Section is the name of the TOML table the key lives in.
	Section string
	// Name is the key's name within its section.
	Name string
	// Type is the TOML type the file must give the key.
	Type Type
	// Default is the value when the file does not set the key; it must be of
	// Type (any integer type for Int).
	Default any
	// Validate, when set, checks a value the file sets; its error fails the
	// load. It is not applied to Default.
	Validate func(any) error
}

func (k Key) id() string { return k.Section + "." + k.Name }

// oauthSection is the one section whose shape the registry does not describe:
// it holds the repeatable [[oauth.providers]] table.
const oauthSection = "oauth"

// Registry holds the keys and the whole sections a load accepts. They are
// registered before the load, from one goroutine; the strictness of the file
// is decided here, not by struct tags, because every package adds keys of its
// own.
type Registry struct {
	keys map[string]map[string]Key
	// sections holds the validator of each section registered as a whole.
	sections map[string]func(map[string]any) error
	// errs remembers refused registrations: a package that registers at
	// start-up may ignore Register's error, and the load must still fail.
	errs []error
}

// NewRegistry returns a registry holding the keys the configuration loader
// owns: those of [server], [database], [workspace], [logging] and [limits].
func NewRegistry() *Registry {
	r := &Registry{keys: map[string]map[string]Key{}, sections: map[string]func(map[string]any) error{}}
	for _, k := range ownKeys {
		// A refusal is remembered and fails the load; the loader's own keys
		// are fixed and well formed, so there is none.
		_ = r.Register(k)
	}
	return r
}

// Register adds a key. A key whose section.key is already registered, or a
// key that is not well formed, is refused: Register returns the error and the
// next Load fails with it.
func (r *Registry) Register(k Key) error {
	err := r.register(k)
	if err != nil {
		r.errs = append(r.errs, err)
	}
	return err
}

func (r *Registry) register(k Key) error {
	switch {
	case k.Section == "" || k.Name == "":
		return fmt.Errorf("configuration key %q: section and name must not be empty", k.id())
	case k.Section == oauthSection:
		return fmt.Errorf("configuration key %s: section [%s] is owned by the configuration loader", k.id(), oauthSection)
	case r.sections[k.Section] != nil:
		return fmt.Errorf("configuration key %s: section [%s] is registered as a whole", k.id(), k.Section)
	case r.keys[k.Section][k.Name].Type != 0:
		return fmt.Errorf("configuration key %s is registered twice", k.id())
	}
	def, err := coerce(k.Type, k.Default)
	if err != nil {
		return fmt.Errorf("configuration key %s: default: %w", k.id(), err)
	}
	k.Default = def
	if r.keys[k.Section] == nil {
		r.keys[k.Section] = map[string]Key{}
	}
	r.keys[k.Section][k.Name] = k
	return nil
}

// RegisterSection makes a package the owner of a whole section: the load does
// not check the section's keys against the registry and, when the file holds
// the section, hands validate its parsed content once, with the environment
// references in its string values replaced at any depth. Tables are
// map[string]any, arrays of tables []map[string]any and integers int64, as
// the TOML parser gives them. validate's error fails the load with its
// message unchanged, so it can list every failure of the section together.
//
// A section is registered either key by key or as a whole, never both: a
// section registered twice, or one in which a key is registered, is refused;
// RegisterSection returns the error and the next Load fails with it.
func (r *Registry) RegisterSection(name string, validate func(map[string]any) error) error {
	err := r.registerSection(name, validate)
	if err != nil {
		r.errs = append(r.errs, err)
	}
	return err
}

func (r *Registry) registerSection(name string, validate func(map[string]any) error) error {
	switch {
	case name == "":
		return errors.New("configuration section: name must not be empty")
	case validate == nil:
		return fmt.Errorf("configuration section [%s]: validator must not be nil", name)
	case name == oauthSection:
		return fmt.Errorf("configuration section [%s] is owned by the configuration loader", name)
	case r.sections[name] != nil:
		return fmt.Errorf("configuration section [%s] is registered twice", name)
	case len(r.keys[name]) > 0:
		return fmt.Errorf("configuration section [%s] cannot be registered as a whole: keys are registered in it", name)
	}
	r.sections[name] = validate
	return nil
}

// coerce returns v as the Go value of t, or an error when v is not of t.
func coerce(t Type, v any) (any, error) {
	switch t {
	case String:
		if s, ok := v.(string); ok {
			return s, nil
		}
	case Int:
		var n int64
		switch x := v.(type) {
		case int:
			return x, nil
		case int64:
			n = x
		case int32:
			n = int64(x)
		case int16:
			n = int64(x)
		case int8:
			n = int64(x)
		default:
			return nil, fmt.Errorf("must be %s", t)
		}
		if n < math.MinInt || n > math.MaxInt {
			return nil, fmt.Errorf("must be %s that fits in %d bits", t, strconv.IntSize)
		}
		return int(n), nil
	case Bool:
		if b, ok := v.(bool); ok {
			return b, nil
		}
	default:
		return nil, fmt.Errorf("unknown key type %d", int(t))
	}
	return nil, fmt.Errorf("must be %s", t)
}

// The loader's own keys. database.path and workspace.path default to empty,
// which stands for "not set": their defaults depend on the environment and
// are resolved after the load.
var ownKeys = []Key{
	{Section: "server", Name: "bind", Type: String, Default: "0.0.0.0", Validate: nonEmpty},
	{Section: "server", Name: "port", Type: Int, Default: 8080, Validate: intBetween(0, 65535)},
	{Section: "server", Name: "external_url", Type: String, Default: "", Validate: emptyOrHTTPURL},
	{Section: "server", Name: "max_body_size", Type: String, Default: "1MB", Validate: func(v any) error {
		_, err := parseSize(v.(string))
		return err
	}},
	{Section: "database", Name: "path", Type: String, Default: "", Validate: nonEmpty},
	{Section: "workspace", Name: "path", Type: String, Default: "", Validate: nonEmpty},
	{Section: "workspace", Name: "workers", Type: Int, Default: 4, Validate: intBetween(1, math.MaxInt)},
	{Section: "logging", Name: "level", Type: String, Default: "info", Validate: func(v any) error {
		switch strings.ToLower(v.(string)) {
		case "trace", "debug", "info", "warn", "error":
			return nil
		}
		return errors.New("must be one of trace, debug, info, warn and error")
	}},
	{Section: "logging", Name: "log_health_probes", Type: Bool, Default: false},
	{Section: "limits", Name: "requests_per_minute", Type: Int, Default: 600, Validate: intBetween(0, math.MaxInt)},
}

func nonEmpty(v any) error {
	if v.(string) == "" {
		return errors.New("must not be empty")
	}
	return nil
}

func intBetween(lo, hi int) func(any) error {
	return func(v any) error {
		n := v.(int)
		switch {
		case n < lo:
			return fmt.Errorf("must be at least %d", lo)
		case n > hi:
			return fmt.Errorf("must be at most %d", hi)
		}
		return nil
	}
}

func emptyOrHTTPURL(v any) error {
	if v.(string) == "" {
		return nil
	}
	return httpURL(v.(string))
}

// httpURL accepts an absolute http or https URL with a host.
func httpURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("must be an absolute http or https URL")
	}
	return nil
}

var sizePattern = regexp.MustCompile(`^([0-9]+)(kb|mb|gb)$`)

// parseSize reads a positive integer followed by KB, MB or GB, in any letter
// case and in powers of 1024, as a number of bytes.
func parseSize(s string) (int64, error) {
	bad := errors.New("must be a positive integer followed by KB, MB or GB, such as 1MB")
	m := sizePattern.FindStringSubmatch(strings.ToLower(s))
	if m == nil {
		return 0, bad
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || n <= 0 {
		return 0, bad
	}
	unit := map[string]int64{"kb": 1 << 10, "mb": 1 << 20, "gb": 1 << 30}[m[2]]
	if n > math.MaxInt64/unit {
		return 0, errors.New("is too large")
	}
	return n * unit, nil
}
