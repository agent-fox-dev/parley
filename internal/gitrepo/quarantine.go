package gitrepo

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Quarantine is where the objects of a push wait until the push is
// accepted: a temporary object directory inside the repository's own, so
// that moving its files into the repository is a rename on one file
// system. git reads the repository's objects from it, and the repository
// never reads its objects until Migrate moves them in, so a push that is
// refused leaves nothing readable behind once Discard removed it.
type Quarantine struct {
	repo *Repo
	dir  string
	view *Repo
}

// NewQuarantine makes an empty quarantine in the repository.
func (r *Repo) NewQuarantine() (*Quarantine, error) {
	dir, err := os.MkdirTemp(r.objects(), "incoming-")
	if err != nil {
		return nil, fmt.Errorf("make a quarantine: %w", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "pack"), 0o755); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("make a quarantine: %w", err)
	}
	g := r.g
	g.Env = append(slices.Clone(g.Env), "GIT_ALTERNATE_OBJECT_DIRECTORIES="+dir)
	return &Quarantine{repo: r, dir: dir, view: &Repo{g: g, dir: r.dir}}, nil
}

// View is the repository as it would be with the quarantined objects: every
// method of it reads them beside the repository's own, and a ref
// transaction it prepares may point refs at them. Git's
// GIT_QUARANTINE_PATH is not set, since git refuses ref updates under it.
func (q *Quarantine) View() *Repo { return q.view }

// Receive stores the pack read from pack in the quarantine, with git
// index-pack, which completes a thin pack from the repository's objects.
// An error means git could not index what it read: no pack, a truncated
// one or a corrupt one, or ctx ended.
func (q *Quarantine) Receive(ctx context.Context, pack io.Reader) error {
	home, err := privateDir(false)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(home) }()
	g := q.repo.g
	g.Env = append(slices.Clone(g.Env), "GIT_OBJECT_DIRECTORY="+q.dir, "GIT_ALTERNATE_OBJECT_DIRECTORIES="+q.repo.objects())
	cmd := g.command(ctx, home, access{}, []string{"--git-dir=" + q.repo.dir, "index-pack", "--stdin", "--fix-thin"})
	stderr := &limitedBuffer{limit: stderrLimit}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pack, io.Discard, stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("git index-pack: %w", ctxErr)
		}
		return fmt.Errorf("git index-pack: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Migrate moves every object of the quarantine into the repository: the
// files of each pack, its index last, since the index is what makes git
// read a pack, and any loose object.
func (q *Quarantine) Migrate() error {
	entries, err := os.ReadDir(q.dir)
	if err != nil {
		return fmt.Errorf("migrate the quarantine: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(q.dir, e.Name()))
		if err != nil {
			return fmt.Errorf("migrate the quarantine: %w", err)
		}
		slices.SortStableFunc(files, func(a, b os.DirEntry) int {
			return indexLast(a.Name()) - indexLast(b.Name())
		})
		dst := filepath.Join(q.repo.objects(), e.Name())
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return fmt.Errorf("migrate the quarantine: %w", err)
		}
		for _, f := range files {
			if err := os.Rename(filepath.Join(q.dir, e.Name(), f.Name()), filepath.Join(dst, f.Name())); err != nil {
				return fmt.Errorf("migrate the quarantine: %w", err)
			}
		}
	}
	return nil
}

// indexLast orders a pack's index after its other files.
func indexLast(name string) int {
	if strings.HasSuffix(name, ".idx") {
		return 1
	}
	return 0
}

// Discard removes the quarantine with every object still in it.
func (q *Quarantine) Discard() error {
	if err := os.RemoveAll(q.dir); err != nil {
		return fmt.Errorf("discard the quarantine: %w", err)
	}
	return nil
}

// objects is the repository's object directory.
func (r *Repo) objects() string { return filepath.Join(r.dir, "objects") }

// Missing returns, in their order, the ids among ids that name no object of
// the repository.
func (r *Repo) Missing(ctx context.Context, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	out, err := r.g.runInput(ctx, access{}, []byte(strings.Join(ids, "\n")+"\n"),
		"--git-dir="+r.dir, "cat-file", "--batch-check=%(objectname)")
	if err != nil {
		return nil, err
	}
	var missing []string
	for line := range strings.Lines(string(out)) {
		if id, found := strings.CutSuffix(strings.TrimRight(line, "\n"), " missing"); found {
			missing = append(missing, id)
		}
	}
	return missing, nil
}
