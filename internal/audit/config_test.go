package audit_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/audit"
	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/hub"
)

const day = 24 * time.Hour

// defaults are the retention settings of a file that sets none of the keys.
var defaults = audit.Settings{
	MaxAge: 90 * day, MaxRuns: 50, TraceMaxAge: 30 * day, PostmortemMaxAge: 180 * day,
	OrphanRetention: 30 * day, SessionMaxAge: 90 * day, SessionMaxActiveAge: 7 * day,
}

// writeConfig writes text as config.toml in a new directory and returns it.
func writeConfig(t *testing.T, text string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// loadConfig loads text as config.toml through the real loader with the
// retention keys and the keys of [events] registered.
func loadConfig(t *testing.T, text string) (*config.Config, error) {
	t.Helper()
	r := config.NewRegistry()
	if err := audit.Register(r); err != nil {
		t.Fatalf("register the retention keys: %v", err)
	}
	if err := event.Register(r); err != nil {
		t.Fatalf("register the event keys: %v", err)
	}
	return r.Load(config.MapEnv(nil), writeConfig(t, text))
}

func TestSettings_TakeTheDefaultsAndEveryValueInRange(t *testing.T) {
	t.Parallel()
	with := func(fn func(*audit.Settings)) audit.Settings {
		s := defaults
		fn(&s)
		return s
	}
	for _, tc := range []struct {
		name, text string
		want       audit.Settings
	}{
		{"no [audit] and no [sessions]", "", defaults},
		{"audit.max_age 1d", "[audit]\nmax_age = \"1d\"\n", with(func(s *audit.Settings) { s.MaxAge = day })},
		{"audit.max_age 86400s", "[audit]\nmax_age = \"86400s\"\n", with(func(s *audit.Settings) { s.MaxAge = day })},
		{"audit.max_age 36h", "[audit]\nmax_age = \"36h\"\n", with(func(s *audit.Settings) { s.MaxAge = 36 * time.Hour })},
		{"audit.trace_max_age 1440m", "[audit]\ntrace_max_age = \"1440m\"\n",
			with(func(s *audit.Settings) { s.TraceMaxAge = day })},
		{"audit.postmortem_max_age 365d", "[audit]\npostmortem_max_age = \"365d\"\n",
			with(func(s *audit.Settings) { s.PostmortemMaxAge = 365 * day })},
		{"audit.orphan_retention 1d", "[audit]\norphan_retention = \"1d\"\n",
			with(func(s *audit.Settings) { s.OrphanRetention = day })},
		{"sessions.max_age 2d", "[sessions]\nmax_age = \"2d\"\n", with(func(s *audit.Settings) { s.SessionMaxAge = 2 * day })},
		{"sessions.max_active_age 1h", "[sessions]\nmax_active_age = \"1h\"\n",
			with(func(s *audit.Settings) { s.SessionMaxActiveAge = time.Hour })},
		{"sessions.max_active_age 60m", "[sessions]\nmax_active_age = \"60m\"\n",
			with(func(s *audit.Settings) { s.SessionMaxActiveAge = time.Hour })},
		{"audit.max_runs 1", "[audit]\nmax_runs = 1\n", with(func(s *audit.Settings) { s.MaxRuns = 1 })},
		{"audit.max_runs 1000", "[audit]\nmax_runs = 1000\n", with(func(s *audit.Settings) { s.MaxRuns = 1000 })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := loadConfig(t, tc.text)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if got := audit.SettingsFrom(cfg); got != tc.want {
				t.Errorf("settings = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestSettings_RefuseAMalformedOrOutOfRangeValueNamingTheKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ section, name, value string }{
		{"audit", "max_age", `"23h"`},
		{"audit", "max_age", `"0d"`},
		{"audit", "max_age", `"ninety"`},
		{"audit", "max_age", `"-1d"`},
		{"audit", "max_age", `"1w"`},
		{"audit", "max_age", `90`},
		{"audit", "trace_max_age", `"86399s"`},
		{"audit", "postmortem_max_age", `"12h"`},
		{"audit", "orphan_retention", `""`},
		{"sessions", "max_age", `"23h"`},
		{"sessions", "max_active_age", `"59m"`},
		{"audit", "max_runs", `0`},
		{"audit", "max_runs", `-3`},
		{"audit", "max_runs", `"50"`},
	} {
		key := tc.section + "." + tc.name
		// The name leaves out key, which would be in the path of the
		// test's directory and so in every error of the load.
		t.Run("["+tc.section+"] "+tc.name+"="+tc.value, func(t *testing.T) {
			t.Parallel()
			_, err := loadConfig(t, "["+tc.section+"]\n"+tc.name+" = "+tc.value+"\n")
			if err == nil {
				t.Fatalf("load accepted %s = %s", key, tc.value)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error %q does not name %s", err, key)
			}
		})
	}
}

func TestSettings_RefuseAnEventRetentionLongerThanTheAuditMaxAge(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, text string
		ok         bool
	}{
		{"30d within 90d", "[events]\nretention = \"30d\"\n[audit]\nmax_age = \"90d\"\n", true},
		{"2d equal to 2d", "[events]\nretention = \"2d\"\n[audit]\nmax_age = \"2d\"\n", true},
		{"1h within 1d", "[events]\nretention = \"1h\"\n[audit]\nmax_age = \"1d\"\n", true},
		{"91d over 90d", "[events]\nretention = \"91d\"\n[audit]\nmax_age = \"90d\"\n", false},
		{"the default 30d over 7d", "[audit]\nmax_age = \"7d\"\n", false},
		{"25h over 1d", "[events]\nretention = \"25h\"\n[audit]\nmax_age = \"1d\"\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := loadConfig(t, tc.text)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			s, err := audit.NewSettings(cfg, event.SettingsFrom(cfg).Retention)
			switch {
			case tc.ok && err != nil:
				t.Errorf("building the settings failed: %v", err)
			case tc.ok && s != audit.SettingsFrom(cfg):
				t.Errorf("settings = %+v, want the loaded %+v", s, audit.SettingsFrom(cfg))
			case !tc.ok && err == nil:
				t.Errorf("building the settings accepted them")
			case !tc.ok && (!strings.Contains(err.Error(), "events.retention") || !strings.Contains(err.Error(), "audit.max_age")):
				t.Errorf("error %q does not name both events.retention and audit.max_age", err)
			}
		})
	}
}

func TestHub_StopsTheStartOnARefusedRetentionLimit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, text string
		keys       []string
	}{
		{"max_runs below 1", "[audit]\nmax_runs = 0\n", []string{"audit.max_runs"}},
		{"event retention over the audit max age", "[events]\nretention = \"91d\"\n",
			[]string{"events.retention", "audit.max_age"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stderr bytes.Buffer
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			// A start that is not refused listens on a port the system
			// chooses, on the loopback interface only.
			text := "[server]\nbind = \"127.0.0.1\"\nport = 0\n" + tc.text
			code := hub.Run(ctx, hub.Options{WorkDir: writeConfig(t, text), Stderr: &stderr})
			if code != 1 {
				t.Errorf("Run returned %d, want 1", code)
			}
			lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
			if len(lines) != 1 {
				t.Errorf("stderr has %d lines, want one: %q", len(lines), stderr.String())
			}
			for _, k := range tc.keys {
				if !strings.Contains(stderr.String(), k) {
					t.Errorf("stderr %q does not name %s", stderr.String(), k)
				}
			}
			if strings.Contains(stderr.String(), "listening") {
				t.Errorf("stderr %q has a listening line", stderr.String())
			}
		})
	}
}
