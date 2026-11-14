package sandbox_test

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/sandbox"
)

func TestSettings_LooksUpProfilesAndCredentialsWithTheirDefaults(t *testing.T) {
	t.Parallel()
	f := sandboxFile{
		noProfile: true,
		set:       map[string]string{"idle_timeout": `"45m"`},
		tail: profileTable("b", "ttl = \"1h\"\n") +
			profileTable("a", "ttl = \"2h\"\ndefault = true\nidle_timeout = \"0s\"\ncredentials = [\"model\"]\n") +
			credentialTable("model"),
	}
	s, err := loadSandbox(t, f, nil)
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, p := range s.Profiles() {
		names = append(names, p.Name)
	}
	if !reflect.DeepEqual(names, []string{"a", "b"}) {
		t.Errorf("names of Profiles() = %q, want [a b]", names)
	}

	b, ok := s.Profile("b")
	want := sandbox.Profile{
		Name: "b", TTL: time.Hour, IdleTimeout: 45 * time.Minute, HubAccess: "fork",
		Credentials: []string{}, OptionalCredentials: []string{},
		ExposeSecrets: []string{}, OptionalExposeSecrets: []string{},
		Env: map[string]string{}, Checkpoint: true, AllowProposals: true,
	}
	if !ok || !reflect.DeepEqual(b, want) {
		t.Errorf("Profile(b) = %+v, %v; want %+v, true", b, ok, want)
	}

	a, _ := s.Profile("a")
	if a.IdleTimeout != 0 || !reflect.DeepEqual(a.Credentials, []string{"model"}) {
		t.Errorf("Profile(a) idle timeout, credentials = %v, %q; want 0, [model]", a.IdleTimeout, a.Credentials)
	}
	if d, ok := s.DefaultProfile(); !ok || d.Name != "a" {
		t.Errorf("DefaultProfile() = %q, %v; want a, true", d.Name, ok)
	}
	if p, ok := s.Profile("zz"); ok {
		t.Errorf("Profile(zz) = %+v, true; want no such profile", p)
	}

	wantCredential := sandbox.Credential{Name: "model", ProviderProfile: "anthropic", Secret: "MODEL_KEY"}
	if c, ok := s.Credential("model"); !ok || c != wantCredential {
		t.Errorf("Credential(model) = %+v, %v; want %+v, true", c, ok, wantCredential)
	}
	if c, ok := s.Credential("zz"); ok {
		t.Errorf("Credential(zz) = %+v, true; want no such credential", c)
	}
	if cs := s.Credentials(); len(cs) != 1 || cs[0] != wantCredential {
		t.Errorf("Credentials() = %+v, want [%+v]", cs, wantCredential)
	}

	t.Run("without a default", func(t *testing.T) {
		t.Parallel()
		s, err := loadSandbox(t, sandboxFile{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if p, ok := s.DefaultProfile(); ok {
			t.Errorf("DefaultProfile() = %+v, true; want none", p)
		}
	})
}

func TestSettings_KeepsTheProfilesUnchangedWhatACallerDoesWithThem(t *testing.T) {
	t.Parallel()
	s, err := loadSandbox(t, sandboxFile{profile: "env = { A = \"a\" }\nexpose_secrets = [\"NPM_TOKEN\"]\n"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := s.Profile("agent")
	if !ok || len(s.Profiles()) != 1 {
		t.Fatalf("Profile(agent) = %+v, %v with %d profiles; want the one profile", p, ok, len(s.Profiles()))
	}
	p.Env["A"] = "changed"
	p.ExposeSecrets[0] = "CHANGED"
	list := s.Profiles()
	list[0].Name = "changed"
	list[0].Env["B"] = "added"
	d, _ := s.Profile("agent")
	if d.Env["A"] != "a" || len(d.Env) != 1 || d.ExposeSecrets[0] != "NPM_TOKEN" || s.Profiles()[0].Name != "agent" {
		t.Errorf("Profile(agent) = %+v after its copies were changed; want it as loaded", d)
	}
}

func TestFromConfig_ReadsEveryKeyOfAProfile(t *testing.T) {
	t.Parallel()
	body := `default = true
description = "for agents"
image = "ghcr.io/acme/agent:1"
cpu = "2"
memory = "4Gi"
gpu = 1
idle_timeout = "10m"
policy = "policy.yaml"
credentials = ["model"]
optional_credentials = ["other"]
expose_secrets = ["NPM_TOKEN"]
optional_expose_secrets = ["PIP_TOKEN"]
hub_access = "none"
env = { LANG = "C.UTF-8", _X = "y" }
checkpoint = false
allow_proposals = false
`
	s, err := loadSandbox(t, sandboxFile{profile: body, tail: credentialTable("model") + credentialTable("other")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := s.Profile("agent")
	if !filepath.IsAbs(p.PolicyPath) || filepath.Base(p.PolicyPath) != "policy.yaml" {
		t.Errorf("PolicyPath = %q, want the absolute path of policy.yaml in the config dir", p.PolicyPath)
	}
	p.PolicyPath = ""
	want := sandbox.Profile{
		Name: "agent", Default: true, Description: "for agents", Image: "ghcr.io/acme/agent:1",
		CPU: "2", Memory: "4Gi", GPU: 1, TTL: 4 * time.Hour, IdleTimeout: 10 * time.Minute,
		Credentials: []string{"model"}, OptionalCredentials: []string{"other"},
		ExposeSecrets: []string{"NPM_TOKEN"}, OptionalExposeSecrets: []string{"PIP_TOKEN"},
		HubAccess: "none", Env: map[string]string{"LANG": "C.UTF-8", "_X": "y"},
	}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("Profile(agent) = %+v\nwant %+v", p, want)
	}
}

func TestFromConfig_RefusesAnInvalidProfileNamingItsKey(t *testing.T) {
	t.Parallel()
	// only replaces the profile 'agent' by the tables of tail.
	only := func(tail string) sandboxFile { return sandboxFile{noProfile: true, tail: tail} }
	cases := []struct {
		name    string
		file    sandboxFile
		want    []string // nil: the section is accepted
		notWant string
	}{
		{"no profile", only(""), []string{"sandbox.profiles: "}, ""},
		{"no name", sandboxFile{tail: "[[sandbox.profiles]]\nttl = \"1h\"\n"}, []string{"sandbox.profiles[1].name"}, ""},
		{"a name not matching the pattern", sandboxFile{tail: profileTable("Bad Name", "ttl = \"1h\"\n")}, []string{"sandbox.profiles[1].name"}, ""},
		{"a name of 64 characters", sandboxFile{tail: profileTable(strings.Repeat("n", 64), "ttl = \"1h\"\n")}, []string{"sandbox.profiles[1].name"}, ""},
		{"a name of the wrong type", sandboxFile{tail: "[[sandbox.profiles]]\nname = 7\nttl = \"1h\"\n"}, []string{"sandbox.profiles[1].name"}, ""},
		{"a name another profile has", sandboxFile{tail: profileTable("agent", "ttl = \"1h\"\n")}, []string{"sandbox.profiles[agent].name"}, ""},
		{"a second default", sandboxFile{profile: "default = true\n", tail: profileTable("b", "ttl = \"1h\"\ndefault = true\n")},
			[]string{"sandbox.profiles[b].default"}, "sandbox.profiles[agent].default"},
		{"default of the wrong type", sandboxFile{profile: "default = \"yes\"\n"}, []string{"sandbox.profiles[agent].default"}, ""},
		{"no ttl", only(profileTable("agent", "")), []string{"sandbox.profiles[agent].ttl"}, ""},
		{"a ttl of 0", only(profileTable("agent", "ttl = \"0s\"\n")), []string{"sandbox.profiles[agent].ttl"}, ""},
		{"a ttl below 0", only(profileTable("agent", "ttl = \"-1h\"\n")), []string{"sandbox.profiles[agent].ttl"}, ""},
		{"a ttl that is not a duration", only(profileTable("agent", "ttl = \"4 hours\"\n")), []string{"sandbox.profiles[agent].ttl"}, ""},
		{"a ttl one second above max_ttl", only(profileTable("agent", "ttl = \"86401s\"\n")), []string{"sandbox.profiles[agent].ttl"}, ""},
		{"a ttl above a max_ttl the section sets", sandboxFile{set: map[string]string{"max_ttl": `"2h"`}}, []string{"sandbox.profiles[agent].ttl"}, ""},
		{"an idle_timeout below 0", sandboxFile{profile: "idle_timeout = \"-1m\"\n"}, []string{"sandbox.profiles[agent].idle_timeout"}, ""},
		{"a gpu below 0", sandboxFile{profile: "gpu = -1\n"}, []string{"sandbox.profiles[agent].gpu"}, ""},
		{"a gpu of the wrong type", sandboxFile{profile: "gpu = \"1\"\n"}, []string{"sandbox.profiles[agent].gpu"}, ""},
		{"a hub_access other than fork and none", sandboxFile{profile: "hub_access = \"all\"\n"}, []string{"sandbox.profiles[agent].hub_access"}, ""},
		{"a policy file that cannot be read", sandboxFile{profile: "policy = \"missing.yaml\"\n"}, []string{"sandbox.profiles[agent].policy"}, ""},
		{"an image of the wrong type", sandboxFile{profile: "image = 1\n"}, []string{"sandbox.profiles[agent].image"}, ""},
		{"credentials not a list of strings", sandboxFile{profile: "credentials = \"model\"\n"}, []string{"sandbox.profiles[agent].credentials"}, ""},
		{"env not a table", sandboxFile{profile: "env = \"A=1\"\n"}, []string{"sandbox.profiles[agent].env"}, ""},
		{"checkpoint of the wrong type", sandboxFile{profile: "checkpoint = 1\n"}, []string{"sandbox.profiles[agent].checkpoint"}, ""},
		{"two profiles each with a failure", only(profileTable("a", "ttl = \"0s\"\n") + profileTable("b", "ttl = \"1h\"\nhub_access = \"all\"\n")),
			[]string{"sandbox.profiles[a].ttl", "sandbox.profiles[b].hub_access"}, ""},
		{"together with a failure of the section", sandboxFile{set: map[string]string{"reconcile_interval": `"1s"`}, profile: "gpu = -1\n"},
			[]string{"sandbox.reconcile_interval", "sandbox.profiles[agent].gpu"}, ""},

		{"a name of 63 characters", sandboxFile{tail: profileTable(strings.Repeat("n", 63), "ttl = \"1h\"\n")}, nil, ""},
		{"a ttl equal to max_ttl", only(profileTable("agent", "ttl = \"24h\"\n")), nil, ""},
		{"an idle_timeout and a gpu of 0", sandboxFile{profile: "idle_timeout = \"0s\"\ngpu = 0\n"}, nil, ""},
		{"hub_access none", sandboxFile{profile: "hub_access = \"none\"\n"}, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, err := loadSandbox(t, tc.file, nil)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("load: %v; want the section accepted", err)
				}
				return
			}
			wantErrorNaming(t, s, err, tc.want...)
			if tc.notWant != "" && strings.Contains(err.Error(), tc.notWant) {
				t.Errorf("error %q names %q", err, tc.notWant)
			}
		})
	}
}

func TestFromConfig_RefusesAnInvalidCredentialOrReferenceToOne(t *testing.T) {
	t.Parallel()
	model := credentialTable("model")
	cases := []struct {
		name    string
		profile string
		tail    string
		want    []string // nil: the section is accepted
	}{
		{"an entry without a name", "", "[[sandbox.credentials]]\nprovider_profile = \"anthropic\"\nsecret = \"MODEL_KEY\"\n",
			[]string{"sandbox.credentials[0].name"}},
		{"a name not matching the pattern", "", credentialTable("Bad Name"), []string{"sandbox.credentials[0].name"}},
		{"a name another entry has", "", model + model, []string{"sandbox.credentials[model].name"}},
		{"an empty provider_profile", "", strings.Replace(model, `"anthropic"`, `""`, 1), []string{"sandbox.credentials[model].provider_profile"}},
		{"no provider_profile", "", strings.Replace(model, "provider_profile = \"anthropic\"\n", "", 1), []string{"sandbox.credentials[model].provider_profile"}},
		{"an empty secret", "", strings.Replace(model, `"MODEL_KEY"`, `""`, 1), []string{"sandbox.credentials[model].secret"}},
		{"a secret of the wrong type", "", strings.Replace(model, `"MODEL_KEY"`, "1", 1), []string{"sandbox.credentials[model].secret"}},
		{"credentials naming no entry", "credentials = [\"nope\"]\n", model,
			[]string{"sandbox.profiles[agent].credentials", `"nope"`}},
		{"optional_credentials naming no entry", "optional_credentials = [\"nope\"]\n", model,
			[]string{"sandbox.profiles[agent].optional_credentials", `"nope"`}},
		{"a name in both lists", "credentials = [\"model\"]\noptional_credentials = [\"model\"]\n", model,
			[]string{"sandbox.profiles[agent].optional_credentials", `"model"`, "sandbox.profiles[agent].credentials"}},

		{"both lists naming entries", "credentials = [\"model\"]\noptional_credentials = [\"other\"]\n", model + credentialTable("other"), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, err := loadSandbox(t, sandboxFile{profile: tc.profile, tail: tc.tail}, nil)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("load: %v; want the section accepted", err)
				}
				return
			}
			wantErrorNaming(t, s, err, tc.want...)
		})
	}
}

func TestFromConfig_RefusesAnEnvKeyThatIsNotAVariableNameNamingTheKeyAlone(t *testing.T) {
	t.Parallel()
	const value = "val-SECRETISH"
	quoted := `"` + value + `"`
	cases := []struct {
		name, key string
		value     string // the entry's value as TOML
		refused   bool
	}{
		{"PARLEY_X", "PARLEY_X", quoted, true},
		{"OPENSHELL_Y", "OPENSHELL_Y", quoted, true},
		{"1ABC", "1ABC", quoted, true},
		{"A-B", "A-B", quoted, true},
		{"the empty key", "", quoted, true},
		{"a value that is not a string", "A", "7", true},
		{"PARLEYX", "PARLEYX", quoted, false},
		{"parley_lower", "parley_lower", quoted, false},
		{"_X", "_X", quoted, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, err := loadSandbox(t, sandboxFile{profile: "env = { \"" + tc.key + "\" = " + tc.value + " }\n"}, nil)
			if !tc.refused {
				if err != nil {
					t.Fatalf("load: %v; want the key accepted", err)
				}
				return
			}
			wantErrorNaming(t, s, err, "sandbox.profiles[agent].env", `"`+tc.key+`"`)
			if strings.Contains(err.Error(), value) {
				t.Errorf("error %q contains the entry's value", err)
			}
		})
	}
}

func TestFromConfig_RefusesAnExposedSecretNameBreakingTheNameRules(t *testing.T) {
	t.Parallel()
	names := []string{
		"1TOKEN", "A-B", "PARLEY_TOKEN", "OPENSHELL_X",
		"GIT_PAT", "git_pat", "Git_Username", "GIT_PASSWORD", "UPSTREAM_GIT_PAT", "UPSTREAM_GIT_USERNAME",
		"upstream_git_password", "FORGE_WEBHOOK_SECRET",
		"MY_VAR", // a key of the profile's env
	}
	type row struct {
		name, profile, list, value string
	}
	var rows []row
	for _, list := range []string{"expose_secrets", "optional_expose_secrets"} {
		for _, n := range names {
			rows = append(rows, row{list + " " + n, list + " = [\"" + n + "\"]\nenv = { MY_VAR = \"v\" }\n", list, n})
		}
		rows = append(rows, row{list + " a name in both lists",
			"expose_secrets = [\"NPM_TOKEN\"]\noptional_expose_secrets = [\"NPM_TOKEN\"]\n", list, "NPM_TOKEN"})
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, err := loadSandbox(t, sandboxFile{profile: tc.profile}, nil)
			wantErrorNaming(t, s, err, "sandbox.profiles[agent]."+tc.list, `"`+tc.value+`"`)
		})
	}
	t.Run("names of secrets that are not stored", func(t *testing.T) {
		t.Parallel()
		_, err := loadSandbox(t, sandboxFile{profile: "expose_secrets = [\"NPM_TOKEN\"]\noptional_expose_secrets = [\"PIP_TOKEN\"]\n"}, nil)
		if err != nil {
			t.Fatalf("load: %v; want the profile accepted", err)
		}
	})
}
