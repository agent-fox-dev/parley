package config_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/config"
)

func TestRegistry_HandsAWholeSectionToItsValidator(t *testing.T) {
	t.Parallel()
	sandbox := "[sandbox]\ngateway_url = \"https://${HOST}\"\nanything = 1\n" +
		"[sandbox.profiles.default]\nimage = \"img\"\n" +
		"[[sandbox.credentials]]\nname = \"a\"\ntoken = \"pa$$${SECRET}\"\n"
	parsed := map[string]any{
		"gateway_url": "https://gw.example",
		"anything":    int64(1),
		"profiles":    map[string]any{"default": map[string]any{"image": "img"}},
		"credentials": []map[string]any{{"name": "a", "token": "pa$s"}},
	}
	env := config.MapEnv(map[string]string{"HOST": "gw.example", "SECRET": "s"})
	cases := []struct {
		name      string
		file      string
		env       config.Env
		wantErr   []string // nil: the load succeeds
		wantCalls int
		want      map[string]any // the section's content; nil: absent
	}{
		{name: "section absent", file: "[server]\nport = 1\n", env: env},
		{name: "section with tables, lists and references", file: sandbox, env: env, wantCalls: 1, want: parsed},
		{name: "reference to an unset variable", file: "[sandbox]\ngateway_url = \"${NOPE}\"\n", env: noEnv(),
			wantErr: []string{"NOPE", "sandbox"}},
		{name: "unknown key in another section", file: "[sandbox]\nx = 1\n[server]\nprot = 1\n", env: env,
			wantErr: []string{"server.prot"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls []map[string]any
			reg := config.NewRegistry()
			if err := reg.RegisterSection("sandbox", func(section map[string]any) error {
				calls = append(calls, section)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			cfg, err := reg.Load(tc.env, dirWith(t, tc.file))
			if tc.wantErr != nil {
				if cfg != nil || err == nil {
					t.Fatalf("Load = %v, %v; want no configuration and an error", cfg, err)
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not contain %q", err, want)
					}
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if len(calls) != tc.wantCalls {
				t.Fatalf("validator called %d times, want %d", len(calls), tc.wantCalls)
			}
			if tc.want != nil && !reflect.DeepEqual(calls[0], tc.want) {
				t.Errorf("validator got %#v, want %#v", calls[0], tc.want)
			}
			if cfg == nil {
				return
			}
			got, present := cfg.Section("sandbox")
			if present != (tc.want != nil) || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Section = %#v, %v; want %#v, %v", got, present, tc.want, tc.want != nil)
			}
		})
	}
}

func TestRegistry_FailsTheLoadWithTheSectionValidatorsError(t *testing.T) {
	t.Parallel()
	msg := "sandbox.gateway_url: missing; sandbox.profiles[default].image: empty; sandbox.nosuch: unknown key"
	calls := 0
	reg := config.NewRegistry()
	if err := reg.RegisterSection("sandbox", func(map[string]any) error {
		calls++
		return errors.New(msg)
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := reg.Load(noEnv(), dirWith(t, "[sandbox]\nnosuch = 1\n"))
	if cfg != nil || err == nil {
		t.Fatalf("Load = %v, %v; want no configuration and an error", cfg, err)
	}
	if !strings.Contains(err.Error(), msg) {
		t.Errorf("error %q does not contain the validator's message", err)
	}
	if calls != 1 {
		t.Errorf("validator called %d times, want 1", calls)
	}
}

func TestRegistry_RefusesAConflictingSectionRegistration(t *testing.T) {
	t.Parallel()
	// Each file holds the section concerned and is valid for the
	// registrations that are accepted, so that only the refused one can
	// fail the load, and a validator that ran would be called.
	cases := []struct {
		name     string
		section  string
		file     string
		register func(reg *config.Registry, v func(map[string]any) error)
	}{
		{"section registered twice", "sandbox", "[sandbox]\nx = 1\n", func(reg *config.Registry, v func(map[string]any) error) {
			_ = reg.RegisterSection("sandbox", v)
			_ = reg.RegisterSection("sandbox", v)
		}},
		{"section of a registered key", "forks", "[forks]\ndefault_ttl = \"1d\"\n", func(reg *config.Registry, v func(map[string]any) error) {
			_ = reg.Register(config.Key{Section: "forks", Name: "default_ttl", Type: config.String, Default: "7d"})
			_ = reg.RegisterSection("forks", v)
		}},
		{"key in a registered section", "sandbox", "[sandbox]\nx = 1\n", func(reg *config.Registry, v func(map[string]any) error) {
			_ = reg.RegisterSection("sandbox", v)
			_ = reg.Register(config.Key{Section: "sandbox", Name: "x", Type: config.Int, Default: 0})
		}},
		{"section the loader owns keys in", "server", "[server]\nport = 1\n", func(reg *config.Registry, v func(map[string]any) error) {
			_ = reg.RegisterSection("server", v)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			reg := config.NewRegistry()
			tc.register(reg, func(map[string]any) error { calls++; return nil })
			cfg, err := reg.Load(noEnv(), dirWith(t, tc.file))
			if cfg != nil || err == nil {
				t.Fatalf("Load = %v, %v; want no configuration and an error", cfg, err)
			}
			if !strings.Contains(err.Error(), tc.section) {
				t.Errorf("error %q does not name section %q", err, tc.section)
			}
			if calls != 0 {
				t.Errorf("validator called %d times, want 0", calls)
			}
		})
	}
}
