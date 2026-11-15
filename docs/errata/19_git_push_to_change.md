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
