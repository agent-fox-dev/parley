# 02. Declare every CLI command through one helper

## Status

Accepted.

## Context

`parley` will hold close to two hundred commands, written by many packages,
and the people and agents who script it rely on all of them behaving alike:
one JSON document on stdout or nothing, every error as one error envelope on
stderr, and an exit status from a fixed table. Cobra, left to itself, does
not give that. It prints its own `Error: …` line and usage text, it answers
an unknown word under a command group with that group's help and exit
status `0`, and it reports a bad flag, a wrong number of arguments and an
unknown command as plain errors that cannot be told apart from a failure in
a command's own code. If every command set up cobra by hand, each would
need to repeat the same wiring, and the first one that did not would break
the contract for its callers.

## Decision

Every command is declared through `declare` in `internal/cli/command.go`,
and every command group through `group`; the root command is a group too.

- A command is a `command` value: its last word, its description, the
  route it calls (`Method`, and `Path` relative to `/api/v1` with `:param`
  for its parameters, both empty when it calls no single route), its named
  arguments with required or optional, its `auth` level (`none`, `user`,
  `admin`), its own flags and its run step. A command states only what
  differs from the others; later fields add what some commands need.
- `declare` keeps the declaration, as JSON, in the cobra command's
  `Annotations` under `parley.declaration`, so that what describes the
  command (its help, for one) is built from the same value it runs from.
- `declare` sets cobra's `Args` from the declared arguments, and a
  wrong number of them is a `usage_error`. `group` refuses any word that
  is not one of its children as a `usage_error`, and prints its help when
  it is given none. The root's flag error handler, which every command
  inherits, turns a flag that is unknown or does not parse into a
  `usage_error`. All of these happen before a run step starts, so a usage
  error reads no setting and sends no request.
- A command that declares no run step gets the default one: it sends its
  `Method` to its `Path` through the shared client and prints the hub's
  answer with the shared printer (`internal/cli/output.go`). What a command
  adds to the printing is declared too: `Confirm`, the document a success
  without a body prints in place of `{}`; `ShowsOnce`, which adds the
  show-once warning after the result; and `FailOn`, the `--fail-on-…` flags
  whose condition, when it holds, ends the command with `condition_not_met`
  once the result is printed.
- A run step returns an error and never writes one. `run` in
  `internal/cli/root.go` prints the error with `printError` and returns
  `exitCode(err)`, both in `internal/cli/errors.go`. Cobra's own error and
  usage output is silenced, so stderr holds the envelope alone.
- There are two kinds of error. `hubError` is the hub's error envelope, kept
  as received and printed indented again by two spaces, never decoded, so its
  members and their order reach stderr unchanged; it maps to exit status `1`. `cliError` is an error the CLI
  raises itself, of one of five types, each with its constructor:
  `usage_error` and `client_error` map to `2`, `wait_failed` and
  `wait_timeout` to `1`, `condition_not_met` to `3`. It is printed as
  `{"error":{"code":0,"message":…,"error_type":…}}`, indented by two
  spaces and followed by a newline. Any other error, which only a failure
  inside the CLI produces, is printed and counted as a `client_error`,
  because the exit status must stay one of `0` to `3`.

## Consequences

- A package that adds commands writes a `command` value per command and
  a `group` per command group; it does not touch cobra's error handling,
  argument checks or printing, and cannot get them wrong.
- The exit status of a run depends on the error alone, so it is the same
  table on every command, and a script can branch on it.
- A run step that wants a particular exit status must return the matching
  error kind; any error it returns that is not one of them is a
  `client_error` with exit status `2`.
- Tests add probe commands to a fresh tree through the unexported
  `options.extra` and declare them with the same helper, so a test of the
  shared behaviour runs through exactly the code real commands use.
