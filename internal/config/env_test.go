package config_test

import (
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/config"
)

// provider returns a file holding one GitHub provider whose client_id and
// client_secret are written as given.
func provider(clientID, clientSecret string) string {
	return "[[oauth.providers]]\nname = \"github\"\nclient_id = \"" + clientID + "\"\nclient_secret = \"" + clientSecret + "\"\n"
}

func TestLoad_ReplacesEnvironmentReferencesInStringValues(t *testing.T) {
	t.Parallel()
	env := config.MapEnv(map[string]string{"HOST": "hub.example", "SECRET": "s3cr3t", "EMPTY": "", "LVL": "debug"})
	clientSecret := func(c *config.Config) string { return c.OAuth.Providers[0].ClientSecret }
	cases := []struct {
		name string
		file string
		get  func(*config.Config) string
		want string
	}{
		{"variable in a URL", "[server]\nexternal_url = \"https://${HOST}\"\n",
			func(c *config.Config) string { return c.Server.ExternalURL }, "https://hub.example"},
		{"whole value of a provider's secret", provider("id", "${SECRET}"), clientSecret, "s3cr3t"},
		{"empty variable and two references", provider("a${EMPTY}b${SECRET}", "x"),
			func(c *config.Config) string { return c.OAuth.Providers[0].ClientID }, "abs3cr3t"},
		{"doubled dollar", provider("id", "pa$$word"), clientSecret, "pa$word"},
		{"doubled dollar before a brace", provider("id", "$${SECRET}"), clientSecret, "${SECRET}"},
		{"lone dollar", provider("id", "cost $5"), clientSecret, "cost $5"},
		{"trailing dollar", provider("id", "cost$"), clientSecret, "cost$"},
		{"validated key", "[logging]\nlevel = \"${LVL}\"\n",
			func(c *config.Config) string { return c.Logging.Level }, "debug"},
		{"key another package registered", "[x]\ny = \"${HOST}\"\n",
			func(c *config.Config) string { return c.String("x", "y") }, "hub.example"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := config.NewRegistry()
			if err := reg.Register(config.Key{Section: "x", Name: "y", Type: config.String, Default: ""}); err != nil {
				t.Fatal(err)
			}
			cfg, err := reg.Load(env, dirWith(t, tc.file))
			if err != nil {
				t.Fatal(err)
			}
			if got := tc.get(cfg); got != tc.want {
				t.Errorf("value = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoad_FailsOnAReferenceToAnUnsetVariable(t *testing.T) {
	t.Parallel()
	// The case names hold neither the variable nor the key: the error quotes
	// the file's path, which holds the test's name.
	cases := []struct {
		name, file, variable, key string
	}{
		{"URL", "[server]\nexternal_url = \"https://${NOPE}\"\n", "NOPE", "server.external_url"},
		{"provider", provider("id", "${GH_SECRET}"), "GH_SECRET", "client_secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load(noEnv(), dirWith(t, tc.file))
			if cfg != nil || err == nil {
				t.Fatalf("Load = %v, %v; want no configuration and an error", cfg, err)
			}
			for _, want := range []string{tc.variable, tc.key} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestLoad_RefusesAMalformedReferenceAsAnInvalidValue(t *testing.T) {
	t.Parallel()
	env := config.MapEnv(map[string]string{"HOST": "h"})
	// x.y takes any string, so only the reference can make its value invalid.
	keys := []struct{ label, key string }{{"URL", "server.external_url"}, {"free string", "x.y"}}
	for _, k := range keys {
		key := k.key
		section, name, _ := strings.Cut(key, ".")
		for _, value := range []string{"https://${HOST", "${}", "${1ST}", "${A B}", "${A-B}"} {
			t.Run(k.label+" "+value, func(t *testing.T) {
				t.Parallel()
				reg := config.NewRegistry()
				if err := reg.Register(config.Key{Section: "x", Name: "y", Type: config.String, Default: ""}); err != nil {
					t.Fatal(err)
				}
				cfg, err := reg.Load(env, dirWith(t, "["+section+"]\n"+name+" = \""+value+"\"\n"))
				if cfg != nil || err == nil {
					t.Fatalf("Load = %v, %v; want no configuration and an error", cfg, err)
				}
				if !strings.Contains(err.Error(), key) {
					t.Errorf("error %q does not name %s", err, key)
				}
			})
		}
	}
}

func TestLoad_ReplacesReferencesOnlyInsideParsedStringValues(t *testing.T) {
	t.Parallel()
	evil := "\"\n[server]\nport = 1\n"
	env := config.MapEnv(map[string]string{"EVIL": evil, "BAD": "ftp://x", "LVL": "loud"})

	t.Run("a variable holding TOML syntax", func(t *testing.T) {
		t.Parallel()
		cfg, err := config.Load(env, dirWith(t, provider("id", "${EVIL}")))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Server.Port != 8080 {
			t.Errorf("port = %d, want 8080", cfg.Server.Port)
		}
		if got := cfg.OAuth.Providers[0].ClientSecret; got != evil {
			t.Errorf("client secret = %q, want %q", got, evil)
		}
	})

	invalid := []struct {
		name, file, key, value string
	}{
		{"URL", "[server]\nexternal_url = \"${BAD}\"\n", "server.external_url", "ftp://x"},
		{"level", "[logging]\nlevel = \"${LVL}\"\n", "logging.level", "loud"},
		{"quoted table header", "[\"${LVL}\"]\nx = 1\n", "[${LVL}]: unknown section", "loud"},
		{"bare table header", "[${LVL}]\nx = 1\n", "line 1", "loud"},
	}
	for _, tc := range invalid {
		t.Run("invalid expansion of the "+tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load(env, dirWith(t, tc.file))
			if cfg != nil || err == nil {
				t.Fatalf("Load = %v, %v; want no configuration and an error", cfg, err)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error %q does not contain %q", err, tc.key)
			}
			if strings.Contains(err.Error(), tc.value) {
				t.Errorf("error %q holds the variable's value %q", err, tc.value)
			}
		})
	}
}
