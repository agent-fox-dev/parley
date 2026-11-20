package harness

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// Origin is a git origin outside the hub, served over TLS: a bare
// repository a scenario writes commits to directly.
type Origin struct {
	*gitrepotest.Origin
	t testing.TB
	// Main is the head of main after the seeding.
	Main string
}

// WithRepoPath serves an origin at /<path>, such as acme/widgets.git, for a
// forge that reads the owner and the repository from the URL.
var WithRepoPath = gitrepotest.WithRepoPath

// Origin returns an origin seeded with main (three commits), dev and the
// tag v1, which git reads but cannot push to.
func (h *Hub) Origin(opts ...gitrepotest.Option) *Origin {
	h.t.Helper()
	return seed(h.t, gitrepotest.NewOrigin(h.t, opts...))
}

// PushableOrigin is Origin served by git's smart transport, which takes
// pushes, such as those of a sync or an archive.
func (h *Hub) PushableOrigin(opts ...gitrepotest.Option) *Origin {
	h.t.Helper()
	return seed(h.t, gitrepotest.SmartOrigin(h.t, opts...))
}

func seed(tb testing.TB, o *gitrepotest.Origin) *Origin {
	main, _, _ := o.History()
	return &Origin{Origin: o, t: tb, Main: main}
}

// CommitFiles adds on top of branch a commit with msg whose tree is the
// branch's with the files, at the top level, and returns its id.
func (o *Origin) CommitFiles(branch, msg string, files map[string]string) string {
	o.t.Helper()
	var parents []string
	if head := o.Head(branch); head != "" {
		parents = append(parents, head)
	}
	id := gitrepotest.Commit(o.t, o.Dir(), msg, files, parents...)
	gitrepotest.SetRef(o.t, o.Dir(), "refs/heads/"+branch, id)
	o.Publish()
	return id
}

// Downstream returns an origin whose main is up's main, the origin of a
// carry-patch workspace whose upstream is up.
func (h *Hub) Downstream(up *Origin) *Origin {
	h.t.Helper()
	o := gitrepotest.NewOrigin(h.t)
	gitrepotest.Git(h.t, up.Dir(), "push", "--quiet", o.Dir(), "refs/heads/main:refs/heads/main")
	o.Publish()
	return &Origin{Origin: o, t: h.t, Main: up.Main}
}

// PatchBranch creates the branch name on main with one commit, Patch
// <name>, adding the files at the top level, and returns its id: a patch
// branch of a carry-patch workspace, which must be on the origin before
// the workspace is registered.
func (o *Origin) PatchBranch(name string, files map[string]string) string {
	o.t.Helper()
	o.Branch(name, o.Main)
	return o.CommitFiles(name, "Patch "+name, files)
}

// Workspace is a workspace registered on a hub.
type Workspace struct {
	Slug string
	// Remote is the workspace's git URL on the hub.
	Remote string
	Owner  *User
	Origin *Origin
	// Body is the workspace object once it was ready.
	Body map[string]any
}

// Workspace registers slug, owned by owner, from a new seeded origin, and
// waits until it is ready.
func (h *Hub) Workspace(owner *User, slug string) *Workspace {
	h.t.Helper()
	return h.Register(owner, slug, h.Origin(), nil)
}

// SizedOrigin returns an origin seeded as Origin is, whose main then gets
// one more commit adding random, so incompressible, files until the
// origin's packs hold at least size bytes. The files are streamed into one
// git fast-import, so that the test process never holds them in memory.
func (h *Hub) SizedOrigin(size int64) *Origin {
	h.t.Helper()
	o := h.Origin()
	// fast-import writes an import of fewer objects than its unpack limit
	// as loose objects; the origin's history is to be one pack.
	cmd := exec.Command("git", "--git-dir="+o.Dir(), "-c", "fastimport.unpackLimit=1", "fast-import", "--quiet")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + h.gitHome, "GIT_CONFIG_NOSYSTEM=1"}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		h.t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		h.t.Fatalf("start git fast-import: %v", err)
	}
	werr := writeSizedCommit(stdin, o.Main, size)
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil || werr != nil {
		h.t.Fatalf("git fast-import: %v %v\n%s", err, werr, stderr.String())
	}
	o.Publish()
	o.Main = o.Head("main")
	if packed := packBytes(h.t, o.Dir()); packed < size {
		h.t.Fatalf("the origin's packs hold %d bytes, want at least %d", packed, size)
	}
	return o
}

// sizedBlob is the size of each file of a sized origin's commit: large
// enough to keep the files few, far below fast-import's big-file threshold.
const sizedBlob = 32 << 20

// writeSizedCommit writes to w the fast-import stream of a commit on main,
// on top of parent, adding random files of size bytes in all. An error of
// a write to the buffer also comes back from its flush.
func writeSizedCommit(w io.Writer, parent string, size int64) error {
	bw := bufio.NewWriterSize(w, 1<<20)
	buf := make([]byte, 1<<20)
	var n int
	for left := size; left > 0; n++ {
		blob := min(left, sizedBlob)
		left -= blob
		fmt.Fprintf(bw, "blob\nmark :%d\ndata %d\n", n+1, blob)
		for rest := blob; rest > 0; {
			chunk := buf[:min(rest, int64(len(buf)))]
			_, _ = rand.Read(chunk)
			if _, err := bw.Write(chunk); err != nil {
				return err
			}
			rest -= int64(len(chunk))
		}
		_, _ = bw.WriteString("\n")
	}
	msg := fmt.Sprintf("Add %d bytes of random files", size)
	fmt.Fprintf(bw, "commit refs/heads/main\ncommitter Acceptance <acceptance@example.test> %d +0000\ndata %d\n%s\nfrom %s\n",
		time.Now().Unix(), len(msg), msg, parent)
	for i := 1; i <= n; i++ {
		fmt.Fprintf(bw, "M 100644 :%d large/file-%04d.bin\n", i, i)
	}
	_, _ = bw.WriteString("\n")
	return bw.Flush()
}

// packBytes is the size of the packs of the repository whose git directory
// is dir.
func packBytes(tb testing.TB, dir string) int64 {
	tb.Helper()
	packs, err := filepath.Glob(filepath.Join(dir, "objects", "pack", "*.pack"))
	if err != nil {
		tb.Fatal(err)
	}
	var total int64
	for _, p := range packs {
		fi, err := os.Stat(p)
		if err != nil {
			tb.Fatal(err)
		}
		total += fi.Size()
	}
	return total
}

// RegisterWithin is Register waiting up to limit for the workspace to be
// ready, for an origin too large to clone within the usual wait.
func (h *Hub) RegisterWithin(limit time.Duration, owner *User, slug string, o *Origin, extra map[string]any) *Workspace {
	h.t.Helper()
	body := map[string]any{"slug": slug, "git_url": o.URL()}
	for k, v := range extra {
		body[k] = v
	}
	h.Call(http.MethodPost, "/api/v1/workspaces", owner.Key, body).Want(h.t, http.StatusCreated)
	ws := h.WaitReadyWithin(limit, owner.Key, slug)
	return &Workspace{Slug: slug, Remote: str(ws["hub_url"]), Owner: owner, Origin: o, Body: ws}
}

// Register registers slug, owned by owner, from the origin o, with the
// members of extra added to the create's body, and waits until it is
// ready.
func (h *Hub) Register(owner *User, slug string, o *Origin, extra map[string]any) *Workspace {
	h.t.Helper()
	return h.RegisterWithin(30*time.Second, owner, slug, o, extra)
}

// WaitReady polls the workspace slug until it is ready and returns it; a
// workspace that fails fails the test.
func (h *Hub) WaitReady(credential, slug string) map[string]any {
	h.t.Helper()
	return h.WaitReadyWithin(30*time.Second, credential, slug)
}

// WaitReadyWithin is WaitReady with another limit.
func (h *Hub) WaitReadyWithin(limit time.Duration, credential, slug string) map[string]any {
	h.t.Helper()
	var r Response
	EventuallyWithin(h.t, limit, "workspace "+slug+" to be ready", func() bool {
		r = h.Call(http.MethodGet, "/api/v1/workspaces/"+slug, credential, nil)
		if r.Body["status"] == "failed" {
			h.t.Fatalf("workspace %s failed: %s", slug, r.Raw)
		}
		return r.Body["status"] == "ready"
	})
	return r.Body
}

// Fork is a task fork created through the API.
type Fork struct {
	Slug, Remote string
	// Token is the token minted with the fork, "" when none was asked for.
	Token string
	// Body is the create's answer.
	Body map[string]any
}

// Fork creates a fork of parent with credential, minting a token bound to
// it with permissions unless they are nil, with the members of extra added
// to the body (a token member there replaces the harness's).
func (h *Hub) Fork(credential, parent string, permissions []string, extra map[string]any) *Fork {
	h.t.Helper()
	body := map[string]any{}
	if permissions != nil {
		body["token"] = map[string]any{"permissions": permissions}
	}
	for k, v := range extra {
		body[k] = v
	}
	r := h.Call(http.MethodPost, "/api/v1/workspaces/"+parent+"/forks", credential, body).Want(h.t, http.StatusCreated)
	ws, _ := r.Body["workspace"].(map[string]any)
	token, _ := r.Body["token"].(map[string]any)
	return &Fork{Slug: str(ws["slug"]), Remote: str(r.Body["remote"]), Token: str(token["token"]), Body: r.Body}
}

// PushToOrigin sends the hub's branch of ws to the workspace's origin, which
// must take pushes (PushableOrigin), with parley workspace push run with
// the owner's API key, and returns the commit the origin's branch is at
// afterwards.
func (h *Hub) PushToOrigin(ws *Workspace, branch string) string {
	h.t.Helper()
	h.CLI(ws.Owner.Key, "workspace", "push", ws.Slug, branch).Must(h.t)
	return ws.Origin.Head(branch)
}
