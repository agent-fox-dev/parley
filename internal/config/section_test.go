package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestRegisterSection_HandsTheWholeSectionToItsValidator(t *testing.T) {
	sandbox := "[sandbox]\ngateway_url = \"https://${HOST}\"\nanything = 1\n[sandbox.profiles.default]\nimage = \"img\"\n[[sandbox.credentials]]\nname = \"a\"\ntoken = \"pa$$${SECRET}\""
	tests := []struct {
		name    string
		text    *string
		env     map[string]string
		calls   int
		wantErr []string
		check   func(t *testing.T, got map[string]any)
	}{
		{"absent", nil, nil, 0, nil, nil},
		{"present", file(sandbox), map[string]string{"HOST": "gw.example", "SECRET": "s"}, 1, nil, func(t *testing.T, got map[string]any) {
			if got["gateway_url"] != "https://gw.example" || got["anything"] != int64(1) {
				t.Errorf("section = %v", got)
			}
			profiles, _ := got["profiles"].(map[string]any)
			def, _ := profiles["default"].(map[string]any)
			creds, _ := got["credentials"].([]map[string]any)
			if def["image"] != "img" || len(creds) != 1 || creds[0]["token"] != "pa$s" {
				t.Errorf("nested = %v %v", profiles, creds)
			}
		}},
		{"unset variable", file("[sandbox]\ngateway_url = \"${NOPE}\""), nil, 0, []string{"NOPE", "sandbox"}, nil},
		{"another section still checked", file("[sandbox]\nx = 1\n[server]\nprot = 1"), nil, 0, []string{"server.prot"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []map[string]any
			reg := NewRegistry()
			reg.RegisterSection("sandbox", func(s map[string]any) error {
				calls = append(calls, s)
				return nil
			})
			cfg, err := reg.Load(MapEnv(tt.env), dirWith(t, tt.text))
			if tt.wantErr != nil {
				if cfg != nil || err == nil {
					t.Fatalf("got %v, %v; want an error", cfg, err)
				}
				for _, w := range tt.wantErr {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("error %q lacks %q", err, w)
					}
				}
				if tt.name == "unset variable" && len(calls) != 0 {
					t.Error("the validator was called")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(calls) != tt.calls {
				t.Fatalf("validator called %d times, want %d", len(calls), tt.calls)
			}
			got, present := cfg.Section("sandbox")
			if present != (tt.calls == 1) {
				t.Errorf("present = %v", present)
			}
			if tt.check != nil {
				tt.check(t, calls[0])
				if !reflect.DeepEqual(got, calls[0]) {
					t.Errorf("reported section differs from what the validator was given")
				}
			}
		})
	}
}

func TestRegisterSection_TheValidatorsErrorFailsTheLoadUnchanged(t *testing.T) {
	msg := "sandbox.gateway_url: missing; sandbox.profiles[default].image: empty; sandbox.nosuch: unknown key"
	calls := 0
	reg := NewRegistry()
	reg.RegisterSection("sandbox", func(map[string]any) error { calls++; return errors.New(msg) })
	cfg, err := reg.Load(noEnv, dirWith(t, file("[sandbox]\nnosuch = 1")))
	if cfg != nil || err == nil || !strings.Contains(err.Error(), msg) {
		t.Fatalf("got %v, %v; want the validator's message", cfg, err)
	}
	if calls != 1 {
		t.Errorf("validator called %d times", calls)
	}
}

func TestRegisterSection_ConflictingRegistrationsFailTheLoad(t *testing.T) {
	tests := []struct {
		section  string
		register func(*Registry, SectionValidator)
	}{
		{"sandbox", func(r *Registry, v SectionValidator) {
			r.RegisterSection("sandbox", v)
			r.RegisterSection("sandbox", v)
		}},
		{"forks", func(r *Registry, v SectionValidator) {
			r.Register(Key{Section: "forks", Name: "default_ttl", Type: String, Default: "1d"})
			r.RegisterSection("forks", v)
		}},
		{"sandbox", func(r *Registry, v SectionValidator) {
			r.RegisterSection("sandbox", v)
			r.Register(Key{Section: "sandbox", Name: "x", Type: Int, Default: 0})
		}},
		{"server", func(r *Registry, v SectionValidator) { r.RegisterSection("server", v) }},
	}
	for _, tt := range tests {
		called := false
		reg := NewRegistry()
		tt.register(reg, func(map[string]any) error { called = true; return nil })
		cfg, err := reg.Load(noEnv, dirWith(t, file("["+tt.section+"]\n")))
		if cfg != nil || err == nil || !strings.Contains(err.Error(), tt.section) {
			t.Errorf("%s: got %v, %v", tt.section, cfg, err)
		}
		if called {
			t.Errorf("%s: a validator was called", tt.section)
		}
	}
}
