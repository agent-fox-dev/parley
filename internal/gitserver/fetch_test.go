package gitserver_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/gitserver"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
)

const (
	advertisement = "application/x-git-upload-pack-advertisement"
	result        = "application/x-git-upload-pack-result"
	uploadPack    = "/git/acme/demo.git/git-upload-pack"
)

func TestFetch_AdvertisesRefsUnderTheProtocolTheHeaderAsks(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t, hubtest.Config{})
	for _, c := range []struct {
		name, header string
		v2           bool
	}{
		{"no header", "", false},
		{"version=1", "version=1", false},
		{"foo=bar", "foo=bar", false},
		{"version=2", "version=2", true},
		{"version=2 among other entries", "foo=bar:version=2", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			header := http.Header{}
			if c.header != "" {
				header.Set("Git-Protocol", c.header)
			}
			a := w.send(t, http.MethodGet, demoRefs, basic("x", w.ownerKey), header, "")
			if a.status != http.StatusOK || a.header.Get("Content-Type") != advertisement || a.header.Get("Cache-Control") != "no-cache" {
				t.Fatalf("answer = %d Content-Type %q Cache-Control %q, want 200 %s no-cache", a.status,
					a.header.Get("Content-Type"), a.header.Get("Cache-Control"), advertisement)
			}
			pkts := pktlines(a.body)
			mentions := func(s string) bool {
				return slices.ContainsFunc(pkts, func(p string) bool { return strings.Contains(p, s) })
			}
			starts := func(s string) bool {
				return slices.ContainsFunc(pkts, func(p string) bool { return strings.HasPrefix(p, s) })
			}
			switch {
			case c.v2 && (len(pkts) == 0 || pkts[0] != "version 2\n" || !starts("ls-refs") || !starts("fetch") || mentions("refs/heads/")):
				t.Errorf("pkt-lines = %q, want version 2 with ls-refs and fetch and no ref", pkts)
			case !c.v2 && (len(pkts) < 3 || pkts[0] != "# service=git-upload-pack\n" || pkts[1] != flushPkt ||
				!strings.Contains(pkts[2], "\x00") || !mentions(w.head+" refs/heads/main")):
				t.Errorf("pkt-lines = %q, want the service line, a flush and the refs with the capabilities on the first", pkts)
			}
		})
	}
}

func TestFetch_ClonesWithStockGitWhateverThePathsOfAPAT(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t, hubtest.Config{})
	docsOnly := w.IssuePAT(t, identity.PATSpec{UserID: w.owner, Permissions: []string{"git:read"}, Paths: []string{"docs/**"}, CreatedAt: w.clock()})
	for _, c := range []struct {
		name, protocol, password string
	}{
		{"protocol 0", "0", w.ownerKey},
		{"protocol 2", "2", w.ownerKey},
		{"pat limited to docs/**", "2", docsOnly},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			out, err := clientGit(t, dir, []string{"GIT_TRACE_PACKET=1"}, "-c", "protocol.version="+c.protocol, "clone", "--progress", w.remote(c.password, "acme/demo.git"), "d")
			if err != nil {
				t.Fatalf("git clone: %v\n%s", err, out)
			}
			if !strings.Contains(out, "remote: Enumerating objects") {
				t.Errorf("git clone --progress showed no progress of the hub's git:\n%s", out)
			}
			if got, _ := clientGit(t, filepath.Join(dir, "d"), nil, "rev-parse", "HEAD"); strings.TrimSpace(got) != w.head {
				t.Errorf("HEAD of the clone = %s, want %s", got, w.head)
			}
			if _, err := os.Stat(filepath.Join(dir, "d", "README")); err != nil {
				t.Errorf("the clone lacks README: %v", err)
			}
			if v2 := strings.Contains(out, "< version 2"); v2 != (c.protocol == "2") {
				t.Errorf("served under version 2 = %v, want %v; trace:\n%s", v2, c.protocol == "2", out)
			}
		})
	}
}

func TestFetch_SendsTheHistoryAShallowFetchAsksFor(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t, hubtest.Config{})
	h := w.extendHistory(t)
	remote := w.remote(w.ownerKey, "acme/demo.git")
	// Each step runs git with args, the first in the directory the clone d
	// is made in, the others in d, after which d's HEAD has commits commits.
	type step struct {
		args    []string
		commits int
	}
	for _, c := range []struct {
		name  string
		steps []step
	}{
		{"depth, deepen, unshallow", []step{{[]string{"clone", "--depth", "1", remote, "d"}, 1}, {[]string{"fetch", "--deepen", "1"}, 2}, {[]string{"fetch", "--unshallow"}, 4}}},
		{"shallow-since", []step{{[]string{"clone", "--shallow-since=" + h.dates[1].Format(time.RFC3339), remote, "d"}, 3}}},
		{"shallow-exclude", []step{{[]string{"clone", "--shallow-exclude=v0", remote, "d"}, 3}}},
	} {
		for _, version := range []string{"0", "2"} {
			t.Run(c.name+"/protocol "+version, func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				d := filepath.Join(dir, "d")
				for i, s := range c.steps {
					in := d
					if i == 0 {
						in = dir
					}
					if out, err := clientGit(t, in, nil, append([]string{"-c", "protocol.version=" + version}, s.args...)...); err != nil {
						t.Fatalf("git %s: %v\n%s", s.args[0], err, out)
					}
					if got, _ := clientGit(t, d, nil, "rev-list", "--count", "HEAD"); strings.TrimSpace(got) != strconv.Itoa(s.commits) {
						t.Fatalf("after git %s %s the clone holds %s commits, want %d", s.args[0], s.args[1], strings.TrimSpace(got), s.commits)
					}
				}
				_, err := os.Stat(filepath.Join(d, ".git", "shallow"))
				if shallow, want := err == nil, c.steps[len(c.steps)-1].commits < len(h.main); shallow != want {
					t.Errorf("the clone is shallow = %v, want %v", shallow, want)
				}
			})
		}
	}
}

func TestFetch_SendsOnlyTheObjectsAClientLacks(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t, hubtest.Config{})
	dir := t.TempDir()
	versions := []string{"0", "2"}
	for _, v := range versions {
		if out, err := clientGit(t, dir, nil, "-c", "protocol.version="+v, "clone", w.remote(w.ownerKey, "acme/demo.git"), "d"+v); err != nil {
			t.Fatalf("git clone: %v\n%s", err, out)
		}
	}
	stream := "commit refs/heads/main\ncommitter P <p@example.invalid> now\ndata 6\nthird\nfrom refs/heads/main^0\nM 100644 inline README\ndata 2\n3\n"
	if _, err := gitrepotest.Run(w.repo, stream, "fast-import", "--quiet", "--date-format=now"); err != nil {
		t.Fatal(err)
	}
	head := gitrepotest.Refs(t, w.repo, "refs/heads/")["main"]
	for _, v := range versions {
		d := filepath.Join(dir, "d"+v)
		out, err := clientGit(t, d, nil, "-c", "protocol.version="+v, "fetch", "--progress", "origin")
		if err != nil {
			t.Fatalf("protocol %s: git fetch: %v\n%s", v, err, out)
		}
		if got, _ := clientGit(t, d, nil, "rev-parse", "origin/main"); strings.TrimSpace(got) != head {
			t.Errorf("protocol %s: origin/main = %s, want %s", v, got, head)
		}
		if !strings.Contains(out, "remote: Total 3 ") {
			t.Errorf("protocol %s: the fetch did not receive 3 objects (commit, tree, blob):\n%s", v, out)
		}
	}
}

func TestFetch_SendsThePackOfAPlainOrGzipRequest(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t, hubtest.Config{})
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	_, _ = io.WriteString(zw, fetchRequest(w.head))
	_ = zw.Close()
	for _, c := range []struct{ name, encoding, body string }{
		{"plain", "", fetchRequest(w.head)},
		{"gzip", "gzip", zipped.String()},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			header := http.Header{"Content-Type": {"application/x-git-upload-pack-request"}}
			if c.encoding != "" {
				header.Set("Content-Encoding", c.encoding)
			}
			a := w.send(t, http.MethodPost, uploadPack, basic("x", w.ownerKey), header, c.body)
			if a.status != http.StatusOK || a.header.Get("Content-Type") != result || a.header.Get("Cache-Control") != "no-cache" {
				t.Fatalf("answer = %d Content-Type %q Cache-Control %q, want 200 %s no-cache", a.status,
					a.header.Get("Content-Type"), a.header.Get("Cache-Control"), result)
			}
			if !strings.Contains(a.body, "PACK") {
				t.Errorf("body = %q, want a pack", a.body)
			}
		})
	}
}

// blockingRunner writes NAK and then holds the exchange until release is
// closed or its context ends.
type blockingRunner struct{ release chan struct{} }

func (b blockingRunner) UploadPack(ctx context.Context, req gitrepo.ServeRequest) error {
	if _, err := io.WriteString(req.Stdout, pkt("NAK\n")); err != nil {
		return err
	}
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return nil
}

func TestFetch_SendsGitsFirstBytesBeforeGitEnds(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	defer close(release)
	w := newGitWorld(t, hubtest.Config{Compose: func(c *hub.Composition) {
		c.AdjustGitServer(func(d *gitserver.Deps) { d.Runner = blockingRunner{release} })
	}})
	req, err := http.NewRequest(http.MethodPost, w.URL(uploadPack), strings.NewReader(fetchRequest(w.head)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", basic("x", w.ownerKey))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatalf("the answer did not start while git was running: %v", err)
	}
	defer res.Body.Close()
	got := make([]byte, len(pkt("NAK\n")))
	if _, err := io.ReadFull(res.Body, got); err != nil || string(got) != pkt("NAK\n") {
		t.Fatalf("read %q (%v) while git was running, want %q", got, err, pkt("NAK\n"))
	}
}

func TestFetch_ReportsABadRequestInBand(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t, hubtest.Config{})
	for _, c := range []struct {
		name, body, encoding, want string
		runsGit                    bool
	}{
		{"empty body", "", "", "ERR empty request", false},
		{"invalid gzip", "not gzip", "gzip", "ERR invalid gzip body", false},
		{"not an upload-pack request", "0009junk", "", "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			header := http.Header{}
			if c.encoding != "" {
				header.Set("Content-Encoding", c.encoding)
			}
			before := w.runs.n.Load()
			a := w.send(t, http.MethodPost, uploadPack, basic("x", w.ownerKey), header, c.body)
			if a.status != http.StatusOK || a.header.Get("Content-Type") != result {
				t.Fatalf("answer = %d Content-Type %q, want 200 %s", a.status, a.header.Get("Content-Type"), result)
			}
			pkts := pktlines(a.body)
			i := slices.IndexFunc(pkts, func(p string) bool { return strings.HasPrefix(p, "ERR ") })
			if i < 0 || (c.want != "" && strings.TrimSuffix(pkts[i], "\n") != c.want) || strings.Contains(a.body, "PACK") {
				t.Errorf("body = %q, want one ERR pkt-line %q and no pack", a.body, c.want)
			}
			if ran := w.runs.n.Load() > before; ran != c.runsGit {
				t.Errorf("git ran = %v, want %v", ran, c.runsGit)
			}
		})
	}
}

// Not parallel: it puts a fake git first on the PATH of the test process,
// which sleeps in place of upload-pack, recording its process id, and runs
// the real git for every other command.
func TestFetch_EndsARequestThatOutlivesTheTimeoutAndItsGit(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	w := newGitWorld(t, hubtest.Config{Compose: func(c *hub.Composition) {
		c.AdjustGitServer(func(d *gitserver.Deps) { d.Timeout = 200 * time.Millisecond })
	}})
	bin := t.TempDir()
	pidFile := filepath.Join(bin, "pid")
	script := "#!/bin/sh\nfor a; do [ \"$a\" = upload-pack ] && { echo $$ > '" + pidFile + "'; exec sleep 30; }; done\nexec '" + realGit + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	start := time.Now()
	a := w.send(t, http.MethodPost, uploadPack, basic("x", w.ownerKey), nil, fetchRequest(w.head))
	if d := time.Since(start); d > 2200*time.Millisecond {
		t.Errorf("the request ended after %v, want within 2.2s", d)
	}
	if pkts := pktlines(a.body); a.status != http.StatusOK || len(pkts) == 0 || pkts[len(pkts)-1] != "ERR request timed out" {
		t.Errorf("answer = %d %q, want 200 ending with the pkt-line ERR request timed out", a.status, a.body)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("upload-pack never ran: %v", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if err := syscall.Kill(pid, 0); err == nil {
		t.Errorf("the process %d started for the request is still running", pid)
	}
}

func TestFetch_TimeoutComesFromTheConfiguration(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, text string
		want       time.Duration
		invalid    bool
	}{
		{"not set", "", gitserver.DefaultTimeout, false},
		{"set", "[git]\ntimeout = \"90s\"\n", 90 * time.Second, false},
		{"zero", "[git]\ntimeout = \"0s\"\n", 0, true},
		{"not a duration", "[git]\ntimeout = \"soon\"\n", 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(c.text), 0o600); err != nil {
				t.Fatal(err)
			}
			r := config.NewRegistry()
			if err := gitserver.Register(r); err != nil {
				t.Fatal(err)
			}
			cfg, err := r.Load(config.MapEnv(nil), dir)
			switch {
			case c.invalid && (err == nil || !strings.Contains(err.Error(), "git.timeout")):
				t.Errorf("load = %v, want an error naming git.timeout", err)
			case !c.invalid && err != nil:
				t.Fatalf("load: %v", err)
			case !c.invalid && gitserver.TimeoutFrom(cfg) != c.want:
				t.Errorf("timeout = %v, want %v", gitserver.TimeoutFrom(cfg), c.want)
			}
		})
	}
}
