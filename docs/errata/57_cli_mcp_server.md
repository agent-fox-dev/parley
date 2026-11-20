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
`TestMCP_RefusesASandboxToolNamedLikeACommandTool`.

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

## Smaller choices

- The default selection is 68 command tools: the package counts 69, with
  `sandbox shell`, which is never served.
- A tool's error for an answer of the hub without an envelope is
  `http_<status>`; the client's error carries the status for it, and the
  envelope the shell prints is unchanged.
- A command's progress lines and warnings on stderr are not part of a
  tool's result.
- The first path's stderr has six lines, not five: the five commands and
  `code_mode` itself.
- `go.mod` takes agentkit-go and its module `mcpserve` at pseudo-versions
  of the commit `20c16c9`, since no release holds the `runner` package
  yet.
