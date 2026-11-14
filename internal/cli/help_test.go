package cli

import (
	"encoding/json"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// helpEnv unsets PARLEY_ENDPOINT_URL and PARLEY_API_KEY, points
// PARLEY_CONFIG_DIR at a directory whose config.toml is not TOML, so that
// reading it would fail, and returns a stub hub to count requests with. A
// test that calls it must not run in parallel.
func helpEnv(t *testing.T) *stubHub {
	t.Helper()
	dir := t.TempDir()
	writeConfigFile(t, dir, "endpoint_url = = not TOML\n")
	t.Setenv("PARLEY_CONFIG_DIR", dir)
	unsetEnv(t, "PARLEY_ENDPOINT_URL")
	unsetEnv(t, "PARLEY_API_KEY")
	return newStubHub(t)
}

// runHelpProbes runs parley with args and the group "probe" holding
// exactly "probe get <name>" and "probe admin purge [reason]".
func runHelpProbes(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return runCLIWith(t, withProbeGroup(defaultOptions(), probeGet, probeAdmin), args...)
}

// helpJSON runs parley with args and the help probes, checks that it
// succeeded with one JSON document indented by 2 spaces on stdout and
// nothing on stderr, and returns stdout.
func helpJSON(t *testing.T, args ...string) string {
	t.Helper()
	code, stdout, stderr := runHelpProbes(t, args...)
	if code != 0 || stderr != "" {
		t.Fatalf("parley %v: exit status %d, stderr %q; want 0 and nothing", args, code, stderr)
	}
	if want := indented(t, stdout); stdout != want {
		t.Errorf("parley %v: stdout is not indented by 2 spaces:\n%s", args, stdout)
	}
	return stdout
}

// flagNamed is the flag name in the JSON help c.
func flagNamed(c commandHelp, name string) flagHelp {
	for _, f := range c.Flags {
		if f.Name == name {
			return f
		}
	}
	return flagHelp{}
}

// orNull is *s quoted, or null.
func orNull(s *string) string {
	if s == nil {
		return "null"
	}
	return strconv.Quote(*s)
}

func flagNames(c commandHelp) []string {
	names := []string{}
	for _, f := range c.Flags {
		names = append(names, f.Name)
	}
	return names
}

func commandNames(commands []commandHelp) []string {
	names := []string{}
	for _, c := range commands {
		names = append(names, c.Name)
	}
	return names
}

// memberNames is the sorted names of the members of the JSON object doc.
func memberNames(t *testing.T, doc string) []string {
	t.Helper()
	var members map[string]json.RawMessage
	oneJSONDocument(t, doc, &members)
	return sortedKeys(members)
}

// The test sets environment variables, so it must not run in parallel.
func TestHelp_PrintsTheHelpOfTheRootOrAGroupGivenNoCommand(t *testing.T) {
	tests := []struct {
		args []string
		// subcommands maps each direct subcommand to its description.
		subcommands map[string]string
	}{
		{nil, map[string]string{
			"admin":   "Administer the hub's users, organizations, API keys and access tokens",
			"keys":    "List, refresh and revoke your API keys",
			"tokens":  "Create, read, change and revoke your personal access tokens",
			"help":    "Print the help of parley or of a command",
			"login":   "Sign in to the hub and save the hub URL and the credential",
			"orgs":    "List and read the organizations you belong to",
			"secrets": "Create, list, update and delete secrets, whose values the hub never shows",
			"user":    "Read and update your own profile",
			"version": "Print the client's build and, given a hub URL, the hub's build",
		}},
		{[]string{"secrets"}, map[string]string{
			"create": "Create secrets in your user scope, an organization's or a workspace's",
			"list":   "List the keys of a scope's secrets, every page of them",
			"update": "Replace the value of one secret",
			"delete": "Delete one secret",
		}},
		{[]string{"probe"}, map[string]string{"get": "Get a thing", "admin": "Administer the things"}},
		{[]string{"probe", "admin"}, map[string]string{"purge": "Purge the things"}},
	}
	for _, tt := range tests {
		// With the settings set as well, so that help would have the hub to
		// send to if it sent anything.
		for _, settings := range []bool{false, true} {
			name := "parley " + strings.Join(tt.args, " ")
			if settings {
				name += " with the settings set"
			}
			t.Run(name, func(t *testing.T) {
				hub := helpEnv(t)
				if settings {
					t.Setenv("PARLEY_ENDPOINT_URL", hub.URL)
					t.Setenv("PARLEY_API_KEY", "k")
				}

				code, stdout, stderr := runHelpProbes(t, tt.args...)

				if code != 0 {
					t.Errorf("exit status = %d, want 0", code)
				}
				for sub, description := range tt.subcommands {
					line := regexp.MustCompile(`(?m)^\s+` + regexp.QuoteMeta(sub) + `\s+` + regexp.QuoteMeta(description) + `$`)
					if !line.MatchString(stdout) {
						t.Errorf("stdout does not list %q with %q:\n%s", sub, description, stdout)
					}
				}
				if stderr != "" {
					t.Errorf("stderr = %q, want it empty", stderr)
				}
				if n := len(hub.recorded()); n != 0 {
					t.Errorf("the hub got %d requests, want none", n)
				}
			})
		}
	}
}

// The test sets environment variables, so it must not run in parallel.
func TestHelp_PrintsTheSameTextForTheHelpCommandAndTheHelpFlag(t *testing.T) {
	tests := []struct {
		words []string
		// holds are texts the help must contain.
		holds []string
	}{
		{[]string{"probe", "get"}, []string{"Get a thing", "name", "--limit", "50", "--endpoint-url", "--api-key", "--json"}},
		{[]string{"probe", "admin", "purge"}, []string{"Purge the things", "reason", "--confirm"}},
		{[]string{"version"}, []string{"Print the client's build"}},
		{[]string{"probe"}, []string{"Commands that exist only in tests", "get", "admin"}},
		{[]string{"help"}, []string{"Print the help of parley or of a command", "command"}},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.words, " "), func(t *testing.T) {
			hub := helpEnv(t)

			codeA, stdoutA, stderrA := runHelpProbes(t, append([]string{"help"}, tt.words...)...)
			codeB, stdoutB, stderrB := runHelpProbes(t, append(slices.Clone(tt.words), "--help")...)

			if codeA != 0 || codeB != 0 {
				t.Errorf("exit statuses = %d and %d, want 0 and 0", codeA, codeB)
			}
			if stdoutA != stdoutB {
				t.Errorf("parley help %s printed\n%s\nparley %[1]s --help printed\n%s", strings.Join(tt.words, " "), stdoutA, stdoutB)
			}
			for _, s := range tt.holds {
				if !strings.Contains(stdoutA, s) {
					t.Errorf("the help does not contain %q:\n%s", s, stdoutA)
				}
			}
			if stderrA != "" || stderrB != "" {
				t.Errorf("stderr = %q and %q, want both empty", stderrA, stderrB)
			}
			if n := len(hub.recorded()); n != 0 {
				t.Errorf("the hub got %d requests, want none", n)
			}
		})
	}
}

// The test sets environment variables, so it must not run in parallel.
func TestHelp_DescribesACommandAsJSON(t *testing.T) {
	hub := helpEnv(t)
	members := []string{"args", "auth", "description", "flags", "method", "name", "path"}

	out := helpJSON(t, "help", "probe", "get", "--json")
	if got := memberNames(t, out); !slices.Equal(got, members) {
		t.Errorf("members = %v, want %v", got, members)
	}
	var d commandHelp
	oneJSONDocument(t, out, &d)
	if d.Name != "probe get" || d.Description != "Get a thing" || d.Method == nil || *d.Method != "GET" ||
		d.Path == nil || *d.Path != "/things/:name" || d.Auth != "user" {
		t.Errorf("probe get = %s", out)
	}
	if want := []argSpec{{Name: "name", Required: true}}; !slices.Equal(d.Args, want) {
		t.Errorf("args = %+v, want %+v", d.Args, want)
	}
	if got, want := flagNames(d), []string{"api-key", "endpoint-url", "json", "limit"}; !slices.Equal(got, want) {
		t.Errorf("flags = %v, want %v", got, want)
	}
	if got, want := flagNamed(d, "limit"), (flagHelp{"limit", "int", false, "50", "the most things to get"}); got != want {
		t.Errorf("flag limit = %+v, want %+v", got, want)
	}
	if got := flagNamed(d, "json"); got.Type != "bool" || got.Default != "false" || got.Required || got.Description == "" {
		t.Errorf("flag json = %+v, want a bool, not required, default false, described", got)
	}

	var p commandHelp
	oneJSONDocument(t, helpJSON(t, "probe", "admin", "purge", "--help", "--json"), &p)
	if p.Name != "probe admin purge" || p.Auth != "admin" || p.Method == nil || *p.Method != "DELETE" {
		t.Errorf("probe admin purge = %+v", p)
	}
	if want := []argSpec{{Name: "reason", Required: false}}; !slices.Equal(p.Args, want) {
		t.Errorf("args = %+v, want %+v", p.Args, want)
	}
	if got := flagNamed(p, "confirm"); !got.Required || got.Type != "bool" {
		t.Errorf("flag confirm = %+v, want a required bool", got)
	}
	if got, want := flagNames(p), []string{"api-key", "confirm", "endpoint-url", "json"}; !slices.Equal(got, want) {
		t.Errorf("flags = %v, want %v", got, want)
	}

	var v commandHelp
	oneJSONDocument(t, helpJSON(t, "help", "version", "--json"), &v)
	if v.Method == nil || *v.Method != "GET" || v.Path == nil || *v.Path != "/version" || v.Auth != "none" {
		t.Errorf("version = %+v", v)
	}
	if v.Args == nil || len(v.Args) != 0 {
		t.Errorf("version args = %#v, want an empty array", v.Args)
	}

	out = helpJSON(t, "help", "help", "--json")
	var h commandHelp
	oneJSONDocument(t, out, &h)
	if h.Method != nil || h.Path != nil || h.Auth != "none" {
		t.Errorf("help = %s, want method and path null and auth none", out)
	}
	if !strings.Contains(out, `"method": null`) || !strings.Contains(out, `"path": null`) {
		t.Errorf("help = %s, want method and path written as null", out)
	}
	if n := len(hub.recorded()); n != 0 {
		t.Errorf("the hub got %d requests, want none", n)
	}
}

// The test sets environment variables, so it must not run in parallel.
func TestHelp_DescribesEveryCommandUnderAGroupAsJSON(t *testing.T) {
	hub := helpEnv(t)

	out := helpJSON(t, "help", "probe", "--json")
	if got := memberNames(t, out); !slices.Equal(got, []string{"commands"}) {
		t.Errorf("members = %v, want [commands]", got)
	}
	var g struct {
		Commands []json.RawMessage `json:"commands"`
	}
	oneJSONDocument(t, out, &g)
	var commands []commandHelp
	for _, raw := range g.Commands {
		var c commandHelp
		oneJSONDocument(t, string(raw), &c)
		commands = append(commands, c)
		if got := memberNames(t, string(raw)); len(got) != 7 {
			t.Errorf("%s has the members %v, want those of one command", c.Name, got)
		}
	}
	if got, want := commandNames(commands), []string{"probe admin purge", "probe get"}; !slices.Equal(got, want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
	var inGroup, alone any
	oneJSONDocument(t, string(g.Commands[1]), &inGroup)
	oneJSONDocument(t, helpJSON(t, "help", "probe", "get", "--json"), &alone)
	if !reflect.DeepEqual(inGroup, alone) {
		t.Errorf("probe get in the group = %s, alone = %v", g.Commands[1], alone)
	}
	for _, args := range [][]string{{"probe", "--help", "--json"}, {"probe", "--json"}} {
		if got := helpJSON(t, args...); got != out {
			t.Errorf("parley %v printed\n%s\nwant what parley help probe --json printed\n%s", args, got, out)
		}
	}

	root := helpJSON(t, "help", "--json")
	var r struct {
		Commands []commandHelp `json:"commands"`
	}
	oneJSONDocument(t, root, &r)
	names := commandNames(r.Commands)
	if !slices.IsSorted(names) {
		t.Errorf("commands = %v, want them sorted by name", names)
	}
	for _, name := range []string{"help", "version", "probe get", "probe admin purge"} {
		if !slices.Contains(names, name) {
			t.Errorf("commands = %v, want %q among them", names, name)
		}
	}
	for _, name := range []string{"probe", "probe admin"} {
		if slices.Contains(names, name) {
			t.Errorf("commands = %v, want no group %q among them", names, name)
		}
	}
	for _, args := range [][]string{{"--help", "--json"}, {"--json"}} {
		if got := helpJSON(t, args...); got != root {
			t.Errorf("parley %v printed\n%s\nwant what parley help --json printed\n%s", args, got, root)
		}
	}
	if n := len(hub.recorded()); n != 0 {
		t.Errorf("the hub got %d requests, want none", n)
	}
}

// The test sets environment variables, so it must not run in parallel.
func TestHelp_RefusesWordsThatNameNoCommand(t *testing.T) {
	tests := [][]string{
		{"help", "nosuch"},
		{"help", "probe", "nosuch"},
		{"help", "nosuch", "--json"},
		{"help", "probe", "get", "nosuch"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			hub := helpEnv(t)

			code, stdout, stderr := runHelpProbes(t, args...)

			message := wantClientFailure(t, code, stdout, stderr, usageErrorType)
			if !strings.Contains(message, "nosuch") {
				t.Errorf("message %q does not name nosuch", message)
			}
			if n := len(hub.recorded()); n != 0 {
				t.Errorf("the hub got %d requests, want none", n)
			}
		})
	}
}

// Every command a package adds to parley must be declared: this walks the
// real tree, without probes, so an undeclared command fails it.
//
// The test sets environment variables, so it must not run in parallel.
func TestHelp_DescribesEveryCommandOfTheRealTree(t *testing.T) {
	loginEnv(t)

	code, stdout, stderr := runCLI(t, "help", "--json")

	if code != 0 || stderr != "" {
		t.Fatalf("exit status %d, stderr %q; want 0 and nothing", code, stderr)
	}
	var r struct {
		Commands []commandHelp `json:"commands"`
	}
	oneJSONDocument(t, stdout, &r)
	if len(r.Commands) < 2 {
		t.Errorf("commands = %v, want at least 2", commandNames(r.Commands))
	}
	methods := []string{"GET", "POST", "PUT", "PATCH", "DELETE"}
	for _, c := range r.Commands {
		if c.Description == "" {
			t.Errorf("%q has no description", c.Name)
		}
		if !slices.Contains([]string{"none", "user", "admin"}, string(c.Auth)) {
			t.Errorf("%q has the auth %q, want none, user or admin", c.Name, c.Auth)
		}
		noRoute := c.Method == nil && c.Path == nil
		route := c.Method != nil && c.Path != nil && slices.Contains(methods, *c.Method) && strings.HasPrefix(*c.Path, "/")
		if !noRoute && !route {
			t.Errorf("%q has the method %s and the path %s, want both null or a method and a path beginning with /", c.Name, orNull(c.Method), orNull(c.Path))
		}
	}
}
