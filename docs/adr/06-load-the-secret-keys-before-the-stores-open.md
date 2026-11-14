# 06. Load the secret keys before the stores open

## Status

Accepted.

## Context

The hub seals every stored secret under a key the operator supplies in the
environment (`PARLEY_SECRETS_KEY`, `PARLEY_SECRETS_KEY_PREVIOUS`) or in the
file `[secrets].key_file` names. A key that is malformed, or a key file that
cannot be read, must stop the start before the hub listens, with the one
`cannot start` line every failed start writes, naming the variable or the
file and never the value. A start with good keys, or with none, must tell
the operator what it found: whether a current and a previous key are
configured, how many stored values neither key opens, and, without a current
key, that secrets cannot be stored.

`hub.Run` reads no process environment: `Options.Env` is its only source, so
that tests start hubs with environments of their own in parallel. Until now
only the configuration loader read it, to replace `${NAME}` references. A
part of the composition saw the loaded configuration in its configuration
check and the runtime in its start hook, but not the environment.

The hub also promises that a failed start writes exactly one line. Start
hooks run one after the other before the bind, so a line a hook writes when
it succeeds would precede the error line of a later hook, or of a bind that
fails.

## Decision

- **The composition keeps the hub's environment.** `(*run).configure` sets
  the unexported field `env` of the `Composition` to `Options.Env` (an empty
  environment when it is nil) before the parts run. Parts of `internal/hub`
  read it; it is not exported, and `Options.Compose` does not see it.
- **The keys load in a configuration check.** `composeSecrets` adds a check
  with `addConfigCheck` that calls `secret.LoadKeys(c.env, path)`, with the
  path of `[secrets].key_file` taken from the config dir when it is
  relative, and keeps the keys for the start hook that binds the secret
  component. A loading error is the check's error: the start stops with one
  `cannot start` line and exit status 1, before any store is opened and
  before the listener is bound. The bootstrap and `backup` load the
  configuration the same way, so they refuse a bad key too.
- **Start reports are written once the listener is bound.** A part that
  reports how it started computes the report in its start hook and adds a
  writer with the unexported `addStartReport(func(*slog.Logger))`. The hub
  calls the writers, in the order they were added, from the server's
  `OnListen`, right before the `listening` line. The secret component's
  report is the `secret encryption` line and, without a current key, the
  warn line `secrets cannot be stored: no encryption key configured`.

## Consequences

- A malformed key costs nothing to discover: no store is opened, no schema
  step runs, nothing is created.
- Another part that needs a value from the environment that is not a
  configuration key reads `c.env` in its configuration check, in the same
  way, and keeps what it loaded for its start hook.
- A failed start still writes one line and nothing else, whatever stage
  fails. The start lines of the parts and the `listening` line are written
  by one goroutine in order, so a reader that waits for the `listening`
  line has seen every start report.
- The report is written after the bind rather than in the start hook, so a
  start that fails after the secrets hook has run does not tell the
  operator about the keys; its error line is the one thing to act on.
