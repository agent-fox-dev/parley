package codeindex_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/codeindex"
	"github.com/agent-fox-dev/parley/internal/config"
)

func TestCheckPlatform_RefusesAPlatformTheIndexCannotRunOn(t *testing.T) {
	t.Parallel()
	on := config.CodeIndex{Enabled: true, MaxFiles: 10, MaxSize: 1 << 20, BuildTimeout: 1e9, Workers: 1}
	unsupported := func(string, string, string, codeindex.Bounds) (codeindex.Index, error) {
		return nil, codeindex.ErrUnsupported
	}
	t.Run("the index cannot run here", func(t *testing.T) {
		var c codeindex.Component
		c.Bind(codeindex.Deps{Settings: on, DataDir: t.TempDir(), Open: unsupported})
		err := c.CheckPlatform()
		if err == nil || !strings.Contains(err.Error(), "[code_index]") || !strings.Contains(err.Error(), runtime.GOOS+"/"+runtime.GOARCH) ||
			!strings.Contains(err.Error(), "not supported on this platform") {
			t.Errorf("CheckPlatform = %v, want one error that says the code index is not supported on %s/%s and names [code_index]",
				err, runtime.GOOS, runtime.GOARCH)
		}
	})
	t.Run("the real index on this platform", func(t *testing.T) {
		var c codeindex.Component
		c.Bind(codeindex.Deps{Settings: on, DataDir: t.TempDir()})
		if err := c.CheckPlatform(); err != nil {
			t.Errorf("CheckPlatform = %v, want nil", err)
		}
	})
	t.Run("a hub without the section is not checked", func(t *testing.T) {
		var c codeindex.Component
		c.Bind(codeindex.Deps{DataDir: t.TempDir(), Open: func(string, string, string, codeindex.Bounds) (codeindex.Index, error) {
			t.Error("an index was made on a hub without [code_index]")
			return nil, errors.New("not asked for")
		}})
		if err := c.CheckPlatform(); err != nil {
			t.Errorf("CheckPlatform = %v, want nil", err)
		}
	})
}

func TestFeatureOff_BuildsNothingAndUsesNoDisk(t *testing.T) {
	t.Parallel()
	w := newWorld(t, "")
	w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n"})
	w.commit(t, "demo", "main", map[string]string{"b.go": "package a\n"})
	w.pass(t)

	if _, err := os.Stat(filepath.Join(w.dataDir, codeindex.DirName)); !os.IsNotExist(err) {
		t.Errorf("the data directory holds %s (%v), want no directory for the code index", codeindex.DirName, err)
	}
	if jobs := w.jobs(t, "demo"); len(jobs) != 0 {
		t.Errorf("the queue holds the code_index jobs %q of demo, want none", jobs)
	}
	for outcome, n := range w.builds(t) {
		if n != 0 {
			t.Errorf("parley_code_index_builds_total{outcome=%q} = %v, want 0", outcome, n)
		}
	}
}
