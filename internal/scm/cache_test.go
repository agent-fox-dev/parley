package scm_test

import (
	"context"
	"net/http"
	"regexp"
	"slices"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/scm"
)

const (
	immutable  = "private, max-age=31536000, immutable"
	revalidate = "private, no-cache"
)

// quotedTag is the form of an entity tag: a non-empty quoted string.
var quotedTag = regexp.MustCompile(`^"[^"]+"$`)

func TestRead_MarksAnAnswerAddressedByFullIdsImmutable(t *testing.T) {
	w := newWorld(t)
	c, c1 := w.id["c2"], w.id["c1"]
	for _, path := range []string{
		"/commits?ref=" + c, "/commits/" + c, "/tree?ref=" + c, "/file?ref=" + c + "&path=f.txt",
		"/diff?base=" + c1 + "&head=" + c, "/compare?base=" + c1 + "&head=" + c,
		"/blame?ref=" + c + "&path=f.txt", "/search?ref=" + c + "&q=c",
	} {
		t.Run(path, func(t *testing.T) {
			r := w.get(t, scmPath("demo", path), w.ownerKey)
			if r.status != http.StatusOK || r.header.Get("Cache-Control") != immutable || !quotedTag.MatchString(r.header.Get("ETag")) {
				t.Errorf("%d, Cache-Control %q, ETag %q; want 200, %q and a quoted tag (body %s)",
					r.status, r.header.Get("Cache-Control"), r.header.Get("ETag"), immutable, r.body)
			}
		})
	}
}

func TestRead_AnswersAnyOtherReadWithATagToRevalidate(t *testing.T) {
	w := newWorld(t)
	c := w.id["c2"]
	paths := []string{"/diff?base=main&head=" + c, "/file?ref=" + c[:7] + "&path=f.txt", "/commits/" + c[:12]}
	for _, rt := range scmRoutes {
		paths = append(paths, rt.ok)
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			r := w.get(t, scmPath("demo", path), w.ownerKey)
			if r.status != http.StatusOK || r.header.Get("Cache-Control") != revalidate || !quotedTag.MatchString(r.header.Get("ETag")) {
				t.Errorf("%d, Cache-Control %q, ETag %q; want 200, %q and a quoted tag (body %s)",
					r.status, r.header.Get("Cache-Control"), r.header.Get("ETag"), revalidate, r.body)
			}
			if again := w.get(t, scmPath("demo", path), w.ownerKey); again.header.Get("ETag") != r.header.Get("ETag") {
				t.Errorf("the same request again: ETag %q, want %q", again.header.Get("ETag"), r.header.Get("ETag"))
			}
		})
	}

	file := scmPath("demo", "/file?ref=main&path=f.txt")
	raw := w.get(t, file, w.ownerKey)
	asJSON := w.get(t, file, w.ownerKey, "Accept", "application/json")
	if asJSON.header.Get("ETag") == raw.header.Get("ETag") || !slices.Contains(raw.header.Values("Vary"), "Accept") ||
		!slices.Contains(asJSON.header.Values("Vary"), "Accept") {
		t.Errorf("raw ETag %q Vary %q, JSON ETag %q Vary %q; want two tags and Vary: Accept on both",
			raw.header.Get("ETag"), raw.header.Values("Vary"), asJSON.header.Get("ETag"), asJSON.header.Values("Vary"))
	}
	head := w.send(t, http.MethodHead, file, w.ownerKey)
	if head.status != http.StatusOK || head.header.Get("ETag") != raw.header.Get("ETag") || head.header.Get("Cache-Control") != revalidate {
		t.Errorf("HEAD: %d, ETag %q, Cache-Control %q; want 200 with the GET's %q and %q",
			head.status, head.header.Get("ETag"), head.header.Get("Cache-Control"), raw.header.Get("ETag"), revalidate)
	}

	gitrepotest.Git(t, w.repo, "update-ref", "refs/heads/main", w.id["c1"])
	if moved := w.get(t, file, w.ownerKey); moved.header.Get("ETag") == raw.header.Get("ETag") {
		t.Errorf("after main moved: ETag %q, want another than %q", moved.header.Get("ETag"), raw.header.Get("ETag"))
	}
	refs := w.get(t, scmPath("demo", "/refs"), w.ownerKey).header.Get("ETag")
	gitrepotest.Git(t, w.repo, "update-ref", "refs/heads/x", w.id["c1"])
	if after := w.get(t, scmPath("demo", "/refs"), w.ownerKey).header.Get("ETag"); after == refs {
		t.Errorf("refs after the branch x was made: ETag %q, want another than %q", after, refs)
	}
}

func TestRead_AnswersNotModifiedToAnIfNoneMatchHoldingItsTag(t *testing.T) {
	w := newWorld(t)
	for _, tc := range []struct{ name, path string }{
		{"file by name", "/file?ref=main&path=f.txt"},
		{"file by full id", "/file?ref=" + w.id["c2"] + "&path=f.txt"},
		{"refs list", "/refs"},
		{"diff by names", "/diff?base=main&head=feature/x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := scmPath("demo", tc.path)
			first := w.get(t, path, w.ownerKey)
			tag := first.header.Get("ETag")
			for _, inm := range []string{tag, `"other", ` + tag, "W/" + tag} {
				r := w.get(t, path, w.ownerKey, "If-None-Match", inm)
				if r.status != http.StatusNotModified || len(r.body) != 0 || r.header.Get("ETag") != tag ||
					r.header.Get("Cache-Control") != first.header.Get("Cache-Control") {
					t.Errorf("If-None-Match %s: %d, %d bytes, ETag %q, Cache-Control %q; want 304, no body, %q and %q",
						inm, r.status, len(r.body), r.header.Get("ETag"), r.header.Get("Cache-Control"), tag, first.header.Get("Cache-Control"))
				}
			}
			if r := w.get(t, path, w.ownerKey, "If-None-Match", `"other"`); r.status != http.StatusOK || len(r.body) == 0 {
				t.Errorf(`If-None-Match "other": %d with %d bytes, want 200 with the answer`, r.status, len(r.body))
			}
		})
	}
	file := scmPath("demo", "/file?ref=main&path=f.txt")
	tag := w.get(t, file, w.ownerKey).header.Get("ETag")
	gitrepotest.Git(t, w.repo, "update-ref", "refs/heads/main", w.id["c1"])
	if r := w.get(t, file, w.ownerKey, "If-None-Match", tag); r.status != http.StatusOK {
		t.Errorf("after main moved, the old tag: %d, want 200", r.status)
	}
}

func TestRead_AnswersNotModifiedWithoutTheReadsCostlyPart(t *testing.T) {
	calls := &costlyCalls{}
	w := newWorld(t, func(d *scm.Deps) {
		open := d.Open
		d.Open = func(ctx context.Context, slug string) (scm.Repository, error) {
			repo, err := open(ctx, slug)
			return costlyRepo{Repository: repo, calls: calls}, err
		}
	})
	for _, rt := range scmRoutes {
		t.Run(rt.ok, func(t *testing.T) {
			path := scmPath("demo", rt.ok)
			tag := w.get(t, path, w.ownerKey).header.Get("ETag")
			calls.reset()
			if r := w.get(t, path, w.ownerKey, "If-None-Match", tag); r.status != http.StatusNotModified {
				t.Fatalf("If-None-Match %s: %d, want 304", tag, r.status)
			}
			if got := calls.reset(); len(got) != 0 {
				t.Errorf("the 304 ran %q, want no read past the revisions", got)
			}
		})
	}
}

func TestRead_LeavesAnErrorWithoutATagAndUncached(t *testing.T) {
	w := newWorld(t)
	for _, rt := range scmRoutes {
		t.Run(rt.bad, func(t *testing.T) {
			r := w.get(t, scmPath("demo", rt.bad), w.ownerKey)
			if r.status < http.StatusBadRequest || r.header.Get("ETag") != "" || r.header.Get("Cache-Control") != "no-store" ||
				r.header.Get("Vary") != "" {
				t.Errorf("%d, ETag %q, Cache-Control %q, Vary %q; want an error with no tag, no-store and no Vary",
					r.status, r.header.Get("ETag"), r.header.Get("Cache-Control"), r.header.Get("Vary"))
			}
		})
	}
	if r := w.get(t, scmPath("demo", "/file?path=f.txt"), w.ownerKey, "Range", "bytes=99-"); r.status != http.StatusRequestedRangeNotSatisfiable ||
		r.header.Get("ETag") != "" || r.header.Get("Cache-Control") != "no-store" {
		t.Errorf("a range past the end: %d, ETag %q, Cache-Control %q; want 416 with no tag and no-store",
			r.status, r.header.Get("ETag"), r.header.Get("Cache-Control"))
	}
}

// costlyCalls records the reads past the revisions a repository was asked
// for.
type costlyCalls struct {
	mu    sync.Mutex
	names []string
}

func (c *costlyCalls) add(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.names = append(c.names, name)
}

// reset forgets what was recorded and returns it.
func (c *costlyCalls) reset() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	names := c.names
	c.names = nil
	return names
}

// costlyRepo is a repository that records the reads a route makes once its
// revisions are resolved, the first of each route and the costly ones.
type costlyRepo struct {
	scm.Repository
	calls *costlyCalls
}

func (r costlyRepo) History(ctx context.Context, from string, q gitrepo.HistoryQuery) ([]gitrepo.Commit, error) {
	r.calls.add("History")
	return r.Repository.History(ctx, from, q)
}

func (r costlyRepo) ReadCommits(ctx context.Context, ids []string) ([]gitrepo.Commit, error) {
	r.calls.add("ReadCommits")
	return r.Repository.ReadCommits(ctx, ids)
}

func (r costlyRepo) DiffChanges(ctx context.Context, from, to string) ([]gitrepo.FileChange, error) {
	r.calls.add("DiffChanges")
	return r.Repository.DiffChanges(ctx, from, to)
}

func (r costlyRepo) TrialRebase(ctx context.Context, onto, head string) (*gitrepo.ConflictReport, error) {
	r.calls.add("TrialRebase")
	return r.Repository.TrialRebase(ctx, onto, head)
}

func (r costlyRepo) ReadTree(ctx context.Context, commit, path string, recursive bool) (gitrepo.Tree, error) {
	r.calls.add("ReadTree")
	return r.Repository.ReadTree(ctx, commit, path, recursive)
}

func (r costlyRepo) LookupFile(ctx context.Context, commit, path string) (gitrepo.TreeEntry, error) {
	r.calls.add("LookupFile")
	return r.Repository.LookupFile(ctx, commit, path)
}

func (r costlyRepo) Blame(ctx context.Context, commit, path string, first, last int) ([]gitrepo.BlameRange, error) {
	r.calls.add("Blame")
	return r.Repository.Blame(ctx, commit, path, first, last)
}

func (r costlyRepo) Grep(ctx context.Context, commit string, q gitrepo.GrepQuery, each func(gitrepo.GrepMatch) bool) error {
	r.calls.add("Grep")
	return r.Repository.Grep(ctx, commit, q, each)
}
