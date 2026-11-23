# 12. Start git without forking the hub on macOS

## Status

Accepted.

## Context

The hub runs git as a child process for every operation it does not do
in process (`internal/gitrepo`), thousands of times in a busy hub and
tens of thousands in one run of the test suite. `os/exec` starts a
process the same way everywhere except Linux: it forks the whole Go
process and execs git in the child. On Linux it uses `CLONE_VFORK`, which
copies nothing. On macOS the fork marks every page of the hub
copy-on-write, and the hub then takes a page fault on each page it writes
while the child starts and after it has exec'd. Measured from a busy Go
process (four goroutines writing a 150 MB heap), 2,000 starts of `git
--version` cost the parent 15.6 s of system time with `os/exec` and 2.3 s
with `posix_spawn`, which macOS implements in the kernel without copying
the parent (3.9 million page faults against 35 thousand).

On the development machines, which are Macs, that cost was the largest
part of the test suite's system time, which was larger than its user
time in every package that starts hubs, and it kept the full gate over
its budget.

The options were fewer git processes, by moving more git operations in
process, which earlier repairs did where the format allows it and which
leaves the rest; a long-lived helper process that forks git for the hub,
which adds a protocol and a process to supervise; or `posix_spawn` itself,
which the standard library does not offer and which needs cgo, which
`parleyd` already links for the outline parsers.

## Decision

A new leaf package, `internal/spawn`, runs a command as `os/exec` does,
but on macOS, when it is built with cgo, starts it with `posix_spawn`.
Its `Cmd` embeds the `exec.Cmd` it is configured through and has its own
`Start`, `Wait`, `Run`, `Output`, `CombinedOutput`, `StdinPipe` and
`StdoutPipe`, which keep `os/exec`'s semantics: the same pipes and
copying, `Cancel` when the context ends, `WaitDelay` and
`*exec.ExitError`. A spawned process inherits no descriptor but its
standard input, output and error, has every signal at its default
disposition and none blocked, and is in a process group of its own when
`SysProcAttr.Setpgid` asks for one. A command that asks for anything else
of `SysProcAttr`, or for `ExtraFiles`, and every command on other
platforms, is started by `os/exec`.

`internal/gitrepo` starts every git process through it (`Git.command`),
and so do the test helpers that run git many times (`gitrepotest` and the
packages' own git helpers), which run in test processes as large as a
hub.

## Consequences

- On Linux, where the hub image runs, nothing changes. A macOS build
  without cgo starts git with `os/exec` as before.
- On macOS the full test suite uses about 15% less CPU (938 → 797 s on
  the same package order).
- Only the methods `spawn.Cmd` overrides take the `posix_spawn` path. Its
  caller must not mix them with other `exec.Cmd` methods, such as
  `StderrPipe`, which start the process with `os/exec`.
- New code that runs a process many times from a large process, in
  production or in a test helper, should use `internal/spawn`. A command
  run a few times can keep `os/exec`.
