package event

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
)

// loadEvents loads text as config.toml through the real loader with the
// keys of [events] registered.
func loadEvents(t *testing.T, text string) (*config.Config, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	r := config.NewRegistry()
	if err := Register(r); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return r.Load(config.MapEnv(nil), dir)
}

func TestConfig_DefaultsAndValues(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		text string
		want Settings
	}{
		{"no [events] section", "", Settings{Retention: 720 * time.Hour, MaxStreams: 500}},
		{"both keys set", "[events]\nretention = \"1h\"\nmax_sse_connections = 1\n", Settings{Retention: time.Hour, MaxStreams: 1}},
		{"retention in days", "[events]\nretention = \"7d\"\n", Settings{Retention: 7 * 24 * time.Hour, MaxStreams: 500}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := loadEvents(t, tc.text)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if got := SettingsFrom(cfg); got != tc.want {
				t.Errorf("settings = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestConfig_RejectsInvalidValues(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		line string
		key  string
	}{
		{`retention = "59m"`, "events.retention"},
		{`retention = "0h"`, "events.retention"},
		{`retention = "soon"`, "events.retention"},
		{`retention = 30`, "events.retention"},
		{`max_sse_connections = 0`, "events.max_sse_connections"},
		{`max_sse_connections = -1`, "events.max_sse_connections"},
		{`max_sse_connections = "many"`, "events.max_sse_connections"},
	} {
		t.Run(tc.line, func(t *testing.T) {
			t.Parallel()
			_, err := loadEvents(t, "[events]\n"+tc.line+"\n")
			if err == nil {
				t.Fatalf("load accepted %s", tc.line)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error %q does not name %s", err, tc.key)
			}
		})
	}
}

func TestRetention_IsTheLoadedValue(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(d *Deps) { d.Retention = 48 * time.Hour })
	if got := f.events.Retention(); got != 48*time.Hour {
		t.Errorf("Retention() = %v, want 48h", got)
	}
}
