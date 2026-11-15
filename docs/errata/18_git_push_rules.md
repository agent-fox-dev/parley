# Errata: 18_git_push_rules

Where the implementation of package 18 (`.specs/18_git_push_rules`)
differs from what the package says, or decides what it leaves open.

## An engine without a settings reader uses the settings' own defaults

The package has the engine read the effective `protected_branches` and
`protect.owner_push` through a settings reader, and gives each collaborator
whose package may be absent a default, without naming one for the settings
reader. A `pushrule.Engine` bound without a `SettingsReader` uses the
defaults of the settings themselves (`internal/workspace/settings.go`):
`protected_branches` holds the workspace's default branch (none when it has
no default branch) and `protect.owner_push` is true. An engine without a
reader therefore still protects the default branch, rather than protecting
nothing and failing open. The hub always binds a reader.
