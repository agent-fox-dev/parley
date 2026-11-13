package sandbox_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFromConfig_FillsEveryKeyTheSectionDoesNotSetWithItsDefault(t *testing.T) {
	t.Parallel()
	s, err := loadSandbox(t, sandboxFile{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fields := []struct {
		name      string
		got, want any
	}{
		{"Enabled", s.Enabled, true},
		{"Runtime", s.Runtime, "openshell"},
		{"GatewayURL", s.GatewayURL, "https://gw.test:8443"},
		{"OpenShellWorkspace", s.OpenShellWorkspace, "parley"},
		{"MaxTTL", s.MaxTTL, 24 * time.Hour},
		{"IdleTimeout", s.IdleTimeout, 30 * time.Minute},
		{"MaxSandboxes", s.MaxSandboxes, 200},
		{"MaxPerOrg", s.MaxPerOrg, 100},
		{"MaxPerWorkspace", s.MaxPerWorkspace, 50},
		{"MaxPerUser", s.MaxPerUser, 20},
		{"AllowWorkspaceRules", s.AllowWorkspaceRules, true},
		{"MaxExecTimeout", s.MaxExecTimeout, 30 * time.Minute},
		{"MaxConcurrentExecs", s.MaxConcurrentExecs, 8},
		{"MaxTransferSize", s.MaxTransferSize, int64(1073741824)},
		{"LogRetention", s.LogRetention, 168 * time.Hour},
		{"ReconcileInterval", s.ReconcileInterval, time.Minute},
		{"TLSCert", s.TLSCert, ""},
		{"TLSKey", s.TLSKey, ""},
		{"TLSCA", s.TLSCA, ""},
		{"OIDCIssuer", s.OIDCIssuer, ""},
		{"OIDCClientID", s.OIDCClientID, ""},
		{"OIDCClientSecretFile", s.OIDCClientSecretFile, ""},
		{"Token", s.Token(), tokenContent},
		{"number of credentials", len(s.Credentials), 0},
		{"number of profiles", len(s.Profiles), 1},
	}
	for _, f := range fields {
		if f.got != f.want {
			t.Errorf("%s = %#v, want %#v", f.name, f.got, f.want)
		}
	}
	if !filepath.IsAbs(s.TokenFile) || filepath.Base(s.TokenFile) != "token" {
		t.Errorf("TokenFile = %q, want the absolute path of token in the config dir", s.TokenFile)
	}
}

func TestFromConfig_TakesTheValuesTheSectionSetsAfterReplacingReferences(t *testing.T) {
	t.Parallel()
	f := sandboxFile{set: map[string]string{
		"gateway_url":           `"${GW}"`,
		"max_sandboxes":         "10",
		"reconcile_interval":    `"10s"`,
		"log_retention":         `"30d"`,
		"max_transfer_size":     `"2mb"`,
		"idle_timeout":          `"0s"`,
		"runtime":               `"openshell"`,
		"openshell_workspace":   `"team-1_a"`,
		"allow_workspace_rules": "false",
	}}
	s, err := loadSandbox(t, f, map[string]string{"GW": "https://gw.example:9443/"})
	if err != nil {
		t.Fatal(err)
	}
	fields := []struct {
		name      string
		got, want any
	}{
		{"GatewayURL", s.GatewayURL, "https://gw.example:9443/"},
		{"MaxSandboxes", s.MaxSandboxes, 10},
		{"ReconcileInterval", s.ReconcileInterval, 10 * time.Second},
		{"LogRetention", s.LogRetention, 30 * 24 * time.Hour},
		{"MaxTransferSize", s.MaxTransferSize, int64(2 << 20)},
		{"IdleTimeout", s.IdleTimeout, time.Duration(0)},
		{"OpenShellWorkspace", s.OpenShellWorkspace, "team-1_a"},
		{"AllowWorkspaceRules", s.AllowWorkspaceRules, false},
	}
	for _, f := range fields {
		if f.got != f.want {
			t.Errorf("%s = %#v, want %#v", f.name, f.got, f.want)
		}
	}
}

func TestFromConfig_RefusesEveryInvalidValueNamingItsKey(t *testing.T) {
	t.Parallel()
	// oidc and tls replace the token file by another complete way to
	// authenticate, so that a row fails on its own key alone.
	oidc := map[string]string{"token_file": "", "oidc_issuer": `"https://idp.test"`,
		"oidc_client_id": `"parley"`, "oidc_client_secret_file": `"secret"`}
	tls := map[string]string{"token_file": "", "tls_cert": `"cert"`, "tls_key": `"key"`}
	with := func(base map[string]string, key, value string) map[string]string {
		m := map[string]string{key: value}
		for k, v := range base {
			if k != key {
				m[k] = v
			}
		}
		return m
	}
	cases := []struct {
		name string
		set  map[string]string
		want []string // nil: the section is accepted
	}{
		{"runtime other than openshell", map[string]string{"runtime": `"docker"`}, []string{"sandbox.runtime"}},
		{"runtime of the wrong type", map[string]string{"runtime": "1"}, []string{"sandbox.runtime"}},
		{"gateway_url missing", map[string]string{"gateway_url": ""}, []string{"sandbox.gateway_url"}},
		{"gateway_url not a URL", map[string]string{"gateway_url": `"://gw"`}, []string{"sandbox.gateway_url"}},
		{"gateway_url without a scheme", map[string]string{"gateway_url": `"gw.test:8443"`}, []string{"sandbox.gateway_url"}},
		{"gateway_url of another scheme", map[string]string{"gateway_url": `"ftp://gw.test"`}, []string{"sandbox.gateway_url"}},
		{"gateway_url http to a remote host", map[string]string{"gateway_url": `"http://gw.test:8080"`}, []string{"sandbox.gateway_url"}},
		{"gateway_url http to an address outside loopback", map[string]string{"gateway_url": `"http://10.0.0.1"`}, []string{"sandbox.gateway_url"}},
		{"gateway_url with user information", map[string]string{"gateway_url": `"https://u:p@gw.test"`}, []string{"sandbox.gateway_url"}},
		{"gateway_url with a query", map[string]string{"gateway_url": `"https://gw.test?a=1"`}, []string{"sandbox.gateway_url"}},
		{"gateway_url with a fragment", map[string]string{"gateway_url": `"https://gw.test#f"`}, []string{"sandbox.gateway_url"}},
		{"gateway_url with a path", map[string]string{"gateway_url": `"https://gw.test/api"`}, []string{"sandbox.gateway_url"}},
		{"gateway_url of the wrong type", map[string]string{"gateway_url": "8443"}, []string{"sandbox.gateway_url"}},
		{"openshell_workspace not matching the pattern", map[string]string{"openshell_workspace": `"Parley"`}, []string{"sandbox.openshell_workspace"}},
		{"openshell_workspace too long", map[string]string{"openshell_workspace": `"` + strings.Repeat("a", 64) + `"`}, []string{"sandbox.openshell_workspace"}},
		{"max_ttl of 0", map[string]string{"max_ttl": `"0s"`}, []string{"sandbox.max_ttl"}},
		{"max_ttl not a duration", map[string]string{"max_ttl": `"1h30m"`}, []string{"sandbox.max_ttl"}},
		{"max_exec_timeout of 0", map[string]string{"max_exec_timeout": `"0m"`}, []string{"sandbox.max_exec_timeout"}},
		{"log_retention of 0", map[string]string{"log_retention": `"0d"`}, []string{"sandbox.log_retention"}},
		{"idle_timeout below 0", map[string]string{"idle_timeout": `"-1m"`}, []string{"sandbox.idle_timeout"}},
		{"max_sandboxes below 1", map[string]string{"max_sandboxes": "0"}, []string{"sandbox.max_sandboxes"}},
		{"max_per_org below 1", map[string]string{"max_per_org": "0"}, []string{"sandbox.max_per_org"}},
		{"max_per_workspace below 1", map[string]string{"max_per_workspace": "0"}, []string{"sandbox.max_per_workspace"}},
		{"max_per_user below 1", map[string]string{"max_per_user": "-1"}, []string{"sandbox.max_per_user"}},
		{"max_concurrent_execs below 1", map[string]string{"max_concurrent_execs": "0"}, []string{"sandbox.max_concurrent_execs"}},
		{"max_sandboxes of the wrong type", map[string]string{"max_sandboxes": `"ten"`}, []string{"sandbox.max_sandboxes"}},
		{"allow_workspace_rules of the wrong type", map[string]string{"allow_workspace_rules": `"yes"`}, []string{"sandbox.allow_workspace_rules"}},
		{"max_ttl of the wrong type", map[string]string{"max_ttl": "24"}, []string{"sandbox.max_ttl"}},
		{"max_transfer_size not a size", map[string]string{"max_transfer_size": `"1TB"`}, []string{"sandbox.max_transfer_size"}},
		{"max_transfer_size of the wrong type", map[string]string{"max_transfer_size": "1024"}, []string{"sandbox.max_transfer_size"}},
		{"reconcile_interval below 10s", map[string]string{"reconcile_interval": `"9s"`}, []string{"sandbox.reconcile_interval"}},
		{"reconcile_interval above 10m", map[string]string{"reconcile_interval": `"601s"`}, []string{"sandbox.reconcile_interval"}},
		{"tls_cert unreadable", with(tls, "tls_cert", `"missing"`), []string{"sandbox.tls_cert"}},
		{"tls_key unreadable", with(tls, "tls_key", `"missing"`), []string{"sandbox.tls_key"}},
		{"tls_key a directory", with(tls, "tls_key", `"."`), []string{"sandbox.tls_key"}},
		{"tls_ca unreadable", map[string]string{"tls_ca": `"missing"`}, []string{"sandbox.tls_ca"}},
		{"token_file unreadable", map[string]string{"token_file": `"missing"`}, []string{"sandbox.token_file"}},
		{"token_file empty after trimming", map[string]string{"token_file": `"empty"`}, []string{"sandbox.token_file"}},
		{"oidc_client_secret_file unreadable", with(oidc, "oidc_client_secret_file", `"missing"`), []string{"sandbox.oidc_client_secret_file"}},
		{"oidc_client_secret_file empty after trimming", with(oidc, "oidc_client_secret_file", `"empty"`), []string{"sandbox.oidc_client_secret_file"}},
		{"oidc_issuer http to a remote host", with(oidc, "oidc_issuer", `"http://idp.test"`), []string{"sandbox.oidc_issuer"}},
		{"oidc_issuer with user information", with(oidc, "oidc_issuer", `"https://u:p@idp.test"`), []string{"sandbox.oidc_issuer"}},
		{"oidc_issuer with a query", with(oidc, "oidc_issuer", `"https://idp.test/realm?a=1"`), []string{"sandbox.oidc_issuer"}},
		{"oidc_client_id of the wrong type", with(oidc, "oidc_client_id", "7"), []string{"sandbox.oidc_client_id"}},
		{"three invalid keys together", map[string]string{"runtime": `"docker"`, "max_ttl": `"0s"`, "reconcile_interval": `"5s"`},
			[]string{"sandbox.runtime", "sandbox.max_ttl", "sandbox.reconcile_interval"}},

		{"reconcile_interval of exactly 10s", map[string]string{"reconcile_interval": `"10s"`}, nil},
		{"reconcile_interval of exactly 10m", map[string]string{"reconcile_interval": `"10m"`}, nil},
		{"gateway_url http to 127.0.0.1", map[string]string{"gateway_url": `"http://127.0.0.1:8080"`}, nil},
		{"gateway_url http to localhost", map[string]string{"gateway_url": `"http://localhost:8080"`}, nil},
		{"gateway_url http to another loopback address", map[string]string{"gateway_url": `"http://127.1.2.3"`}, nil},
		{"gateway_url http to ::1", map[string]string{"gateway_url": `"http://[::1]:8080"`}, nil},
		{"gateway_url with the path /", map[string]string{"gateway_url": `"https://gw.test/"`}, nil},
		{"oidc_issuer with a path", with(oidc, "oidc_issuer", `"https://idp.test/realms/parley"`), nil},
		{"idle_timeout of 0", map[string]string{"idle_timeout": `"0s"`}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, err := loadSandbox(t, sandboxFile{set: tc.set}, nil)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("load: %v; want the section accepted", err)
				}
				return
			}
			wantErrorNaming(t, s, err, tc.want...)
			if len(tc.want) > 1 && strings.Count(err.Error(), "; ") < len(tc.want)-1 {
				t.Errorf("error %q does not separate its %d failures by '; '", err, len(tc.want))
			}
		})
	}
}

func TestFromConfig_ReportsAnUnknownKeyWithEveryOtherFailure(t *testing.T) {
	t.Parallel()
	credential := "[[sandbox.credentials]]\nname = \"model\"\nprovider_profile = \"anthropic\"\nsecret = \"MODEL_KEY\"\n"
	cases := []struct {
		name string
		file sandboxFile
		want []string
	}{
		{"in the section", sandboxFile{set: map[string]string{"gatway_url": `"https://gw.test"`}},
			[]string{"sandbox.gatway_url"}},
		{"in a profile", sandboxFile{profile: "imge = \"img\"\n"},
			[]string{"sandbox.profiles[agent].imge"}},
		{"in a credential", sandboxFile{tail: credential + "secrt = \"x\"\n"},
			[]string{"sandbox.credentials[model].secrt"}},
		{"in a profile without a usable name", sandboxFile{tail: "[[sandbox.profiles]]\nttl = \"1h\"\nimge = \"img\"\n"},
			[]string{"sandbox.profiles[1].imge"}},
		{"together with an invalid value", sandboxFile{set: map[string]string{"gatway_url": `"x"`, "max_ttl": `"0s"`}},
			[]string{"sandbox.gatway_url", "sandbox.max_ttl"}},
		{"in a profile together with an invalid value", sandboxFile{set: map[string]string{"max_ttl": `"0s"`}, profile: "imge = \"img\"\n"},
			[]string{"sandbox.profiles[agent].imge", "sandbox.max_ttl"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, err := loadSandbox(t, tc.file, nil)
			wantErrorNaming(t, s, err, tc.want...)
		})
	}
}

func TestFromConfig_RequiresExactlyOneWayToAuthenticate(t *testing.T) {
	t.Parallel()
	pair := map[string]string{"tls_cert": `"cert"`, "tls_key": `"key"`}
	triple := map[string]string{"oidc_issuer": `"https://idp.test"`, "oidc_client_id": `"parley"`,
		"oidc_client_secret_file": `"secret"`}
	join := func(ms ...map[string]string) map[string]string {
		out := map[string]string{}
		for _, m := range ms {
			for k, v := range m {
				out[k] = v
			}
		}
		return out
	}
	noToken := map[string]string{"token_file": ""}
	cases := []struct {
		name string
		set  map[string]string
		want []string // nil: the section is accepted
	}{
		{"no way", noToken, []string{"sandbox.gateway_url", "none"}},
		{"token_file and the TLS pair", pair, []string{"sandbox.gateway_url", "mutual TLS, token_file"}},
		{"token_file and the OIDC keys", triple, []string{"sandbox.gateway_url", "token_file, OIDC client credentials"}},
		{"all three", join(pair, triple), []string{"sandbox.gateway_url", "mutual TLS, token_file, OIDC client credentials"}},
		{"tls_cert alone", join(noToken, map[string]string{"tls_cert": `"cert"`}), []string{"sandbox.tls_key"}},
		{"tls_key alone", join(noToken, map[string]string{"tls_key": `"key"`}), []string{"sandbox.tls_cert"}},
		{"oidc_issuer alone", join(noToken, map[string]string{"oidc_issuer": `"https://idp.test"`}),
			[]string{"sandbox.oidc_client_id", "sandbox.oidc_client_secret_file"}},
		{"oidc_issuer and oidc_client_id", join(noToken, map[string]string{"oidc_issuer": `"https://idp.test"`, "oidc_client_id": `"parley"`}),
			[]string{"sandbox.oidc_client_secret_file"}},
		{"token_file with tls_ca", map[string]string{"tls_ca": `"ca"`}, nil},
		{"the TLS pair with tls_ca", join(noToken, pair, map[string]string{"tls_ca": `"ca"`}), nil},
		{"the OIDC keys alone", join(noToken, triple), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, err := loadSandbox(t, sandboxFile{set: tc.set}, nil)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("load: %v; want the section accepted", err)
				}
				return
			}
			wantErrorNaming(t, s, err, tc.want...)
		})
	}
	t.Run("the credentials of the way are kept", func(t *testing.T) {
		t.Parallel()
		s, err := loadSandbox(t, sandboxFile{set: join(noToken, triple)}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if s.OIDCClientSecret() != secretContent || s.Token() != "" {
			t.Errorf("OIDCClientSecret, Token = %q, %q; want the secret file's content and nothing", s.OIDCClientSecret(), s.Token())
		}
	})
}

func TestFromConfig_NeverPutsACredentialInItsError(t *testing.T) {
	t.Parallel()
	userinfo := `"https://u:pw-SECRET-3@gw.test"`
	cases := []struct {
		name string
		set  map[string]string
	}{
		{"gateway_url with user information", map[string]string{"gateway_url": userinfo}},
		{"oidc_issuer with user information", map[string]string{"oidc_issuer": `"https://u:pw-SECRET-3@idp.test/r"`,
			"oidc_client_id": `"parley"`, "oidc_client_secret_file": `"secret"`}},
		{"user information in a URL that is not one", map[string]string{"gateway_url": `"https://u:pw-SECRET-3@gw.test:port"`}},
		{"user information with a path", map[string]string{"gateway_url": `"http://u:pw-SECRET-3@gw.test/x?y#z"`}},
		{"secret files beside invalid values", map[string]string{"oidc_issuer": `"https://idp.test"`, "oidc_client_id": `"parley"`,
			"oidc_client_secret_file": `"secret"`, "gateway_url": userinfo, "max_ttl": `"0s"`}},
		{"credential file named as a duration", map[string]string{"max_ttl": `"tok-SECRET-1"`, "gateway_url": userinfo}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := loadSandbox(t, sandboxFile{set: tc.set}, nil)
			if err == nil {
				t.Fatal("load succeeded; want it refused")
			}
			for _, secret := range []string{tokenContent, secretContent, "pw-SECRET-3"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error %q contains %q", err, secret)
				}
			}
		})
	}
}

func TestFromConfig_RequiresTheExternalURLWithSandboxesOn(t *testing.T) {
	t.Parallel()
	s, err := loadSandbox(t, sandboxFile{noExternalURL: true, set: map[string]string{"max_ttl": `"0s"`}}, nil)
	wantErrorNaming(t, s, err, "server.external_url", "sandbox.max_ttl")

	s, err = loadSandbox(t, sandboxFile{noExternalURL: true, noSection: true}, nil)
	if err != nil {
		t.Fatalf("load without [sandbox]: %v", err)
	}
	if s.Enabled {
		t.Error("Enabled = true without a [sandbox] section")
	}
}

func TestFromConfig_BoundsMaxPerOrgByMaxSandboxes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		set           map[string]string
		wantErr       bool
		wantMaxPerOrg int
	}{
		{"set above max_sandboxes", map[string]string{"max_sandboxes": "50", "max_per_org": "60"}, true, 0},
		{"not set below the default", map[string]string{"max_sandboxes": "50"}, false, 50},
		{"set equal to max_sandboxes", map[string]string{"max_sandboxes": "50", "max_per_org": "50"}, false, 50},
		{"not set above the default", map[string]string{"max_sandboxes": "500"}, false, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, err := loadSandbox(t, sandboxFile{set: tc.set}, nil)
			if tc.wantErr {
				wantErrorNaming(t, s, err, "sandbox.max_per_org")
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if s.MaxPerOrg != tc.wantMaxPerOrg {
				t.Errorf("MaxPerOrg = %d, want %d", s.MaxPerOrg, tc.wantMaxPerOrg)
			}
		})
	}
}
