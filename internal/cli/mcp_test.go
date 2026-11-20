package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// listedTool is one tool as parley mcp --list prints it.
type listedTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema struct {
		Type                 string                    `json:"type"`
		Properties           map[string]map[string]any `json:"properties"`
		Required             []string                  `json:"required"`
		AdditionalProperties *bool                     `json:"additionalProperties"`
	} `json:"input_schema"`
}

// mcpList runs parley mcp --list with args and opts, in an empty config
// dir and without an endpoint or a credential, and returns the tools it
// prints. It fails the test when the command does not succeed.
func mcpList(t *testing.T, opts options, args ...string) []listedTool {
	t.Helper()
	code, stdout, stderr := runMCP(t, opts, append([]string{"--list"}, args...)...)
	if code != 0 || stderr != "" {
		t.Fatalf("mcp --list %v: exit status %d, stderr %q; want 0 and nothing", args, code, stderr)
	}
	var tools []listedTool
	oneJSONDocument(t, stdout, &tools)
	return tools
}

// runMCP runs parley mcp with args and opts with no setting of the
// process's own environment in reach. A test that calls it must not run in
// parallel.
func runMCP(t *testing.T, opts options, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("PARLEY_CONFIG_DIR", t.TempDir())
	unsetEnv(t, "PARLEY_ENDPOINT_URL")
	unsetEnv(t, "PARLEY_API_KEY")
	return runCLIWith(t, opts, append([]string{"mcp"}, args...)...)
}

// toolNames are the names of tools, in order.
func toolNames(tools []listedTool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}

// toolNamed is the tool of tools called name; it fails the test when there
// is none.
func toolNamed(t *testing.T, tools []listedTool, name string) listedTool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("no tool %s among %v", name, toolNames(tools))
	return listedTool{}
}

// helpCommands is the JSON help of every command of the tree opts builds.
func helpCommands(t *testing.T, opts options) []commandHelp {
	t.Helper()
	code, stdout, _ := runCLIWith(t, opts, "help", "--json")
	if code != 0 {
		t.Fatalf("help --json: exit status %d", code)
	}
	var r commandList
	oneJSONDocument(t, stdout, &r)
	return r.Commands
}

// toolNameOf is the name of the tool of the command called name.
func toolNameOf(name string) string {
	return strings.ReplaceAll(strings.ReplaceAll(name, " ", "_"), "-", "_")
}

// commandsOf are the tool names of the commands of cmds that keep holds
// for, sorted.
func commandsOf(cmds []commandHelp, keep func(group string, c commandHelp) bool) []string {
	var names []string
	for _, c := range cmds {
		if group, _, _ := strings.Cut(c.Name, " "); keep(group, c) {
			names = append(names, toolNameOf(c.Name))
		}
	}
	slices.Sort(names)
	return names
}

// reads tells whether the command c calls a GET route.
func reads(c commandHelp) bool { return c.Method != nil && *c.Method == http.MethodGet }

// neverServedGroups are the groups parley mcp serves no command of.
var neverServedGroups = []string{"admin", "keys", "tokens", "secrets", "login", "credential-helper", "help", "version", "mcp"}

// nameableGroups are the groups of cmds that --groups may name, sorted.
func nameableGroups(cmds []commandHelp) []string {
	var groups []string
	for _, c := range cmds {
		group, _, _ := strings.Cut(c.Name, " ")
		if !slices.Contains(neverServedGroups, group) && !slices.Contains(groups, group) {
			groups = append(groups, group)
		}
	}
	slices.Sort(groups)
	return groups
}

// wantUsageError checks that a run ended as a usage error naming each of
// names, with nothing on stdout.
func wantUsageError(t *testing.T, code int, stdout, stderr string, names ...string) {
	t.Helper()
	var env cliEnvelope
	if code != exitClientError || stdout != "" || json.Unmarshal([]byte(stderr), &env) != nil || env.Error.ErrorType != usageErrorType {
		t.Fatalf("exit status %d, stdout %q, stderr %q; want 2, nothing and one usage_error envelope", code, stdout, stderr)
	}
	for _, name := range names {
		if !strings.Contains(env.Error.Message, name) {
			t.Errorf("the message %q does not name %q", env.Error.Message, name)
		}
	}
}

// probeTyped builds "probe typed <KEY=VALUE> [note]": POST /typed, auth
// user, with a flag of every type a command's flag has, one of them
// required and one marked as running until interrupted.
func probeTyped() *cobra.Command {
	return declare(command{
		Name:        "typed",
		Description: "Take a flag of every type",
		Method:      http.MethodPost,
		Path:        "/typed",
		Args:        []argSpec{{Name: "KEY=VALUE", Required: true}, {Name: "note"}},
		Auth:        authUser,
		Flags: func(fs *pflag.FlagSet) {
			fs.Bool("dry-run", false, "only say what would be done")
			fs.String("title", "", "the title")
			fs.StringArray("put", nil, "a file to put, repeatable")
			fs.Int("limit", 0, "the most to take")
			fs.Int64("max-bytes", 0, "the most bytes to take")
			fs.Duration("ttl", 0, "how long it lives")
			fs.String("reason", "", "why")
			cobra.MarkFlagRequired(fs, "reason")
			fs.Bool("follow", false, "go on until interrupted")
			markUntilInterrupted(fs, "follow")
		},
	})
}

// probeSecret builds "probe secret": a command that shows a secret once.
func probeSecret() *cobra.Command {
	return declare(command{
		Name: "secret", Description: "Show a secret once", Method: http.MethodPost, Path: "/secrets", Auth: authUser, ShowsOnce: true,
	})
}

// probeTerminal builds "probe terminal": a command that needs a terminal.
func probeTerminal() *cobra.Command {
	return declare(command{
		Name: "terminal", Description: "Open a terminal", Method: http.MethodGet, Path: "/terminal", Auth: authUser, Interactive: true,
	})
}

// probePublic builds "probe public": a command that needs no credential.
func probePublic() *cobra.Command {
	return declare(command{
		Name: "public", Description: "Read what everyone may", Method: http.MethodGet, Path: "/public", Auth: authNone,
		Run: func(*call) error { return nil },
	})
}

func TestMCPList_PrintsTheDefaultSelectionAsTools(t *testing.T) {
	hub := newStubHub(t)
	cmds := helpCommands(t, defaultOptions())
	tools := mcpList(t, defaultOptions(), "--no-code-mode")

	defaults := []string{"activity", "change", "claim", "fork", "land", "sandbox", "scm"}
	want := commandsOf(cmds, func(group string, c commandHelp) bool {
		if c.Name == "sandbox shell" {
			return false
		}
		return slices.Contains(defaults, group) || group == "workspace" && reads(c)
	})
	if got := toolNames(tools); !slices.Equal(got, want) {
		t.Errorf("the default selection is\n%v\nwant the commands of %v and the reading ones of workspace, by name:\n%v", got, defaults, want)
	}
	for _, name := range []string{"change_land", "scm_commit", "fork_create", "activity", "sandbox_exec", "workspace_get"} {
		toolNamed(t, tools, name)
	}
	if slices.Contains(toolNames(tools), "workspace_delete") {
		t.Error("the default selection holds workspace_delete")
	}
	if len(tools) > 80 {
		t.Errorf("the default selection has %d tools, want at most 80", len(tools))
	}
	if n := len(hub.requests); n != 0 {
		t.Errorf("mcp --list sent %d requests, want none", n)
	}
}

func TestMCPList_ServesTheGroupsNamed(t *testing.T) {
	cmds := helpCommands(t, defaultOptions())
	in := func(groups ...string) func(string, commandHelp) bool {
		return func(group string, _ commandHelp) bool { return slices.Contains(groups, group) }
	}
	for _, row := range []struct {
		groups string
		want   []string
	}{
		{"scm", commandsOf(cmds, in("scm"))},
		{"scm:read", commandsOf(cmds, func(g string, c commandHelp) bool { return g == "scm" && reads(c) })},
		{"issue,pr:read", commandsOf(cmds, func(g string, c commandHelp) bool { return g == "issue" || g == "pr" && reads(c) })},
		{"workspace", commandsOf(cmds, in("workspace"))},
		{" claim , land ", commandsOf(cmds, in("claim", "land"))},
	} {
		t.Run(row.groups, func(t *testing.T) {
			got := toolNames(mcpList(t, defaultOptions(), "--no-code-mode", "--groups", row.groups))
			if !slices.Equal(got, row.want) || len(got) == 0 {
				t.Errorf("--groups %s serves\n%v\nwant\n%v", row.groups, got, row.want)
			}
		})
	}
	if got := toolNames(mcpList(t, defaultOptions(), "--no-code-mode", "--groups", "scm:read")); slices.Contains(got, "scm_commit") {
		t.Error("scm:read serves scm_commit")
	}
	if got := toolNames(mcpList(t, defaultOptions(), "--no-code-mode", "--groups", "workspace")); !slices.Contains(got, "workspace_delete") {
		t.Error("workspace does not serve workspace_delete")
	}
}

func TestMCPList_NeverServesWhatAdministersTheHubShowsASecretOrNeedsATerminal(t *testing.T) {
	opts := withProbeGroup(defaultOptions(), probeGet, probeSecret, probeTerminal, probePublic, probeAdmin)
	cmds := helpCommands(t, opts)
	groups := nameableGroups(cmds)
	tools := mcpList(t, opts, "--no-code-mode", "--groups", strings.Join(groups, ","))

	byTool := map[string]commandHelp{}
	for _, c := range cmds {
		byTool[toolNameOf(c.Name)] = c
	}
	for _, tool := range tools {
		c, known := byTool[tool.Name]
		group, _, _ := strings.Cut(c.Name, " ")
		if !known || c.Auth != authUser || slices.Contains(neverServedGroups, group) {
			t.Errorf("the tool %s is served; its command %q has the auth %q", tool.Name, c.Name, c.Auth)
		}
	}
	names := toolNames(tools)
	for _, never := range []string{"sandbox_shell", "events_tail", "mcp", "probe_secret", "probe_terminal", "probe_public", "probe_admin_purge"} {
		if slices.Contains(names, never) {
			t.Errorf("%s is served", never)
		}
	}
	for _, served := range []string{"probe_get", "events_list", "sandbox_logs", "webhook_create"} {
		if !slices.Contains(names, served) {
			t.Errorf("%s is not served", served)
		}
	}
}

func TestMCP_RefusesAGroupItNeverServes(t *testing.T) {
	for _, group := range neverServedGroups {
		for _, row := range []struct{ name, groups string }{
			{"alone", group}, {"with :read", group + ":read"}, {"after a served group", "scm," + group},
		} {
			for _, list := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s %s, --list %v", group, row.name, list), func(t *testing.T) {
					hub := newStubHub(t)
					args := onHub(hub.URL, "--groups", row.groups)
					if list {
						args = append(args, "--list")
					}
					code, stdout, stderr := runMCP(t, defaultOptions(), args...)
					wantUsageError(t, code, stdout, stderr, group)
					if n := len(hub.requests); n != 0 {
						t.Errorf("%d requests were sent, want none", n)
					}
				})
			}
		}
	}
}

func TestMCP_RefusesAGroupItDoesNotKnow(t *testing.T) {
	for _, groups := range []string{"nope", "scm,,change", "scm:write", "", "scm:", ","} {
		t.Run(groups, func(t *testing.T) {
			code, stdout, stderr := runMCP(t, defaultOptions(), "--list", "--groups", groups)
			wantUsageError(t, code, stdout, stderr, "scm", "change", "workspace")
			if strings.Contains(stderr, "admin") {
				t.Errorf("the message %s lists admin among the groups to name", stderr)
			}
		})
	}
}

func TestMCPList_NarrowsTheSelectionToReadingCommandsWithReadOnly(t *testing.T) {
	names := toolNames(mcpList(t, defaultOptions(), "--no-code-mode", "--read-only"))
	for _, name := range []string{"change_show", "scm_log", "activity", "workspace_get"} {
		if !slices.Contains(names, name) {
			t.Errorf("--read-only does not serve %s", name)
		}
	}
	for _, name := range []string{"change_land", "scm_commit", "fork_create", "sandbox_exec", "claim_create"} {
		if slices.Contains(names, name) {
			t.Errorf("--read-only serves %s", name)
		}
	}
	narrowed := toolNames(mcpList(t, defaultOptions(), "--no-code-mode", "--read-only", "--groups", "scm"))
	if want := toolNames(mcpList(t, defaultOptions(), "--no-code-mode", "--groups", "scm:read")); !slices.Equal(narrowed, want) {
		t.Errorf("--read-only --groups scm serves %v, want what scm:read serves, %v", narrowed, want)
	}
}

// probeMany builds n commands "probe c<i>" of the group probe.
func probeMany(n int) []func() *cobra.Command {
	builders := make([]func() *cobra.Command, 0, n)
	for i := range n {
		builders = append(builders, func() *cobra.Command {
			return declare(command{
				Name: fmt.Sprintf("c%03d", i), Description: "One of many", Method: http.MethodGet, Path: "/many", Auth: authUser,
			})
		})
	}
	return builders
}

func TestMCP_RefusesMoreThan150Tools(t *testing.T) {
	fits := withProbeGroup(defaultOptions(), probeMany(150)...)
	if got := len(mcpList(t, fits, "--no-code-mode", "--groups", "probe")); got != 150 {
		t.Errorf("a selection of 150 commands lists %d tools, want 150", got)
	}
	for _, list := range []bool{true, false} {
		tooMany := withProbeGroup(defaultOptions(), probeMany(151)...)
		hub := newStubHub(t)
		args := onHub(hub.URL, "--no-code-mode", "--groups", "probe")
		if list {
			args = append(args, "--list")
		}
		code, stdout, stderr := runMCP(t, tooMany, args...)
		wantUsageError(t, code, stdout, stderr, "151", "--groups")
	}
}

func TestMCPList_ServesACommandOfAGroupWithNothingSaidAboutIt(t *testing.T) {
	opts := withProbes(defaultOptions(), probeTyped)
	tools := mcpList(t, opts, "--no-code-mode", "--groups", "scm,probe")
	names := toolNames(tools)
	for _, name := range []string{"probe_get", "probe_count", "probe_typed", "scm_log"} {
		if !slices.Contains(names, name) {
			t.Errorf("%s is not served with --groups scm,probe: %v", name, names)
		}
	}
	get := toolNamed(t, tools, "probe_get")
	if get.Description != "Get a thing" || get.InputSchema.Properties["name"]["type"] != "string" || get.InputSchema.Properties["limit"]["type"] != "integer" ||
		!slices.Equal(get.InputSchema.Required, []string{"name"}) {
		t.Errorf("probe_get is %+v; want the command's description, its argument name required and its flag limit an integer", get)
	}
}

func TestMCPList_NamesAndDescribesEachToolAfterItsCommand(t *testing.T) {
	cmds := helpCommands(t, defaultOptions())
	tools := mcpList(t, defaultOptions(), "--no-code-mode", "--groups", "change,scm,sandbox,webhook,workspace")
	byTool := map[string]commandHelp{}
	for _, c := range cmds {
		byTool[toolNameOf(c.Name)] = c
	}
	seen := map[string]bool{}
	for _, tool := range tools {
		c, known := byTool[tool.Name]
		if !known || tool.Description != c.Description || tool.Description == "" {
			t.Errorf("the tool %s has the description %q; want that of its command, %q", tool.Name, tool.Description, c.Description)
		}
		if strings.ContainsAny(tool.Name, " -") || seen[tool.Name] {
			t.Errorf("the tool name %q has a space or a hyphen, or is given twice", tool.Name)
		}
		seen[tool.Name] = true
	}
	for _, name := range []string{"change_land", "sandbox_rule_delete", "scm_commit"} {
		toolNamed(t, tools, name)
	}
}

func TestMCPList_GivesEachToolItsCommandsArgumentsAndFlagsAsParameters(t *testing.T) {
	opts := withProbes(defaultOptions(), probeTyped)
	tools := mcpList(t, opts, "--no-code-mode", "--groups", "change,scm,sandbox,probe")
	type param struct{ name, typ string }
	for _, row := range []struct {
		tool     string
		params   []param
		required []string
	}{
		{"change_land", []param{{"slug", "string"}, {"number", "string"}, {"wait", "boolean"}, {"stack", "boolean"}, {"timeout", "string"}, {"expected_version", "string"}},
			[]string{"number", "slug"}},
		{"scm_commit", []param{{"slug", "string"}, {"put", "array"}, {"delete", "array"}, {"move", "array"}, {"message", "string"}, {"draft", "boolean"}},
			[]string{"branch", "message", "slug"}},
		{"sandbox_upload", []param{{"workspace_slug", "string"}, {"id", "string"}, {"local_path", "string"}, {"sandbox_path", "string"}},
			[]string{"id", "local_path", "sandbox_path", "workspace_slug"}},
		{"probe_typed", []param{{"key_value", "string"}, {"note", "string"}, {"dry_run", "boolean"}, {"title", "string"}, {"put", "array"},
			{"limit", "integer"}, {"max_bytes", "integer"}, {"ttl", "string"}, {"reason", "string"}},
			[]string{"key_value", "reason"}},
	} {
		t.Run(row.tool, func(t *testing.T) {
			tool := toolNamed(t, tools, row.tool)
			s := tool.InputSchema
			if s.Type != "object" || s.AdditionalProperties == nil || *s.AdditionalProperties {
				t.Errorf("the input schema is of the type %q with additionalProperties %v; want an object that allows no other parameter", s.Type, s.AdditionalProperties)
			}
			for _, p := range row.params {
				got := s.Properties[p.name]
				if got == nil || got["type"] != p.typ {
					t.Errorf("the parameter %s is %v; want the type %s", p.name, got, p.typ)
					continue
				}
				if d, _ := got["description"].(string); d == "" {
					t.Errorf("the parameter %s has no description", p.name)
				}
				if items, _ := got["items"].(map[string]any); p.typ == "array" && items["type"] != "string" {
					t.Errorf("the parameter %s is an array of %v; want strings", p.name, items)
				}
			}
			required := slices.Clone(s.Required)
			slices.Sort(required)
			if !slices.Equal(required, row.required) {
				t.Errorf("required = %v, want %v", required, row.required)
			}
		})
	}
	if d := toolNamed(t, tools, "probe_typed").InputSchema.Properties["title"]["description"]; d != "the title" {
		t.Errorf("the parameter title is described as %q, want the flag's description", d)
	}
}

func TestMCPList_LeavesOutTheFlagsNoToolCanUse(t *testing.T) {
	opts := withProbes(defaultOptions(), probeTyped)
	tools := mcpList(t, opts, "--no-code-mode", "--groups", strings.Join(nameableGroups(helpCommands(t, opts)), ","))
	for _, tool := range tools {
		for _, left := range []string{"api_key", "endpoint_url", "json", "poll_interval", "help"} {
			if _, has := tool.InputSchema.Properties[left]; has {
				t.Errorf("the tool %s has the parameter %s", tool.Name, left)
			}
		}
	}
	has := func(tool, param string) bool {
		_, ok := toolNamed(t, tools, tool).InputSchema.Properties[param]
		return ok
	}
	if has("sandbox_logs", "follow") || has("probe_typed", "follow") {
		t.Error("a flag that makes its command run until interrupted is a parameter")
	}
	if !has("scm_log", "follow") || !has("change_land", "wait") || !has("change_land", "timeout") || !has("sandbox_logs", "tail") {
		t.Error("follow of scm_log, wait and timeout of change_land or tail of sandbox_logs is missing")
	}
}
