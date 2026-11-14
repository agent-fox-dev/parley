package cli

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// workspaceStringFlags are the string flags of parley workspace create, each
// with the body member it is sent as when given; the order is the order of
// their help.
var workspaceStringFlags = []struct {
	flag, member, usage string
}{
	{"slug", "slug", "the workspace's slug: 3 to 64 characters of a to z, digits and hyphens"},
	{"git-url", "git_url", "the repository to register: https://<host>/<path> or git@<host>:<path>"},
	{"default-branch", "default_branch", "the branch the workspace treats as its default (default: origin's)"},
	{"org", "org_id", "the id or slug of the organization the workspace is recorded in (default: your personal organization)"},
	{"display-name", "display_name", "the workspace's display name (default: the slug)"},
	{"description", "description", "the workspace's description"},
	{"workspace-mode", "workspace_mode", "standard or carry_patch (default: standard)"},
	{"upstream-url", "upstream_url", "with carry_patch, the upstream project the workspace tracks"},
	{"integration-branch", "integration_branch", "with carry_patch, the branch the hub rebuilds as upstream plus the patches"},
	{"git-pat", "git_pat", "a token origin accepts, stored as the workspace secret GIT_PAT"},
	{"git-username", "git_username", "with --git-password, the user name origin accepts, stored as the workspace secret GIT_USERNAME"},
	{"git-password", "git_password", "with --git-username, the password origin accepts, stored as the workspace secret GIT_PASSWORD"},
}

// workspaceUpdateFlags are the flags of parley workspace update that change
// one member of the workspace: each value flag sends its string, and its
// clear flag sends null in its place.
var workspaceUpdateFlags = []struct {
	flag, clear, member, usage string
}{
	{"display-name", "clear-display-name", "display_name", "the workspace's display name; an empty value is the slug"},
	{"description", "clear-description", "description", "the workspace's description"},
	{"org", "clear-org", "org_id", "the id or slug of the organization to move the workspace and its forks to"},
}

// slugArg is the <slug> argument: the slug of a workspace.
var slugArg = argSpec{Name: "slug", Required: true}

// settingFlag is the repeatable flag that sets one workspace setting.
const settingFlag = "setting"

// protectedBranchesSetting is the setting whose value is a list of branch
// patterns, so --setting splits its value at commas.
const protectedBranchesSetting = "protected_branches"

// newWorkspaceCommand builds parley workspace: the commands of the
// workspaces the hub holds.
func newWorkspaceCommand() *cobra.Command {
	create := declare(command{
		Name:        "create",
		Description: "Register a git repository as a workspace; with --wait, wait until the hub has cloned it",
		Method:      http.MethodPost,
		Path:        "/workspaces",
		Auth:        authUser,
		Flags: func(fs *pflag.FlagSet) {
			for _, f := range workspaceStringFlags {
				fs.String(f.flag, "", f.usage)
			}
			addSettingFlag(fs)
			_ = cobra.MarkFlagRequired(fs, "slug")
			_ = cobra.MarkFlagRequired(fs, "git-url")
		},
		Wait: &waitFor{
			Start: func(c *call) (apiRequest, error) {
				body, err := workspaceCreateBody(c.cmd)
				if err != nil {
					return apiRequest{}, err
				}
				return apiRequest{method: http.MethodPost, path: "/workspaces", body: body}, nil
			},
			Poll: func(c *call, _ []byte) (apiRequest, error) {
				slug, _ := c.cmd.Flags().GetString("slug")
				return apiRequest{method: http.MethodGet, path: "/workspaces/" + pathSegment(slug)}, nil
			},
			Classify: byState("status", []string{"ready"}, []string{"failed"}),
		},
	})
	checkLine := create.Args
	create.Args = func(cmd *cobra.Command, args []string) error {
		if err := checkLine(cmd, args); err != nil {
			return err
		}
		if err := checkWorkspaceCredential(cmd); err != nil {
			return err
		}
		_, err := workspaceCreateBody(cmd)
		return err
	}
	list := declare(command{
		Name:        "list",
		Description: "List the workspaces you can read, newest first, without archived workspaces and forks unless asked",
		Method:      http.MethodGet,
		Path:        "/workspaces",
		Auth:        authUser,
		Flags: func(fs *pflag.FlagSet) {
			fs.Bool("include-archived", false, "list archived workspaces too")
			fs.Bool("include-forks", false, "list task forks too")
			fs.String("parent", "", "list only the forks of the workspace with this slug")
			fs.Int("limit", 0, "the most workspaces the page holds; the hub's default, 50, when not given")
			fs.String("cursor", "", "the next_cursor of the page before, to read the page after it")
		},
		Run: listWorkspaces,
	})
	get := declare(command{
		Name:        "get",
		Description: "Print a workspace with its settings and your access to it",
		Method:      http.MethodGet,
		Path:        "/workspaces/:slug",
		Args:        []argSpec{slugArg},
		Auth:        authUser,
	})
	update := declare(command{
		Name:        "update",
		Description: "Change a workspace's display name, description, organization or settings",
		Method:      http.MethodPatch,
		Path:        "/workspaces/:slug",
		Args:        []argSpec{slugArg},
		Auth:        authUser,
		Flags: func(fs *pflag.FlagSet) {
			for _, f := range workspaceUpdateFlags {
				fs.String(f.flag, "", f.usage)
			}
			for _, f := range workspaceUpdateFlags {
				fs.Bool(f.clear, false, "send null as the "+f.member+", which the hub resets to its default; wins over --"+f.flag)
			}
			addSettingFlag(fs)
		},
		Run: func(c *call) error {
			body, err := workspaceUpdateBody(c.cmd)
			if err != nil {
				return err
			}
			return sendWith(c, nil, body)
		},
	})
	checkUpdateLine := update.Args
	// The body is built here too, so that an update with nothing to send
	// or a --setting the parser refuses fails with the other usage errors,
	// before any setting is resolved or any request sent (ADR 02).
	update.Args = func(cmd *cobra.Command, args []string) error {
		if err := checkUpdateLine(cmd, args); err != nil {
			return err
		}
		_, err := workspaceUpdateBody(cmd)
		return err
	}
	return group("workspace", "Register, list, read and change the workspaces of the hub", create, list, get, update)
}

// listWorkspaces is the run step of parley workspace list: it sends a query
// parameter for each flag given and none for a flag not given, so the hub's
// defaults apply, and prints the page. A value the hub would refuse (a limit
// of 0, a cursor of another query) is sent as given, so that the hub's own
// error says what is wrong with it.
func listWorkspaces(c *call) error {
	fs := c.cmd.Flags()
	query := url.Values{}
	for _, name := range []string{"include-archived", "include-forks"} {
		if include, _ := fs.GetBool(name); include {
			query.Set(strings.ReplaceAll(name, "-", "_"), "true")
		}
	}
	for _, name := range []string{"parent", "limit", "cursor"} {
		if f := fs.Lookup(name); f.Changed {
			query.Set(name, f.Value.String())
		}
	}
	return sendWith(c, query, nil)
}

// workspaceUpdateBody is the body of PATCH /workspaces/:slug for cmd's
// flags: a member for each flag given, a value flag given empty sending the
// empty string, a clear flag sending null and winning over its value flag,
// and the settings of every --setting. With none of them given there is
// nothing to change, which is a usage error.
func workspaceUpdateBody(cmd *cobra.Command) (map[string]any, error) {
	fs := cmd.Flags()
	body := map[string]any{}
	for _, f := range workspaceUpdateFlags {
		if clear, _ := fs.GetBool(f.clear); clear {
			body[f.member] = nil
		} else if fs.Changed(f.flag) {
			body[f.member], _ = fs.GetString(f.flag)
		}
	}
	if err := addSettings(fs, body); err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, usageError("%s needs at least one of --display-name, --description, --org, --clear-display-name, --clear-description, --clear-org and --setting", cmd.CommandPath())
	}
	return body, nil
}

// addSettingFlag registers the repeatable --setting flag on fs. It is a
// string array, not a slice: a value such as protected_branches=main,release/*
// must reach the parser whole.
func addSettingFlag(fs *pflag.FlagSet) {
	fs.StringArray(settingFlag, nil, "set a workspace setting as key=value, repeatable: an empty value sends null, true and false send booleans, protected_branches takes a comma-separated list, anything else is sent as a string")
}

// addSettings puts the settings of the --setting values on fs into body,
// parsed by ParseSettingFlags, when the flag was given.
func addSettings(fs *pflag.FlagSet, body map[string]any) error {
	if !fs.Changed(settingFlag) {
		return nil
	}
	// GetStringArray reads the values back from their text, which loses a
	// lone empty value; the slice itself keeps it.
	values := fs.Lookup(settingFlag).Value.(pflag.SliceValue).GetSlice()
	settings, err := ParseSettingFlags(values)
	if err != nil {
		return err
	}
	body["settings"] = settings
	return nil
}

// checkWorkspaceCredential refuses, as usage errors, a credential flag given
// with an empty value, --git-pat with --git-username or --git-password, and
// one of --git-username and --git-password without the other, so a
// credential the hub would refuse is never sent.
func checkWorkspaceCredential(cmd *cobra.Command) error {
	fs := cmd.Flags()
	for _, name := range []string{"git-pat", "git-username", "git-password"} {
		if value, _ := fs.GetString(name); fs.Changed(name) && value == "" {
			return usageError("%s needs a value for --%s", cmd.CommandPath(), name)
		}
	}
	pat, user, password := fs.Changed("git-pat"), fs.Changed("git-username"), fs.Changed("git-password")
	switch {
	case pat && (user || password):
		return usageError("%s takes --git-pat or --git-username with --git-password, not both", cmd.CommandPath())
	case user && !password:
		return usageError("%s needs --git-password with --git-username", cmd.CommandPath())
	case password && !user:
		return usageError("%s needs --git-username with --git-password", cmd.CommandPath())
	}
	return nil
}

// workspaceCreateBody is the body of POST /workspaces for cmd's flags: a
// member for each flag given, even with an empty value, which the hub
// judges, and none for a flag not given, which leaves the hub's default.
func workspaceCreateBody(cmd *cobra.Command) (map[string]any, error) {
	fs := cmd.Flags()
	body := map[string]any{}
	for _, f := range workspaceStringFlags {
		if fs.Changed(f.flag) {
			body[f.member], _ = fs.GetString(f.flag)
		}
	}
	if err := addSettings(fs, body); err != nil {
		return nil, err
	}
	return body, nil
}

// ParseSettingFlags is the settings document of the values of --setting
// flags, each key=value: the text is split at its first =, and the value is
// null when empty; else, for protected_branches, the list of its
// comma-separated parts; else true or false as a boolean; else the string
// itself. The last value of a key given more than once wins. A value
// without = or with an empty key is a usage error. It is the one parser of
// --setting, so every command that sets settings reads them alike.
func ParseSettingFlags(values []string) (map[string]any, error) {
	settings := make(map[string]any, len(values))
	for _, v := range values {
		key, value, ok := strings.Cut(v, "=")
		switch {
		case !ok:
			return nil, usageError("--%s %q has no =: give key=value", settingFlag, v)
		case key == "":
			return nil, usageError("--%s %q has no key before =", settingFlag, v)
		}
		switch {
		case value == "":
			settings[key] = nil
		case key == protectedBranchesSetting:
			settings[key] = strings.Split(value, ",")
		case value == "true" || value == "false":
			settings[key] = value == "true"
		default:
			settings[key] = value
		}
	}
	return settings, nil
}
