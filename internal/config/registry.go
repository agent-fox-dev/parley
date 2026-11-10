package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Type is the type of a configuration value.
type Type int

// The value types a key can have.
const (
	String Type = iota
	Int
	Bool
)

func (t Type) String() string {
	switch t {
	case Int:
		return "an integer"
	case Bool:
		return "a boolean"
	}
	return "a string"
}

// Key is a configuration key a package owns: its section, its name, its
// type, its default and an optional rule that refuses a value set in the
// file. Values reach Validate as string, int or bool.
type Key struct {
	Section  string
	Name     string
	Type     Type
	Default  any
	Validate func(any) error
}

func (k Key) id() string { return k.Section + "." + k.Name }

// Registry holds the configuration keys every package registered. A key in
// the file that nobody registered is refused by Load.
type Registry struct {
	keys     map[string]*Key
	order    []string
	sections map[string]bool
	whole    map[string]SectionValidator
	errs     []error
}

// NewRegistry returns a registry holding the keys this package owns.
func NewRegistry() *Registry {
	r := &Registry{keys: map[string]*Key{}, sections: map[string]bool{}, whole: map[string]SectionValidator{}}
	r.registerOwn()
	return r
}

// Register adds a key. Registering a section.key a second time is
// remembered and fails the next Load with an error naming it.
func (r *Registry) Register(k Key) {
	id := k.id()
	if r.whole[k.Section] != nil {
		r.errs = append(r.errs, fmt.Errorf("configuration key %s is in section [%s], which is registered as a whole", id, k.Section))
		return
	}
	if _, dup := r.keys[id]; dup {
		r.errs = append(r.errs, fmt.Errorf("configuration key %s is registered twice", id))
		return
	}
	kk := k
	r.keys[id] = &kk
	r.order = append(r.order, id)
	r.sections[k.Section] = true
}

// The keys this package owns.
func (r *Registry) registerOwn() {
	r.Register(Key{Section: "server", Name: "bind", Type: String, Default: "0.0.0.0", Validate: nonEmpty})
	r.Register(Key{Section: "server", Name: "port", Type: Int, Default: 8080, Validate: between(0, 65535)})
	r.Register(Key{Section: "server", Name: "external_url", Type: String, Default: "", Validate: emptyOrHTTPURL})
	r.Register(Key{Section: "server", Name: "max_body_size", Type: String, Default: "1MB", Validate: validSize})
	r.Register(Key{Section: "database", Name: "path", Type: String, Default: "", Validate: nonEmpty})
	r.Register(Key{Section: "workspace", Name: "path", Type: String, Default: "", Validate: nonEmpty})
	r.Register(Key{Section: "workspace", Name: "workers", Type: Int, Default: 4, Validate: atLeast(1)})
	r.Register(Key{Section: "logging", Name: "level", Type: String, Default: "info", Validate: validLevel})
	r.Register(Key{Section: "logging", Name: "log_health_probes", Type: Bool, Default: false})
	r.Register(Key{Section: "limits", Name: "requests_per_minute", Type: Int, Default: 600, Validate: atLeast(0)})
}

func nonEmpty(v any) error {
	if v.(string) == "" {
		return errors.New("must not be empty")
	}
	return nil
}

func between(lo, hi int) func(any) error {
	return func(v any) error {
		if n := v.(int); n < lo || n > hi {
			return fmt.Errorf("must be from %d to %d", lo, hi)
		}
		return nil
	}
}

func atLeast(lo int) func(any) error {
	return func(v any) error {
		if v.(int) < lo {
			return fmt.Errorf("must be at least %d", lo)
		}
		return nil
	}
}

// httpURL reports whether s is an absolute http or https URL.
func httpURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func emptyOrHTTPURL(v any) error {
	if s := v.(string); s != "" && !httpURL(s) {
		return errors.New("must be empty or an absolute http or https URL")
	}
	return nil
}

var sizePattern = regexp.MustCompile(`(?i)^([0-9]+)(KB|MB|GB)$`)

// ParseSize reads a size such as 1MB: a positive integer followed by KB,
// MB or GB in any letter case, in powers of 1024.
func ParseSize(s string) (int64, error) {
	m := sizePattern.FindStringSubmatch(s)
	if m == nil {
		return 0, errors.New("must be a positive integer followed by KB, MB or GB")
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || n <= 0 {
		return 0, errors.New("must be a positive integer followed by KB, MB or GB")
	}
	shift := map[string]uint{"KB": 10, "MB": 20, "GB": 30}[strings.ToUpper(m[2])]
	if n > (1<<62)>>shift {
		return 0, errors.New("is too large")
	}
	return n << shift, nil
}

func validSize(v any) error {
	_, err := ParseSize(v.(string))
	return err
}

var levels = map[string]bool{"trace": true, "debug": true, "info": true, "warn": true, "error": true}

func validLevel(v any) error {
	if !levels[strings.ToLower(v.(string))] {
		return errors.New("must be trace, debug, info, warn or error")
	}
	return nil
}

// SectionValidator checks a whole section a package owns. It receives the
// section's parsed content (keys, tables and arrays of tables) after the
// environment references in its string values are replaced, and returns
// one error that reports every failure of the section together.
type SectionValidator func(section map[string]any) error

// RegisterSection registers a whole section with its validator: the loader
// does not check the section's keys, and hands the section to the validator
// once when the file holds it. A section is registered either key by key or
// as a whole; a conflicting registration fails the next Load with an error
// naming the section.
func (r *Registry) RegisterSection(name string, v SectionValidator) {
	switch {
	case r.whole[name] != nil:
		r.errs = append(r.errs, fmt.Errorf("configuration section [%s] is registered twice", name))
	case r.sections[name]:
		r.errs = append(r.errs, fmt.Errorf("configuration section [%s] has registered keys and cannot be registered as a whole", name))
	default:
		r.whole[name] = v
	}
}
