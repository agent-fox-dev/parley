// Package gitrepo runs the git command for the hub, as a subprocess and with
// no git library: against an origin (list its refs, clone it) and on a bare
// copy on disk (its branches, its configuration and the origin heads kept as
// the refs refs/remotes/origin/<name>). It imports nothing of the project.
//
// Every git process runs with a fixed minimal environment, away from the
// configuration, credential stores and keys of the account that runs the
// hub, with terminal prompts disabled, and is killed with every process it
// started when its context ends. A credential reaches git only through the
// environment of the one process that needs it (see Credential).
package gitrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// waitDelay is how long a git process whose context ended may take to give
// back its output after it was killed.
const waitDelay = 2 * time.Second

// stderrLimit is how much of git's standard error an error keeps.
const stderrLimit = 8 << 10

// Git runs the git command. Its zero value runs git from PATH with the
// minimal environment alone.
type Git struct {
	// Env are variables, as NAME=value, added to the minimal environment of
	// every git process, such as GIT_SSL_CAINFO for an origin whose
	// certificate a private authority signed, or a proxy.
	Env []string
	// Trace, when set, is called with the arguments of every git invocation,
	// the program name excluded, before it runs.
	Trace func(args []string)
}

// Remote is what an origin lists: its branches and its tags, each name with
// the object id it points at, and its default branch.
type Remote struct {
	// DefaultBranch is the branch origin's HEAD names, "" when it names
	// none that exists.
	DefaultBranch string
	// Branches maps each branch name (without refs/heads/) to its commit id.
	Branches map[string]string
	// Tags maps each tag name (without refs/tags/) to the id it points at.
	Tags map[string]string
}

// ListRemote lists the branches, the tags and the default branch of the
// origin at url, presenting cred when origin asks for one. An error means
// that origin refused the request, could not be reached, or that ctx ended.
func (g Git) ListRemote(ctx context.Context, url string, cred Credential) (Remote, error) {
	out, err := g.run(ctx, access{cred, url}, "-c", "credential.helper=", "ls-remote", "--symref", "--", url)
	if err != nil {
		return Remote{}, err
	}
	r := Remote{Branches: map[string]string{}, Tags: map[string]string{}}
	for line := range strings.Lines(string(out)) {
		left, name, ok := strings.Cut(strings.TrimRight(line, "\n"), "\t")
		if !ok {
			continue
		}
		if target, isSymref := strings.CutPrefix(left, "ref: "); isSymref {
			if b, isBranch := strings.CutPrefix(target, "refs/heads/"); isBranch && name == "HEAD" {
				r.DefaultBranch = b
			}
			continue
		}
		if b, isBranch := strings.CutPrefix(name, "refs/heads/"); isBranch {
			r.Branches[b] = left
		} else if tag, isTag := strings.CutPrefix(name, "refs/tags/"); isTag && !strings.HasSuffix(tag, "^{}") {
			r.Tags[tag] = left
		}
	}
	if _, exists := r.Branches[r.DefaultBranch]; !exists {
		r.DefaultBranch = ""
	}
	return r, nil
}

// Clone makes a bare copy of the origin at url in dir, which must not exist
// or be empty: every branch and every tag with their full history, HEAD at
// origin's default branch and remote.origin.url set to url. It presents
// cred when origin asks for one. On an error git removes what it made.
func (g Git) Clone(ctx context.Context, url string, cred Credential, dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("clone into %s: %w", dir, err)
	}
	_, err = g.run(ctx, access{cred, url}, "-c", "credential.helper=", "clone", "--bare", "--quiet", "--", url, abs)
	return err
}

// Repo is a repository on disk, opened with Git.Open. Its methods run git on
// it alone: never against origin and never with a credential.
type Repo struct {
	g   Git
	dir string
}

// Open opens the repository whose git directory is dir, as a bare copy has
// it. It returns an error when dir does not exist or is not a repository: a
// file, an empty directory or a directory git does not take for one.
func (g Git) Open(ctx context.Context, dir string) (*Repo, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dir, err)
	}
	r := &Repo{g: g, dir: abs}
	if _, err := r.git(ctx, "rev-parse", "--git-dir"); err != nil {
		return nil, fmt.Errorf("open %s: %w", dir, err)
	}
	return r, nil
}

// Dir is the repository's git directory.
func (r *Repo) Dir() string { return r.dir }

// DefaultBranch is the branch the repository's HEAD names. It returns an
// error when HEAD names no branch.
func (r *Repo) DefaultBranch(ctx context.Context) (string, error) {
	out, err := r.git(ctx, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return "", err
	}
	b, ok := strings.CutPrefix(strings.TrimSpace(string(out)), "refs/heads/")
	if !ok {
		return "", fmt.Errorf("HEAD of %s names no branch", r.dir)
	}
	return b, nil
}

// SetDefaultBranch points the repository's HEAD at branch, which need not
// exist yet.
func (r *Repo) SetDefaultBranch(ctx context.Context, branch string) error {
	_, err := r.git(ctx, "symbolic-ref", "HEAD", "refs/heads/"+branch)
	return err
}

// ResolveBranch is the commit id branch points at; found is false, with no
// error, when there is no such branch or it points at no commit.
func (r *Repo) ResolveBranch(ctx context.Context, branch string) (id string, found bool, err error) {
	return r.resolve(ctx, "refs/heads/"+branch)
}

// Branches maps every branch of the repository to its commit id.
func (r *Repo) Branches(ctx context.Context) (map[string]string, error) {
	return r.refs(ctx, "refs/heads/")
}

// OriginURL is the URL of the repository's remote origin, as the clone
// stored it.
func (r *Repo) OriginURL(ctx context.Context) (string, error) {
	url, found, err := r.Config(ctx, "remote.origin.url")
	if err == nil && !found {
		err = fmt.Errorf("%s has no remote.origin.url", r.dir)
	}
	return url, err
}

// Config reads the value of key from the repository's own configuration;
// found is false, with no error, when it is not set there.
func (r *Repo) Config(ctx context.Context, key string) (value string, found bool, err error) {
	out, err := r.git(ctx, "config", "--local", "--get", key)
	if exitCode(err) == 1 {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return strings.TrimSuffix(string(out), "\n"), true, nil
}

// SetConfig writes value under key in the repository's own configuration.
func (r *Repo) SetConfig(ctx context.Context, key, value string) error {
	_, err := r.git(ctx, "config", "--local", key, value)
	return err
}

// originPrefix is where a copy keeps the last head seen on origin of each
// branch.
const originPrefix = "refs/remotes/origin/"

// OriginHeads maps every branch whose origin head the repository keeps to
// that head's commit id.
func (r *Repo) OriginHeads(ctx context.Context) (map[string]string, error) {
	return r.refs(ctx, originPrefix)
}

// OriginHead is the origin head the repository keeps for branch; found is
// false, with no error, when it keeps none.
func (r *Repo) OriginHead(ctx context.Context, branch string) (id string, found bool, err error) {
	return r.resolve(ctx, originPrefix+branch)
}

// SetOriginHead records id as the origin head of branch, the ref
// refs/remotes/origin/<branch>.
func (r *Repo) SetOriginHead(ctx context.Context, branch, id string) error {
	_, err := r.git(ctx, "update-ref", originPrefix+branch, id)
	return err
}

// DeleteOriginHead removes the origin head of branch; one that does not
// exist is no error.
func (r *Repo) DeleteOriginHead(ctx context.Context, branch string) error {
	_, err := r.git(ctx, "update-ref", "-d", originPrefix+branch)
	return err
}

// Remove removes the copy or partial clone at dir and everything in it;
// nothing at dir is no error.
func Remove(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove %s: %w", dir, err)
	}
	return nil
}

// resolve is the commit id ref points at; found is false when there is no
// such ref or it points at no commit.
func (r *Repo) resolve(ctx context.Context, ref string) (string, bool, error) {
	out, err := r.git(ctx, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if exitCode(err) == 1 {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return strings.TrimSpace(string(out)), true, nil
}

// refs maps every ref below prefix, by its name without the prefix, to the
// object id it points at.
func (r *Repo) refs(ctx context.Context, prefix string) (map[string]string, error) {
	out, err := r.git(ctx, "for-each-ref", "--format=%(objectname) %(refname)", prefix)
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for line := range strings.Lines(string(out)) {
		id, ref, ok := strings.Cut(strings.TrimRight(line, "\n"), " ")
		name, under := strings.CutPrefix(ref, prefix)
		if ok && under && name != "HEAD" {
			refs[name] = id
		}
	}
	return refs, nil
}

// git runs git on the repository.
func (r *Repo) git(ctx context.Context, args ...string) ([]byte, error) {
	return r.g.run(ctx, access{}, append([]string{"--git-dir=" + r.dir}, args...)...)
}

// access is the credential a git process presents and the URL of the origin
// it presents it to; the zero value presents none.
type access struct {
	cred Credential
	url  string
}

// run runs git with args under ctx, with the minimal environment, Env and
// a's credential, and returns its standard output. A failure's error holds git's
// standard error; when ctx ended, it wraps ctx's error instead. The process
// runs in a private home directory of its own, outside any repository, so
// that no repository around the hub's working directory takes part; the
// directory is removed when git has returned.
func (g Git) run(ctx context.Context, a access, args ...string) ([]byte, error) {
	return g.runInput(ctx, a, nil, args...)
}

// runInput is run with stdin as git's standard input, none when nil.
func (g Git) runInput(ctx context.Context, a access, stdin []byte, args ...string) ([]byte, error) {
	out, err := g.runOutput(ctx, a, stdin, args...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// runOutput is runInput that returns git's standard output after a failure
// too, for a command such as git push --porcelain that reports what it did
// there also when it exits with an error.
func (g Git) runOutput(ctx context.Context, a access, stdin []byte, args ...string) ([]byte, error) {
	dir, err := privateDir(!a.cred.IsZero())
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if g.Trace != nil {
		g.Trace(append([]string(nil), args...))
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = homePath(dir)
	cmd.Env = append(append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + homePath(dir),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		// ssh reads a password or a host key answer from the terminal, which
		// GIT_TERMINAL_PROMPT does not stop: batch mode makes it fail
		// instead of hanging a git@ origin's call. Env comes later, so a
		// caller's GIT_SSH_COMMAND replaces it.
		"GIT_SSH_COMMAND=ssh -o BatchMode=yes",
		"LC_ALL=C",
	}, g.Env...), credentialEnv(dir, a.cred, a.url)...)
	var stdout bytes.Buffer
	stderr := &limitedBuffer{limit: stderrLimit}
	cmd.Stdout, cmd.Stderr = &stdout, stderr
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	cmd.WaitDelay = waitDelay
	killGroupOnCancel(cmd)
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return stdout.Bytes(), fmt.Errorf("git %s: %w", subcommand(args), ctxErr)
		}
		return stdout.Bytes(), fmt.Errorf("git %s: %w: %s", subcommand(args), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// subcommand is the git command args run, for an error's text.
func subcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-c":
			i++
		case !strings.HasPrefix(args[i], "-"):
			return args[i]
		}
	}
	return "git"
}

// exitCode is the exit status of the git process err reports, or -1 when
// err reports none.
func exitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

// limitedBuffer keeps the first limit bytes written to it and drops the
// rest.
type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }
