# Errata: 01_hub_skeleton

Where the implementation of package 01 (`.specs/01_hub_skeleton`) differs
from what the package says.

## pflag is v1.0.9, not v1.0.10

The package's verified external API names `github.com/spf13/pflag` v1.0.10.
`github.com/spf13/cobra` v1.10.2 requires v1.0.9, and minimal version
selection picks that version, so `go.mod` lists pflag v1.0.9 as an indirect
requirement. `(*FlagSet).StringVar(p *string, name, value, usage string)`, the
one pflag call the client makes, has the same signature in v1.0.9.

## The stop waits for accepted connections

The package says a stop stops accepting connections and gives in-flight
requests the shutdown grace. `http.Server.Shutdown` alone does not do that for
a connection that is accepted but whose request has not been read yet: net/http
drops such a request once shutdown has begun, so a client that has already
sent its request gets no response. `(*server.Server).Serve` therefore takes
three steps. It closes the listener itself, then waits up to 100 ms for
connections that are waiting for their first request, and then calls
`Shutdown`. A connection counts as waiting from the moment `Accept` returns it.

## Three checks are set up differently

- The in-flight request of the graceful-stop check in
  `internal/server/server_test.go` waits on a channel the test closes, not a
  fixed 200 ms. The test can then check, without a timing race, that new
  connections are refused and that `Serve` has not returned while the request
  is in flight. The outcomes it asserts are the check's own.
- `run` in `cmd/parleyd` has no way to mount a test route. The check that run
  exits 0 after the grace has expired therefore holds a request in flight with
  a request header that never ends.
- The default shutdown grace (15 s) and the default timeout of
  `parley version` (5 s) are asserted as values inside the behaviour tests that
  shorten them; a test does not wait that long.
