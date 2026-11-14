# Errata: 10_workspace_create_and_clone

Where the implementation of package 10 (`.specs/10_workspace_create_and_clone`)
differs from what the package says, or decides what it leaves open.

## The personal organization is found by owner and name

The package says a create without `org_id` uses "the caller's personal
organization", but no record marks an org as personal. The org
`identity.CreateUserAndPersonalOrg` makes is owned by the user and named after
the username, so `identity.PersonalOrg(ctx, q, userID, username)` selects the
org with that owner and that name. When there is none, the create answers
`400 invalid_request` with `org_id: the caller has no personal organization`;
the caller can name an org with `org_id`. The caller must be an active member
of the chosen org (`identity.ActiveOrgIDs`), personal or named, or the answer is
`403 forbidden`.

## Credential members are not nullable

The request table gives `git_pat`, `git_username` and `git_password` as
non-empty strings, without "or null". A `null` credential member is therefore
refused (`400 invalid_request`, `<member>: must be a string`), not read as
absent, unlike the members the table marks "or null".

## Secret calls take a context first

Package 39's operations take a context first (see
`docs/errata/39_secrets.md`), so the credential is stored with
`Put(ctx, tx, secret.ScopeWorkspace, slug, name, value)` and read with
`Get(ctx, secret.ScopeWorkspace, slug, name)`.
`workspace.ResolveOriginCredential(ctx, secrets, slug)` takes the reader as a
parameter, so that a reader that fails can be given to it.

## Registrations are methods of the workspace service

`RegisterForkView` and `RegisterPostCloneStep` are methods of
`*workspace.Service`, not package functions, so that two hubs in one process
(as the tests run them) share no registration. `RegisterSetting` is a method
of the service's `SettingsRegistry` (`Service.Settings()`) and returns an
error for a key registered twice.

## The hub's git sees a fixed part of its environment

Besides `PATH`, the hub passes `GIT_SSL_CAINFO`, `SSL_CERT_FILE`,
`HTTPS_PROXY`, `HTTP_PROXY` and `NO_PROXY` from its own environment to git, so
that an operator with a private certificate authority or a proxy can reach
origin; the tests make the hub's git trust their origin the same way. The
minimal environment also sets `GIT_SSH_COMMAND=ssh -o BatchMode=yes`, so that
a `git@` origin fails instead of waiting on a prompt (see
`docs/configuration.md`).

## Clone workers are queued before the hub listens and run once it does

The start hook "workspaces" sweeps interrupted clones and queues the
outstanding clone work (`Workers.Start`) before the hub listens, as the
package asks. The goroutines are launched by the background work
"workspace_clones" (`Workers.Launch`), which runs only after the listener is
bound, so that a start that fails after the hook leaves no clone running on a
closed store.

## The test origin serves smart HTTP for an empty repository

The test origin `gitrepotest.NewOrigin` serves git's file-based HTTP transport
as the package says. Git cannot read a repository without refs over that
transport, so `gitrepotest.WithSmartHTTP()` serves `git http-backend` instead;
the empty-repository case of the clone tests uses it.

## The wait test polls through the CLI's test seam

The check of `parley workspace create --wait` writes `--poll-interval 10ms`
and `--timeout 50ms`. The CLI's durations are whole numbers of `s`, `m`, `h` or
`d`, so the test polls every 10 ms through the foundation's test option and
times out with `--timeout 1s`, as the CLI foundation's own wait tests do.

## Origin's credential is given to origin's host alone

Git follows a redirect of its first request to origin
(`http.followRedirects=initial`), and the package does not say which host may
receive the credential. The askpass program the hub gives git answers only a
prompt that names origin's own scheme, host and port, as the workspace's
`origin_url` gives them; for a prompt about any other host it answers nothing
and git fails. A redirect within origin (a renamed repository on the same
host) therefore keeps working with the credential, and a redirect to another
host, or to another port or scheme of the same host, does not receive it: a
private repository there fails as unauthorized. This matches the forge's rule
that a redirect is followed only to the same scheme, host and port.
