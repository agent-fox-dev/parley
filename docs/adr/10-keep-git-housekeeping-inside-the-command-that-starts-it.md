# 10. Keep git's housekeeping inside the command that starts it

## Status

Accepted.

## Context

Many git commands end in automatic housekeeping: a fetch runs
`git maintenance run --auto`, a commit or a merge `git gc --auto`. By
default it detaches (`maintenance.autoDetach`, falling back to
`gc.autoDetach`, both true): the process daemonizes, leaves the process
group `internal/gitrepo` starts git in, and outlives the call.

In the hub image `parleyd` is PID 1 with no init, the entry point the
image's specification asks for. PID 1 inherits every orphan of the
container, and `parleyd` waits only for the processes it started itself,
so a detached housekeeping process that ends stays a zombie until the
container stops. A long-running hub would fill its process table with
them. A detached process is also out of reach of the kill that ends a git
command with its context, and still writes into the repository after the
call returned.

The options were an init process as the image's entry point, which
changes what the image is; `parleyd` reaping every orphan when it is
PID 1, which races the `Wait` of `os/exec` for the processes the hub does
start; turning automatic housekeeping off, which leaves the hub's
repositories never repacked, since nothing else maintains them; or keeping
the housekeeping and running it attached.

## Decision

Every git process `internal/gitrepo` starts runs with
`GIT_CONFIG_PARAMETERS='maintenance.autoDetach=false' 'gc.autoDetach=false'`
in its minimal environment. Git hands the setting on to the commands it
starts, and a caller's `-c` options add to it. The housekeeping a command
starts therefore runs in its foreground, as a child of that git, inside
its process group, and has ended when the call returns. The image keeps
`parleyd` as PID 1.

Where a command clears the configuration of the environment for the
command it starts (a push to a local repository starts `receive-pack`
without it), the caller turns the housekeeping off on that command's
own command line, as `Repo.SetRefFrom` does with `receive.autogc=false`.

## Consequences

- A call that triggers housekeeping takes as long as the housekeeping;
  it happens once in many writes, and the call's context bounds it, its
  kill taking the housekeeping with it.
- A repository is repacked only by the housekeeping its own commands
  trigger, as with a stock git.
- A process other than git that the hub starts and that detaches would
  still leave an orphan; none does.
