// Package codeindex keeps, for every repository workspace of a hub whose
// configuration has [code_index], a tree, the files of the head commit of
// the workspace's default branch on disk, and a code index built from it,
// and brings both up to date in the background whenever that head moves.
//
// A hub without the section builds nothing and uses no disk for it.
package codeindex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/jobs"
	"github.com/agent-fox-dev/parley/internal/metrics"
	"github.com/agent-fox-dev/parley/internal/store"
)

// Section is the configuration section that turns the code index on.
const Section = "code_index"

// SettingName is the workspace setting that turns one workspace's index
// off.
const SettingName = "code_index"

// DirName is the directory under the hub's data directory that holds every
// tree and index.
const DirName = "code-index"

// Deps are what the component needs. Settings and DataDir are required;
// every other member has a default.
type Deps struct {
	// Settings is the hub's [code_index]; its zero value is a hub without
	// the section.
	Settings config.CodeIndex
	// DataDir is the hub's data directory.
	DataDir string
	// Open makes an index; nil is OpenSearchIndex.
	Open Opener
	// Store holds the records, and Jobs runs the builds; both are required
	// on a hub with the section.
	Store *store.Store
	Jobs  *jobs.Queue
	// Workspace reads what the component needs of the workspace slug; found
	// is false when there is none.
	Workspace func(ctx context.Context, slug string) (ws Workspace, found bool, err error)
	// Workspaces lists every repository workspace of the hub.
	Workspaces func(ctx context.Context) ([]Workspace, error)
	// Repos opens the repository of the workspace slug.
	Repos func(ctx context.Context, slug string) (Repository, error)
	// Metrics is told of every build that ended; nil tells no one.
	Metrics metrics.Recorder
	// Scrub removes what the hub keeps secret from a failure's text and
	// bounds its length; nil keeps the text.
	Scrub func(text string) string
	// ReadTimeout is how long a search or a symbol lookup may take, the
	// read timeout of the source control reads; zero or less is 30 s.
	ReadTimeout time.Duration
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Component keeps the trees and indexes. Its zero value is ready to be
// composed; it works once Bind has given it its dependencies.
type Component struct {
	deps Deps

	mu sync.Mutex
	// attempts are what the attempts of the build jobs did, by job id, from
	// a run to its settlement.
	attempts map[string]*attempt
	// building is the commit of the last build of each workspace this
	// process began, by slug.
	building map[string]string
	// open are the indexes loaded for use, by slug.
	open map[string]*openIndex
	// pins counts the queries that load or read the index of a build, by
	// slug/commit, and removing holds the builds whose directory the sweep
	// is taking: a pinned build's directory stays, and one being removed
	// is not pinned.
	pins     map[string]int
	removing map[string]bool
	// loads are the loads of an index under way, by slug/commit.
	loads map[string]*load
	// changed is closed, and replaced, whenever the settlement of a build
	// has committed: what a query that waits for the index waits on.
	changed chan struct{}
}

// Bind gives the component its dependencies, the defaults in place of the
// members d leaves nil.
func (c *Component) Bind(d Deps) {
	if d.Open == nil {
		d.Open = OpenSearchIndex
	}
	if d.Metrics == nil {
		d.Metrics = metrics.Discard
	}
	if d.Scrub == nil {
		d.Scrub = func(text string) string { return text }
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.ReadTimeout <= 0 {
		d.ReadTimeout = 30 * time.Second
	}
	c.deps = d
}

// Enabled reports whether the hub keeps code indexes at all.
func (c *Component) Enabled() bool { return c.deps.Settings.Enabled }

// CheckPlatform is the start check of a hub with [code_index]: an index
// that cannot be made on this platform is an error that says so, since such
// a hub would queue builds that can never succeed. A hub without the
// section makes no index and is not checked.
func (c *Component) CheckPlatform() error {
	if !c.Enabled() {
		return nil
	}
	probe, err := os.MkdirTemp("", "parley-code-index-probe-")
	if err != nil {
		return fmt.Errorf("[%s]: check that the code index runs here: %w", Section, err)
	}
	defer func() { _ = os.RemoveAll(probe) }()
	idx, err := c.deps.Open(filepath.Join(probe, "tree"), filepath.Join(probe, "index"), filepath.Join(probe, "run"), c.bounds())
	if errors.Is(err, ErrUnsupported) {
		return fmt.Errorf("the code index is not supported on this platform (%s/%s): remove [%s] from the configuration",
			runtime.GOOS, runtime.GOARCH, Section)
	}
	if err != nil {
		return fmt.Errorf("[%s]: check that the code index runs here: %w", Section, err)
	}
	return idx.Close()
}

// bounds are the bounds of a build, from the section.
func (c *Component) bounds() Bounds {
	s := c.deps.Settings
	return Bounds{MaxFiles: s.MaxFiles, MaxBytes: s.MaxSize, MaxBuildTime: s.BuildTimeout}
}
