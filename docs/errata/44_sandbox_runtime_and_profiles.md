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
