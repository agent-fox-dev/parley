package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/agentkit-go/core"
	"github.com/agent-fox-dev/agentkit-go/runner"
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

// mcpDefaults are the settings of parley mcp given no flag but
// --no-code-mode.
func mcpDefaults() mcpSettings { return mcpSettings{groups: mcpDefaultGroups, noCodeMode: true} }

// toolSessionOn is the tool session parley mcp builds for s over the tree
// of opts, with the hub at hubURL and the credential k.
func toolSessionOn(t *testing.T, opts options, hubURL string, s mcpSettings) *toolSession {
	t.Helper()
	// No setting of the process's own environment is in reach of a call.
	t.Setenv("PARLEY_CONFIG_DIR", t.TempDir())
	unsetEnv(t, "PARLEY_ENDPOINT_URL")
	unsetEnv(t, "PARLEY_API_KEY")
	session, err := newToolSession(opts, settings{hubURL: hubURL, credential: "k"}, s)
	if err != nil {
		t.Fatalf("build the tools of %+v: %v", s, err)
	}
	return session
}

// callTool calls the tool name of session with the parameters input.
func callTool(t *testing.T, session *toolSession, name string, input any) toolResult {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return session.call(t.Context(), name, raw)
}

// last is the last request the stub hub got; it fails the test when there
// is none.
func (h *stubHub) last(t *testing.T) stubRequest {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.requests) == 0 {
		t.Fatal("the hub got no request")
	}
	return h.requests[len(h.requests)-1]
}

// requestCount is how many requests the stub hub got.
func (h *stubHub) requestCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.requests)
}

// wantFailed checks that r is a failed result with the error code.
func wantFailed(t *testing.T, r toolResult, code string) {
	t.Helper()
	if r.OK || r.Error != code || r.Detail == "" {
		t.Fatalf("the result is %+v; want a failed one with the error %s and a detail", r, code)
	}
}

const changeThree = `{"number":3,"state":"open","title":"One"}`

func TestToolCall_RunsTheCommandAndGivesWhatItPrints(t *testing.T) {
	hub := newStubHub(t).
		answer(http.MethodGet, "/api/v1/workspaces/demo/changes/3", http.StatusOK, changeThree).
		answer(http.MethodPost, "/api/v1/workspaces/demo/claims", http.StatusCreated, `{"id":"c1"}`).
		answer(http.MethodGet, "/api/v1/things", http.StatusOK, `[1,2]`).
		answer(http.MethodPost, "/api/v1/typed", http.StatusNoContent, "")
	opts := withProbes(defaultOptions(), probeTyped)
	s := mcpDefaults()
	s.groups += ",probe"
	session := toolSessionOn(t, opts, hub.URL, s)

	t.Run("an object is the data", func(t *testing.T) {
		r := callTool(t, session, "change_show", map[string]any{"slug": "demo", "number": "3", "include": "stack"})
		if !r.OK || r.Data["title"] != "One" || r.Data["number"] != 3.0 || r.Error != "" {
			t.Fatalf("the result is %+v; want the hub's object as data", r)
		}
		byTool := hub.last(t)
		if code, _, stderr := runCLIWith(t, opts, onHub(hub.URL, "change", "show", "demo", "3", "--include", "stack")...); code != 0 {
			t.Fatalf("the shell command: exit status %d, %s", code, stderr)
		}
		byShell := hub.last(t)
		if byTool.method != byShell.method || byTool.rawPath != byShell.rawPath || byTool.query != byShell.query ||
			byTool.header.Get("Authorization") != byShell.header.Get("Authorization") || byTool.header.Get("Authorization") == "" {
			t.Errorf("the tool sent %s %s?%s with %q; the shell command sends %s %s?%s with %q", byTool.method, byTool.rawPath, byTool.query,
				byTool.header.Get("Authorization"), byShell.method, byShell.rawPath, byShell.query, byShell.header.Get("Authorization"))
		}
	})
	t.Run("a body is the one the shell command sends", func(t *testing.T) {
		r := callTool(t, session, "claim_create", map[string]any{"slug": "demo", "intent": "fix it; quickly", "paths": "a/**,b.txt", "ttl": "2h"})
		if !r.OK || r.Data["id"] != "c1" {
			t.Fatalf("the result is %+v; want the claim", r)
		}
		byTool := hub.last(t)
		runCLIWith(t, opts, onHub(hub.URL, "claim", "create", "demo", "--intent", "fix it; quickly", "--paths", "a/**,b.txt", "--ttl", "2h")...)
		if byShell := hub.last(t); byTool.body != byShell.body || byTool.body == "" {
			t.Errorf("the tool sent the body %s; the shell command sends %s", byTool.body, byShell.body)
		}
	})
	t.Run("a value that is not an object is the member result", func(t *testing.T) {
		r := callTool(t, session, "probe_count", map[string]any{"limit": 2})
		if want := map[string]any{"result": []any{1.0, 2.0}}; !r.OK || !reflect.DeepEqual(r.Data, want) {
			t.Errorf("the result is %+v; want the data %v", r, want)
		}
		if q := hub.last(t).query; q != "" && !strings.Contains(q, "limit=2") {
			t.Errorf("the query is %q", q)
		}
	})
	t.Run("nothing printed is the empty object", func(t *testing.T) {
		r := callTool(t, session, "probe_typed", map[string]any{
			"key_value": "A=1", "reason": "because", "put": []string{"a=1", "b=2"}, "dry_run": true, "limit": 3, "max_bytes": 1 << 40, "ttl": "90s",
		})
		if !r.OK || r.Data == nil || len(r.Data) != 0 {
			t.Errorf("the result is %+v; want the data {}", r)
		}
	})
	t.Run("an argument that begins with a dash is an argument", func(t *testing.T) {
		hub.answer(http.MethodGet, "/api/v1/things/-x", http.StatusOK, `{"name":"-x"}`)
		if r := callTool(t, session, "probe_get", map[string]any{"name": "-x"}); !r.OK || r.Data["name"] != "-x" {
			t.Errorf("the result is %+v; want the thing -x", r)
		}
	})
}

func TestToolCall_GivesTheWorkspaceOfTheSessionToACallThatNamesNone(t *testing.T) {
	hub := newStubHub(t).
		answer(http.MethodGet, "/api/v1/workspaces/demo/changes/3", http.StatusOK, changeThree).
		answer(http.MethodGet, "/api/v1/workspaces/demo--t1/changes/3", http.StatusOK, `{"number":3,"title":"Fork's"}`)
	requiredOf := func(s mcpSettings, tool string) []string {
		for _, ct := range toolSessionOn(t, defaultOptions(), hub.URL, s).tools {
			if ct.name == tool {
				var schema struct{ Required []string }
				if err := json.Unmarshal(ct.schema(), &schema); err != nil {
					t.Fatal(err)
				}
				return schema.Required
			}
		}
		t.Fatalf("no tool %s", tool)
		return nil
	}
	fixed := mcpDefaults()
	fixed.workspace = "demo"
	for tool, param := range map[string]string{"change_show": "slug", "sandbox_get": "workspace_slug", "fork_create": "parent"} {
		if slices.Contains(requiredOf(fixed, tool), param) {
			t.Errorf("with --workspace, %s still requires %s", tool, param)
		}
		if !slices.Contains(requiredOf(mcpDefaults(), tool), param) {
			t.Errorf("without --workspace, %s does not require %s", tool, param)
		}
	}
	session := toolSessionOn(t, defaultOptions(), hub.URL, fixed)
	if r := callTool(t, session, "change_show", map[string]any{"number": "3"}); !r.OK || hub.last(t).path != "/api/v1/workspaces/demo/changes/3" {
		t.Errorf("a call without slug: %+v, sent to %s; want the workspace demo", r, hub.last(t).path)
	}
	if r := callTool(t, session, "change_show", map[string]any{"slug": "demo--t1", "number": "3"}); !r.OK || r.Data["title"] != "Fork's" {
		t.Errorf("a call with another slug: %+v; want the fork's change", r)
	}
}

func TestToolCall_GivesAnErrorOfTheHubAsAFailedResult(t *testing.T) {
	hub := newStubHub(t)
	session := toolSessionOn(t, defaultOptions(), hub.URL, mcpDefaults())
	const path = "/api/v1/workspaces/demo/changes/3"
	for _, row := range []struct {
		name         string
		status       int
		body         string
		code, detail string
	}{
		{"an envelope", 404, `{"error":{"code":404,"message":"no such change","error_type":"change_not_found"}}`, "change_not_found", "no such change"},
		{"a refused credential", 401, `{"error":{"code":401,"message":"unknown credential","error_type":"unauthenticated"}}`, "unauthenticated", "unknown credential"},
		{"an answer that is no envelope", 502, `<html>bad gateway</html>`, "http_502", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			hub.answer(http.MethodGet, path, row.status, row.body)
			r := callTool(t, session, "change_show", map[string]any{"slug": "demo", "number": "3"})
			wantFailed(t, r, row.code)
			if row.detail != "" && r.Detail != row.detail {
				t.Errorf("the detail is %q, want the envelope's message %q", r.Detail, row.detail)
			}
		})
	}
	hub.answer(http.MethodGet, path, http.StatusOK, changeThree)
	if r := callTool(t, session, "change_show", map[string]any{"slug": "demo", "number": "3"}); !r.OK {
		t.Errorf("the next call is %+v; want it to succeed", r)
	}
}

// probeFailing builds "probe failing": GET /things, with --fail-on-any,
// whose condition always holds.
func probeFailing() *cobra.Command {
	return declare(command{
		Name: "failing", Description: "Fail on anything", Method: http.MethodGet, Path: "/things", Auth: authUser,
		FailOn: []failOn{{Name: "any", Description: "exit 3 whatever the answer", Check: func([]byte) (bool, string) { return true, "the condition holds" }}},
	})
}

func TestToolCall_GivesAnErrorOfTheCLIAsAFailedResult(t *testing.T) {
	hub := newStubHub(t).
		answer(http.MethodDelete, "/api/v1/workspaces/demo", http.StatusNoContent, "").
		answer(http.MethodGet, "/api/v1/things", http.StatusOK, `{"things":2}`)
	opts := withProbeGroup(defaultOptions(), probeFailing)
	s := mcpSettings{groups: "workspace,probe", noCodeMode: true}
	session := toolSessionOn(t, opts, hub.URL, s)

	t.Run("a command that destroys data, without confirm", func(t *testing.T) {
		wantFailed(t, callTool(t, session, "workspace_delete", map[string]any{"slug": "demo"}), usageErrorType)
		wantFailed(t, callTool(t, session, "workspace_delete", map[string]any{"slug": "demo", "confirm": false}), usageErrorType)
		if n := hub.requestCount(); n != 0 {
			t.Errorf("%d requests were sent, want none", n)
		}
	})
	t.Run("the same with confirm", func(t *testing.T) {
		if r := callTool(t, session, "workspace_delete", map[string]any{"slug": "demo", "confirm": true}); !r.OK || hub.last(t).method != http.MethodDelete {
			t.Errorf("the result is %+v after %s; want the DELETE sent", r, hub.last(t).method)
		}
	})
	t.Run("a condition that holds", func(t *testing.T) {
		r := callTool(t, session, "probe_failing", map[string]any{"fail_on_any": true})
		wantFailed(t, r, conditionNotMetType)
		if r.Data["things"] != 2.0 {
			t.Errorf("the data is %v; want the document the command printed", r.Data)
		}
	})
	t.Run("a hub that cannot be reached", func(t *testing.T) {
		gone := newStubHub(t)
		gone.Close()
		unreachable := toolSessionOn(t, opts, gone.URL, s)
		wantFailed(t, callTool(t, unreachable, "workspace_get", map[string]any{"slug": "demo"}), clientErrorType)
	})
}

func TestToolCall_RefusesParametersTheToolDoesNotTake(t *testing.T) {
	hub := newStubHub(t)
	session := toolSessionOn(t, defaultOptions(), hub.URL, mcpDefaults())
	for _, row := range []struct {
		name  string
		input string
		names string
	}{
		{"a parameter the schema lacks", `{"slug":"demo","number":"3","nope":1}`, "nope"},
		{"a flag left out of every tool", `{"slug":"demo","number":"3","api_key":"x"}`, "api_key"},
		{"a boolean given as a string", `{"slug":"demo","number":"3","wait":"yes"}`, "wait"},
		{"a missing required parameter", `{"slug":"demo"}`, "number"},
		{"a string given as a number", `{"slug":"demo","number":3}`, "number"},
		{"an array given as a string", `{"slug":"demo","number":"3","override":["x"]}`, "override"},
		{"parameters that are not an object", `[1]`, "object"},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := session.call(t.Context(), "change_land", json.RawMessage(row.input))
			wantFailed(t, r, usageErrorType)
			if !strings.Contains(r.Detail, row.names) {
				t.Errorf("the detail %q does not name %s", r.Detail, row.names)
			}
		})
	}
	wantFailed(t, session.call(t.Context(), "no_such_tool", json.RawMessage(`{}`)), usageErrorType)
	if n := hub.requestCount(); n != 0 {
		t.Errorf("%d requests were sent, want none", n)
	}
}

// untouchable is a standard input that fails the test when it is read.
type untouchable struct{ t *testing.T }

func (u untouchable) Read([]byte) (int, error) {
	u.t.Error("a tool call read the process's standard input")
	return 0, io.EOF
}

func TestToolCall_RefusesAValueThatWouldReadStandardInput(t *testing.T) {
	hub := newStubHub(t)
	opts := defaultOptions()
	opts.stdin = untouchable{t}
	session := toolSessionOn(t, opts, hub.URL, mcpSettings{groups: "policy,change,issue", noCodeMode: true})
	for _, row := range []struct {
		tool  string
		input map[string]any
	}{
		{"policy_set", map[string]any{"slug": "demo", "file": "-"}},
		{"change_open", map[string]any{"slug": "demo", "source": "feat", "title": "One", "intent": "-"}},
		{"issue_comment", map[string]any{"slug": "demo", "number": "1", "body_file": "-"}},
	} {
		t.Run(row.tool, func(t *testing.T) {
			wantFailed(t, callTool(t, session, row.tool, row.input), usageErrorType)
		})
	}
	if n := hub.requestCount(); n != 0 {
		t.Errorf("%d requests were sent, want none", n)
	}
}

func TestToolCall_WaitsInsideTheCallForACommandThatCanWait(t *testing.T) {
	opts := withProbeGroup(defaultOptions(), probeRun)
	opts.pollInterval = time.Millisecond
	s := mcpSettings{groups: "probe", noCodeMode: true}
	t.Run("until the operation ends", func(t *testing.T) {
		hub := jobHub(t, "queued", jobStates("queued", "running", "done")...)
		r := callTool(t, toolSessionOn(t, opts, hub.URL, s), "probe_run", map[string]any{"wait": true})
		if !r.OK || r.Data["state"] != "done" || hub.count(http.MethodGet) != 3 {
			t.Errorf("the result is %+v after %d polls; want the finished job after three", r, hub.count(http.MethodGet))
		}
	})
	t.Run("without wait, the answer to the start", func(t *testing.T) {
		hub := jobHub(t, "queued", jobStates("done")...)
		r := callTool(t, toolSessionOn(t, opts, hub.URL, s), "probe_run", map[string]any{})
		if !r.OK || r.Data["state"] != "queued" || hub.count(http.MethodGet) != 0 {
			t.Errorf("the result is %+v after %d polls; want the queued job and no poll", r, hub.count(http.MethodGet))
		}
	})
	t.Run("at most for its timeout", func(t *testing.T) {
		hub := jobHub(t, "queued", jobStates("running")...)
		r := callTool(t, toolSessionOn(t, opts, hub.URL, s), "probe_run", map[string]any{"wait": true, "timeout": "1s"})
		wantFailed(t, r, waitTimeoutType)
	})
	t.Run("an operation that fails", func(t *testing.T) {
		hub := jobHub(t, "queued", jobStates("running", "dead")...)
		r := callTool(t, toolSessionOn(t, opts, hub.URL, s), "probe_run", map[string]any{"wait": true})
		wantFailed(t, r, waitFailedType)
		if r.Data["state"] != "dead" {
			t.Errorf("the data is %v; want the job's last record", r.Data)
		}
	})
}

func TestToolCall_ReadsAndWritesLocalPathsFromTheWorkingDirectory(t *testing.T) {
	const files = "/api/v1/workspaces/demo/sandboxes/sbx_1/files"
	hub := newStubHub(t).
		answer(http.MethodPut, files, http.StatusOK, `{"path":"/workspace/in.txt","bytes":6}`).
		handle(http.MethodGet, files, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write([]byte("from the sandbox\n"))
		})
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("in.txt", []byte("local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	session := toolSessionOn(t, defaultOptions(), hub.URL, mcpDefaults())

	up := callTool(t, session, "sandbox_upload", map[string]any{"workspace_slug": "demo", "id": "sbx_1", "local_path": "in.txt", "sandbox_path": "in.txt"})
	if !up.OK || hub.last(t).body != "local\n" {
		t.Errorf("the upload is %+v and sent %q; want the file's bytes", up, hub.last(t).body)
	}
	down := callTool(t, session, "sandbox_download", map[string]any{"workspace_slug": "demo", "id": "sbx_1", "sandbox_path": "out.txt", "local_path": "out.txt"})
	got, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if !down.OK || err != nil || string(got) != "from the sandbox\n" {
		t.Errorf("the download is %+v and wrote %q (%v); want the sandbox's bytes in the working directory", down, got, err)
	}
}

func TestToolCall_EndsACallThatOutlastsItsBound(t *testing.T) {
	if got := defaultOptions().toolCallBound; got != 30*time.Minute {
		t.Errorf("a tool call is bounded at %s, want 30m", got)
	}
	cancelled := make(chan struct{})
	hub := newStubHub(t).handle(http.MethodGet, "/api/v1/workspaces/demo", func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		close(cancelled)
	})
	opts := defaultOptions()
	opts.toolCallBound = 50 * time.Millisecond
	session := toolSessionOn(t, opts, hub.URL, mcpDefaults())
	start := time.Now()
	r := callTool(t, session, "workspace_get", map[string]any{"slug": "demo"})
	if r.OK || r.Error != "timeout" || time.Since(start) > 5*time.Second {
		t.Errorf("the result is %+v after %s; want a failed one with the error timeout at the bound", r, time.Since(start))
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Error("the hub did not see the request's context end")
	}
}

// runnerOn is the runner of the tool session of s over the tree of opts,
// with what it logs written to the returned buffer.
func runnerOn(t *testing.T, opts options, hubURL string, s mcpSettings) (*runner.Runner, *bytes.Buffer) {
	t.Helper()
	var stderr bytes.Buffer
	r, err := toolSessionOn(t, opts, hubURL, s).runner(&stderr)
	if err != nil {
		t.Fatalf("build the runner of %+v: %v", s, err)
	}
	return r, &stderr
}

// run calls the tool name of r with the parameters input.
func runTool(t *testing.T, r *runner.Runner, name string, input any) core.ToolResult {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return r.Call(t.Context(), name, raw)
}

// callLine is a line parley mcp writes to stderr for a tool call.
type callLine map[string]any

// callLines decodes the lines of stderr, each one JSON object.
func callLines(t *testing.T, stderr string) []callLine {
	t.Helper()
	var lines []callLine
	for _, line := range nonEmptyLines(stderr) {
		var l callLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatalf("the stderr line %q is not one JSON object: %v", line, err)
		}
		lines = append(lines, l)
	}
	return lines
}

func TestMCP_ServesACommandToolThroughTheRunnerAsTheSessionCallsIt(t *testing.T) {
	hub := newStubHub(t).answer(http.MethodGet, "/api/v1/workspaces/demo/changes/3", http.StatusOK, changeThree)
	r, _ := runnerOn(t, defaultOptions(), hub.URL, mcpDefaults())
	var names []string
	for _, tool := range r.Tools() {
		names = append(names, tool.Name)
	}
	if want := toolNames(mcpList(t, defaultOptions(), "--no-code-mode")); !slices.Equal(names, want) {
		t.Errorf("the runner serves\n%v\nwant the tools --list prints\n%v", names, want)
	}
	res := runTool(t, r, "change_show", map[string]any{"slug": "demo", "number": "3"})
	if !res.OK || res.Data["title"] != "One" {
		t.Errorf("change_show through the runner is %+v; want the hub's change", res)
	}
	hub.answer(http.MethodGet, "/api/v1/workspaces/demo/changes/3", http.StatusNotFound, `{"error":{"code":404,"message":"no such change","error_type":"change_not_found"}}`)
	res = runTool(t, r, "change_show", map[string]any{"slug": "demo", "number": "3"})
	if res.OK || res.Error != "change_not_found" || res.Detail != "no such change" {
		t.Errorf("a refused call through the runner is %+v; want a failed result with the hub's error", res)
	}
	// The runner checks the parameters against the tool's schema before the
	// tool runs, and names what it refuses.
	sent := hub.requestCount()
	for param, input := range map[string]map[string]any{
		"nope":    {"slug": "demo", "number": "3", "nope": 1},
		"number":  {"slug": "demo"},
		"include": {"slug": "demo", "number": "3", "include": []string{"x"}},
	} {
		res := runTool(t, r, "change_show", input)
		if res.OK || res.Error != "invalid_arguments" || !strings.Contains(res.Detail, param) {
			t.Errorf("a call with a bad %s through the runner is %+v; want a failed result naming it", param, res)
		}
	}
	if hub.requestCount() != sent {
		t.Error("a call with bad parameters sent a request")
	}
}

func TestMCP_InstructsTheHarnessInAtMostThreeSentences(t *testing.T) {
	for _, row := range []struct {
		name     string
		s        mcpSettings
		has, not []string
	}{
		{"no flag", mcpSettings{}, []string{"code_mode"}, []string{"workspace", "sandbox"}},
		{"a workspace", mcpSettings{workspace: "demo"}, []string{"demo", "code_mode"}, []string{"sandbox"}},
		{"a workspace and a sandbox", mcpSettings{workspace: "demo--fix", sandbox: "sbx_0123"}, []string{"demo--fix", "sbx_0123", "code_mode"}, nil},
		{"no code mode", mcpSettings{workspace: "demo", noCodeMode: true}, []string{"demo"}, []string{"code_mode"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			got := mcpInstructions(row.s)
			if n := strings.Count(got, ". ") + 1; n > 3 || !strings.HasSuffix(got, ".") || strings.Contains(got, "\n") {
				t.Errorf("the instructions have %d sentences: %q; want at most three", n, got)
			}
			for _, name := range row.has {
				if !strings.Contains(got, name) {
					t.Errorf("the instructions %q do not name %s", got, name)
				}
			}
			for _, name := range row.not {
				if strings.Contains(got, name) {
					t.Errorf("the instructions %q name %s", got, name)
				}
			}
		})
	}
}

func TestMCP_EndsWithoutAnEndpointOrACredentialBeforeItServes(t *testing.T) {
	hub := newStubHub(t)
	for _, row := range []struct {
		name string
		args []string
	}{
		{"no endpoint", []string{"--api-key", "k"}},
		{"no credential", []string{"--endpoint-url", hub.URL}},
		{"neither", nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			code, stdout, stderr := runMCP(t, defaultOptions(), row.args...)
			var env cliEnvelope
			if code != exitClientError || stdout != "" || json.Unmarshal([]byte(stderr), &env) != nil || env.Error.ErrorType != clientErrorType {
				t.Errorf("exit status %d, stdout %q, stderr %q; want 2, nothing and one client_error envelope", code, stdout, stderr)
			}
			if listCode, _, _ := runMCP(t, defaultOptions(), append([]string{"--list"}, row.args...)...); listCode != 0 {
				t.Errorf("--list in the same state exits %d, want 0", listCode)
			}
		})
	}
}

func TestMCP_WritesOneLineToStderrForEveryToolCall(t *testing.T) {
	const in, out = "IN-7f3a", "OUT-9c1e"
	hub := newStubHub(t).
		answer(http.MethodGet, "/api/v1/workspaces/demo/changes/3", http.StatusOK, `{"number":3,"title":"`+out+`"}`).
		answer(http.MethodGet, "/api/v1/workspaces/demo/changes/4", http.StatusNotFound, `{"error":{"code":404,"message":"`+out+`","error_type":"change_not_found"}}`)
	r, stderr := runnerOn(t, defaultOptions(), hub.URL, mcpDefaults())

	runTool(t, r, "change_show", map[string]any{"slug": "demo", "number": "3", "include": in})
	runTool(t, r, "change_show", map[string]any{"slug": "demo", "number": "4", "include": in})
	runTool(t, r, "change_show", map[string]any{"slug": "demo", "nope": in})

	lines := callLines(t, stderr.String())
	if len(lines) != 3 {
		t.Fatalf("three calls wrote %d lines: %s", len(lines), stderr)
	}
	for i, want := range []callLine{
		{"tool": "change_show", "ok": true},
		{"tool": "change_show", "ok": false, "error": "change_not_found"},
		// agentkit-go refuses a parameter the schema lacks before the tool
		// runs.
		{"tool": "change_show", "ok": false, "error": "not_run"},
	} {
		got := lines[i]
		if ms, isNumber := got["duration_ms"].(float64); !isNumber || ms < 0 {
			t.Errorf("line %d has the duration_ms %v, want a number of at least 0", i, got["duration_ms"])
		}
		delete(got, "duration_ms")
		if !reflect.DeepEqual(got, want) {
			t.Errorf("line %d is %v, want %v and duration_ms", i, got, want)
		}
	}
	if s := stderr.String(); strings.Contains(s, in) || strings.Contains(s, out) {
		t.Errorf("stderr holds an argument or a part of a result: %s", s)
	}
}

func TestMCP_KeepsTheCredentialItReadAtStart(t *testing.T) {
	hub := newStubHub(t).answer(http.MethodGet, "/api/v1/workspaces/demo", http.StatusOK, `{"slug":"demo"}`)
	r, _ := runnerOn(t, defaultOptions(), hub.URL, mcpDefaults())
	// What a command run from the shell would read now is another hub and
	// another credential.
	other := newStubHub(t)
	t.Setenv("PARLEY_ENDPOINT_URL", other.URL)
	t.Setenv("PARLEY_API_KEY", "another")
	writeConfigFile(t, os.Getenv("PARLEY_CONFIG_DIR"), "endpoint_url = \""+other.URL+"\"\napi_key = \"a-third\"\n")

	if res := runTool(t, r, "workspace_get", map[string]any{"slug": "demo"}); !res.OK || hub.last(t).header.Get("Authorization") != "Bearer k" || other.requestCount() != 0 {
		t.Errorf("the call is %+v, sent with %q; want the hub and the credential read at start", res, hub.last(t).header.Get("Authorization"))
	}
	hub.answer(http.MethodGet, "/api/v1/workspaces/demo", http.StatusUnauthorized, `{"error":{"code":401,"message":"unknown credential","error_type":"unauthenticated"}}`)
	if res := runTool(t, r, "workspace_get", map[string]any{"slug": "demo"}); res.OK || res.Error != "unauthenticated" {
		t.Errorf("a call the hub answers 401 is %+v; want a failed result with the error unauthenticated", res)
	}
	before := hub.requestCount()
	runTool(t, r, "workspace_get", map[string]any{"slug": "demo"})
	if hub.requestCount() != before+1 || hub.last(t).header.Get("Authorization") != "Bearer k" {
		t.Error("after a 401 the next call was not sent with the credential read at start")
	}
}

// withCodeMode are the settings of parley mcp given no flag.
func withCodeMode() mcpSettings { return mcpSettings{groups: mcpDefaultGroups} }

// script calls code_mode of r with the script src.
func runScript(t *testing.T, r *runner.Runner, src string) core.ToolResult {
	t.Helper()
	return runTool(t, r, "code_mode", map[string]any{"script": src})
}

// resultText is everything a result carries, as one text to search.
func resultText(res core.ToolResult) string {
	data, _ := json.Marshal(res.Data)
	return res.Text + "\n" + string(data) + "\n" + res.Error + "\n" + res.Detail
}

func TestMCPList_ListsCodeModeAfterTheOtherTools(t *testing.T) {
	tools := mcpList(t, defaultOptions())
	without := mcpList(t, defaultOptions(), "--no-code-mode")
	last := tools[len(tools)-1]
	if last.Name != "code_mode" || len(tools) != len(without)+1 || !slices.Equal(toolNames(tools[:len(tools)-1]), toolNames(without)) {
		t.Fatalf("the tools end with %s and are %d; want the %d tools of --no-code-mode and then code_mode", last.Name, len(tools), len(without))
	}
	if last.InputSchema.Properties["script"]["type"] != "string" || !strings.Contains(last.Description, "change_land") {
		t.Errorf("code_mode takes %v and is described in %d bytes; want a script, and the bound tools declared in its description", last.InputSchema.Properties, len(last.Description))
	}
	if slices.Contains(toolNames(without), "code_mode") {
		t.Error("--no-code-mode lists code_mode")
	}
	if len(tools) > 81 {
		t.Errorf("the default selection has %d tools, want at most 80 and code_mode", len(tools))
	}
}

func TestCodeMode_ChainsToolCallsInOneScript(t *testing.T) {
	hub := newStubHub(t).
		answer(http.MethodGet, "/api/v1/workspaces/demo", http.StatusOK, `{"slug":"demo","default_branch":"main"}`).
		answer(http.MethodGet, "/api/v1/workspaces/demo/changes/3", http.StatusOK, changeThree).
		answer(http.MethodGet, "/api/v1/workspaces/demo/changes/4", http.StatusOK, `{"number":4,"state":"landed","title":"Two"}`)
	r, stderr := runnerOn(t, defaultOptions(), hub.URL, withCodeMode())
	tools := r.Tools()
	if last := tools[len(tools)-1]; last.Name != "code_mode" {
		t.Fatalf("the last tool is %s, want code_mode", last.Name)
	}
	res := runScript(t, r, `
def main():
    ws = workspace_get(slug="demo")
    changes = parallel([call(change_show, slug="demo", number="3"), call(change_show, slug="demo", number="4")])
    return ws["default_branch"] + ":" + ",".join([c["title"] for c in changes])
`)
	if !res.OK || !strings.Contains(resultText(res), "main:One,Two") {
		t.Fatalf("the script's result is %+v; want what it returned, main:One,Two", res)
	}
	if n := hub.requestCount(); n != 3 {
		t.Errorf("the hub got %d requests, want the three the script made", n)
	}
	lines := callLines(t, stderr.String())
	var called []string
	for _, l := range lines {
		called = append(called, l["tool"].(string))
	}
	slices.Sort(called)
	if want := []string{"change_show", "change_show", "code_mode", "workspace_get"}; !slices.Equal(called, want) {
		t.Errorf("stderr has lines for %v, want one for each call and one for the script: %v", called, want)
	}
}

func TestCodeMode_MakesAScriptsCallAsADirectCallIsMade(t *testing.T) {
	hub := newStubHub(t).
		answer(http.MethodGet, "/api/v1/workspaces/demo/changes/3", http.StatusOK, changeThree).
		answer(http.MethodGet, "/api/v1/workspaces/demo/changes/9", http.StatusNotFound, `{"error":{"code":404,"message":"no such change","error_type":"change_not_found"}}`)
	r, _ := runnerOn(t, defaultOptions(), hub.URL, withCodeMode())

	runTool(t, r, "change_show", map[string]any{"slug": "demo", "number": "3", "include": "stack"})
	direct := hub.last(t)
	if res := runScript(t, r, `result = change_show(slug="demo", number="3", include="stack")["title"]`); !res.OK || !strings.Contains(resultText(res), "One") {
		t.Fatalf("the script's result is %+v; want the change's title", res)
	}
	if byScript := hub.last(t); byScript.rawPath != direct.rawPath || byScript.query != direct.query || byScript.header.Get("Authorization") != direct.header.Get("Authorization") {
		t.Errorf("the script's call sent %s?%s; a direct call sends %s?%s", byScript.rawPath, byScript.query, direct.rawPath, direct.query)
	}

	sent := hub.requestCount()
	res := runScript(t, r, `
def main():
    missing = change_show(slug="demo", number="9")
    refused = change_show(slug="demo", number="3", nope="x")
    if is_error(missing) and is_error(refused):
        return "missing=" + missing.error + " refused=" + refused.error
    return "no error"
`)
	if !res.OK || !strings.Contains(resultText(res), "missing=change_not_found refused=invalid_arguments") {
		t.Errorf("the script's result is %+v; want the errors a direct call gets, for the script to branch on", res)
	}
	if n := hub.requestCount() - sent; n != 1 {
		t.Errorf("the script's two calls sent %d requests, want one: the refused call sends none", n)
	}
}

func TestCodeMode_BoundsAScriptByTheScriptTimeout(t *testing.T) {
	for _, row := range []struct {
		flag string
		want time.Duration
	}{{"", 10 * time.Minute}, {"1s", time.Second}, {"90s", 90 * time.Second}, {"30m", 30 * time.Minute}} {
		s := withCodeMode()
		s.scriptTimeout = row.flag
		if got := toolSessionOn(t, defaultOptions(), "http://hub.invalid", s).scriptTimeout; got != row.want {
			t.Errorf("--script-timeout %q bounds a script at %s, want %s", row.flag, got, row.want)
		}
	}
	hub := newStubHub(t).handle(http.MethodGet, "/api/v1/workspaces/demo", func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	s := withCodeMode()
	s.scriptTimeout = "1s"
	r, _ := runnerOn(t, defaultOptions(), hub.URL, s)
	start := time.Now()
	res := runScript(t, r, `result = workspace_get(slug="demo")`)
	if res.OK || time.Since(start) > 5*time.Second || !strings.Contains(strings.ToLower(resultText(res)), "time") {
		t.Errorf("after %s the script's result is %+v; want it ended at its bound of 1s with a timeout error", time.Since(start), res)
	}
}

func TestMCP_RefusesAScriptTimeoutOutOfRange(t *testing.T) {
	for _, value := range []string{"0s", "999ms", "31m", "soon", "1d"} {
		t.Run(value, func(t *testing.T) {
			code, stdout, stderr := runMCP(t, defaultOptions(), "--list", "--script-timeout", value)
			wantUsageError(t, code, stdout, stderr, "script-timeout")
		})
	}
	for _, value := range []string{"1s", "30m"} {
		if code, _, stderr := runMCP(t, defaultOptions(), "--list", "--script-timeout", value); code != 0 {
			t.Errorf("--script-timeout %s: exit status %d, %s; want it accepted", value, code, stderr)
		}
	}
}
