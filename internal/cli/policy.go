package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// policyPath is the route of a workspace's landing policy.
const policyPath = "/workspaces/:slug/policy"

// newPolicyCommand builds parley policy: the commands that read, replace
// and try out the landing policy of a workspace.
func newPolicyCommand() *cobra.Command {
	return group("policy", "Read, replace and try out the landing policy of a workspace", newPolicyGetCommand(), newPolicySetCommand(), newPolicyEvaluateCommand())
}

// newPolicyGetCommand builds parley policy get: the current policy, or one
// of its kept revisions.
func newPolicyGetCommand() *cobra.Command {
	get := declare(command{
		Name:        "get",
		Description: "Show the landing policy of a workspace, or one of its last revisions",
		Method:      http.MethodGet,
		Path:        policyPath,
		Args:        []argSpec{slugArg},
		Auth:        authUser,
		Flags: func(fs *pflag.FlagSet) {
			fs.String("revision", "", "show this revision, a non-negative integer, instead of the current one")
		},
		Run: func(c *call) error {
			var query url.Values
			if n, ok := numberFlag(c.cmd.Flags(), "revision"); ok {
				query = url.Values{"revision": {n}}
			}
			return sendWith(c, query, nil)
		},
	})
	return checkFlags(get, func(cmd *cobra.Command) error {
		return nonNegativeFlags(cmd, "revision")
	})
}

// newPolicySetCommand builds parley policy set: the replacement of the
// policy with the document of a JSON or TOML file.
func newPolicySetCommand() *cobra.Command {
	var document json.RawMessage
	set := declare(command{
		Name:        "set",
		Description: "Replace the landing policy of a workspace with the document of a JSON or TOML file",
		Method:      http.MethodPut,
		Path:        policyPath,
		Args:        []argSpec{slugArg},
		Auth:        authUser,
		Flags: func(fs *pflag.FlagSet) {
			fs.String("file", "", "the policy document: a JSON file, a TOML file (a name ending in .toml), or - for standard input")
			fs.String("if-revision", "", "replace the policy only while its current revision is this one, a non-negative integer")
			_ = cobra.MarkFlagRequired(fs, "file")
		},
		Run: func(c *call) error {
			r := apiRequest{method: c.decl.Method, path: routePath(c.decl.Path, c.args), body: document}
			if n, ok := numberFlag(c.cmd.Flags(), "if-revision"); ok {
				// The hub answers the revision as the ETag "<n>", so the
				// condition names it the same way.
				r.header = http.Header{"If-Match": {`"` + n + `"`}}
			}
			resp, err := c.client.send(c.ctx, r)
			if err != nil {
				return err
			}
			return c.printAnswer(resp)
		},
	})
	return checkFlags(set, func(cmd *cobra.Command) error {
		if err := nonNegativeFlags(cmd, "if-revision"); err != nil {
			return err
		}
		var err error
		document, err = readPolicy(cmd, "file")
		return err
	})
}

// newPolicyEvaluateCommand builds parley policy evaluate: what a list of
// paths, or an existing change, would need to land, against the stored
// policy or a candidate document.
func newPolicyEvaluateCommand() *cobra.Command {
	var paths []string
	var candidate json.RawMessage
	evaluate := declare(command{
		Name:        "evaluate",
		Description: "Show what a list of paths, or an existing change, would need to land under the policy, without writing anything",
		Method:      http.MethodPost,
		Path:        policyPath + "/evaluate",
		Args:        []argSpec{slugArg},
		Auth:        authUser,
		Flags: func(fs *pflag.FlagSet) {
			fs.StringArrayVar(&paths, "paths", nil, "the repository paths to evaluate, comma-separated, repeatable")
			fs.String("change", "", "evaluate the files of this change, a non-negative integer, instead of paths")
			fs.String("target", "", "the target branch the paths would land on (default: the workspace's default branch)")
			fs.String("policy-file", "", "evaluate against this candidate document, JSON or TOML as for policy set --file, instead of the stored policy")
		},
		Run: func(c *call) error {
			fs := c.cmd.Flags()
			body := map[string]any{}
			if fs.Changed("paths") {
				list := []string{}
				for _, p := range paths {
					list = append(list, strings.Split(p, ",")...)
				}
				body["paths"] = list
			}
			if n, ok := numberFlag(fs, "change"); ok {
				// The value was checked to be decimal digits, so it is sent
				// as the JSON number it reads.
				body["change"] = json.Number(n)
			}
			if fs.Changed("target") {
				body["target_branch"], _ = fs.GetString("target")
			}
			if candidate != nil {
				body["policy"] = candidate
			}
			return sendWith(c, nil, body)
		},
	})
	return checkFlags(evaluate, func(cmd *cobra.Command) error {
		fs := cmd.Flags()
		if fs.Changed("paths") == fs.Changed("change") {
			return usageError("%s takes exactly one of --paths and --change", cmd.CommandPath())
		}
		if err := nonNegativeFlags(cmd, "change"); err != nil {
			return err
		}
		if !fs.Changed("policy-file") {
			return nil
		}
		var err error
		candidate, err = readPolicy(cmd, "policy-file")
		return err
	})
}

// nonNegativeFlags refuses, as a usage error, a value of each flag of flags
// given that is not a non-negative integer in decimal digits alone. A
// revision or a change number may be 0, which withNumbers refuses.
func nonNegativeFlags(cmd *cobra.Command, flags ...string) error {
	for _, flag := range flags {
		f := cmd.Flags().Lookup(flag)
		if !f.Changed {
			continue
		}
		if _, ok := nonNegative(f.Value.String()); !ok {
			return usageError("%s takes --%s <n>, a non-negative integer, got %q", cmd.CommandPath(), flag, f.Value.String())
		}
	}
	return nil
}

// numberFlag is the value of the flag name, a number nonNegativeFlags has
// checked, in its shortest form, and whether the flag was given.
func numberFlag(fs *pflag.FlagSet, name string) (string, bool) {
	if !fs.Changed(name) {
		return "", false
	}
	value, _ := fs.GetString(name)
	n, _ := nonNegative(value)
	return strconv.FormatInt(n, 10), true
}

// nonNegative is s read as a non-negative integer written in decimal
// digits alone, and whether it is one.
func nonNegative(s string) (int64, bool) {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// readPolicy is the policy document of the file the flag names, as the
// JSON text the hub takes: - reads standard input; a name ending in .toml
// is read as TOML; anything else as JSON and, when it is not JSON, as
// TOML, whose table default and array of tables rules become the members
// of the same name. Owners keep the policy in either form beside their
// code, so both are read; the hub judges the document itself. A file that
// cannot be read, is empty or blank, or is neither is a usage error, before
// any request.
func readPolicy(cmd *cobra.Command, flag string) (json.RawMessage, error) {
	name, _ := cmd.Flags().GetString(flag)
	var data []byte
	var err error
	if name == "-" {
		data, err = io.ReadAll(optionsFrom(cmd.Context()).stdin)
	} else {
		data, err = os.ReadFile(name)
	}
	if err != nil {
		return nil, usageError("%s cannot read the policy of --%s: %v", cmd.CommandPath(), flag, err)
	}
	source := name
	if name == "-" {
		source = "standard input"
	}
	// Empty TOML is a valid empty document, the policy that lets every
	// change land: a file truncated or a pipe that gave nothing must not
	// replace the policy with it.
	if strings.TrimSpace(string(data)) == "" {
		return nil, usageError("%s takes --%s <path>, a policy document: %s is empty", cmd.CommandPath(), flag, source)
	}
	isTOML := strings.HasSuffix(name, ".toml")
	if !isTOML && json.Valid(data) {
		return data, nil
	}
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		if isTOML {
			return nil, usageError("%s takes --%s <path>, a policy document: %s is not valid TOML: %v", cmd.CommandPath(), flag, source, err)
		}
		return nil, usageError("%s takes --%s <path>, a policy document: %s is neither valid JSON nor valid TOML: %v", cmd.CommandPath(), flag, source, err)
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		return nil, usageError("%s cannot send the TOML of --%s as JSON: %v", cmd.CommandPath(), flag, err)
	}
	return encoded, nil
}
