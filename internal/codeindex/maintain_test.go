package codeindex_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/agent-fox-dev/parley/internal/codeindex"
)

func TestMaintenance_QueuesWhatIsBehindAndRemovesWhatIsStray(t *testing.T) {
	t.Parallel()
	w := newWorld(t, enabled)
	first := w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n"})
	w.indexed(t, "demo")
	head := w.commit(t, "demo", "main", map[string]string{"b.go": "package a\n"})
	built := w.reaches(t, "demo", head)

	// The record falls behind the head with no notice of it, and the
	// directory gains what belongs to no workspace and to no build.
	if _, err := w.Store.Main().Exec(`UPDATE code_index_records SET indexed_sha = ? WHERE workspace_slug = 'demo'`, first); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(w.dataDir, codeindex.DirName)
	for _, stray := range []string{filepath.Join(root, "ghost", "0123", "tree"), filepath.Join(root, "demo", "leftover", "tree")} {
		if err := os.MkdirAll(stray, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	w.pass(t)
	rec := w.reaches(t, "demo", head)
	if !rec.BuiltAt.After(*built.BuiltAt) {
		t.Errorf("the index of demo was built at %s, want a build after the pass found it behind (%s)", rec.BuiltAt, built.BuiltAt)
	}
	w.pass(t)
	if got := w.entries(t, ""); !slices.Equal(got, []string{"demo"}) {
		t.Errorf("the code index directory holds %q, want demo alone", got)
	}
	if got := w.entries(t, "demo"); !slices.Equal(got, []string{head}) {
		t.Errorf("the directory of demo holds %q, want the build of its head, %s, alone", got, head)
	}
}
