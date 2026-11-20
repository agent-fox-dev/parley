package image_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/test/image"
)

const sandboxDockerfile = "containers/sandbox/Dockerfile"

// objectID is the output of git rev-parse HEAD in a repository of SHA-1
// object names.
var objectID = regexp.MustCompile(`^[0-9a-f]{40}$`)

func TestSandboxImage_SatisfiesTheImageContract(t *testing.T) {
	engine := engineFor(t)
	t.Parallel()
	ref := buildImage(t, engine, sandboxDockerfile, "parley-sandbox")

	t.Run("the reference image", func(t *testing.T) {
		t.Parallel()
		r := run(t, engine, "run", "--rm", ref, "sh", "-c",
			`cd "$(mktemp -d)" && git init -q . && echo x > f && git add f && git -c user.name=t -c user.email=t@x commit -q -m m && git rev-parse HEAD`)
		lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
		if r.code != 0 || !objectID.MatchString(lines[len(lines)-1]) {
			t.Errorf("git init, commit and rev-parse HEAD: exit code %d, output %q; want 0 and an object id\n%s", r.code, r.stdout, r.stderr)
		}
		if err := image.CheckContract(engine, ref); err != nil {
			t.Errorf("CheckContract(%s) = %v, want nil", ref, err)
		}
	})

	t.Run("an image without git", func(t *testing.T) {
		t.Parallel()
		noGit := buildFrom(t, engine, finalBase(t, sandboxDockerfile), "parley-sandbox-without-git")
		err := image.CheckContract(engine, noGit)
		if err == nil {
			t.Fatalf("CheckContract(%s) = nil, want an error", noGit)
		}
		if msg := err.Error(); !strings.Contains(msg, noGit) || !strings.Contains(msg, "git") {
			t.Errorf("CheckContract(%s) = %q; want an error naming the image and git", noGit, msg)
		}
	})

	t.Run("the image PARLEY_SANDBOX_IMAGE names", func(t *testing.T) {
		t.Parallel()
		own := os.Getenv("PARLEY_SANDBOX_IMAGE")
		if own == "" {
			t.Skip("PARLEY_SANDBOX_IMAGE names no image to check")
		}
		if err := image.CheckContract(engine, own); err != nil {
			t.Errorf("CheckContract(%s) = %v, want nil", own, err)
		}
	})
}

func TestSandboxImage_HoldsGitAShellCertificatesAndParleyButNoParleyd(t *testing.T) {
	engine := engineFor(t)
	t.Parallel()
	ref := buildImage(t, engine, sandboxDockerfile, "parley-sandbox")

	t.Run("git 2.38 or later", func(t *testing.T) {
		t.Parallel()
		out := must(t, engine, "run", "--rm", ref, "git", "--version")
		v, err := gitrepo.ParseVersion(out)
		if err != nil || !v.AtLeast(2, 38) {
			t.Errorf("git --version = %q (%v); want 2.38 or later", out, err)
		}
	})

	for _, tt := range []struct {
		name string
		env  []string
	}{
		{name: "parley with the build values"},
		{name: "parley with the build values and no hub to reach", env: []string{"-e", "PARLEY_ENDPOINT_URL=http://127.0.0.1:9"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			args := append(append([]string{"run", "--rm"}, tt.env...), ref, "parley", "version")
			r := run(t, engine, args...)
			if r.code != 0 {
				t.Fatalf("parley version: exit code %d\n%s%s", r.code, r.stdout, r.stderr)
			}
			var doc struct {
				CLI struct {
					Version string `json:"version"`
				} `json:"cli"`
			}
			if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil || doc.CLI.Version != buildVersion {
				t.Errorf("parley version printed %q (%v); want cli.version %q", r.stdout, err, buildVersion)
			}
		})
	}

	for _, tt := range []struct {
		name, script string
		succeeds     bool
	}{
		{name: "a POSIX shell at /bin/sh", script: "true", succeeds: true},
		{name: "the base image's CA certificates", script: "test -s /etc/ssl/certs/ca-certificates.crt", succeeds: true},
		{name: "no parleyd", script: "command -v parleyd", succeeds: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := run(t, engine, "run", "--rm", ref, "/bin/sh", "-c", tt.script)
			if (r.code == 0) != tt.succeeds {
				t.Errorf("/bin/sh -c %q: exit code %d, want it to succeed: %t\n%s%s", tt.script, r.code, tt.succeeds, r.stdout, r.stderr)
			}
		})
	}
}

func TestSandboxImage_RunsItsCommandUnchangedAsUser1000WithAWritableHome(t *testing.T) {
	engine := engineFor(t)
	t.Parallel()
	ref := buildImage(t, engine, sandboxDockerfile, "parley-sandbox")

	for _, tt := range []struct {
		name    string
		command []string
		stdout  string
	}{
		{name: "the command alone", command: []string{"echo", "hi"}, stdout: "hi\n"},
		{name: "as user 1000", command: []string{"id", "-u"}, stdout: "1000\n"},
		{name: "a writable home", command: []string{"sh", "-c", `touch "$HOME/x"`}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := run(t, engine, append([]string{"run", "--rm", ref}, tt.command...)...)
			if r.code != 0 || r.stdout != tt.stdout {
				t.Errorf("%q: exit code %d, stdout %q; want 0 and %q\n%s", tt.command, r.code, r.stdout, tt.stdout, r.stderr)
			}
		})
	}
}

// finalBase returns the image the last stage of dockerfile, a path from the
// repository root, starts from.
func finalBase(t *testing.T, dockerfile string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", dockerfile))
	if err != nil {
		t.Fatal(err)
	}
	var base string
	for line := range strings.Lines(string(data)) {
		if f := strings.Fields(line); len(f) >= 2 && strings.EqualFold(f[0], "FROM") {
			base = f[1]
		}
	}
	if base == "" {
		t.Fatalf("%s has no FROM line", dockerfile)
	}
	return base
}

// buildFrom builds an image that is base and nothing more, and returns its
// reference. The image is removed when t ends.
func buildFrom(t *testing.T, engine, base, name string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM "+base+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ref := fmt.Sprintf("%s:test-%d-%d", name, os.Getpid(), time.Now().UnixNano())
	if r := runFor(t, buildLimit, engine, "build", "-t", ref, dir); r.code != 0 {
		t.Fatalf("%s build FROM %s: exit code %d\n%s", engine, base, r.code, tail(r.stdout+r.stderr, 40))
	}
	t.Cleanup(func() { run(t, engine, "rmi", "-f", ref) })
	return ref
}
