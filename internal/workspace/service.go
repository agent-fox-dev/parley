package workspace

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/scrub"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/store"
)

// Secrets is the secret component as the service uses it: it stores a
// workspace's origin credential in the create transaction and reads it back
// for every call to origin. *secret.Component satisfies it.
type Secrets interface {
	Available() bool
	Put(ctx context.Context, tx *sql.Tx, scope secret.Scope, scopeID, name, value string) error
	SecretReader
}

// Events is the event component as the service uses it: it writes an event
// in the caller's transaction. *event.Component satisfies it.
type Events interface {
	Emit(ctx context.Context, tx *sql.Tx, in event.Input) (event.Record, error)
}

// Credential is what git presents to origin: a user name and a password or
// token. The zero value is no credential. It is gitrepo's type, so that a
// resolved credential goes to git unchanged.
type Credential = gitrepo.Credential

// OriginLister lists the refs of an origin with a credential. An error means
// that origin refused the credential or could not be reached.
type OriginLister interface {
	ListOrigin(ctx context.Context, url string, cred Credential) error
}

// Deps are what the service is bound to. The hub builds them in its start
// hook; a test changes them through the hub's composition before they are
// bound.
type Deps struct {
	// Store holds the workspace records.
	Store *store.Store
	// Secrets stores and reads the workspaces' origin credentials.
	Secrets Secrets
	// Events writes the workspace events.
	Events Events
	// Lister lists an origin's refs with a credential.
	Lister OriginLister
	// Notify tells the clone workers that the workspace slug has clone work;
	// it is called after the transaction that recorded the work commits. Nil
	// tells no one.
	Notify func(slug string)
	// Scrubber cleans text that leaves the hub of secret values.
	Scrubber *scrub.Scrubber
	// Logger is the hub's logger; nil discards.
	Logger *slog.Logger
	// Now is the hub's clock; nil means time.Now.
	Now func() time.Time
	// ExternalURL is [server].external_url, the URL clients reach the hub
	// at; empty when it is not configured.
	ExternalURL string
	// Root is [workspace].path, the directory that holds the workspaces'
	// copies.
	Root string
}

// Service is the workspace feature of one hub: the workspace routes and the
// work behind them. The hub uses its zero value, which holds an empty
// settings registry, and binds it once at start with Bind, before it
// serves a request.
type Service struct {
	deps     Deps
	settings SettingsRegistry

	viewMu   sync.RWMutex
	forkView ForkView
}

// Bind binds the service to d. It is called once, before the hub serves.
func (s *Service) Bind(d Deps) {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	s.deps = d
}

// Settings is the registry of setting keys the service applies on create and
// shows in every workspace object. It holds no key until another part
// registers them on it.
func (s *Service) Settings() *SettingsRegistry { return &s.settings }

// Routes returns the workspace routes of the REST API.
func (s *Service) Routes() []server.Route {
	return []server.Route{s.createRoute()}
}
