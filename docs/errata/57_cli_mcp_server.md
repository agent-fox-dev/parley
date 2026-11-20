# Errata: 57_cli_mcp_server

Where the implementation of package 57 (`.specs/57_cli_mcp_server`) differs
from what the package says, or decides what it leaves open.

## agentkit-go checks a call's parameters before the tool runs

The package has a parameter that is not in a tool's schema, one of the wrong
type and a missing required one end as a failed result with the error
`usage_error`. In a session the calls go through agentkit-go's runner, which
checks the parameters against the tool's input schema before the tool's
handler runs, as it does for every tool it runs, a script's calls included.
Such a call is a failed result with agentkit-go's error `invalid_arguments`,
whose detail names each parameter it refuses, and nothing is sent to the
hub. The runner also turns a number given for a string parameter into its
text, so `number: 3` is accepted as `"3"`.

The command tool's own check, with the error `usage_error`, still stands
behind it for what the runner lets through: a value of `-` that would read
standard input, an argument given after one that was left out, a command's
own refusal such as a missing `confirm`.

A call the runner refuses before the tool ran is written to stderr with
`"error": "not_run"`, since the runner tells the log only that the call
failed. Code: `internal/cli/mcp_serve.go` (`callLog`); tests:
`TestMCP_ServesACommandToolThroughTheRunnerAsTheSessionCallsIt`,
`TestMCP_WritesOneLineToStderrForEveryToolCall`.

## A session lists its tools by name

The package serves `code_mode` after the other tools, and a sandbox's tools
after the command tools. `parley mcp --list` prints them in that order. In a
session, `tools/list` is answered by the MCP SDK behind agentkit-go's
`mcpserve`, which lists a server's tools by name, so `code_mode` and a
sandbox's tools stand among the command tools there. Which tools are served
and what `code_mode` is bound to are the same in both. Test:
`TestMCP_SaysAtStartHowManyToolsItServesAndWhatCodeModeCosts`.

## The sandbox's own `code_mode` is left to the session's

The package refuses to start when a sandbox tool has the name of a command
tool or of `code_mode`. A tool server serves a `code_mode` of its own among
its tools (PRD 18 lists thirteen, `code_mode` among them), so with that rule
`parley mcp --sandbox` would start for no real sandbox. The session's
`code_mode` is bound to the sandbox's other tools and to the commands, which
is what the package's second flow asks for, so the sandbox's own is not
served while the session serves its own. With `--no-code-mode` it is a
sandbox tool like the others: its scripts run in the sandbox, over the
sandbox's tools alone. A sandbox tool with the name of a command tool is
still refused. Tests:
`TestSandboxTools_LeaveTheSandboxsOwnCodeModeToTheSessions`,
`TestMCP_RefusesASandboxToolNamedLikeAnotherTool`.

## Words after `--` and repeating arguments are array parameters

The package gives a tool one string parameter per named argument. Three
commands take words after `--` that are no argument of their declaration
(`sandbox exec`, which needs them, `sandbox create` and `fork create`), and
the last argument of seven commands takes every remaining word. A tool with
strings alone could not run a command in a sandbox at all, and could give a
repeating argument one word only. The declaration now names the words after
`--` (`Trailing`), and a tool has them, and a repeating argument, as an
array of strings, one word per element; `sandbox_exec` requires `command`.

A command that takes words after `--` gets its arguments before it, so
there an argument that begins with a dash is refused (`usage_error`); every
other command gets its arguments after `--`, where a dash is a character
like any other. Test:
`TestToolCall_GivesTheWordsAfterTheDoubleDashAndARepeatingArgumentAsArrays`.

## Every tool runs its command with `--json`

`parley sandbox exec` writes its command's output as it arrives unless
`--json` is given, and then prints the collected answer. A tool's result is
a JSON document, so every command a tool runs gets `--json`, which changes
nothing for the others. A command that fails in the sandbox is a failed
result with the error `command_exit` and the collected answer (exit code,
output) as its data; an output that is not one JSON document, such as the
lines of `sandbox_logs`, is `{"result": "<text>"}`.

## More than `ShowsOnce` keeps a command from being a tool

The package's decision 16 takes "shows a secret once" as the declaration's
`ShowsOnce`, which only commands of `keys` and `tokens` set. Three more
commands print a secret the hub shows once and write the warning
themselves, because they print it only in some of their forms: `webhook
create`, `webhook rotate-secret` and `forge webhook set`. The declaration
has a second mark, `PrintsSecret`, which they set, and neither kind is
served. `fork create` stays a tool, as decision 16 says.

## A read-only selection sends the hub nothing but `GET`s

The package narrows a read-only selection by the method of a command's
route. `change approvals` is a `GET` command whose `--withdraw` sends a
`DELETE`. A flag can be marked as one that makes its command write
(`markWrites`), and a tool selected for reading, by `--read-only` or by
`name:read`, lacks such a flag. With `--read-only`, of a sandbox's full set
its `code_mode` is left out too, with the four tools that change the
sandbox, since its scripts could call them; the hub's own read-only set is
served whole. Test:
`TestMCP_ServesNothingThatWritesToTheHubInAReadOnlySession`.

`--read-only` is about the hub. A reading tool can still write a local file
(`sandbox_download`, `output` of `scm_cat`), and several tools read local
files, as their commands do; `docs/cli.md` lists them.

## Smaller choices

- A boolean parameter given as `false` is left out of the command's line:
  some commands tell a flag given as false from one not given.
- A group named twice in `--groups` is a `usage_error`.
- A line on stderr names a failed result's error only when it is a code,
  and `other` otherwise: a sandbox tool's error is what a program in the
  sandbox printed.
- A call cancelled from outside, by the end of the session or of the
  script that made it, is the failed result `cancelled`, not `timeout`.
- A result reaches the harness as agentkit-go's `mcpserve` sends it: one
  text content with the JSON object `{ok, data, error, detail}`, without
  structured content.
- After the first SIGINT or SIGTERM a second one ends the process at once.

- The default selection is 68 command tools: the package counts 69, with
  `sandbox shell`, which is never served.
- A tool's error for an answer of the hub without an envelope is
  `http_<status>`; the client's error carries the status for it, and the
  envelope the shell prints is unchanged.
- A command's progress lines and warnings on stderr are not part of a
  tool's result.
- The first path's stderr has six lines, not five: the five commands and
  `code_mode` itself.
- `go.mod` takes agentkit-go and its module `mcpserve` at `v0.10.0`, the
  first release with the `runner` package.
