# Errata: 19_git_push_to_change

Where the implementation of package 19 (`.specs/19_git_push_to_change`)
differs from what the package says, or decides what it leaves open.

## Command order is the order git sends the commands in

The package has the lines of a push follow its command order, and its
check expects `git push -o parley.change origin b a refs/tags/v1`, where `a`
already has a change and `b` is new, to print the line of `b` before the
line of `a`. Stock git does not send its commands in the order of its
command line: it sends the commands of the refs the hub advertised first,
in the advertised order, and those of new refs after them, so that push
sends `refs/heads/a` before `refs/heads/b`. The mapping takes the branches
in the order the commands arrive (`ChangeMapper.Map`), which is the order of
the status report as well, and the line of `a` comes first. The test reads
the order from git's packet trace and checks the lines against it.

## A branch that already has a change gets its version from the ref writer

The check of "the same options on every open of one push" expects a
record-version call through the mapping's change service for the branch
that already has an open change. The package's own decisions say
otherwise: the ordinary version of a push is recorded by the change
feature's ref-updated stage inside the ref update, and the mapping reads
it from the ref writer's result (`pushrule.UpdateResult.Version`); the
service's record operation is used only after a lost race to open. The
unit test gives that branch a version the ref writer reported, and checks
that the push opens changes for the other branches only, each with the
same options. A record-version request (`VersionRequest`) has no field for
a target, a title, an intent or a draft flag, so no version request can
carry them.
