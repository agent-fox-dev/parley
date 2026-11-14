package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// listPageLimit is the page size a command that reads a whole list asks
// for: the most the hub serves, so the list takes the fewest requests.
const listPageLimit = "200"

// secretScope is one scope a secret command acts on: its name as create
// prints it and the path of its secret routes, encoded already.
type secretScope struct {
	name, path string
}

// secretEntry is one KEY=VALUE of a create, as the hub takes it.
type secretEntry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// newSecretsCommand builds parley secrets: the secrets of the caller's user,
// of an organization and of a workspace. The hub never answers a value, so
// none of these commands prints one.
func newSecretsCommand() *cobra.Command {
	return group("secrets", "Create, list, update and delete secrets, whose values the hub never shows",
		withSecretArgs(declare(command{
			Name:        "create",
			Description: "Create secrets in your user scope, an organization's or a workspace's",
			Method:      http.MethodPost,
			Path:        "/user/secrets",
			Args:        []argSpec{{Name: "KEY=VALUE", Required: true, Repeats: true}},
			Auth:        authUser,
			Flags:       secretFlags(true, "create the secrets in"),
			Run:         createSecrets,
		}), true, checkEntries),
		withSecretArgs(declare(command{
			Name:        "list",
			Description: "List the keys of a scope's secrets, every page of them",
			Method:      http.MethodGet,
			Path:        "/user/secrets",
			Auth:        authUser,
			Flags:       secretFlags(false, "list the secrets of"),
			Run:         listSecrets,
		}), false, nil),
		withSecretArgs(declare(command{
			Name:        "update",
			Description: "Replace the value of one secret",
			Method:      http.MethodPatch,
			Path:        "/user/secrets/:key",
			// More than one entry is refused by checkOneEntry, whose
			// message, unlike the count of arguments', quotes no value.
			Args:  []argSpec{{Name: "KEY=VALUE", Required: true, Repeats: true}},
			Auth:  authUser,
			Flags: secretFlags(true, "update the secret of"),
			Run:   updateSecret,
		}), false, checkOneEntry),
		withSecretArgs(declare(command{
			Name:        "delete",
			Description: "Delete one secret",
			Method:      http.MethodDelete,
			Path:        "/user/secrets/:key",
			Args:        []argSpec{{Name: "KEY", Required: true}},
			Auth:        authUser,
			Flags:       secretFlags(false, "delete the secret of"),
			Run:         deleteSecret,
			Confirm:     func(c *call) any { return map[string]string{"deleted": c.args["KEY"]} },
		}), false, checkKey),
	)
}

// secretFlags registers the scope flags, and --from-stdin when fromStdin,
// with what the command does to the scope in their help.
func secretFlags(fromStdin bool, does string) func(fs *pflag.FlagSet) {
	return func(fs *pflag.FlagSet) {
		fs.Bool("user", false, does+" your user scope (the scope without a flag)")
		fs.String("org", "", does+" the organization with this slug or id")
		fs.String("workspace", "", does+" the workspace with this slug")
		if fromStdin {
			fs.Bool("from-stdin", false, "take one bare KEY as the argument and read its value from standard input, without one trailing line ending")
		}
	}
}

// withSecretArgs refuses, as usage errors with the other usage errors and
// before any setting is resolved or any request sent, an empty --org or
// --workspace, which would name no scope; more than one scope flag unless
// many; and arguments check refuses. A refused command line sends nothing,
// so no part of it can reach a scope it did not mean.
func withSecretArgs(cmd *cobra.Command, many bool, check func(cmd *cobra.Command, args []string) error) *cobra.Command {
	checkLine := cmd.Args
	cmd.Args = func(cmd *cobra.Command, args []string) error {
		if err := checkLine(cmd, args); err != nil {
			return err
		}
		fs := cmd.Flags()
		for _, name := range []string{"org", "workspace"} {
			if slug, _ := fs.GetString(name); fs.Changed(name) && slug == "" {
				return usageError("%s needs a slug as the value of --%s", cmd.CommandPath(), name)
			}
		}
		if !many && len(secretScopes(cmd)) > 1 {
			return usageError("%s takes at most one of --user, --org and --workspace", cmd.CommandPath())
		}
		if check != nil {
			return check(cmd, args)
		}
		return nil
	}
	return cmd
}

// secretScopes are the scopes the scope flags of cmd name, in the order
// user, org, workspace, or the user's alone when none is given.
func secretScopes(cmd *cobra.Command) []secretScope {
	fs := cmd.Flags()
	var scopes []secretScope
	if user, _ := fs.GetBool("user"); user {
		scopes = append(scopes, secretScope{"user", "/user/secrets"})
	}
	if slug, _ := fs.GetString("org"); fs.Changed("org") {
		scopes = append(scopes, secretScope{"org", "/orgs/" + pathSegment(slug) + "/secrets"})
	}
	if slug, _ := fs.GetString("workspace"); fs.Changed("workspace") {
		scopes = append(scopes, secretScope{"workspace", "/workspaces/" + pathSegment(slug) + "/secrets"})
	}
	if len(scopes) == 0 {
		scopes = append(scopes, secretScope{"user", "/user/secrets"})
	}
	return scopes
}

// fromStdin tells whether cmd was given --from-stdin.
func fromStdin(cmd *cobra.Command) bool {
	given, _ := cmd.Flags().GetBool("from-stdin")
	return given
}

// parseEntries is the entries of args: the arguments joined as if separated
// by commas, split at every comma, and each entry split at its first =, so a
// value may hold = but not a comma. Key names are the hub's to judge. A
// message names an entry by its position, never by its text, which may be
// part of a value.
func parseEntries(cmd *cobra.Command, args []string) ([]secretEntry, error) {
	var entries []secretEntry
	for i, entry := range strings.Split(strings.Join(args, ","), ",") {
		key, value, ok := strings.Cut(entry, "=")
		switch {
		case !ok:
			return nil, usageError("%s: entry %d has no =: give KEY=VALUE, or one KEY with --from-stdin", cmd.CommandPath(), i+1)
		case key == "":
			return nil, usageError("%s: entry %d has no key before =", cmd.CommandPath(), i+1)
		}
		entries = append(entries, secretEntry{Key: key, Value: value})
	}
	return entries, nil
}

// checkBareKey refuses what --from-stdin cannot take: anything but one key,
// without = or a comma, which would make it look like entries.
func checkBareKey(cmd *cobra.Command, args []string) error {
	if len(args) != 1 || args[0] == "" || strings.ContainsAny(args[0], "=,") {
		return usageError("%s --from-stdin takes one KEY without = or a comma", cmd.CommandPath())
	}
	return nil
}

// checkEntries refuses arguments that are not entries, or with --from-stdin
// not one bare key.
func checkEntries(cmd *cobra.Command, args []string) error {
	if fromStdin(cmd) {
		return checkBareKey(cmd, args)
	}
	_, err := parseEntries(cmd, args)
	return err
}

// checkOneEntry is checkEntries for a command that takes exactly one entry.
func checkOneEntry(cmd *cobra.Command, args []string) error {
	if fromStdin(cmd) {
		return checkBareKey(cmd, args)
	}
	entries, err := parseEntries(cmd, args)
	if err == nil && len(entries) != 1 {
		return usageError("%s takes one KEY=VALUE, got %d entries", cmd.CommandPath(), len(entries))
	}
	return err
}

// checkKey refuses an empty key, which would leave the route's last segment
// empty and so name another route.
func checkKey(cmd *cobra.Command, args []string) error {
	if args[0] == "" {
		return usageError("%s needs a KEY that is not empty", cmd.CommandPath())
	}
	return nil
}

// entriesOf is the entries of the command line c runs, the arguments as
// withSecretArgs accepted them, or with --from-stdin the one key with what
// standard input holds, without one trailing line ending.
func entriesOf(c *call) ([]secretEntry, error) {
	args := c.cmd.Flags().Args()
	if !fromStdin(c.cmd) {
		return parseEntries(c.cmd, args)
	}
	data, err := io.ReadAll(c.opts.stdin)
	if err != nil {
		return nil, clientError("cannot read the value from stdin: %v", err)
	}
	value := strings.TrimSuffix(string(data), "\n")
	if len(value) < len(data) {
		value = strings.TrimSuffix(value, "\r")
	}
	return []secretEntry{{Key: args[0], Value: value}}, nil
}

// createSecrets is the run step of parley secrets create. It sends the
// entries to every scope given, going on after one fails, prints the items
// of the scopes that succeeded and writes the error of each that failed to
// stderr once. The last failure is returned, and run prints it, so each is
// written once and the exit status is that failure's.
func createSecrets(c *call) error {
	entries, err := entriesOf(c)
	if err != nil {
		return err
	}
	type result struct {
		Scope string          `json:"scope"`
		Items json.RawMessage `json:"items"`
	}
	results := []result{}
	var failures []error
	for _, s := range secretScopes(c.cmd) {
		items, err := createIn(c, s, entries)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		results = append(results, result{s.name, items})
	}
	// Items are JSON the client has checked, so this cannot fail.
	doc, _ := json.Marshal(map[string]any{"results": results})
	if err := c.printResult(doc); err != nil {
		return err
	}
	if len(failures) == 0 {
		return nil
	}
	for _, err := range failures[:len(failures)-1] {
		printError(c.stderr, err)
	}
	return failures[len(failures)-1]
}

// createIn sends entries to the secrets of scope s and returns the items
// the hub answered.
func createIn(c *call, s secretScope, entries []secretEntry) (json.RawMessage, error) {
	resp, err := c.client.request(c.ctx, http.MethodPost, s.path, nil, map[string]any{"entries": entries})
	if err != nil {
		return nil, err
	}
	var answer struct {
		Items json.RawMessage `json:"items"`
	}
	if json.Unmarshal(resp.body, &answer) != nil || answer.Items == nil {
		return nil, clientError("the hub answered %d to the create in the %s scope without items", resp.status, s.name)
	}
	return answer.Items, nil
}

// listSecrets is the run step of parley secrets list: every page of the
// scope's secrets as one document.
func listSecrets(c *call) error {
	return printWholeList(c, secretScopes(c.cmd)[0].path)
}

// printWholeList reads every page of the list at path, the largest pages the
// hub serves, following next_cursor until it is null, and prints the items
// of all of them in order as one document {"items":[…]}, without
// next_cursor, which a whole list does not have.
func printWholeList(c *call, path string) error {
	items := []json.RawMessage{}
	query := url.Values{"limit": {listPageLimit}}
	for {
		resp, err := c.client.request(c.ctx, http.MethodGet, path, query, nil)
		if err != nil {
			return err
		}
		var page struct {
			Items      []json.RawMessage `json:"items"`
			NextCursor *string           `json:"next_cursor"`
		}
		if err := json.Unmarshal(resp.body, &page); err != nil {
			return clientError("the hub answered %d with a page that is not a list: %v", resp.status, err)
		}
		items = append(items, page.Items...)
		if page.NextCursor == nil {
			break
		}
		// A cursor that does not move would be followed for ever.
		if *page.NextCursor == query.Get("cursor") {
			return clientError("the hub answered the cursor it was sent as the next one")
		}
		query.Set("cursor", *page.NextCursor)
	}
	// The items are JSON the client has decoded, so this cannot fail.
	doc, _ := json.Marshal(map[string]any{"items": items})
	return c.printResult(doc)
}

// keyPath is the route of the secret key in the scope the flags of c name,
// the key encoded as one path segment.
func keyPath(c *call, key string) string {
	return secretScopes(c.cmd)[0].path + "/" + pathSegment(key)
}

// updateSecret is the run step of parley secrets update: the new value to
// the secret's route, and the hub's answer, the entry without a value.
func updateSecret(c *call) error {
	entries, err := entriesOf(c)
	if err != nil {
		return err
	}
	e := entries[0]
	resp, err := c.client.request(c.ctx, http.MethodPatch, keyPath(c, e.Key), nil, map[string]string{"value": e.Value})
	if err != nil {
		return err
	}
	return c.printAnswer(resp)
}

// deleteSecret is the run step of parley secrets delete. The hub answers no
// body, so the command prints its confirmation, {"deleted":"<KEY>"} with the
// key as typed.
func deleteSecret(c *call) error {
	resp, err := c.client.request(c.ctx, http.MethodDelete, keyPath(c, c.args["KEY"]), nil, nil)
	if err != nil {
		return err
	}
	return c.printAnswer(resp)
}
