package webhook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
)

// loadWebhooks loads text as config.toml through the real loader with the
// keys of [webhooks] registered.
func loadWebhooks(t *testing.T, text string) (*config.Config, error) {
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

func TestConfig_LoadsDefaultsAndValues(t *testing.T) {
	t.Parallel()
	defaults := Settings{MaxPerWorkspace: 20, Timeout: 10 * time.Second}
	with := func(change func(*Settings)) Settings {
		s := defaults
		change(&s)
		return s
	}
	for _, tc := range []struct {
		name string
		text string
		want Settings
	}{
		{"no [webhooks] section", "", defaults},
		{"max_per_workspace 0", "[webhooks]\nmax_per_workspace = 0\n", with(func(s *Settings) { s.MaxPerWorkspace = 0 })},
		{"timeout 1s", "[webhooks]\ntimeout = \"1s\"\n", with(func(s *Settings) { s.Timeout = time.Second })},
		{"timeout 60s", "[webhooks]\ntimeout = \"60s\"\n", with(func(s *Settings) { s.Timeout = time.Minute })},
		{"timeout 1m", "[webhooks]\ntimeout = \"1m\"\n", with(func(s *Settings) { s.Timeout = time.Minute })},
		{"both switches on", "[webhooks]\nallow_private = true\nallow_insecure = true\n",
			with(func(s *Settings) { s.AllowPrivate, s.AllowInsecure = true, true })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := loadWebhooks(t, tc.text)
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
		{`max_per_workspace = -1`, "webhooks.max_per_workspace"},
		{`max_per_workspace = "many"`, "webhooks.max_per_workspace"},
		{`timeout = "999ms"`, "webhooks.timeout"},
		{`timeout = "0s"`, "webhooks.timeout"},
		{`timeout = "61s"`, "webhooks.timeout"},
		{`timeout = 10`, "webhooks.timeout"},
		{`allow_private = "yes"`, "webhooks.allow_private"},
		{`allow_insecure = "yes"`, "webhooks.allow_insecure"},
		{`retries = 3`, "webhooks.retries"},
	} {
		t.Run(tc.line, func(t *testing.T) {
			t.Parallel()
			_, err := loadWebhooks(t, "[webhooks]\n"+tc.line+"\n")
			if err == nil {
				t.Fatalf("load accepted %s", tc.line)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error %q does not name %s", err, tc.key)
			}
		})
	}
}
