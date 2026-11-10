package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// helpProbe is the probe group of the help checks: probe get <name> and
// probe admin purge [reason].
func helpProbe() *cobra.Command {
	return Group("probe", "probe commands",
		Declare(Spec{Use: "get", Description: "gets a thing", Method: "GET", Path: "/things/:name",
			Args: []Arg{{Name: "name", Required: true}}, Auth: AuthUser,
			Flags: func(fs *pflag.FlagSet) { fs.Int("limit", 50, "the most things to return") }}),
		Group("admin", "admin probe commands",
			Declare(Spec{Use: "purge", Description: "purges things", Method: "DELETE", Path: "/things",
				Args: []Arg{{Name: "reason", Required: false}}, Auth: AuthAdmin,
				Flags: func(fs *pflag.FlagSet) {
					fs.Bool("confirm", false, "confirm the purge")
					_ = cobra.MarkFlagRequired(fs, "confirm")
				}})))
}

// helpEnv unsets the settings' variables, points PARLEY_CONFIG_DIR at a
// directory whose config.toml is not valid TOML, and returns a count of the
// requests a hub receives.
func helpEnv(t *testing.T) *atomic.Int64 {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("endpoint_url = [unclosed"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PARLEY_CONFIG_DIR", dir)
	t.Setenv("PARLEY_ENDPOINT_URL", "")
	t.Setenv("PARLEY_API_KEY", "")
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { n.Add(1) }))
	t.Cleanup(srv.Close)
	return &n
}

func runHelp(t *testing.T, args ...string) cliResult {
	t.Helper()
	return runProbe(t, []*cobra.Command{helpProbe()}, args...)
}

func TestHelp_RootAndGroupsListTheirSubcommands(t *testing.T) {
	requests := helpEnv(t)
	cases := []struct {
		args []string
		want []string
	}{
		{nil, []string{"version", "help", "login", "probe", "probe commands"}},
		{[]string{"probe"}, []string{"get", "gets a thing", "admin", "admin probe commands"}},
		{[]string{"probe", "admin"}, []string{"purge", "purges things"}},
	}
	for _, c := range cases {
		r := runHelp(t, c.args...)
		if r.code != 0 || r.stderr != "" {
			t.Errorf("%v: exit %d, stderr %q", c.args, r.code, r.stderr)
		}
		for _, w := range c.want {
			if !strings.Contains(r.stdout, w) {
				t.Errorf("%v: help does not name %q:\n%s", c.args, w, r.stdout)
			}
		}
	}
	if requests.Load() != 0 {
		t.Errorf("%d requests sent", requests.Load())
	}
}

func TestHelp_HelpWordsEqualsDashDashHelp(t *testing.T) {
	requests := helpEnv(t)
	for _, words := range [][]string{{"probe", "get"}, {"probe", "admin", "purge"}, {"version"}, {"probe"}, {"help"}} {
		a := runHelp(t, append([]string{"help"}, words...)...)
		b := runHelp(t, append(slices.Clone(words), "--help")...)
		if a.code != 0 || b.code != 0 || a.stderr != "" || b.stderr != "" {
			t.Errorf("%v: exits %d/%d, stderr %q/%q", words, a.code, b.code, a.stderr, b.stderr)
		}
		if a.stdout != b.stdout {
			t.Errorf("%v: help differs:\n--- help ---\n%s\n--- --help ---\n%s", words, a.stdout, b.stdout)
		}
		if words[0] == "probe" && len(words) == 2 {
			for _, w := range []string{"gets a thing", "name", "--limit", "50", "--endpoint-url", "--api-key", "--json"} {
				if !strings.Contains(a.stdout, w) {
					t.Errorf("probe get help does not name %q:\n%s", w, a.stdout)
				}
			}
		}
	}
	if requests.Load() != 0 {
		t.Errorf("%d requests sent", requests.Load())
	}
}

// helpDoc decodes a JSON help document, failing unless it is indented by 2
// spaces and stderr is empty.
func helpDoc(t *testing.T, r cliResult) map[string]any {
	t.Helper()
	if r.code != 0 || r.stderr != "" {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	var d map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &d); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, r.stdout)
	}
	if r.stdout != reindent(t, r.stdout) {
		t.Errorf("stdout is not indented by 2 spaces:\n%s", r.stdout)
	}
	return d
}

func keys(d map[string]any) []string {
	var k []string
	for name := range d {
		k = append(k, name)
	}
	slices.Sort(k)
	return k
}

func flagOf(t *testing.T, d map[string]any, name string) map[string]any {
	t.Helper()
	for _, f := range d["flags"].([]any) {
		if m := f.(map[string]any); m["name"] == name {
			return m
		}
	}
	t.Fatalf("no flag %q in %v", name, d["flags"])
	return nil
}

func names(list []any, member string) []string {
	var out []string
	for _, e := range list {
		out = append(out, e.(map[string]any)[member].(string))
	}
	return out
}

func TestHelp_JSONDescribesACommand(t *testing.T) {
	helpEnv(t)
	d := helpDoc(t, runHelp(t, "help", "probe", "get", "--json"))
	if got := keys(d); !reflect.DeepEqual(got, []string{"args", "auth", "description", "flags", "method", "name", "path"}) {
		t.Errorf("members %v", got)
	}
	if d["name"] != "probe get" || d["description"] != "gets a thing" || d["method"] != "GET" || d["path"] != "/things/:name" || d["auth"] != "user" {
		t.Errorf("probe get = %v", d)
	}
	if !reflect.DeepEqual(d["args"], []any{map[string]any{"name": "name", "required": true}}) {
		t.Errorf("args %v", d["args"])
	}
	if got := names(d["flags"].([]any), "name"); !reflect.DeepEqual(got, []string{"api-key", "endpoint-url", "json", "limit"}) {
		t.Errorf("flags %v", got)
	}
	want := map[string]any{"name": "limit", "type": "int", "required": false, "default": "50", "description": "the most things to return"}
	if got := flagOf(t, d, "limit"); !reflect.DeepEqual(got, want) {
		t.Errorf("limit = %v", got)
	}

	p := helpDoc(t, runHelp(t, "probe", "admin", "purge", "--help", "--json"))
	confirm := flagOf(t, p, "confirm")
	if p["name"] != "probe admin purge" || p["auth"] != "admin" || confirm["required"] != true || confirm["type"] != "bool" ||
		!reflect.DeepEqual(p["args"], []any{map[string]any{"name": "reason", "required": false}}) {
		t.Errorf("probe admin purge = %v", p)
	}

	v := helpDoc(t, runHelp(t, "help", "version", "--json"))
	if v["method"] != "GET" || v["path"] != "/version" || v["auth"] != "none" {
		t.Errorf("version = %v", v)
	}
	h := helpDoc(t, runHelp(t, "help", "help", "--json"))
	if m, ok := h["method"]; !ok || m != nil {
		t.Errorf("help method = %v", h["method"])
	}
	if p, ok := h["path"]; !ok || p != nil || h["auth"] != "none" {
		t.Errorf("help = %v", h)
	}
}

func TestHelp_JSONOfAGroupListsEveryCommandUnderIt(t *testing.T) {
	helpEnv(t)
	g := helpDoc(t, runHelp(t, "help", "probe", "--json"))
	if got := keys(g); !reflect.DeepEqual(got, []string{"commands"}) {
		t.Fatalf("members %v", got)
	}
	cmds := g["commands"].([]any)
	if got := names(cmds, "name"); !reflect.DeepEqual(got, []string{"probe admin purge", "probe get"}) {
		t.Errorf("commands %v", got)
	}
	single := helpDoc(t, runHelp(t, "help", "probe", "get", "--json"))
	if len(cmds) == 2 && !reflect.DeepEqual(cmds[1], any(single)) {
		t.Errorf("group element %v, single %v", cmds[1], single)
	}

	r := helpDoc(t, runHelp(t, "help", "--json"))
	all := names(r["commands"].([]any), "name")
	if !slices.IsSorted(all) {
		t.Errorf("not sorted: %v", all)
	}
	for _, w := range []string{"help", "version", "login", "probe get", "probe admin purge"} {
		if !slices.Contains(all, w) {
			t.Errorf("no %q in %v", w, all)
		}
	}
	for _, w := range []string{"probe", "probe admin"} {
		if slices.Contains(all, w) {
			t.Errorf("group %q in %v", w, all)
		}
	}
	if rr := helpDoc(t, runHelp(t, "--help", "--json")); !reflect.DeepEqual(rr, r) {
		t.Errorf("parley --help --json differs from parley help --json")
	}
}

func TestHelp_UnknownWordsAreAUsageError(t *testing.T) {
	helpEnv(t)
	for _, args := range [][]string{{"help", "nosuch"}, {"help", "probe", "nosuch"}, {"help", "nosuch", "--json"}} {
		r := runHelp(t, args...)
		if r.code != 2 || r.stdout != "" {
			t.Errorf("%v: exit %d, stdout %q", args, r.code, r.stdout)
			continue
		}
		e := r.envelope(t)
		if e["code"] != float64(0) || e["error_type"] != "usage_error" || !strings.Contains(e["message"].(string), "nosuch") {
			t.Errorf("%v: envelope %v", args, e)
		}
	}
}

func TestHelp_EveryCommandOfTheRealTreeDeclaresItself(t *testing.T) {
	cliEnv(t)
	r := runCLI(t, "", "help", "--json")
	d := helpDoc(t, r)
	cmds := d["commands"].([]any)
	if len(cmds) < 2 {
		t.Fatalf("%d commands", len(cmds))
	}
	methods := []string{"GET", "POST", "PUT", "PATCH", "DELETE"}
	for _, e := range cmds {
		c := e.(map[string]any)
		if c["description"] == "" {
			t.Errorf("%v has no description", c["name"])
		}
		if !slices.Contains([]any{"none", "user", "admin"}, c["auth"]) {
			t.Errorf("%v has auth %v", c["name"], c["auth"])
		}
		m, mok := c["method"].(string)
		p, pok := c["path"].(string)
		bothNull := c["method"] == nil && c["path"] == nil
		if !bothNull && !(mok && pok && slices.Contains(methods, m) && strings.HasPrefix(p, "/")) {
			t.Errorf("%v has method %v and path %v", c["name"], c["method"], c["path"])
		}
		if c["auth"] == "none" && c["name"] == "version" {
			if v := runCLI(t, "", "version"); v.code != 0 {
				t.Errorf("version without settings: exit %d, %s", v.code, v.stderr)
			}
		}
	}
}
