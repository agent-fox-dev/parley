package cli

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// queryKind is how a flag of a parley scm read becomes query parameters.
type queryKind int

const (
	// textFlag sends its value as given, for the hub to judge.
	textFlag queryKind = iota
	// intFlag takes an integer, which the flag parser checks, and sends it.
	intFlag
	// listFlag repeats, and sends one parameter for each value.
	listFlag
	// switchFlag is a boolean that sends its fixed value when given.
	switchFlag
	// linesFlag takes <start>:<end>, two positive integers, and sends
	// start_line and end_line.
	linesFlag
)

// queryFlag is a flag of a parley scm read. A flag that is not given sends
// nothing, so that the hub's default applies.
type queryFlag struct {
	name, usage string
	kind        queryKind
	// param is the query parameter the flag is sent as.
	param string
	// sends is the value a switch sends.
	sends string
	// oneOf, when not empty, are the only values a text flag takes.
	oneOf []string
}

// The flags several reads share.
var (
	refFlag = func(what string) queryFlag {
		return queryFlag{name: "ref", param: "ref", usage: "the revision whose " + what + " (default: the workspace's default branch)"}
	}
	limitFlag  = queryFlag{name: "limit", param: "limit", kind: intFlag, usage: "the most items the page holds (default: the hub's)"}
	cursorFlag = queryFlag{name: "cursor", param: "cursor", usage: "the next_cursor of the page before, to read the page after it"}
	globsFlag  = func(what string) queryFlag {
		return queryFlag{name: "path", param: "path", kind: listFlag, usage: "a glob of the paths " + what + ", repeatable; * within a segment, ** across segments"}
	}
)

// scmRead declares one parley scm read: GET of a route under
// /workspaces/:slug/scm with a query parameter for each argument params
// names and each flag of query given.
type scmRead struct {
	command
	// params maps a positional argument to the query parameter it is sent
	// as; an optional argument not given sends nothing.
	params map[string]string
	query  []queryFlag
	// check refuses a combination of flags, as a usage error.
	check func(cmd *cobra.Command) error
	// send replaces sending the query and printing the JSON answer.
	send func(c *call, query url.Values) error
}

// newSCMCommand builds parley scm: the reads of a workspace's repository
// without a clone.
func newSCMCommand() *cobra.Command {
	slugOnly := []argSpec{slugArg}
	return group("scm", "Read the refs, commits, files, diffs and blame of a workspace without a clone",
		scmRead{
			command: command{Name: "refs", Description: "List the branches, tags and hub-owned refs of a workspace", Path: "/workspaces/:slug/scm/refs", Args: slugOnly},
			query: []queryFlag{
				{name: "prefix", param: "prefix", usage: "the start of the full ref names listed (default: refs/heads/)"},
				limitFlag, cursorFlag,
			},
		}.build(),
		scmRead{
			command: command{Name: "log", Description: "List the history of a revision, newest first", Path: "/workspaces/:slug/scm/commits", Args: slugOnly},
			query: []queryFlag{
				refFlag("history is listed"),
				{name: "path", param: "path", usage: "only the commits that changed this path"},
				{name: "follow", param: "follow", kind: switchFlag, sends: "true", usage: "follow --path, one file, across renames"},
				{name: "since", param: "since", usage: "only the commits committed at or after this RFC 3339 time"},
				{name: "until", param: "until", usage: "only the commits committed at or before this RFC 3339 time"},
				{name: "author", param: "author", usage: "only the commits whose author's name or e-mail contains this text"},
				{name: "stats", param: "stats", kind: switchFlag, sends: "true", usage: "add each commit's insertions and deletions"},
				limitFlag, cursorFlag,
			},
		}.build(),
		scmRead{
			command: command{Name: "show", Description: "Show one commit", Path: "/workspaces/:slug/scm/commits/:rev", Args: []argSpec{slugArg, {Name: "rev", Required: true}}},
			query: []queryFlag{
				{name: "files", param: "files", kind: switchFlag, sends: "true", usage: "add the files the commit changed"},
			},
		}.build(),
		scmRead{
			command: command{Name: "ls", Description: "List a directory of a revision", Path: "/workspaces/:slug/scm/tree", Args: []argSpec{slugArg, {Name: "path"}}},
			params:  map[string]string{"path": "path"},
			query: []queryFlag{
				refFlag("tree is listed"),
				{name: "recursive", param: "recursive", kind: switchFlag, sends: "true", usage: "list every entry below the directory"},
			},
		}.build(),
		scmRead{
			command: command{
				Name: "cat", Description: "Write one file of a revision, its raw bytes, to stdout or a file", Path: "/workspaces/:slug/scm/file",
				Args:  []argSpec{slugArg, {Name: "path", Required: true}},
				Flags: func(fs *pflag.FlagSet) { fs.String("output", "", "write the bytes to this file instead of stdout") },
			},
			params: map[string]string{"path": "path"},
			query:  []queryFlag{refFlag("file is read")},
			send:   catFile,
		}.build(),
		scmRead{
			command: command{Name: "diff", Description: "Show the file changes between two revisions", Path: "/workspaces/:slug/scm/diff", Args: []argSpec{slugArg, {Name: "base", Required: true}, {Name: "head", Required: true}}},
			params:  map[string]string{"base": "base", "head": "head"},
			query: []queryFlag{
				{name: "mode", param: "mode", oneOf: []string{"merge_base", "direct"}, usage: "merge_base compares the merge base with head, direct base itself (default: merge_base)"},
				globsFlag("kept"),
				{name: "patch", param: "format", kind: switchFlag, sends: "patch", usage: "print the unified diff text instead of JSON"},
				{name: "name-only", param: "patches", kind: switchFlag, sends: "false", usage: "leave the patch text out of the files"},
			},
			check: func(cmd *cobra.Command) error {
				patch, _ := cmd.Flags().GetBool("patch")
				nameOnly, _ := cmd.Flags().GetBool("name-only")
				if patch && nameOnly {
					return usageError("%s takes --patch or --name-only, not both", cmd.CommandPath())
				}
				return nil
			},
			send: diffRevisions,
		}.build(),
		scmRead{
			command: command{
				Name: "compare", Description: "Tell whether head still rebases onto base, with the conflict as data", Path: "/workspaces/:slug/scm/compare",
				Args: []argSpec{slugArg, {Name: "base", Required: true}, {Name: "head", Required: true}},
				FailOn: []failOn{{
					Name:        "conflict",
					Description: "exit 3 when head does not rebase onto base without a conflict",
					Check:       conflictIn,
				}},
			},
			params: map[string]string{"base": "base", "head": "head"},
		}.build(),
		scmRead{
			command: command{Name: "blame", Description: "Show which commit last changed each line of a file", Path: "/workspaces/:slug/scm/blame", Args: []argSpec{slugArg, {Name: "path", Required: true}}},
			params:  map[string]string{"path": "path"},
			query: []queryFlag{
				refFlag("file is blamed"),
				{name: "lines", kind: linesFlag, usage: "only the lines <start>:<end>, 1-based and inclusive"},
			},
		}.build(),
		scmRead{
			command: command{Name: "search", Description: "Search the file contents of a revision", Path: "/workspaces/:slug/scm/search", Args: []argSpec{slugArg, {Name: "query", Required: true}}},
			params:  map[string]string{"query": "q"},
			query: []queryFlag{
				refFlag("files are searched"),
				globsFlag("searched"),
				{name: "regex", param: "regex", kind: switchFlag, sends: "true", usage: "take the query as an extended regular expression"},
				{name: "ignore-case", param: "case_sensitive", kind: switchFlag, sends: "false", usage: "match without regard to case"},
			},
		}.build(),
	)
}

// build declares r. Its query is built while the command line is checked,
// so that a flag value it refuses is a usage error before any setting is
// resolved or any request sent (ADR 02).
func (r scmRead) build() *cobra.Command {
	decl := r.command
	decl.Method = http.MethodGet
	decl.Auth = authUser
	own := decl.Flags
	decl.Flags = func(fs *pflag.FlagSet) {
		for _, f := range r.query {
			f.add(fs)
		}
		if own != nil {
			own(fs)
		}
	}
	decl.Run = func(c *call) error {
		query, err := r.queryOf(c.cmd, c.args)
		if err != nil {
			return err
		}
		if r.send != nil {
			return r.send(c, query)
		}
		return sendWith(c, query, nil)
	}
	cmd := declare(decl)
	checkLine := cmd.Args
	cmd.Args = func(cmd *cobra.Command, args []string) error {
		if err := checkLine(cmd, args); err != nil {
			return err
		}
		named := map[string]string{}
		for i, a := range args {
			named[decl.Args[i].Name] = a
		}
		_, err := r.queryOf(cmd, named)
		return err
	}
	return cmd
}

// queryOf is the query of the read: the arguments params names, then each
// flag given.
func (r scmRead) queryOf(cmd *cobra.Command, args map[string]string) (url.Values, error) {
	if r.check != nil {
		if err := r.check(cmd); err != nil {
			return nil, err
		}
	}
	query := url.Values{}
	for arg, param := range r.params {
		if value, ok := args[arg]; ok {
			query.Set(param, value)
		}
	}
	fs := cmd.Flags()
	for _, f := range r.query {
		if !fs.Changed(f.name) {
			continue
		}
		if err := f.addTo(query, fs, cmd.CommandPath()); err != nil {
			return nil, err
		}
	}
	return query, nil
}

// add adds f to fs.
func (f queryFlag) add(fs *pflag.FlagSet) {
	switch f.kind {
	case intFlag:
		fs.Int(f.name, 0, f.usage)
	case listFlag:
		fs.StringArray(f.name, nil, f.usage)
	case switchFlag:
		fs.Bool(f.name, false, f.usage)
	case textFlag, linesFlag:
		fs.String(f.name, "", f.usage)
	}
}

// addTo adds the parameters of f, given, to query.
func (f queryFlag) addTo(query url.Values, fs *pflag.FlagSet, commandPath string) error {
	switch f.kind {
	case listFlag:
		values, _ := fs.GetStringArray(f.name)
		query[f.param] = append(query[f.param], values...)
	case switchFlag:
		if on, _ := fs.GetBool(f.name); on {
			query.Set(f.param, f.sends)
		}
	case linesFlag:
		value, _ := fs.GetString(f.name)
		start, end, ok := lineSpan(value)
		if !ok {
			return usageError("%s takes --%s <start>:<end>, two positive integers, got %q", commandPath, f.name, value)
		}
		query.Set("start_line", strconv.Itoa(start))
		query.Set("end_line", strconv.Itoa(end))
	case textFlag, intFlag:
		value := fs.Lookup(f.name).Value.String()
		if len(f.oneOf) > 0 && !slices.Contains(f.oneOf, value) {
			return usageError("%s takes --%s %s, got %q", commandPath, f.name, strings.Join(f.oneOf, " or "), value)
		}
		query.Set(f.param, value)
	}
	return nil
}

// lineSpan reads <start>:<end>, two positive integers.
func lineSpan(value string) (start, end int, ok bool) {
	first, last, found := strings.Cut(value, ":")
	if !found {
		return 0, 0, false
	}
	start, err1 := strconv.Atoi(first)
	end, err2 := strconv.Atoi(last)
	if err1 != nil || err2 != nil || start < 1 || end < 1 {
		return 0, 0, false
	}
	return start, end, true
}

// readRaw sends the read with query and returns the body of its 200
// answer, whatever its form, with accept as the Accept header ("" for
// application/json). The body is read whole, however large: the hub bounds
// what it serves ([scm].max_file_size).
func readRaw(c *call, query url.Values, accept string) ([]byte, error) {
	resp, err := c.client.raw(c.ctx, apiRequest{method: c.decl.Method, path: routePath(c.decl.Path, c.args), query: query, accept: accept})
	switch {
	case err != nil:
		return nil, err
	case resp.status >= 400 && isHubEnvelope(resp.body):
		return nil, &hubError{status: resp.status, body: resp.body}
	case resp.status >= 400:
		return nil, clientError("the hub answered %d without an error envelope", resp.status)
	case resp.status != http.StatusOK:
		return nil, clientError("the hub answered %d, where a read answers 200", resp.status)
	}
	return resp.body, nil
}

// catFile is the run step of parley scm cat. It asks for the raw form, so
// the bytes are the file's own and no JSON framing, and writes them only
// once the hub has answered 200: a refusal leaves stdout empty and the
// --output file, if one exists, as it was.
func catFile(c *call, query url.Values) error {
	// Any form but JSON is the raw one, and an error is JSON either way.
	body, err := readRaw(c, query, "*/*")
	if err != nil {
		return err
	}
	if c.cmd.Flags().Changed("output") {
		out, _ := c.cmd.Flags().GetString("output")
		if err := os.WriteFile(out, body, 0o666); err != nil {
			return clientError("cannot write the file: %v", err)
		}
		return nil
	}
	if _, err := c.stdout.Write(body); err != nil {
		return clientError("cannot write the file's bytes: %v", err)
	}
	return nil
}

// diffRevisions is the run step of parley scm diff: with --patch it prints
// the unified diff text as received, otherwise the JSON answer.
func diffRevisions(c *call, query url.Values) error {
	if patch, _ := c.cmd.Flags().GetBool("patch"); !patch {
		return sendWith(c, query, nil)
	}
	body, err := readRaw(c, query, "")
	if err != nil {
		return err
	}
	if _, err := c.stdout.Write(body); err != nil {
		return clientError("cannot print the patch: %v", err)
	}
	return nil
}

// conflictIn is the condition of parley scm compare --fail-on-conflict:
// the answer's mergeable is conflict.
func conflictIn(result []byte) (bool, string) {
	var answer struct {
		Mergeable string `json:"mergeable"`
	}
	if json.Unmarshal(result, &answer) != nil || answer.Mergeable != "conflict" {
		return false, ""
	}
	return true, "head does not rebase onto base without a conflict: mergeable is conflict"
}
