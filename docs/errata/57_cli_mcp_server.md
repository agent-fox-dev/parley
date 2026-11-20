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
