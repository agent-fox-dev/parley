package awareness_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/awareness"
	"github.com/agent-fox-dev/parley/internal/config"
)

func TestConfig_ReadsTheClaimLimitsAndRefusesInvalidValues(t *testing.T) {
	t.Parallel()
	defaults := awareness.Config{DefaultClaimTTL: time.Hour, MaxClaimTTL: 24 * time.Hour, MaxClaimsPerWorkspace: 1000}
	for _, tc := range []struct {
		name string
		text string
		want awareness.Config
		// wantErr is what the error names; "" for a load that succeeds.
		wantErr string
	}{
		{name: "no section", want: defaults},
		{name: "a shorter default", text: "default_claim_ttl = \"30m\"",
			want: awareness.Config{DefaultClaimTTL: 30 * time.Minute, MaxClaimTTL: 24 * time.Hour, MaxClaimsPerWorkspace: 1000}},
		{name: "every key", text: "default_claim_ttl = \"2h\"\nmax_claim_ttl = \"2d\"\nmax_claims_per_workspace = 5",
			want: awareness.Config{DefaultClaimTTL: 2 * time.Hour, MaxClaimTTL: 48 * time.Hour, MaxClaimsPerWorkspace: 5}},
		{name: "a zero maximum", text: "max_claim_ttl = \"0s\"", wantErr: "awareness.max_claim_ttl"},
		{name: "a zero default", text: "default_claim_ttl = \"0m\"", wantErr: "awareness.default_claim_ttl"},
		{name: "a default above the maximum", text: "default_claim_ttl = \"2d\"\nmax_claim_ttl = \"24h\"",
			wantErr: "awareness.default_claim_ttl"},
		{name: "a zero workspace limit", text: "max_claims_per_workspace = 0", wantErr: "awareness.max_claims_per_workspace"},
		{name: "a negative workspace limit", text: "max_claims_per_workspace = -1", wantErr: "awareness.max_claims_per_workspace"},
		{name: "not a duration", text: "default_claim_ttl = \"soon\"", wantErr: "awareness.default_claim_ttl"},
		{name: "a duration that is not a string", text: "max_claim_ttl = 3600", wantErr: "awareness.max_claim_ttl"},
		{name: "an unregistered key", text: "other = 1", wantErr: "awareness.other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			text := ""
			if tc.text != "" {
				text = "[awareness]\n" + tc.text + "\n"
			}
			if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			reg := config.NewRegistry()
			for _, k := range awareness.ConfigKeys() {
				if err := reg.Register(k); err != nil {
					t.Fatal(err)
				}
			}
			var got awareness.Config
			cfg, err := reg.Load(config.MapEnv(nil), dir)
			if err == nil {
				got, err = awareness.ConfigFrom(cfg)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("load = %+v, %v; want an error naming %s", got, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("config = %+v, want %+v", got, tc.want)
			}
		})
	}
}
