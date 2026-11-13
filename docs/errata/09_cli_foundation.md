# Errata: 09_cli_foundation

Where the implementation of package 09 (`.specs/09_cli_foundation`) differs
from what the package says.

## The first execution path is tested in internal/cli, not in test/e2e

The package's first execution path starts from a configuration file
"written by the save function of `internal/cli/config.go`", holding the
hub's URL with a trailing slash, a well-formed API key the hub does not
know, and a `user_id`, and the package places its test in
`test/e2e/cli_test.go`. The save function, `saveConfig`, is unexported, so
`test/e2e` cannot call it. The one exported way to run it, `parley login`,
cannot leave that file: it drops the hub URL's trailing slashes before it
saves, and it saves only a credential the hub has accepted or issued.
Exporting the save, or adding a test-only route to it, would widen the
client's API for one test, and writing the file by hand in `test/e2e` would
put a stand-in where the path names the real save.

So the path's test is `TestVersion_FindsTheHubThroughTheFileTheSaveWrote`
in `internal/cli/version_test.go`, where the save is reachable. It writes the
file with `saveConfig`, serves the hub's HTTP handler
(`server.New(…).Handler()` of `internal/server`) on a real loopback
listener, runs `parley version` through `Run`, and checks the path's end
state. Only the test code of `internal/cli` imports `internal/server`; the
client itself does not (`go list -deps ./cmd/parley`). The second execution
path, which needs the hub's authentication, stays in `test/e2e/cli_test.go`
against a whole hub.
