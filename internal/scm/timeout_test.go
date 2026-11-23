package scm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/agentkit-go/outline"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/scm"
)

// stalledRepo is a workspace repository whose listing of refs, the first
// git command of every read below, blocks until its context ends; ended
// counts the listings that saw it end.
type stalledRepo struct {
	scm.Repository
	ended *atomic.Int32
}

func (s stalledRepo) ListRefs(ctx context.Context, _ ...string) ([]gitrepo.Ref, error) {
	<-ctx.Done()
	s.ended.Add(1)
	return nil, fmt.Errorf("git for-each-ref: %w", ctx.Err())
}

// lateRepo is a workspace repository that holds a tree it has read until
// its context has ended, and that writes the first half of a file and then
// waits for its context to end.
type lateRepo struct{ scm.Repository }

func (l lateRepo) ReadTree(ctx context.Context, commit, path string, read gitrepo.TreeRead) (gitrepo.Tree, error) {
	tree, err := l.Repository.ReadTree(ctx, commit, path, read)
	<-ctx.Done()
	return tree, err
}

func (l lateRepo) WriteBlob(ctx context.Context, id string, w io.Writer) error {
	var b bytes.Buffer
	if err := l.Repository.WriteBlob(ctx, id, &b); err != nil {
		return err
	}
	if _, err := w.Write(b.Bytes()[:b.Len()/2]); err != nil {
		return err
	}
	<-ctx.Done()
	return fmt.Errorf("git cat-file: %w", ctx.Err())
}

// fetch is get by the owner that gives up after 5s and returns, instead of
// failing on it, the error of the request or of reading its body.
func (w *world) fetch(path string) (reply, error) {
	req, err := http.NewRequest(http.MethodGet, w.URL("/api/v1"+path), nil)
	if err != nil {
		return reply{}, err
	}
	req.Header.Set("Authorization", "Bearer "+w.ownerKey)
	res, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return reply{}, err
	}
	defer res.Body.Close()
	r := reply{status: res.StatusCode, header: res.Header}
	r.body, err = io.ReadAll(res.Body)
	_ = json.Unmarshal(r.body, &r.json)
	return r, err
}

// The timeout tests measure how long an answer takes, so they do not run in
// parallel with the other tests of the package.
func TestRead_EndsAReadThatOutlastsTheReadTimeout(t *testing.T) {
	var ended atomic.Int32
	w := newWorld(t, func(d *scm.Deps) {
		d.ReadTimeout = 50 * time.Millisecond
		d.Open = func(context.Context, string) (scm.Repository, error) { return stalledRepo{ended: &ended}, nil }
	})
	for _, rt := range scmRoutes {
		if !slices.Contains([]string{"tree", "file", "diff", "compare", "blame", "search", "outline", "declaration"}, rt.op) {
			continue
		}
		t.Run(rt.op, func(t *testing.T) {
			before, start := ended.Load(), time.Now()
			r, err := w.fetch(scmPath("demo", rt.ok))
			if err != nil || r.status != http.StatusServiceUnavailable || r.errorType() != "scm_timeout" {
				t.Fatalf("%d %s (%v), want 503 scm_timeout", r.status, r.body, err)
			}
			if d := time.Since(start); d >= 2*time.Second {
				t.Errorf("answered after %v, want within 2s", d)
			}
			if ended.Load() == before {
				t.Error("the read's git command did not see its context end")
			}
		})
	}
}

// The configured read_timeout of 1s, which a test can only wait for if the
// default of 30s is not in force.
func TestRead_AnswersATimeoutReachedAfterTheGitWorkUnlessTheBodyHasBegun(t *testing.T) {
	w := newConfiguredWorld(t, "[scm]\nread_timeout = \"1s\"\n", func(d *scm.Deps) {
		open := d.Open
		d.Open = func(ctx context.Context, slug string) (scm.Repository, error) {
			repo, err := open(ctx, slug)
			if err != nil {
				return nil, err
			}
			return lateRepo{repo}, nil
		}
	})
	t.Run("deadline passed while the body is prepared", func(t *testing.T) {
		start := time.Now()
		r, err := w.fetch(scmPath("demo", "/tree"))
		if err != nil || r.status != http.StatusServiceUnavailable || r.errorType() != "scm_timeout" {
			t.Fatalf("%d %s (%v), want 503 scm_timeout", r.status, r.body, err)
		}
		if d := time.Since(start); d < time.Second || d >= 3*time.Second {
			t.Errorf("answered after %v, want after the read timeout of 1s and within 3s", d)
		}
	})
	t.Run("deadline passed once the body has begun", func(t *testing.T) {
		r, err := w.fetch(scmPath("demo", "/file?path=f.txt"))
		if err == nil {
			t.Errorf("%d %q, a whole answer; want the answer cut off", r.status, r.body)
		}
	})
}

// writtenBlobs is a workspace repository that counts the blobs it has
// written whole, the last git work of an outline before its parse.
type writtenBlobs struct {
	scm.Repository
	written *atomic.Int32
}

func (b writtenBlobs) WriteBlob(ctx context.Context, id string, w io.Writer) error {
	err := b.Repository.WriteBlob(ctx, id, w)
	if err == nil {
		b.written.Add(1)
	}
	return err
}

// A parse cannot always be ended: only part of it looks at its context. The
// read is answered at the read timeout all the same, and no more parses run
// at once than the limit, the ones left behind included.
//
// The read timeout also bounds the git work before the parse, which on a
// busy machine can use all of it, so each read below is asked again until
// one has got to the step it is about. The stalled parse does not end
// before free, so an answer to such a read shows that it was not held for
// the parse.
func TestOutline_AnswersATimeoutWhenTheParseOutlastsTheReadTimeout(t *testing.T) {
	var parses, written atomic.Int32
	release := make(chan struct{})
	w := newWorld(t, func(d *scm.Deps) {
		d.ReadTimeout = 500 * time.Millisecond
		d.MaxOutlines = 1
		open := d.Open
		d.Open = func(ctx context.Context, slug string) (scm.Repository, error) {
			repo, err := open(ctx, slug)
			if err != nil {
				return nil, err
			}
			return writtenBlobs{repo, &written}, nil
		}
		d.Outline = func(ctx context.Context, path string, src []byte) (outline.File, error) {
			parses.Add(1)
			<-release
			return outline.Outline(ctx, path, src, outline.Options{})
		}
	})
	free := sync.OnceFunc(func() { close(release) })
	t.Cleanup(free)
	x := commit(t, w.repo, w.id["c2"], "x.go", "package x\n\nfunc X() {}\n")
	path := scmPath("demo", "/outline?path=x.go&ref="+x)

	// timeOut asks for the outline until reached reports that a read got
	// where name says, and wants each answer to be 503 scm_timeout.
	timeOut := func(name string, reached func() bool) {
		t.Helper()
		for deadline := time.Now().Add(30 * time.Second); ; {
			r, err := w.fetch(path)
			if err != nil || r.status != http.StatusServiceUnavailable || r.errorType() != "scm_timeout" || r.header.Get("ETag") != "" {
				t.Fatalf("%s: %d %s (%v), ETag %q; want 503 scm_timeout without a tag", name, r.status, r.body, err, r.header.Get("ETag"))
			}
			if reached() {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: no read got there within 30s", name)
			}
		}
	}
	timeOut("the parse that stalls", func() bool { return parses.Load() > 0 })
	before := written.Load()
	timeOut("the next one, which waits for it", func() bool { return written.Load() > before })
	if n := parses.Load(); n != 1 {
		t.Errorf("%d parses ran while the first had not ended, want 1", n)
	}

	free()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := w.fetch(path)
		if err == nil && r.status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("once the stalled parse ended: %d %s (%v), want 200", r.status, r.body, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The declaration route parses as the outline route does, so a parse that
// outlasts the read timeout ends the read there too.
func TestDeclaration_AnswersATimeoutWhenTheParseOutlastsTheReadTimeout(t *testing.T) {
	release := make(chan struct{})
	w := newWorld(t, func(d *scm.Deps) {
		d.ReadTimeout = 300 * time.Millisecond
		d.Outline = func(ctx context.Context, path string, src []byte) (outline.File, error) {
			<-release
			return outline.Outline(ctx, path, src, outline.Options{})
		}
	})
	t.Cleanup(sync.OnceFunc(func() { close(release) }))
	start := time.Now()
	r, err := w.fetch(scmPath("demo", "/declaration?path=g.go&name=Hello"))
	if err != nil || r.status != http.StatusServiceUnavailable || r.errorType() != "scm_timeout" {
		t.Fatalf("%d %s (%v), want 503 scm_timeout", r.status, r.body, err)
	}
	if d := time.Since(start); d >= 3*time.Second {
		t.Errorf("answered after %v, want within 3s", d)
	}
}

// emptyBlobs is a workspace repository that writes nothing for a blob, as
// for an empty file.
type emptyBlobs struct{ scm.Repository }

func (emptyBlobs) WriteBlob(context.Context, string, io.Writer) error { return nil }
