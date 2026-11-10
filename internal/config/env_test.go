package config

import (
	"strings"
	"testing"
)

// providerWith is a github provider whose key holds value as a literal
// TOML string.
func providerWith(key, value string) string {
	p := map[string]string{"client_id": "id", "client_secret": "secret"}
	p[key] = value
	return "[[oauth.providers]]\nname = \"github\"\nclient_id = '" + p["client_id"] + "'\nclient_secret = '" + p["client_secret"] + "'\n"
}

func TestLoad_ReplacesEnvironmentReferencesInStringValues(t *testing.T) {
	env := MapEnv(map[string]string{"HOST": "hub.example", "SECRET": "s3cr3t", "EMPTY": "", "LVL": "debug"})
	tests := []struct {
		text string
		get  func(*Config) any
		want string
	}{
		{"[server]\nexternal_url = 'https://${HOST}'", func(c *Config) any { return c.Server.ExternalURL }, "https://hub.example"},
		{providerWith("client_secret", "${SECRET}"), func(c *Config) any { return c.OAuth.Providers[0].ClientSecret }, "s3cr3t"},
		{providerWith("client_id", "a${EMPTY}b${SECRET}"), func(c *Config) any { return c.OAuth.Providers[0].ClientID }, "abs3cr3t"},
		{providerWith("client_secret", "pa$$word"), func(c *Config) any { return c.OAuth.Providers[0].ClientSecret }, "pa$word"},
		{providerWith("client_secret", "$${SECRET}"), func(c *Config) any { return c.OAuth.Providers[0].ClientSecret }, "${SECRET}"},
		{providerWith("client_secret", "cost $5"), func(c *Config) any { return c.OAuth.Providers[0].ClientSecret }, "cost $5"},
		{"[logging]\nlevel = '${LVL}'", func(c *Config) any { return c.Logging.Level }, "debug"},
		{"[x]\ny = '${HOST}'", func(c *Config) any { return c.Value("x", "y") }, "hub.example"},
	}
	for _, tt := range tests {
		reg := NewRegistry()
		reg.Register(Key{Section: "x", Name: "y", Type: String, Default: ""})
		cfg, err := reg.Load(env, dirWith(t, file(tt.text)))
		if err != nil {
			t.Errorf("%q: %v", tt.text, err)
			continue
		}
		if got := tt.get(cfg); got != tt.want {
			t.Errorf("%q: got %q, want %q", tt.text, got, tt.want)
		}
	}
}

func TestLoad_AReferenceToAnUnsetVariableFailsNamingVariableAndKey(t *testing.T) {
	tests := []struct{ text, variable, key string }{
		{"[server]\nexternal_url = \"https://${NOPE}\"", "NOPE", "server.external_url"},
		{providerWith("client_secret", "${GH_SECRET}"), "GH_SECRET", "client_secret"},
	}
	for _, tt := range tests {
		cfg, err := Load(noEnv, dirWith(t, file(tt.text)))
		if cfg != nil || err == nil || !strings.Contains(err.Error(), tt.variable) || !strings.Contains(err.Error(), tt.key) {
			t.Errorf("%q: got %v, %v; want an error naming %s and %s", tt.text, cfg, err, tt.variable, tt.key)
		}
	}
}

func TestLoad_AMalformedReferenceIsAnInvalidValue(t *testing.T) {
	env := MapEnv(map[string]string{"HOST": "h"})
	for _, v := range []string{"https://${HOST", "${}", "${1ST}", "${A B}", "${A-B}"} {
		cfg, err := Load(env, dirWith(t, file("[server]\nexternal_url = '"+v+"'")))
		if cfg != nil || err == nil || !strings.Contains(err.Error(), "server.external_url") {
			t.Errorf("%q: got %v, %v; want an error naming server.external_url", v, cfg, err)
		}
	}
}

func TestLoad_AVariableNeverChangesTheFilesStructure(t *testing.T) {
	evil := "\"\n[server]\nport = 1\n"
	env := MapEnv(map[string]string{"EVIL": evil, "BAD": "ftp://x", "LVL": "loud"})
	cfg, err := Load(env, dirWith(t, file(providerWith("client_secret", "${EVIL}"))))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Port != 8080 || cfg.OAuth.Providers[0].ClientSecret != evil {
		t.Errorf("port %d, secret %q", cfg.Server.Port, cfg.OAuth.Providers[0].ClientSecret)
	}
	for _, tt := range []struct{ text, key, value string }{
		{"[server]\nexternal_url = '${BAD}'", "server.external_url", "ftp://x"},
		{"[logging]\nlevel = '${LVL}'", "logging.level", "loud"},
	} {
		cfg, err := Load(env, dirWith(t, file(tt.text)))
		if cfg != nil || err == nil || !strings.Contains(err.Error(), tt.key) {
			t.Errorf("%q: got %v, %v", tt.text, cfg, err)
			continue
		}
		if strings.Contains(err.Error(), tt.value) {
			t.Errorf("error %q carries the variable's value", err)
		}
	}
	if cfg, err := Load(env, dirWith(t, file("[${LVL}]\nx = 1"))); cfg != nil || err == nil {
		t.Error("a table header with a reference was expanded or accepted")
	}
}
