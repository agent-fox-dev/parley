package hub

import (
	"context"
	"log/slog"
	"path/filepath"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/secret"
)

// secretsHook is the name of the start hook that binds the secret component
// and writes its start report.
const secretsHook = "secrets"

// The configuration key the current secret key may be read from.
const (
	secretsSection = "secrets"
	keyFileName    = "key_file"
)

// composeSecrets registers [secrets].key_file, adds the secret table to the
// transactional store and loads the operator's keys in a configuration check,
// from the hub's environment and the key file, so that a key that cannot be
// loaded stops the start as its one error line before any store is opened
// (docs/adr/06-load-the-secret-keys-before-the-stores-open.md). Its start
// hook binds the secret component to the stores, the loaded keys, the hub's
// clock and scrubber and the event component, and reads the start report,
// which the hub writes once it has bound the listener: the `secret
// encryption` line, and the warning that secrets cannot be stored when no
// current key is configured. The secret routes are registered with the
// component's scope check as an access check: it is added after identity's
// and the workspace access checks, so a workspace secret route has its
// access decided before the scope is resolved. The component exists from the
// composition on, so a later part keeps it through Composition.Secrets; the
// hooks that run after this one may use it.
func composeSecrets(c *Composition) {
	c.secrets = &secret.Component{}
	c.RegisterKey(config.Key{Section: secretsSection, Name: keyFileName, Type: config.String, Default: ""})
	c.AddSchemaStep(secret.SchemaStep)
	var keys secret.Keys
	c.addConfigCheck(func(cfg *config.Config) (err error) {
		keys, err = secret.LoadKeys(c.env, keyFilePath(cfg))
		return err
	})
	var report secret.StartReport
	c.AddStartHook(secretsHook, func(ctx context.Context, rt *Runtime) (err error) {
		c.secrets.Bind(secret.Deps{Store: rt.Store, Keys: keys, Now: rt.Now, Scrubber: rt.Scrubber, Events: c.events})
		report, err = c.secrets.StartReport(ctx)
		return err
	})
	c.addStartReport(func(logger *slog.Logger) { report.Log(logger) })
	c.AddAccessCheck(c.secrets.ScopeCheck)
	for _, r := range c.secrets.Routes() {
		c.AddRoute(r)
	}
}

// keyFilePath is the path [secrets].key_file names, a relative one taken
// from the config dir as every path key of the configuration is; "" when the
// key is not set.
func keyFilePath(cfg *config.Config) string {
	p := cfg.String(secretsSection, keyFileName)
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(cfg.Dir, p)
}

// Secrets returns the hub's secret component, the one place hub code reads,
// stores and removes a secret. A part keeps it when it is composed; the
// component is bound once its start hook has run, so from the start hooks of
// the parts composed after it on.
func (c *Composition) Secrets() *secret.Component { return c.secrets }
