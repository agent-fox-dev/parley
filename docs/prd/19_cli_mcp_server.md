# Serve the CLI's commands as tools over MCP, with code mode

## Intent

An agent whose harness speaks MCP can do what the `parley` CLI does (see
who else is working, fork, read and commit, open and land a change, drive a
sandbox) as tool calls, by starting `parley mcp` on its own machine. With
code mode it does a whole sequence of them in one call, and with a sandbox
named, the same script mixes hub operations with file, search and shell
work in that sandbox. The tools are the commands themselves, so the CLI and
the tools cannot drift apart.

## Goals

1. `parley mcp` serves one MCP session on standard input and output, and a
   harness started with it lists and calls tools with no other setup than
   the CLI's own endpoint and credential.
2. Every served tool is one CLI command: its name, description, parameters
   and result come from the command's declaration, and calling it does what
   running the command does.
3. By default the tools are the commands an agent uses, at most 80 of them;
   the operator of the harness changes the selection with one flag; and
   commands that administer the hub or handle credentials are never served.
4. One `code_mode` call can run a script that makes several tool calls,
   some concurrently, and returns only what the script prints and returns.
5. With `--sandbox`, the sandbox's tools are served beside the hub tools
   and are callable from the same script.
6. The `parley` binary still builds without a C toolchain.

## Non-goals

- Serving MCP from the hub for hub operations. `parley mcp` runs where the
  agent runs; the hub runs no model-written script.
- MCP over HTTP from the CLI. Standard input and output only.
- New hub routes. Everything goes through routes that exist, and through
  the sandbox tool routes of PRD 18.
- Tools that are not commands, and commands invented for tools.
- A model loop, prompts, resources, sampling or elicitation.
- Serving the tools of more than one sandbox in one session.
- Changing what any command does when run from the shell.

## Background

`parley` declares every command through one helper (ADR 02,
`internal/cli/command.go`): its words, description, route, named arguments,
flags with type, default and description, and its `auth` level. The same
declaration drives the command and its JSON help: `parley help --json`
lists 171 commands today in 28 groups (the first word), each with `name`,
`description`, `method`, `path`, `args`, `flags` and `auth`. Flag types are
`bool`, `string`, `stringArray`, `int`, `integer` and `duration`. Every
command prints one JSON document or nothing on stdout, and every error is
one envelope, the hub's or one of the CLI's five types (`usage_error`,
`client_error`, `wait_failed`, `wait_timeout`, `condition_not_met`).

So a tool definition for each command already exists in all but form.
What does not exist is a way for a harness to use it: today an agent shells
out to `parley` and parses stdout, one process per operation.

Group sizes today: `change` 25, `admin` 22, `sandbox` 19, `scm` 11,
`workspace` 11, `issue` 9, `patch` 8, `pr` 7, `rebuild` 7, `tokens` 7,
`forge` 5, `vars` 5, `claim` 4, `fork` 4, `secrets` 4, `keys` 3, `orgs` 3,
`policy` 3, `events` 2, `land` 2, `rerere` 2, `user` 2, and `activity`,
`credential-helper`, `help`, `label`, `login`, `version` 1 each.

Code mode is agentkit-go's `codemode` package: one tool that runs a
model-written Starlark script whose functions are other tools. The script
has no file system, network or clock; it reaches the world only through the
tools bound to it, and its description declares each bound tool as a typed
function, so its size grows with the number of tools. agentkit-go's
`mcpserve` serves a runner's tools over MCP on stdio. Linking these today
needs cgo and the model providers' SDKs; agentkit-go's PRD 16 removes both
costs, and this PRD waits for it.

PRD 18 gives a sandbox whose profile names a tool server two REST routes:
`GET …/sandboxes/:id/tools` lists the caller's tool set with schemas, and
`POST …/sandboxes/:id/tools/:name` makes one call.

## Requirements

### The command

`parley mcp` is declared like every command (ADR 02), with `auth` level
`user`. It resolves the endpoint and the credential as every command does,
before it serves anything.

- Without an endpoint or a credential it ends as any command does: a
  `client_error` envelope on stderr, exit status 2, and nothing on stdout.
- It then serves one MCP session until the client closes its end, and
  exits 0. Only protocol frames go to stdout; one line per tool call
  (tool, outcome, duration, never arguments or results) goes to stderr.
- `initialize` answers with the server name `parley`, the CLI's version and
  instructions of at most three sentences: what the tools are, the fixed
  workspace and sandbox when given, and that `code_mode` chains them.
- SIGINT or SIGTERM ends the session and cancels running calls.
- `parley mcp --list` prints the tools it would serve as one JSON array of
  `{name, description, input_schema}` and exits 0, without starting a
  session. With `--sandbox` it includes the sandbox's tools.
- The credential is read once at start. The hub's `401` on a later call is
  that call's tool error.

### Which commands are tools

`--groups` selects command groups, comma-separated. A group name alone
serves all of the group's commands; `name:read` serves only those whose
route's method is `GET`. The default is
`activity,change,claim,fork,land,sandbox,scm,workspace:read`.

- **Never served**, whatever `--groups` says: the groups `admin`, `keys`,
  `tokens`, `secrets`, `login`, `credential-helper`, `help` and `version`;
  `mcp` itself; every command whose `auth` is `admin` or `none`; every
  command that shows a secret once; and every command that needs a
  terminal or runs until interrupted (`sandbox shell`, `events tail`).
  Naming a never-served group in `--groups` is a `usage_error` that names
  it.
- An unknown group name is a `usage_error` listing the group names.
- `--read-only` narrows whatever is selected to `GET` commands, and with
  `--sandbox` asks for nothing more than the sandbox's read-only set.
- More than 150 tools after selection is a `usage_error` saying how many
  and to narrow `--groups`.
- A command added to a served group later is a tool without further work.

### How a command becomes a tool

- **Name.** The command's words joined by `_`, with `-` replaced by `_`:
  `change land` is `change_land`.
- **Description.** The command's description.
- **Parameters.** One per named argument (a string, required as the
  command requires it) and one per flag, typed by the flag's type
  (`stringArray` is an array of strings; `duration` is a string such as
  `90s`), with the flag's description. Flag names keep their words with
  `-` replaced by `_`. Left out: `api-key`, `endpoint-url`, `json` and
  `poll-interval`, and any flag that makes a command run until
  interrupted (`follow` of `sandbox logs`; `follow` of `scm log`, which
  follows renames, stays).
- **`--workspace <slug>`** makes the `slug` parameter of every tool
  optional, with that workspace as its value when left out. A fork's slug
  can still be passed.
- **The call.** The tool runs the command's own run step in the `parley
  mcp` process, with the given arguments and flags and JSON output.
- **The result.** A command that prints a JSON object succeeds with that
  object as the tool's data. Any other JSON value is `{"result": value}`.
  A command that prints nothing is `{}`.
- **Errors.** An error envelope is a failed tool result, not a protocol
  error: `error` is the envelope's `error_type` (or `http_<code>` when the
  hub gave none), `detail` its message. The CLI's own error types are
  failed results with that type as `error`. A parameter that is not in the
  schema or has the wrong type is `usage_error`, and nothing is sent.
- **No standard input.** A tool never reads stdin or a terminal. A value
  of `-` where the command would read stdin is `usage_error`. No command
  asks a question on a terminal: one that destroys data requires its
  `confirm` flag, which is the tool's `confirm` parameter, and without it
  the call is `usage_error` as from the shell.
- **Waiting.** A command with `--wait` waits inside the tool call, bounded
  by its `timeout` parameter as from the shell.
- **Local files.** A command that reads or writes a local path
  (`sandbox upload`, `sandbox download`) does so on the machine where
  `parley mcp` runs, relative to its working directory.
- A tool call is bounded at 30 minutes; past it the call is cancelled and
  fails with `error: "timeout"`.

### Code mode

`code_mode` is served after the other tools and is bound to all of them.

- Its rules and limits are agentkit-go's, except the script's wall time,
  which is 10 minutes by default and set with `--script-timeout` (from 1
  second to 30 minutes; outside that is a `usage_error`).
- Every call a script makes is a tool call like a direct one: same
  parameters, same result, one stderr line each.
- `--no-code-mode` leaves it out.
- At start `parley mcp` writes one line to stderr with the number of tools
  and the size of the `code_mode` description in bytes.

### A sandbox's tools

`--sandbox <id>` names one sandbox of the workspace given with
`--workspace`, which is then required (`usage_error` without it).

- At start the tools are read from the hub (`GET …/sandboxes/:id/tools`).
  If that fails, `parley mcp` ends with the hub's error envelope on stderr
  and exit status 1: a sandbox without tools (`tools_unavailable`), not
  ready, unknown, or not accessible.
- Each sandbox tool is served under its own name, description and input
  schema, after the command tools and before `code_mode`, and is bound to
  `code_mode`.
- A call is one `POST …/tools/:name`. The hub's `{ok, data, error,
  detail, text}` is the tool's result as it is. A hub error (the sandbox
  stopped, a limit) is a failed result with its `error_type`.
- A sandbox tool whose name equals a command tool's is an error at start
  naming both, exit status 2.
- The set is the one the hub gives the credential: the full set, or the
  read-only set. With `--read-only`, tools of the full set that are not in
  the read-only set are not served.

### End-to-end flows

1. **An agent lands a change.** A harness starts
   `parley mcp --workspace demo`. The agent calls `activity`, then
   `fork_create`, then one `code_mode` script that commits two files with
   `scm_commit` on the fork, opens the change with `change_open` and lands
   it with `change_land` with `wait` set; the script returns the landed
   sha. The hub's audit shows the same requests the shell commands make.
2. **Hub and sandbox in one script.** Started with `--workspace demo--fix
   --sandbox sbx_…`, a script runs `search_files` and `edit_file` in the
   sandbox, `execute` to run the tests and push, then `change_show` on the
   hub, and returns the change's state.
3. **A read-only assistant.** Started with `--read-only`, the tool list
   holds only reading commands, and `change_land` is not in it.
4. **A refusal.** `parley mcp --groups admin` prints a `usage_error` and
   exits 2.

## Design Decisions

1. **The tools are generated from the command declarations** (my decision,
   following ADR 02): one source for the shell, the JSON help and the
   tools.
2. **The agent groups are direct tools and are bound to code mode;
   `--groups` changes the selection** (owner's decision).
3. **`--sandbox` adds one sandbox's tools** (owner's decision).
4. **`--workspace` is optional** (owner's decision). It makes `slug`
   optional with a default instead of removing the parameter: an agent
   that forks has to name the fork, which a removed parameter would
   prevent.
5. **`parley` stays pure Go; agentkit-go is slimmed first** (owner's
   decision). This PRD waits for agentkit-go's PRD 16.
6. **`group:read` instead of a special case for `workspace`**: the default
   wants the workspace reads without archive and delete, and the same
   suffix is useful for every group.
7. **Never-served commands are decided by rule, not by list**, so a new
   admin or secret-showing command is excluded without anyone remembering
   to.
8. **Sandbox tools keep their names** (`read_file`, not
   `sandbox_read_file`): models know them, and command tools always carry
   their group as a prefix, so a clash is unlikely and is an error.
9. **No guard of the CLI's own on `execute`**: it runs in the sandbox, and
   the hub's rules for the route decide who may call it.
10. **Script wall time 10 minutes**, not code mode's 30 seconds: one
    `change_land` with `wait` can take minutes.
11. **Constraint: MCP and code mode come from agentkit-go** (`mcpserve`,
    `codemode`, `runner`), not from new code in Parley.

## Dependencies

- ADR 02 (`docs/adr/02-declare-every-cli-command-through-one-helper.md`)
  and package 09 `cli_foundation`: the declarations this reads. `mcp` is
  declared through the same helper.
- PRD 18 (`docs/prd/18_sandbox_tool_routes.md`, not built): the two REST
  routes `--sandbox` uses. Everything but `--sandbox` works without it.
- agentkit-go PRD 16 (in pull request 185 of that repository, not built):
  the runner, code mode and MCP serving without cgo and providers. Hard
  dependency.
- Package 49 `agent_sessions_and_usage` (active, no task done): when its
  `session` commands exist they are a group like any other and can be
  named in `--groups`; the default list does not name them yet.

## Verified External API

`github.com/agent-fox-dev/agentkit-go`, read in its checkout
(`docs/generated/` and `go doc`):

- `core.Tool` is a struct with `Name`, `Description`,
  `InputSchema *schema.Schema`, `OutputSchema *schema.Schema`,
  `ReachableTools []Tool`, `Handler ToolHandler`, `Execute ResultHandler`.
- `core.ToolResult` has `OK bool`, `Data map[string]any`, `Error string`,
  `Detail string`; `core.OKResult(data map[string]any) ToolResult` and
  `core.ErrResult(code, detail string) ToolResult`.
- `codemode.New(tools []core.Tool, opts codemode.Options) (core.Tool, codemode.BuildInfo, error)`;
  `Options` has `MaxTimeout`, `MaxSteps`, `MaxCalls`,
  `MaxConcurrentCalls`, `MaxOutputBytes`; `BuildInfo` has
  `DescriptionBytes`, `DescriptionChars`, `BoundToolsCount`.
- `agentkit.NewRunner(ts agentkit.Toolset) (*agentkit.Runner, error)`;
  `Toolset` has `Tools`, `Policy`, `Guard`, `After`, `Events`. A reachable
  tool named `execute` or `run_command` needs a `Guard`
  (`guard.AllowAll` exists).
- `mcpserve.New(r *agentkit.Runner, opts mcpserve.Options) (*mcpserve.Server, error)`
  and `(*Server).ServeStdio(ctx context.Context) error`; `Options` has
  `Name`, `Version`, `Instructions`, `Logger`. `ServeStdio` closes
  `os.Stdin` when the session ends.

**Not built**: package `runner` and `runner.New` (agentkit-go PRD 16); the
signatures above are today's, under the root package. **Unverified**: the
signatures of `core.ToolHandler` and `core.ResultHandler`, how a
`schema.Schema` is built from a JSON schema document, and whether a
`core.Tool` with only `Execute` set is served by `mcpserve`.

## Glossary

- **Command tool**: a tool generated from one CLI command's declaration.
- **Sandbox tool**: a tool of a sandbox's tool set (PRD 18), relayed by
  `parley mcp` to the hub.
- **Group**: the first word of a command (`change`, `scm`).
- **Code mode**: the `code_mode` tool, which runs a Starlark script whose
  functions are the other served tools.
