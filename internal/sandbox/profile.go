package sandbox

import (
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Profile is one [[sandbox.profiles]] table, the operator's template for a
// sandbox, with its defaults filled in.
type Profile struct {
	// Name matches namePattern and is unique among the profiles.
	Name string
	// Default reports whether this is the profile a sandbox gets when its
	// creator names none. At most one profile sets it.
	Default     bool
	Description string
	// Image, CPU and Memory are "" when the profile leaves them to the
	// runtime.
	Image  string
	CPU    string
	Memory string
	GPU    int
	// TTL is more than 0 and at most the section's MaxTTL.
	TTL time.Duration
	// IdleTimeout is the section's IdleTimeout when the profile sets none;
	// 0 disables the idle stop.
	IdleTimeout time.Duration
	// PolicyPath is the absolute path of the profile's base policy file, a
	// relative path in the file taken from the config dir; "" for the
	// built-in policy.
	PolicyPath string

	// Credentials and OptionalCredentials are names of
	// [[sandbox.credentials]] tables; no name is in both.
	Credentials         []string
	OptionalCredentials []string
	// ExposeSecrets and OptionalExposeSecrets are names of secrets whose
	// values the sandbox's environment gets. Only their names are checked
	// here; whether the secrets exist is not.
	ExposeSecrets         []string
	OptionalExposeSecrets []string

	// HubAccess is "fork" or "none".
	HubAccess string
	// Env is the environment the sandbox gets. Its values may be secret:
	// no error or answer of the hub quotes them.
	Env            map[string]string
	Checkpoint     bool
	AllowProposals bool

	// PolicyHosts are the hosts the base policy allows, as the runtime
	// reported them when it validated PolicyPath. The start-up check sets
	// them once, before the hub serves; nil for the built-in policy.
	PolicyHosts []string
}

// clone returns p with copies of its lists and its env, so that the caller
// cannot change the loaded profile.
func (p Profile) clone() Profile {
	p.Credentials = slices.Clone(p.Credentials)
	p.OptionalCredentials = slices.Clone(p.OptionalCredentials)
	p.ExposeSecrets = slices.Clone(p.ExposeSecrets)
	p.OptionalExposeSecrets = slices.Clone(p.OptionalExposeSecrets)
	p.Env = maps.Clone(p.Env)
	p.PolicyHosts = slices.Clone(p.PolicyHosts)
	return p
}

// Credential is one [[sandbox.credentials]] table: a hub secret a profile may
// attach to its sandboxes as an OpenShell provider.
type Credential struct {
	// Name matches namePattern and is unique among the credentials.
	Name string
	// ProviderProfile is the id of the OpenShell provider profile that says
	// how the runtime may use the credential.
	ProviderProfile string
	// Secret is the name of the hub secret that holds the credential's
	// value, never the value itself.
	Secret string
}

// Profile returns the profile called name; ok is false when there is none.
func (s *Settings) Profile(name string) (p Profile, ok bool) {
	for _, p := range s.profiles {
		if p.Name == name {
			return p.clone(), true
		}
	}
	return Profile{}, false
}

// DefaultProfile returns the profile that sets default; ok is false when no
// profile does.
func (s *Settings) DefaultProfile() (p Profile, ok bool) {
	for _, p := range s.profiles {
		if p.Default {
			return p.clone(), true
		}
	}
	return Profile{}, false
}

// Profiles returns every profile in name order; empty when sandboxes are
// off.
func (s *Settings) Profiles() []Profile {
	out := make([]Profile, len(s.profiles))
	for i, p := range s.profiles {
		out[i] = p.clone()
	}
	return out
}

// Credential returns the configured credential called name; ok is false when
// there is none.
func (s *Settings) Credential(name string) (c Credential, ok bool) {
	for _, c := range s.credentials {
		if c.Name == name {
			return c, true
		}
	}
	return Credential{}, false
}

// Credentials returns every configured credential in name order.
func (s *Settings) Credentials() []Credential {
	return slices.Clone(s.credentials)
}

// The keys a [[sandbox.profiles]] and a [[sandbox.credentials]] table may
// hold.
var (
	profileKeys = []string{
		"name", "default", "description", "image", "cpu", "memory", "gpu", "ttl", "idle_timeout",
		"policy", "credentials", "optional_credentials", "expose_secrets", "optional_expose_secrets",
		"hub_access", "env", "checkpoint", "allow_proposals",
	}
	credentialKeys = []string{"name", "provider_profile", "secret"}
)

// variablePattern is the shape of an env key and of an exposed secret's
// name: the name of an environment variable.
var variablePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// reservedPrefixes start the names of the variables the hub and the runtime
// set in a sandbox themselves.
var reservedPrefixes = []string{"PARLEY_", "OPENSHELL_"}

// reservedSecrets are the secrets of the hub's own git and forge access,
// which no profile may expose, whatever their letter case.
var reservedSecrets = []string{
	"GIT_PAT", "GIT_USERNAME", "GIT_PASSWORD",
	"UPSTREAM_GIT_PAT", "UPSTREAM_GIT_USERNAME", "UPSTREAM_GIT_PASSWORD",
	"FORGE_WEBHOOK_SECRET",
}

// profilesAndCredentials reads the profiles and the credentials of the
// section into s, in name order, and checks them, the profiles' references
// to credentials last. maxTTLOK is false when the section's max_ttl was
// refused, so that no profile's ttl is measured against its default.
func (d *decoder) profilesAndCredentials(s *Settings, maxTTLOK bool) {
	before := len(d.failures)
	profiles := d.entries("profiles", profileKeys)
	if len(profiles) == 0 && len(d.failures) == before {
		d.fail("profiles", "at least one [["+SectionName+".profiles]] table is required")
	}
	type reference struct {
		prefix, key string
		names       []string
	}
	var references []reference
	names, hasDefault := map[string]bool{}, false
	for _, e := range profiles {
		c := d.child(SectionName+".profiles["+e.label+"].", e.table)
		p := c.profile(s, maxTTLOK, names, hasDefault)
		hasDefault = hasDefault || p.Default
		d.failures = append(d.failures, c.failures...)
		s.profiles = append(s.profiles, p)
		references = append(references,
			reference{c.prefix, "credentials", p.Credentials},
			reference{c.prefix, "optional_credentials", p.OptionalCredentials})
	}

	names = map[string]bool{}
	for _, e := range d.entries("credentials", credentialKeys) {
		c := d.child(SectionName+".credentials["+e.label+"].", e.table)
		cr := Credential{Name: c.name("credential", names)}
		cr.ProviderProfile = c.required("provider_profile")
		cr.Secret = c.required("secret")
		d.failures = append(d.failures, c.failures...)
		s.credentials = append(s.credentials, cr)
	}

	for _, r := range references {
		for _, n := range r.names {
			if !names[n] {
				d.failures = append(d.failures, fmt.Sprintf("%s%s: %q is not the name of a [[%s.credentials]] table", r.prefix, r.key, n, SectionName))
			}
		}
	}
	slices.SortStableFunc(s.profiles, func(a, b Profile) int { return strings.Compare(a.Name, b.Name) })
	slices.SortStableFunc(s.credentials, func(a, b Credential) int { return strings.Compare(a.Name, b.Name) })
}

// profile reads one profile table. names holds the names of the profiles
// before it, and gains this one's; hasDefault reports whether one of them is
// the default.
func (c *decoder) profile(s *Settings, maxTTLOK bool, names map[string]bool, hasDefault bool) Profile {
	p := Profile{Name: c.name("profile", names)}
	p.Default = c.boolean("default", false)
	if p.Default && hasDefault {
		c.fail("default", "another profile is already the default")
	}
	p.Description, _ = c.str("description", "")
	p.Image, _ = c.str("image", "")
	p.CPU, _ = c.str("cpu", "")
	p.Memory, _ = c.str("memory", "")
	p.GPU = c.nonNegative("gpu")

	if _, set := c.value("ttl"); !set {
		c.fail("ttl", "is required")
	} else {
		before := len(c.failures)
		p.TTL = c.positiveDuration("ttl", 0)
		if len(c.failures) == before && maxTTLOK && p.TTL > s.MaxTTL {
			c.fail("ttl", "must be at most sandbox.max_ttl")
		}
	}
	// The duration format has no sign, so an idle_timeout below 0 is refused
	// as one that is not a duration.
	p.IdleTimeout = c.duration("idle_timeout", s.IdleTimeout)
	policy, _ := c.file("policy")
	p.PolicyPath = policy.path

	p.Credentials = c.strings("credentials")
	p.OptionalCredentials = c.strings("optional_credentials")
	for _, n := range p.OptionalCredentials {
		if slices.Contains(p.Credentials, n) {
			c.fail("optional_credentials", fmt.Sprintf("%q is also in %scredentials", n, c.prefix))
		}
	}

	p.HubAccess, _ = c.str("hub_access", "fork")
	if p.HubAccess != "fork" && p.HubAccess != "none" {
		c.fail("hub_access", "must be fork or none")
	}
	p.Env = c.env("env")
	p.Checkpoint = c.boolean("checkpoint", true)
	p.AllowProposals = c.boolean("allow_proposals", true)

	p.ExposeSecrets = c.strings("expose_secrets")
	p.OptionalExposeSecrets = c.strings("optional_expose_secrets")
	c.exposedNames("expose_secrets", p.ExposeSecrets, p.Env)
	c.exposedNames("optional_expose_secrets", p.OptionalExposeSecrets, p.Env)
	for _, n := range p.OptionalExposeSecrets {
		if slices.Contains(p.ExposeSecrets, n) {
			c.fail("optional_expose_secrets", fmt.Sprintf("name %q is also in %sexpose_secrets", n, c.prefix))
		}
	}
	return p
}

// name reads the name of a profile or a credential (what) and checks that it
// is a valid name no table in names has; a valid name joins names.
func (c *decoder) name(what string, names map[string]bool) string {
	v, set := c.value("name")
	n, isString := v.(string)
	switch {
	case !set || isString && n == "":
		c.fail("name", "is required")
	case !isString:
		c.fail("name", "must be a string")
	case !namePattern.MatchString(n):
		c.fail("name", namePatternReason)
	case names[n]:
		c.fail("name", "another "+what+" has this name")
	default:
		names[n] = true
	}
	return n
}

// required reads a string key that must be set and not be empty.
func (c *decoder) required(key string) string {
	v, ok := c.str(key, "")
	if ok && v == "" {
		c.fail(key, "is required")
	}
	return v
}

// nonNegative reads an integer key that must be 0 or more; 0 when the file
// does not set it or sets a value that is refused.
func (c *decoder) nonNegative(key string) int {
	v, set := c.value(key)
	if !set {
		return 0
	}
	n, ok := v.(int64)
	switch {
	case !ok:
		c.fail(key, "must be an integer")
	case n < 0:
		c.fail(key, "must be 0 or more")
	case n > math.MaxInt:
		c.fail(key, "is too large")
	default:
		return int(n)
	}
	return 0
}

// strings reads a list of strings; empty, not nil, when the file does not
// set it or sets a value that is refused.
func (c *decoder) strings(key string) []string {
	out := []string{}
	v, set := c.value(key)
	if !set {
		return out
	}
	list, ok := v.([]any)
	for _, e := range list {
		s, isString := e.(string)
		if !isString {
			ok = false
			break
		}
		out = append(out, s)
	}
	if !ok {
		c.fail(key, "must be a list of strings")
		return []string{}
	}
	return out
}

// env reads a table of environment variables, keeping the entries whose key
// is valid. A failure names the key and never quotes its value, which may be
// a secret a ${NAME} reference brought in.
func (c *decoder) env(key string) map[string]string {
	out := map[string]string{}
	v, set := c.value(key)
	if !set {
		return out
	}
	table, ok := v.(map[string]any)
	if !ok {
		c.fail(key, "must be a table of strings")
		return out
	}
	for _, k := range slices.Sorted(maps.Keys(table)) {
		value, isString := table[k].(string)
		switch {
		case !variablePattern.MatchString(k):
			c.fail(key, fmt.Sprintf("key %q must match %s", k, variablePattern))
		case reservedPrefix(k):
			c.fail(key, fmt.Sprintf("key %q must not start with PARLEY_ or OPENSHELL_", k))
		case !isString:
			c.fail(key, fmt.Sprintf("key %q must have a string value", k))
		default:
			out[k] = value
		}
	}
	return out
}

// exposedNames checks the names of the list key against the name rules of an
// exposed secret: an environment variable's name, outside the reserved
// prefixes and secret names, and not a key of env. It does not look any
// secret up.
func (c *decoder) exposedNames(key string, names []string, env map[string]string) {
	for _, n := range names {
		reason := ""
		_, inEnv := env[n]
		switch {
		case !variablePattern.MatchString(n):
			reason = "must match " + variablePattern.String()
		case reservedPrefix(n):
			reason = "must not start with PARLEY_ or OPENSHELL_"
		case slices.ContainsFunc(reservedSecrets, func(r string) bool { return strings.EqualFold(r, n) }):
			reason = "is a reserved secret name"
		case inEnv:
			reason = "is also a key of " + c.prefix + "env"
		default:
			continue
		}
		c.fail(key, fmt.Sprintf("name %q %s", n, reason))
	}
}

func reservedPrefix(name string) bool {
	return slices.ContainsFunc(reservedPrefixes, func(p string) bool { return strings.HasPrefix(name, p) })
}
