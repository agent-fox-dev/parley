# Errata: 09_cli_foundation

Where the implementation of package 09 (`.specs/09_cli_foundation`) differs
from what the package says.

## The end-to-end test writes the configuration file with the save's encoder

The package's first execution path starts from a configuration file
"written by the save function of `internal/cli/config.go`", holding the
hub's URL with a trailing slash, a well-formed API key the hub does not
know, and a `user_id`. The save function, `saveConfig`, is unexported, and
`test/e2e` cannot call it. The one exported way to run it, `parley login`,
cannot leave that file: it drops the hub URL's trailing slashes before it
saves, and it saves only a credential the hub has accepted or issued.
Exporting the save, or adding a test-only route to it, would widen the
client's API for one test.

So `TestCLI_VersionFindsTheHubThroughTheConfigurationFile`
(`test/e2e/cli_test.go`) writes the file itself, with what `saveConfig`
writes for a missing file: the map of the three keys encoded by
`toml.Marshal` of `github.com/BurntSushi/toml`, the encoder `saveConfig`
uses, in a file of mode `0600` in a directory of mode `0700`. The bytes are
those the save would write. What the save does on its own (the temporary
file, the rename, the mode and the kept keys) is tested in
`internal/cli/config_test.go`.
