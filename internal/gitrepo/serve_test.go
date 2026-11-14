package gitrepo_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// Not parallel: the environment of the test process asks for protocol
// version 2 and hides every branch, through a system, a global and a home
// configuration, none of which may reach the git the hub runs.
func TestUploadPack_ServesTheRequestAsAskedWhateverTheHostSays(t *testing.T) {
	o := gitrepotest.NewOrigin(t)
	head := o.Commit("main", "first")
	hide := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(hide, []byte("[transfer]\n\thideRefs = refs/heads\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(hide), ".gitconfig"), []byte("[transfer]\n\thideRefs = refs/heads\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_SYSTEM", hide)
	t.Setenv("GIT_CONFIG_GLOBAL", hide)
	t.Setenv("HOME", filepath.Dir(hide))
	t.Setenv("GIT_PROTOCOL", "version=2")
	request := pkt("want "+head+"\n") + "0000" + pkt("done\n")
	cases := []struct {
		name       string
		req        gitrepo.ServeRequest
		want, not  string
		wantPrefix string
	}{
		{"version 0 advertisement", gitrepo.ServeRequest{AdvertiseRefs: true}, head + " refs/heads/main", " filter", ""},
		{"version 2 advertisement", gitrepo.ServeRequest{AdvertiseRefs: true, Protocol2: true}, "ls-refs", "refs/heads/", pkt("version 2\n")},
		{"configuration of the run", gitrepo.ServeRequest{AdvertiseRefs: true, Config: []string{"uploadpack.allowFilter=true"}}, " filter", "", ""},
		{"fetch exchange", gitrepo.ServeRequest{Stdin: strings.NewReader(request)}, "PACK", "", pkt("NAK\n")},
	}
	for _, c := range cases {
		var out bytes.Buffer
		c.req.Dir, c.req.Stdout = o.Dir(), &out
		if err := (gitrepo.Git{}).UploadPack(context.Background(), c.req); err != nil {
			t.Errorf("%s: UploadPack = %v", c.name, err)
			continue
		}
		got := out.String()
		if !strings.Contains(got, c.want) || (c.not != "" && strings.Contains(got, c.not)) || !strings.HasPrefix(got, c.wantPrefix) {
			t.Errorf("%s: git wrote %q, want it to start with %q, hold %q and not %q", c.name, got, c.wantPrefix, c.want, c.not)
		}
	}
	var out bytes.Buffer
	err := (gitrepo.Git{}).UploadPack(context.Background(), gitrepo.ServeRequest{Dir: o.Dir(), Stdin: strings.NewReader("0009junk"), Stdout: &out})
	var se *gitrepo.ServeError
	if !errors.As(err, &se) || !strings.Contains(se.Stderr, "hung up") {
		t.Errorf("a truncated request: UploadPack = %v, want a *ServeError with git's text", err)
	}
}

// Not parallel: it puts a fake git first on the PATH of the test process,
// which starts a child of its own, answers, and waits on the child until it
// is killed.
func TestUploadPack_StreamsUntilTheContextEndsAndThenKillsGitWithItsChildren(t *testing.T) {
	bin := t.TempDir()
	child := filepath.Join(bin, "child")
	script := "#!/bin/sh\nsleep 30 &\necho $! > '" + child + "'\nprintf '0008NAK\\n'\nwait\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- (gitrepo.Git{}).UploadPack(ctx, gitrepo.ServeRequest{Dir: bin, Stdin: strings.NewReader("0000"), Stdout: &signalWriter{first: first}})
	}()
	select {
	case <-first:
	case err := <-done:
		t.Fatalf("UploadPack returned %v before git wrote anything to its writer", err)
	case <-time.After(5 * time.Second):
		t.Fatal("git's first bytes did not reach the writer within 5s")
	}
	select {
	case err := <-done:
		t.Fatalf("UploadPack returned %v while git was still running", err)
	default:
	}
	cancel()
	start := time.Now()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("UploadPack = %v, want an error wrapping context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("UploadPack did not return within 3s of the end of its context")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("returned %v after the end of its context, want within 2s", d)
	}
	pid := readPID(t, child)
	for deadline := time.Now().Add(2 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("git's child %d is still running 2s after the return", pid)
		}
	}
}

func TestUploadPack_GivesUpOnAVersion2RequestThatStopsComingWhenTheContextEnds(t *testing.T) {
	t.Parallel()
	o := gitrepotest.NewOrigin(t)
	head := o.Commit("main", "first")
	stalled, sender := io.Pipe()
	defer sender.Close()
	go func() { _, _ = io.WriteString(sender, pkt("command=fetch\n")+"0001"+pkt("want "+head+"\n")) }()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := (gitrepo.Git{}).UploadPack(ctx, gitrepo.ServeRequest{Dir: o.Dir(), Protocol2: true, Stdin: stalled, Stdout: io.Discard})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Errorf("UploadPack = %v after %v, want an error wrapping context.DeadlineExceeded within 2s", err, time.Since(start))
	}
}

// signalWriter closes first at its first write.
type signalWriter struct {
	first  chan struct{}
	closed bool
}

func (s *signalWriter) Write(p []byte) (int, error) {
	if !s.closed {
		s.closed = true
		close(s.first)
	}
	return len(p), nil
}

// readPID reads the process id written to file.
func readPID(t *testing.T, file string) int {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// pkt is s as one pkt-line.
func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }
