# Errata: 02_http_api_conventions

Where the implementation of package 02 (`.specs/02_http_api_conventions`)
differs from what the package says.

## A rate limit of 0 is RateLimitDisabled

The package says a requests-per-minute limit of 0 applies no rate limit, and
that the limit is a server option with the default 600. A Go option struct
cannot tell an option left unset from one set to 0, so
`server.Options.RequestsPerMinute` follows the other zero-value options
(`MaxBodySize`): 0 means the default, 600. The limit is turned off with the
separate option `server.Options.RateLimitDisabled`.

Package 03, which reads `[limits].requests_per_minute` from the configuration
file, must map a configured `0` to `RateLimitDisabled: true`, not pass it as
`RequestsPerMinute`. The limiter itself, `server.NewLimiter(0, now)`, admits
every request, as the package says.

## A public create route ignores Idempotency-Key

The package says every create route accepts an `Idempotency-Key` and that a
record belongs to the caller: the user id or the admin token. A public route
has neither, since it skips authentication. A record without a caller would
replay one anonymous client's answer, secrets aside, to any other client
that guessed the key, so the idempotency stage
(`internal/server/idempotency.go`, `serveCreate`) runs the handler of a
public create route directly and records nothing, whatever the header holds.
No route of this package is both public and a create route.
