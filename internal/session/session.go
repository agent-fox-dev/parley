// Package session keeps the hub's agent sessions: the one record that ties
// an agent's work on a workspace together (its forks, changes, sandboxes,
// pushes, commits, claims and lands) and says what it cost in tokens.
//
// An agent opens a session for a workspace it may write to, reports token
// usage while it works and closes the session with an outcome. The session
// object a route answers is built when the request is served, from the
// session's row, its usage records and the links other packages record.
//
// The package needs things of others, which it declares as narrow
// interfaces the hub fills: the access rule (Access), the minimal record of
// a workspace (WorkspaceReader) and the coverage of a bound token
// (Coverage).
package session

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
	"github.com/agent-fox-dev/parley/internal/store"
)

// Access is the access rule the routes decide a caller's access to a
// workspace with; *access.Service satisfies it.
type Access interface {
	Decide(ctx context.Context, c access.Caller, slug string, need access.Need) (access.Decision, error)
}

// Workspace is the minimal record of a workspace the package reads: its
// kind (access.KindRepository or access.KindFork), the slug of the
// repository a fork was created from ("" for a repository) and its status.
type Workspace struct {
	Slug, Kind, ParentSlug, Status string
}

// WorkspaceReader reads the minimal record of the workspace slug; found is
// false for a slug with no record.
type WorkspaceReader interface {
	Workspace(ctx context.Context, slug string) (w Workspace, found bool, err error)
}

// Coverage answers, for the caller c, whether its credential is bound to a
// workspace and, when it is, whether the binding covers the workspace slug.
// A failed read is an error, never an answer.
type Coverage interface {
	Covers(ctx context.Context, c access.Caller, slug string) (bound, covered bool, err error)
}

// Deps are what the component works with: the transactional store, the
// clock (nil for time.Now), the log (nil discards it), the access rule, the
// workspace records and the coverage of bound tokens. With no Coverage no
// caller counts as bound, because bound tokens do not exist without it.
type Deps struct {
	Store      *store.Store
	Now        func() time.Time
	Logger     *slog.Logger
	Access     Access
	Workspaces WorkspaceReader
	Coverage   Coverage
}

// Component is the session feature. The hub creates its zero value while
// it composes, registers its routes, and binds it in a start hook.
type Component struct {
	deps      Deps
	observers usageObservers
}

// Bind gives the component its dependencies, filling the defaults of the
// optional ones.
func (c *Component) Bind(d Deps) {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	c.deps = d
}

// Object is the session object every session route answers: the stored
// session, with its token summary, its forks, changes and sandboxes and
// its counts as they are when it is built. Every member is present in
// every answer; the lists are empty arrays, never null.
type Object struct {
	ID                       string          `json:"id"`
	WorkspaceSlug            string          `json:"workspace_slug"`
	RunID                    *string         `json:"run_id"`
	NodeID                   *string         `json:"node_id"`
	Archetype                *string         `json:"archetype"`
	Model                    *string         `json:"model"`
	Status                   string          `json:"status"`
	UserID                   *string         `json:"user_id"`
	CredentialType           string          `json:"credential_type"`
	StartedAt                timefmt.Time    `json:"started_at"`
	EndedAt                  *timefmt.Time   `json:"ended_at"`
	DurationMS               *int64          `json:"duration_ms"`
	ErrorMessage             *string         `json:"error_message"`
	CacheCreationInputTokens int64           `json:"cache_creation_input_tokens"`
	Metadata                 json.RawMessage `json:"metadata"`
	TokenSummary             TokenSummary    `json:"token_summary"`
	Forks                    []string        `json:"forks"`
	Changes                  []ChangeRef     `json:"changes"`
	Sandboxes                []SandboxRef    `json:"sandboxes"`
	Counts                   Counts          `json:"counts"`
}

// TokenSummary is the sum of a session's usage records, with the distinct
// model names they name, ascending.
type TokenSummary struct {
	TotalInputTokens     int64    `json:"total_input_tokens"`
	TotalOutputTokens    int64    `json:"total_output_tokens"`
	TotalCacheReadTokens int64    `json:"total_cache_read_tokens"`
	ModelsUsed           []string `json:"models_used"`
}

// ChangeRef is a change a session has a version of.
type ChangeRef struct {
	Workspace string `json:"workspace"`
	Number    int64  `json:"number"`
}

// SandboxRef is a sandbox of a session.
type SandboxRef struct {
	Workspace string `json:"workspace"`
	ID        string `json:"id"`
}

// Counts are the numbers of a session's links of each kind, and of the
// distinct changes its versions belong to.
type Counts struct {
	Forks     int64 `json:"forks"`
	Changes   int64 `json:"changes"`
	Sandboxes int64 `json:"sandboxes"`
	Versions  int64 `json:"versions"`
	Pushes    int64 `json:"pushes"`
	Commits   int64 `json:"commits"`
	Claims    int64 `json:"claims"`
	Lands     int64 `json:"lands"`
}

// set sets the count of the links of kind to n; changes are counted apart.
func (c *Counts) set(kind string, n int64) {
	switch kind {
	case "fork":
		c.Forks = n
	case "sandbox":
		c.Sandboxes = n
	case "version":
		c.Versions = n
	case "push":
		c.Pushes = n
	case "commit":
		c.Commits = n
	case "claim":
		c.Claims = n
	case "land":
		c.Lands = n
	}
}
