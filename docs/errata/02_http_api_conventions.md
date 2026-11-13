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
