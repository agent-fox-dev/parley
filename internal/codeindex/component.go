// Package codeindex keeps, for every repository workspace of a hub whose
// configuration has [code_index], a tree, the files of the head commit of
// the workspace's default branch on disk, and a code index built from it,
// and brings both up to date in the background whenever that head moves.
//
// A hub without the section builds nothing and uses no disk for it.
package codeindex

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/agent-fox-dev/parley/internal/config"
)

// Section is the configuration section that turns the code index on.
const Section = "code_index"

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
}

// Component keeps the trees and indexes. Its zero value is ready to be
// composed; it works once Bind has given it its dependencies.
type Component struct {
	deps Deps
}

// Bind gives the component its dependencies, the defaults in place of the
// members d leaves nil.
func (c *Component) Bind(d Deps) {
	if d.Open == nil {
		d.Open = OpenSearchIndex
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
