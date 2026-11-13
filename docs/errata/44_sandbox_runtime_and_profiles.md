# Errata: 44_sandbox_runtime_and_profiles

Where the implementation of package 44
(`.specs/44_sandbox_runtime_and_profiles`) differs from what the package says.

## The section is checked after the load, not by its section validator

The package has the `[sandbox]` section checked by the validator it registers
with `RegisterSection`, which then returns one error listing every failure.
That validator receives the section's table and nothing else, so it can see
neither `[server].external_url`, whose absence must be reported together
with the section's failures, nor the config dir, against which relative
paths are resolved.

`sandbox.Register(registry)` therefore registers the section with a validator
that accepts any content, and the whole check runs in
`sandbox.FromConfig(cfg *config.Config) (*sandbox.Settings, error)` in
`internal/sandbox/config.go`, over the loaded configuration. The hub calls it
right after the load and before any store is opened, so a bad section still
stops the start with exit status 1, one error line and nothing opened. The
error has the form of the loader's own, `configuration file <path>:
<failure>; <failure>; …`. Without the section, `FromConfig` returns settings
with `Enabled` false and checks nothing.

The failures come in a fixed order: the section's keys in the order of the
configuration reference (`runtime`, `gateway_url`, the TLS, token and OIDC
keys), then the count of the ways to authenticate, the remaining keys,
`[[sandbox.profiles]]`, `[[sandbox.credentials]]`, the profiles' references
to credentials that do not exist, the section's unknown keys in name order,
and last `server.external_url`. No failure quotes a value,
durations and sizes included, so that no value a `${NAME}` reference brought
in reaches the error line.

## Relative paths are taken from the config dir

The package types `tls_cert`, `tls_key`, `tls_ca`, `token_file`,
`oidc_client_secret_file` and a profile's `policy` as paths and does not say
what a relative path is relative to. They are resolved against the config
dir, the directory `config.toml` was looked for in, and `Settings` and
`Profile.PolicyPath` hold them as absolute paths. The configuration
reference says so.

## The metrics recorder is declared in the runtime package

The package declares the recorder the sandbox packages report to in
`internal/sandbox`. The runtime client reports `parley_sandbox_runtime_up`
to it, but `internal/sandbox/runtime` cannot import `internal/sandbox`,
which imports it. The recorder is therefore declared as
`runtime.Recorder` (`Add`, `Set` and `Observe`, each with a metric name, a
label map and a value, each returning an error), with `runtime.NopRecorder`
that records nothing, so that `internal/sandbox` can give out the same
interface under its own name.

## The runtime client's shape

The package lists the operations and the data they exchange, not their
signatures. Where it leaves a choice, the interface in
`internal/sandbox/runtime/runtime.go` makes these:

- A sandbox is named by its name within the workspace, which is how the
  OpenShell SDK addresses one; there is no separate sandbox id.
  `ApproveProposal` and `RejectProposal` take the sandbox's name with the
  proposal's id, because the runtime keeps proposals per sandbox.
- A rule is a name and a JSON object (`runtime.Rule`); the base document is
  bytes.
- The streams end with `io.EOF` when the caller's context ends, which is no
  failure, and with an error of kind unavailable when they break.
- An error keeps text only (`runtime.Error`, kind, operation and reason),
  not the error it was made from, so that what was scrubbed from the text
  cannot be reached through `errors.Unwrap`. An error of none of the four
  kinds counts as unavailable (`runtime.KindOf`).
- The reachability flag starts set, and the recorder hears of it at its
  first change. The hub serves only after its start-up check succeeded
  through the client, so a set flag is true at that point.

## The contract's error-kind rows that need a gateway

The package's check of the error kinds includes a sandbox image the backend
refuses, a gateway that answers unauthenticated and one that answers with a
server error. The in-memory fake has no images to refuse and no
authentication, so the contract (`runtimetest.RunContract`) leaves those
three rows out. Instead, it checks a policy value the backend rejects, in
`ValidatePolicy` and in `CreateSandbox`, with the rejection's reason kept
in the text, and that every operation of an unreachable backend fails as
unavailable. How the OpenShell implementation maps the gateway's answers
to the kinds is tested with that implementation.
