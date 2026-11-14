package workspace

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
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

// AccessRule is the access rule as the workspace list uses it: the set of
// workspaces a caller can read, with the caller's level on each, already
// cut down to the covered set for a PAT bound to a workspace.
// *access.Service satisfies it.
type AccessRule interface {
	Readable(ctx context.Context, c access.Caller) (access.ReadableSet, error)
}

// Credential is what git presents to origin: a user name and a password or
// token. The zero value is no credential. It is gitrepo's type, so that a
// resolved credential goes to git unchanged.
type Credential = gitrepo.Credential

// OriginLister lists the refs of an origin with a credential. An error means
// that origin refused the credential, could not be reached, or that ctx
// ended first; its text may hold what git wrote, so it is scrubbed before it
// is shown anywhere.
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
	// Access is the access rule the workspace list takes the caller's
	// readable set from.
	Access AccessRule
	// Lister lists an origin's refs with a credential.
	Lister OriginLister
	// Git clones a workspace's origin into its copy and opens the copy;
	// nil means gitrepo.Git{}.
	Git Git
	// Fetcher fetches a workspace's origin into its copy for a sync; nil
	// means gitrepo.Git{}.
	Fetcher Fetcher
	// Pusher pushes a branch of a workspace's copy to its origin; nil means
	// gitrepo.Git{}.
	Pusher Pusher
	// InsertWorkspace writes a new workspace record in the create
	// transaction; nil means Insert. A test makes it fail to see the whole
	// create roll back.
	InsertWorkspace func(ctx context.Context, tx *sql.Tx, w Workspace) error
	// BeforeRefSwap, when set, is called by a sync after it classified a
	// branch of the workspace slug and before the compare-and-swap that
	// moves it. It is for tests, which move the branch there as another
	// writer would; it is nil in production.
	BeforeRefSwap func(ctx context.Context, slug, branch string)
	// Notify tells the clone workers that the workspace slug has clone work;
	// it is called after the transaction that recorded the work commits. Nil
	// tells no one.
	Notify func(slug string)
	// Scrubber cleans text that leaves the hub of secret values; nil means
	// a scrubber that knows no value.
	Scrubber *scrub.Scrubber
	// Logger is the hub's logger; nil discards.
	Logger *slog.Logger
	// Now is the hub's clock; nil means time.Now.
	Now func() time.Time
	// ExternalURL is [server].external_url, the URL clients reach the hub
	// at; empty when it is not configured.
	ExternalURL string
	// Root is [workspace].path, the directory that holds the workspaces'
	// copies and the clones in progress.
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

	forkMu     sync.RWMutex
	forkFields map[string]ForkFieldHandler
	forkGuard  ForkUpdateGuard

	stepMu    sync.RWMutex
	postClone map[string]PostCloneStep

	moveMu        sync.RWMutex
	moveObservers []BranchMoveObserver

	excludeMu       sync.RWMutex
	excludeProvider ExcludedBranchProvider

	patchMu     sync.RWMutex
	patchLookup PatchBranchLookup

	extMu   sync.RWMutex
	syncExt SyncExtension

	// fetchLocks serialises the syncs' fetches into one copy.
	fetchLocks keyedLocks
}

// Bind binds the service to d. It is called once, before the hub serves.
func (s *Service) Bind(d Deps) {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.InsertWorkspace == nil {
		d.InsertWorkspace = Insert
	}
	if d.Git == nil {
		d.Git = gitrepo.Git{}
	}
	if d.Fetcher == nil {
		d.Fetcher = gitrepo.Git{}
	}
	if d.Pusher == nil {
		d.Pusher = gitrepo.Git{}
	}
	if d.Scrubber == nil {
		d.Scrubber = scrub.New()
	}
	s.deps = d
}

// Settings is the registry of setting keys the service applies on create and
// shows in every workspace object. It holds no key until another part
// registers them on it.
func (s *Service) Settings() *SettingsRegistry { return &s.settings }

// Routes returns the workspace routes of the REST API.
func (s *Service) Routes() []server.Route {
	return []server.Route{s.createRoute(), s.listRoute(), s.readRoute(), s.updateRoute(), s.syncRoute(), s.pushRoute()}
}
