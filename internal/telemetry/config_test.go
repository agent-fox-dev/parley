package telemetry_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/telemetry"
)

// loadConfig loads text as config.toml through the real loader with the
// package's configuration key registered.
func loadConfig(t *testing.T, text string) (*config.Config, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	r := config.NewRegistry()
	if err := r.Register(telemetry.ConfigKey()); err != nil {
		t.Fatalf("register: %v", err)
	}
	return r.Load(config.MapEnv(nil), dir)
}

func TestConfig_WriteTimeoutDefaultAndValues(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		text string
		want time.Duration
	}{
		{"no [audit] table", "", 2 * time.Second},
		{"100ms", "[audit]\nwrite_timeout = \"100ms\"\n", 100 * time.Millisecond},
		{"500ms", "[audit]\nwrite_timeout = \"500ms\"\n", 500 * time.Millisecond},
		{"2s", "[audit]\nwrite_timeout = \"2s\"\n", 2 * time.Second},
		{"1m", "[audit]\nwrite_timeout = \"1m\"\n", time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := loadConfig(t, tc.text)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if got := telemetry.WriteTimeoutFrom(cfg); got != tc.want {
				t.Errorf("write limit = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestConfig_RejectsInvalidWriteTimeout(t *testing.T) {
	t.Parallel()
	for _, value := range []string{`"99ms"`, `"0s"`, `"-1s"`, `"soon"`, `"2"`, `5`, `""`} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			_, err := loadConfig(t, "[audit]\nwrite_timeout = "+value+"\n")
			if err == nil {
				t.Fatalf("load accepted write_timeout = %s", value)
			}
			if !strings.Contains(err.Error(), "audit.write_timeout") {
				t.Errorf("error %q does not name audit.write_timeout", err)
			}
		})
	}
}
