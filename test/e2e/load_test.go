package e2e

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/store"
)

// loadBatch is the number of rows of each transaction of the load: a store
// that holds a transaction only in part shows as a row count that is not a
// multiple of it.
const loadBatch = 10

// loadSteps create the tables the load writes: load in the transactional
// store and load_a in the analytical one.
var loadSteps = []store.Step{
	{Name: "test_load", Target: store.Main, Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "CREATE TABLE load (writer INTEGER NOT NULL, txn INTEGER NOT NULL, n INTEGER NOT NULL)")
		return err
	}},
	{Name: "test_load_a", Target: store.Analytics, Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "CREATE TABLE load_a (txn INTEGER NOT NULL, n INTEGER NOT NULL)")
		return err
	}},
}

// load is the work a running hub's data is under while it is backed up:
// four writers committing transactions of loadBatch rows to the table load
// of the transactional store, one writer committing them to load_a of the
// analytical store, and one git client per repository pushing commit after
// commit to its main branch. It keeps every error it sees and, per
// repository, the commits pushed and when each push ended.
type load struct {
	stop    chan struct{}
	done    sync.WaitGroup
	started sync.WaitGroup

	mu     sync.Mutex
	errs   []error
	pushes map[string][]push
}

// push is a commit a git client of the load pushed, and when its push
// ended.
type push struct {
	commit string
	at     time.Time
}

// startLoad starts the load on st and on the bare repositories repos, each
// holding a branch main, and returns once every writer has committed and
// every client has pushed at least once, or has failed.
func startLoad(t *testing.T, st *store.Store, repos []string) *load {
	t.Helper()
	l := &load{stop: make(chan struct{}), pushes: map[string][]push{}}
	for w := range 4 {
		l.run(func(txn int) error {
			return st.Tx(context.Background(), func(tx *sql.Tx) error {
				for n := range loadBatch {
					if _, err := tx.Exec("INSERT INTO load (writer, txn, n) VALUES (?, ?, ?)", w, txn, n); err != nil {
						return err
					}
				}
				return nil
			})
		})
	}
	l.run(func(txn int) error {
		tx, err := st.Analytics().Begin()
		if err != nil {
			return err
		}
		for n := range loadBatch {
			if _, err := tx.Exec("INSERT INTO load_a (txn, n) VALUES (?, ?)", txn, n); err != nil {
				tx.Rollback()
				return err
			}
		}
		return tx.Commit()
	})
	for _, repo := range repos {
		// The client's own repository borrows the objects of the one it
		// pushes to, so that the clone copies none of them.
		work := t.TempDir()
		git(t, work, "clone", "--quiet", "--bare", "--shared", repo, ".")
		tree, parent, _ := strings.Cut(git(t, work, "rev-parse", "main^{tree}", "main"), "\n")
		l.run(func(i int) error {
			// commit-tree prints the commit it writes, which saves the
			// client a process per push.
			commit, err := runGit(work, "commit-tree", tree, "-p", parent, "-m", fmt.Sprintf("load %d", i))
			if err != nil {
				return err
			}
			if _, err := runGit(work, "push", "--quiet", repo, commit+":refs/heads/main"); err != nil {
				return err
			}
			l.mu.Lock()
			l.pushes[repo] = append(l.pushes[repo], push{commit, time.Now()})
			l.mu.Unlock()
			parent = commit
			return nil
		})
	}
	l.started.Wait()
	return l
}

// run calls work with 0, 1, 2, ... in a goroutine until the load ends or
// work fails, and records the failure.
func (l *load) run(work func(i int) error) {
	l.done.Add(1)
	l.started.Add(1)
	go func() {
		defer l.done.Done()
		var once sync.Once
		defer once.Do(l.started.Done)
		for i := 0; ; i++ {
			select {
			case <-l.stop:
				return
			default:
			}
			if err := work(i); err != nil {
				l.mu.Lock()
				l.errs = append(l.errs, err)
				l.mu.Unlock()
				return
			}
			once.Do(l.started.Done)
		}
	}()
}

// end stops the load, waits for the work in progress to finish and returns
// every error the load saw.
func (l *load) end() []error {
	close(l.stop)
	l.done.Wait()
	return l.errs
}

// lastPushed returns the last commit pushed to repo whose push ended before
// moment, and the last commit pushed to it at all. The load's commits of a
// repository form one line, so every commit pushed before moment is the
// first one or an ancestor of it.
func (l *load) lastPushed(repo string, moment time.Time) (before, last string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	pushes := l.pushes[repo]
	for _, p := range pushes {
		if p.at.Before(moment) {
			before = p.commit
		}
	}
	if len(pushes) > 0 {
		last = pushes[len(pushes)-1].commit
	}
	return before, last
}

// loadRepos creates the bare repositories of the load under a hub's
// workspace root. Each holds two commits on main and, beside them,
// seedObjects loose objects, as a repository that has taken many pushes
// does: copying them keeps the backup inside a repository for a while, long
// enough for the clients' pushes to land during that copy.
func loadRepos(t *testing.T, workspaceRoot string) []string {
	t.Helper()
	seed := t.TempDir()
	paths := make([]string, seedObjects)
	for i := range paths {
		paths[i] = filepath.Join(seed, strconv.Itoa(i))
		if err := os.WriteFile(paths[i], []byte("seed "+strconv.Itoa(i)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	first, second := filepath.Join(workspaceRoot, "r0.git"), filepath.Join(workspaceRoot, "r1.git")
	bareRepo(t, first)
	git(t, first, append([]string{"hash-object", "-w"}, paths...)...)
	if err := os.CopyFS(second, os.DirFS(first)); err != nil {
		t.Fatalf("copy %s to %s: %v", first, second, err)
	}
	return []string{first, second}
}

// seedObjects is the number of loose objects loadRepos adds to each
// repository.
const seedObjects = 300
