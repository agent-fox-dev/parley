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

## The OpenShell SDK as it is in go.mod

The package records the SDK's surface as unverified. Read in
`github.com/ai-openshell/sdk-go` v0.1.8, it differs as follows, and the
implementation in `internal/sandbox/runtime/openshell` follows the SDK:

- The configuration is `v1.Config{Address, TLS, Auth, …}`, where `TLS` is a
  `*v1.TLSConfig` of file paths (`CertFile`, `KeyFile`, `CAFile`), not a
  `*tls.Config`, and `Auth` a `v1.AuthProvider`. `v1.RefreshableToken`
  takes an `oauth2.TokenSource`; the OIDC client credentials grant is
  `oidc.NewClientCredentialsAuth(oidc.WithIssuer, oidc.WithClientID,
  oidc.WithClientSecret)`, which obtains its token on the first call and
  renews it. An `http://` address is how the SDK is told to connect without
  TLS, and the SDK then refuses a token source; the section allows `http`
  only for a loopback gateway, so the client sends its token there without
  TLS.
- Every call takes the workspace as an argument; the client passes
  `openshell_workspace` to each.
- Sandbox stop and start exist (`Sandboxes().Stop`, `Start`), so
  `StopSandbox` and `StartSandbox` are implemented. A named network rule
  can be added to a sandbox's policy with a merge operation of
  `Config().Update`, so `AddPolicyRule` is implemented too.
- There is no call for `ValidatePolicy` (no validation without a sandbox),
  `GetPolicy` (the SDK holds a typed policy and cannot return the base
  document a sandbox was created with), `WatchSandboxes` (the SDK watches
  one named sandbox, not a workspace), `RunMain`, `MakeDir`, `Stat`, `Walk`
  and `Logs` (the SDK's logs are the gateway's and the supervisor's log
  lines, which carry neither the main process's output by stream nor the
  policy log's decisions, and cannot be followed), and none for `Exec` and
  `ExecStream` with input on stdin. They answer `unsupported` without
  calling the SDK.
- The SDK's in-memory client (`fake.NewClient`) answers provider profiles,
  exec, file transfer, logs, sandbox configuration and policy drafts as not
  implemented, so over it those operations of the client are `unsupported`
  too. Its default gateway release is `fake`, which is not a semantic
  version; `fake.WithGatewayInfo` sets another.

## What the SDK cannot carry of a request is not sent

Where the SDK has a call for an operation but no field for a part of its
request, the part is not sent and the gateway's default applies:

- `CreateSandbox` sends no working directory (the SDK has none at creation)
  and no base document (the SDK takes typed policy rules, not a document).
  The protected and added rules are sent as named network rules, decoded
  from their JSON bodies; a body with a member the SDK's network rule has no
  field for is `unsupported`, and an added rule named like an earlier rule,
  which would replace it, is `invalid`. A profile that names a `policy` file
  cannot start against this implementation (`ValidatePolicy` is
  unsupported), so the base a sandbox is created with is the built-in
  policy.
- `StartSandbox` sends neither the policy value nor the environment: the
  SDK's start takes neither, and the sandbox keeps those the gateway holds
  for it.
- `InstallProviderProfile` installs one credential named after the profile,
  whose placeholder goes into each of the profile's variables, and
  `EnsureProvider` stores a provider's value under the one credential of its
  profile (a profile with another count is `invalid`). The SDK's profile
  endpoint is a host, a port and a protocol, and has no path: the endpoint's
  scheme goes into the protocol, and its paths are not sent.
  `GetProviderProfile` therefore reports the endpoint without paths. For the
  hub credential's profile (`parley-hub`, meant for `/api/v1/` and `/git/`)
  this authorizes the credential for every path of the hub's own host. The
  paths outside those two are the same hub's, so the credential still
  reaches no other party, and narrowing it per workspace belongs to the
  sandbox's policy (design decision 15).
- The SDK reports nothing of a sandbox's working directory, which counts as
  present while the sandbox exists.

## The OpenShell implementation's checks

The contract (`runtimetest.RunContract`) runs against the implementation
over the SDK's in-memory client; the assertions about what that client does
not implement (provider profiles, exec, file transfer, policy drafts) are
skipped. The contract's check that an installed provider profile is returned
by get, paths included, cannot hold for this implementation against a
gateway that keeps profiles, because the paths are not sent; the provider
operations are instead tested against `openshelltest.Gateway`, which keeps
profiles, with the contract's other assertions about them (create or
replace, list with labels and without values, attach, detach, an unknown
provider not found, a deleted provider not found) and the profile reported
without its paths.
The error-kind rows that need a gateway (an image the gateway
refuses, an unauthenticated answer, a server error, a refused connection)
are checked with SDK errors given to the client and, for the refused
connection, a real SDK client to a closed loopback port. The validation of
a rejected policy is checked through `CreateSandbox`, and the missing path
of `Stat` as an operation the SDK lacks. A stream's freedom from the
30-second bound is checked with `ExecStream`, because `Logs` is unsupported.

## How the hub runs the section's check and the start-up check

The package has the section checked by its section validator and the hub
compose a runtime factory and a start hook. Because the check runs over the
loaded configuration (see the first entry), the hub gained an unexported
configuration-check stage, `(*Composition).addConfigCheck`, which `Run`
calls right after the load and before any store is opened; `composeSandbox`
(`internal/hub/sandbox.go`) adds `(*sandbox.Service).Configure` to it, so an
invalid section still ends the start with one line and nothing opened.

The parts composed after the sandbox part, and tests, reach the feature
through `(*hub.Composition).Sandbox() *sandbox.Service`: its `Settings`,
`ExternalURL`, `Client` (nil until the check succeeded) and `Version`.
`(*hub.Composition).SetSandboxRuntime(sandbox.RuntimeFactory)` replaces the
factory, a `func(*sandbox.Settings, sandbox.RuntimeDeps) (runtime.Client,
error)` given the hub's scrubber and recorder, whose default is
`openshell.New`; `SetSandboxRecorder(sandbox.Recorder)` gives the recorder
`parleyd` passes once the metrics registry exists. The start hook registers
the gateway token and the OIDC client secret with the hub's scrubber
whatever factory is used.

`GET /version` was extended without the file `internal/server/version.go`
the package names, which did not exist: the handler moved there, and the
server gained `Options.VersionMembers func() map[string]any`, whose members
are written after the build's and cannot replace them. The hub fills it from
the members its parts add with the unexported `addVersionMember`.

## Choices of the start-up check the package leaves open

- A gateway that reports itself not healthy is refused like one that cannot
  be reached, naming `sandbox.gateway_url`.
- A result of kind unavailable at any step names `sandbox.gateway_url`, not
  only at the first call. A refusal of another kind names the key the step
  depends on: `sandbox.openshell_workspace` for the workspace,
  `sandbox.gateway_url` with `server.external_url` for `parley-hub`.
- The endpoint of `parley-hub` takes the port 443 for `https` and 80 for
  `http` when `[server].external_url` gives none.
- `connected` in `GET /version` is the release as the gateway reported it,
  a leading `v` included; the range comparison drops that `v`. A
  pre-release is below its release and build metadata is ignored, as
  semantic versions are compared.
- The deadline of the check is that of a context, which follows the real
  clock, so the hub counts the 30 seconds on the real clock whatever clock
  `hub.Options.Now` gives. The hub's own tests do not wait those 30 seconds:
  the timeout is checked on `RunStartCheck` with a short budget, and the
  hub's exit on a failed check with a gateway that refuses the hub.
- The hub's test of `sandbox_runtime` uses the supported range of
  `compat.go` and a release inside it, not an injected range: the hub takes
  no range of its own. The range is injected (`CheckOptions.Releases`) in
  the tests of the check.

## How the sandbox routes are declared

The package places the feature check at the router's feature-check
position, and the needs-a-user rule and the runtime-error answer after
workspace access and before the handler validates the body; it does not say
how. The feature check is `server.Route.Feature`, which the pipeline runs
after the body checks and before the binding guard. The other two wrap the
route's handler, which runs after every access check, so no new pipeline
position was added. The declarations are methods of `*sandbox.Service`:
`Route` for the routes of the whole hub (the feature check and the
runtime-error answer), `WorkspaceRoute(kind, route, opts...)` for workspace
routes (also the scope `sandboxes:read` or `sandboxes:write` and the
workspace need `read` or `write`, replacing what the route gives) and the
option `NeedsUser`. Since routes are declared before the configuration is
loaded, the feature check reads whether sandboxes are on per request.

- The disabled answer's message is `sandboxes are not enabled on this hub`.
- The runtime-error wrapper answers only an error of kind `ErrUnavailable`
  or `ErrUnsupported`, tested with `errors.Is`, that is not already a
  `*server.Error`: an error of no runtime kind stays `500 internal`, though
  `runtime.KindOf` would take it for unavailable, so that a fault of the hub
  is not reported as the runtime's. Its message is the runtime error's text
  passed through the hub's scrubber, which the start hook gives the service
  with `SetScrubber` whether or not sandboxes are on.
- There is no binding guard yet; the declarations need none of their own,
  since it is a pipeline position.
