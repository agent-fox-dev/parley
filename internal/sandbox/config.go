// Package sandbox holds the hub's side of the sandbox feature: the [sandbox]
// section of the configuration, which turns the feature on, and its
// validation; the start-up check of the runtime and the supported release
// range; and Service, the feature of one hub, which the parts composed after
// it read.
package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// SectionName is the configuration section the sandbox feature owns. The
// file holding it, even empty, turns sandboxes on.
const SectionName = "sandbox"

// Register makes the sandbox feature the owner of the whole [sandbox]
// section of r. The validator it registers accepts any content: the section
// is checked by FromConfig after the load, because its rules need the config
// dir and [server].external_url, which a section validator does not see.
// The hub calls FromConfig right after the load, before any store is opened.
func Register(r *config.Registry) error {
	return r.RegisterSection(SectionName, func(map[string]any) error { return nil })
}

// Settings is the loaded [sandbox] section with every default filled in. It
// is read once at start and must not be changed afterwards.
type Settings struct {
	// Enabled reports whether the file holds a [sandbox] section. When it is
	// false every other field is its zero value.
	Enabled bool

	// Runtime is the runtime the sandboxes run on: openshell.
	Runtime string
	// GatewayURL is the URL of the OpenShell gateway, as the file gives it.
	GatewayURL string

	// TLSCert, TLSKey and TLSCA are the absolute paths of the client
	// certificate, its key and the CA bundle; empty when not set. Relative
	// paths in the file are taken from the config dir.
	TLSCert string
	TLSKey  string
	TLSCA   string
	// TokenFile is the absolute path of the file holding the bearer token;
	// empty when not set. Its content is read by FromConfig: see Token.
	TokenFile string
	// OIDCIssuer, OIDCClientID and OIDCClientSecretFile configure the OIDC
	// client-credentials grant; empty when not set. The secret file's path
	// is absolute, and its content is read by FromConfig: see
	// OIDCClientSecret.
	OIDCIssuer           string
	OIDCClientID         string
	OIDCClientSecretFile string

	// OpenShellWorkspace is the OpenShell workspace the hub owns.
	OpenShellWorkspace string

	MaxTTL      time.Duration
	IdleTimeout time.Duration // 0 disables the idle stop

	MaxSandboxes    int
	MaxPerOrg       int
	MaxPerWorkspace int
	MaxPerUser      int

	AllowWorkspaceRules bool

	MaxExecTimeout     time.Duration
	MaxConcurrentExecs int
	// MaxTransferSize is in bytes.
	MaxTransferSize int64
	LogRetention    time.Duration
	// ReconcileInterval is between 10 seconds and 10 minutes.
	ReconcileInterval time.Duration

	// profiles are the [[sandbox.profiles]] tables and credentials the
	// [[sandbox.credentials]] tables, each in name order. They are unexported
	// so that they cannot change after the load: the lookups of profile.go
	// return copies.
	profiles    []Profile
	credentials []Credential

	// token and oidcClientSecret are the contents of the two secret files.
	// They are unexported so that they are read through Token and
	// OIDCClientSecret only and encoding/json leaves them out; fmt still
	// prints unexported fields, so Settings must not be formatted with %v.
	token            string
	oidcClientSecret string
}

// Token returns the content of token_file without surrounding white space,
// the bearer token for the life of the process; empty when token_file is not
// set.
func (s *Settings) Token() string { return s.token }

// OIDCClientSecret returns the content of oidc_client_secret_file without
// surrounding white space; empty when it is not set.
func (s *Settings) OIDCClientSecret() string { return s.oidcClientSecret }

// namePattern is the shape of the OpenShell workspace, a profile name and a
// credential name, and namePatternReason the failure of a value without it.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

const namePatternReason = "must be 1 to 63 lower-case letters, digits, _ or -, starting with a letter or a digit"

// FromConfig checks the [sandbox] section of a configuration loaded with
// the section registered (see Register) and returns its settings. Without
// the section it returns settings with Enabled false and checks nothing.
//
// Every failure is collected and returned together as one error, the
// failures separated by "; ", each naming its key as sandbox.<key>,
// sandbox.profiles[<name>].<key>, sandbox.credentials[<name>].<key> (the
// zero-based index in place of a name that is missing or not a valid name)
// or server.external_url. They come in this order: the keys of the section
// in the order of the configuration reference, the ways to authenticate, the
// profiles, the credentials, the profiles' references to credentials, the
// section's unknown keys, then server.external_url, so that the first ones
// survive when the error line is cut. A failure quotes no value but a
// configured name (an env key, a name a list holds, a credential's name),
// so that no message holds a secret, the content of a file, an env value
// or the user information of a URL.
func FromConfig(cfg *config.Config) (*Settings, error) {
	table, present := cfg.Section(SectionName)
	if !present {
		return &Settings{}, nil
	}
	d := &decoder{table: table, prefix: SectionName + ".", dir: cfg.Dir, known: map[string]bool{}}
	s := &Settings{Enabled: true}

	if rt, ok := d.str("runtime", "openshell"); ok && rt != "openshell" {
		d.fail("runtime", "must be openshell")
	}
	s.Runtime = "openshell"
	if gw, ok := d.str("gateway_url", ""); ok && gw == "" {
		d.fail("gateway_url", "is required")
	} else if ok {
		d.checkURL("gateway_url", gw, false)
		s.GatewayURL = gw
	}
	d.authentication(s)

	s.OpenShellWorkspace, _ = d.str("openshell_workspace", "parley")
	if !namePattern.MatchString(s.OpenShellWorkspace) {
		d.fail("openshell_workspace", namePatternReason)
	}
	maxTTLFailures := len(d.failures)
	s.MaxTTL = d.positiveDuration("max_ttl", 24*time.Hour)
	maxTTLOK := len(d.failures) == maxTTLFailures
	s.IdleTimeout = d.duration("idle_timeout", 30*time.Minute)
	maxSandboxes, maxOK := d.count("max_sandboxes", 200)
	s.MaxSandboxes = maxSandboxes
	if _, set := d.table["max_per_org"]; set {
		perOrg, ok := d.count("max_per_org", 100)
		if ok && maxOK && perOrg > maxSandboxes {
			d.fail("max_per_org", "must be at most sandbox.max_sandboxes")
		}
		s.MaxPerOrg = perOrg
	} else {
		d.known["max_per_org"] = true
		s.MaxPerOrg = min(100, maxSandboxes)
	}
	s.MaxPerWorkspace, _ = d.count("max_per_workspace", 50)
	s.MaxPerUser, _ = d.count("max_per_user", 20)
	s.AllowWorkspaceRules = d.boolean("allow_workspace_rules", true)
	s.MaxExecTimeout = d.positiveDuration("max_exec_timeout", 30*time.Minute)
	s.MaxConcurrentExecs, _ = d.count("max_concurrent_execs", 8)
	s.MaxTransferSize = d.size("max_transfer_size", 1<<30)
	s.LogRetention = d.positiveDuration("log_retention", 7*24*time.Hour)
	s.ReconcileInterval = d.duration("reconcile_interval", time.Minute)
	if s.ReconcileInterval < 10*time.Second || s.ReconcileInterval > 10*time.Minute {
		d.fail("reconcile_interval", "must be from 10s to 10m")
	}

	d.profilesAndCredentials(s, maxTTLOK)

	for _, key := range slices.Sorted(maps.Keys(d.table)) {
		if !d.known[key] {
			d.fail(key, "unknown key")
		}
	}
	if cfg.Server.ExternalURL == "" {
		d.failures = append(d.failures, "server.external_url: must be set when the [sandbox] section is present")
	}

	if len(d.failures) > 0 {
		return nil, fmt.Errorf("configuration file %s: %s", cfg.Path, strings.Join(d.failures, "; "))
	}
	return s, nil
}

// authentication reads the TLS, token and OIDC keys into s and checks that
// exactly one way to authenticate is configured.
func (d *decoder) authentication(s *Settings) {
	cert, certSet := d.file("tls_cert")
	key, keySet := d.file("tls_key")
	ca, _ := d.file("tls_ca")
	s.TLSCert, s.TLSKey, s.TLSCA = cert.path, key.path, ca.path

	token, tokenSet := d.file("token_file")
	s.TokenFile = token.path
	if token.read {
		s.token = strings.TrimSpace(string(token.content))
		if s.token == "" {
			d.fail("token_file", "must not be empty")
		}
	}

	issuer, issuerSet := d.str("oidc_issuer", "")
	if issuerSet && issuer != "" {
		d.checkURL("oidc_issuer", issuer, true)
		s.OIDCIssuer = issuer
	}
	clientID, clientIDSet := d.str("oidc_client_id", "")
	s.OIDCClientID = clientID
	secret, secretSet := d.file("oidc_client_secret_file")
	s.OIDCClientSecretFile = secret.path
	if secret.read {
		s.oidcClientSecret = strings.TrimSpace(string(secret.content))
		if s.oidcClientSecret == "" {
			d.fail("oidc_client_secret_file", "must not be empty")
		}
	}
	issuerSet = issuerSet && issuer != ""
	clientIDSet = clientIDSet && clientID != ""

	incomplete := false
	if certSet != keySet {
		incomplete = true
		missing, given := "tls_key", "tls_cert"
		if keySet {
			missing, given = given, missing
		}
		d.fail(missing, "must be set with sandbox."+given+": mutual TLS needs tls_cert and tls_key")
	}
	oidc := map[string]bool{"oidc_issuer": issuerSet, "oidc_client_id": clientIDSet, "oidc_client_secret_file": secretSet}
	if n := countTrue(issuerSet, clientIDSet, secretSet); n > 0 && n < 3 {
		incomplete = true
		for _, k := range []string{"oidc_issuer", "oidc_client_id", "oidc_client_secret_file"} {
			if !oidc[k] {
				d.fail(k, "must be set: OIDC client credentials need oidc_issuer, oidc_client_id and oidc_client_secret_file")
			}
		}
	}

	var ways []string
	if certSet && keySet {
		ways = append(ways, "mutual TLS")
	}
	if tokenSet {
		ways = append(ways, "token_file")
	}
	if issuerSet && clientIDSet && secretSet {
		ways = append(ways, "OIDC client credentials")
	}
	switch {
	case len(ways) > 1:
		d.fail("gateway_url", "configure exactly one way to authenticate, found: "+strings.Join(ways, ", "))
	case len(ways) == 0 && !incomplete:
		d.fail("gateway_url", "configure exactly one way to authenticate, found: none")
	}
}

func countTrue(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

// decoder reads the keys of one table and collects its failures.
type decoder struct {
	table map[string]any
	// prefix comes before a key in a failure: "sandbox." for the section,
	// "sandbox.profiles[<name>]." for one of its profiles.
	prefix string
	// dir is the config dir, against which relative paths are resolved.
	dir string
	// known holds every key that was read, so that the rest are unknown.
	known    map[string]bool
	failures []string
}

func (d *decoder) fail(key, reason string) {
	d.failures = append(d.failures, d.prefix+key+": "+reason)
}

// child returns a decoder for the table of one entry of a list of tables,
// its failures prefixed with prefix. Its failures are the caller's to take.
func (d *decoder) child(prefix string, table map[string]any) *decoder {
	return &decoder{table: table, prefix: prefix, dir: d.dir, known: map[string]bool{}}
}

// value returns the file's value of key and whether the file sets it.
func (d *decoder) value(key string) (any, bool) {
	d.known[key] = true
	v, set := d.table[key]
	return v, set
}

// str returns the key's string, def when the file does not set it. ok is
// false when the value is not a string.
func (d *decoder) str(key, def string) (string, bool) {
	v, set := d.value(key)
	if !set {
		return def, true
	}
	s, ok := v.(string)
	if !ok {
		d.fail(key, "must be a string")
		return def, false
	}
	return s, true
}

func (d *decoder) boolean(key string, def bool) bool {
	v, set := d.value(key)
	if !set {
		return def
	}
	b, ok := v.(bool)
	if !ok {
		d.fail(key, "must be a boolean")
		return def
	}
	return b
}

// count returns the key's integer, def when the file does not set it; it
// must be at least 1. ok is false when the value is refused.
func (d *decoder) count(key string, def int) (int, bool) {
	v, set := d.value(key)
	if !set {
		return def, true
	}
	n, ok := v.(int64)
	switch {
	case !ok:
		d.fail(key, "must be an integer")
		return def, false
	case n < 1:
		d.fail(key, "must be at least 1")
		return def, false
	case n > math.MaxInt:
		d.fail(key, "is too large")
		return def, false
	}
	return int(n), true
}

// duration returns the key's duration in the hub's duration format, def when
// the file does not set it or sets a value that is not one.
func (d *decoder) duration(key string, def time.Duration) time.Duration {
	s, ok := d.str(key, "")
	if _, set := d.table[key]; !set || !ok {
		return def
	}
	// The parser's error quotes the value; no message here does.
	v, err := timefmt.ParseDuration(s)
	if err != nil {
		d.fail(key, "must be a whole number followed by s, m, h or d, such as 30m")
		return def
	}
	return v
}

// positiveDuration is duration for a key whose value must be more than 0.
func (d *decoder) positiveDuration(key string, def time.Duration) time.Duration {
	n := len(d.failures)
	v := d.duration(key, def)
	if len(d.failures) == n && v <= 0 {
		d.fail(key, "must be more than 0")
		return def
	}
	return v
}

// size returns the key's size in bytes, in the size format of
// [server].max_body_size, def when the file does not set it.
func (d *decoder) size(key string, def int64) int64 {
	s, ok := d.str(key, "")
	if _, set := d.table[key]; !set || !ok {
		return def
	}
	n, err := parseSize(s)
	if err != nil {
		d.fail(key, err.Error())
		return def
	}
	return n
}

// file is a path key's value: its absolute path, and its content when it
// could be read.
type file struct {
	path    string
	content []byte
	read    bool
}

// file reads the file a path key names, relative paths taken from the
// config dir. set reports whether the key holds a path, readable or not.
func (d *decoder) file(key string) (f file, set bool) {
	p, ok := d.str(key, "")
	if !ok || p == "" {
		return file{}, false
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(d.dir, p)
	}
	f.path = filepath.Clean(p)
	content, err := os.ReadFile(f.path)
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		d.fail(key, "cannot be read: "+err.Error())
		return f, true
	}
	f.content, f.read = content, true
	return f, true
}

// checkURL applies the rule of gateway_url to a URL value: https, or http to
// a loopback host; no user information, query or fragment; and, unless
// pathAllowed, no path other than /. The reason never quotes the value.
func (d *decoder) checkURL(key, s string, pathAllowed bool) {
	u, err := url.Parse(s)
	reason := ""
	switch {
	case err != nil:
		reason = "must be a URL"
	case u.Scheme != "https" && u.Scheme != "http":
		reason = "must be an https URL, or an http URL to localhost, 127.0.0.0/8 or ::1"
	case u.Opaque != "" || u.Host == "":
		reason = "must have a host"
	case u.User != nil:
		reason = "must not contain user information"
	case u.Scheme == "http" && !loopback(u.Hostname()):
		reason = "may use http only to localhost, 127.0.0.0/8 or ::1"
	case u.RawQuery != "" || u.ForceQuery:
		reason = "must not have a query"
	case strings.Contains(s, "#"):
		reason = "must not have a fragment"
	case !pathAllowed && u.Path != "" && u.Path != "/":
		reason = "must not have a path other than /"
	}
	if reason != "" {
		d.fail(key, reason)
	}
}

// loopback reports whether host is localhost, an address in 127.0.0.0/8 or
// ::1.
func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.Unmap().IsLoopback()
}

// entry is one table of a list of tables, named for its failures.
type entry struct {
	// name is the table's name when it is a string, valid or not.
	name string
	// label names the table in failures: its name when that is a valid name,
	// its zero-based index otherwise.
	label string
	table map[string]any
}

// entries reads the list of tables under key, reporting every key of a
// table that is not one of keys. A table is named by its name when that is a
// valid name, and by its zero-based index otherwise.
func (d *decoder) entries(key string, keys []string) []entry {
	v, set := d.value(key)
	if !set {
		return nil
	}
	list, ok := tables(v)
	if !ok {
		d.fail(key, "must be [["+SectionName+"."+key+"]] tables")
		return nil
	}
	out := make([]entry, 0, len(list))
	for i, t := range list {
		name, _ := t["name"].(string)
		label := name
		if !namePattern.MatchString(name) {
			label = strconv.Itoa(i)
		}
		for _, k := range slices.Sorted(maps.Keys(t)) {
			if !slices.Contains(keys, k) {
				d.fail(key+"["+label+"]."+k, "unknown key")
			}
		}
		out = append(out, entry{name: name, label: label, table: t})
	}
	return out
}

// tables returns v as a list of tables: [[name]] tables decode as one Go
// type and an inline array of tables as another.
func tables(v any) ([]map[string]any, bool) {
	switch x := v.(type) {
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

var sizePattern = regexp.MustCompile(`^([0-9]+)(kb|mb|gb)$`)

// parseSize reads a positive integer followed by KB, MB or GB, in any letter
// case and in powers of 1024, as a number of bytes: the format of
// [server].max_body_size, whose parser the configuration loader keeps
// unexported.
func parseSize(s string) (int64, error) {
	bad := errors.New("must be a positive integer followed by KB, MB or GB, such as 1GB")
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
