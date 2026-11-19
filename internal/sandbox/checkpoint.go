package sandbox

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

const (
	// EventCheckpoint is written with every checkpoint stored on a sandbox.
	EventCheckpoint = "parley.sandbox.checkpoint"
	// CheckpointsMetric counts the checkpoint attempts under the label
	// result: ok when a ref was stored, empty for a checkpoint without
	// refs, failed for one that could not be completed.
	CheckpointsMetric = "parley_sandbox_checkpoints_total"
	// CheckpointBound is how long a checkpoint attempt may take on the
	// hub's clock: every step calls the runtime, which may never answer,
	// and a checkpoint must not hold up the stop or the deletion it
	// precedes.
	CheckpointBound = 5 * time.Minute
	// CheckpointFileLimit is the size in bytes above which an untracked
	// file is left out of a checkpoint.
	CheckpointFileLimit int64 = 100 << 20
	// CheckpointRefPrefix begins the refs a checkpoint stores in the
	// sandbox's workspace: the sandbox id and the branch follow it.
	CheckpointRefPrefix = "refs/parley/sandboxes/"
)

// The checkpoint results, the label of CheckpointsMetric.
const (
	checkpointOK     = "ok"
	checkpointEmpty  = "empty"
	checkpointFailed = "failed"
)

// checkpointName begins the names of the files and the ref a checkpoint
// makes in the sandbox's git directory for a moment.
const checkpointName = "parley-checkpoint-"

// errCheckpointTimeout ends a checkpoint attempt that did not end within
// CheckpointBound.
var errCheckpointTimeout = fmt.Errorf("the checkpoint did not end within %s", CheckpointBound)

// Actor is who an operation on a sandbox acts for, as its event names it.
type Actor struct {
	// Type is one of the event package's Actor constants.
	Type string
	// ID is the user a credential acts for; nil for the admin token and
	// the system.
	ID *string
}

// SystemActor is the hub acting on its own.
var SystemActor = Actor{Type: event.ActorSystem}

// CheckpointRef is a ref a checkpoint stored.
type CheckpointRef struct {
	Branch string `json:"branch"`
	SHA    string `json:"sha"`
	// Dirty is true for a ref that ends in the commit of the uncommitted
	// changes.
	Dirty bool `json:"dirty"`
}

// CheckpointState is the latest checkpoint of a sandbox, as its record
// stores it and its object shows it.
type CheckpointState struct {
	// At is in whole seconds UTC, which JSON writes as RFC 3339 with no
	// fraction.
	At time.Time `json:"at"`
	OK bool      `json:"ok"`
	// Error is why the checkpoint failed, scrubbed, or for one that is ok
	// the untracked files it left out; nil for neither.
	Error *string `json:"error"`
	// Refs are sorted by branch; a failed checkpoint keeps those of the
	// last successful one, which still exist.
	Refs []CheckpointRef `json:"refs"`
}

// Checkpoint saves the git state of the working directory of the sandbox
// rec to its workspace and returns the checkpoint it stored: every local
// branch whose head commit the workspace's repository lacks, and the
// uncommitted changes of the checked-out branch ("HEAD" when none is) as
// one extra commit on its head, each under CheckpointRefPrefix, the sandbox
// id and the branch. The extra commit holds what git does not ignore, but
// untracked files larger than the size limit, which the checkpoint's error
// names. The sandbox's branches, index and working tree stay as they were:
// the commit is built in a temporary index, and the refs travel as one git
// bundle through the runtime's file download, never through a push, so no
// change or version results.
//
// The checkpoint is stored on the record with EventCheckpoint by actor in
// one transaction, and counted in CheckpointsMetric. A sandbox created
// without a checkout, or whose working directory is no git repository, gets
// one without refs. A sandbox without a hub credential gets none at all: it
// returns nil, and runs nothing in the sandbox.
//
// The attempt is all or nothing and bounded by CheckpointBound on the
// hub's clock. One that fails stores no ref, records the failure with its
// reason scrubbed of the sandbox's values, keeps the refs of the last
// successful checkpoint, and returns the stored checkpoint with the
// scrubbed error; the sandbox's state is left as it is. When ctx ends
// first, as when the hub stops, the attempt is abandoned and nothing is
// recorded.
func (s *Service) Checkpoint(ctx context.Context, rec Record, actor Actor) (*CheckpointState, error) {
	if rec.HubAccess == nil {
		return nil, nil
	}
	attempt, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	bound := s.newTimer(CheckpointBound)
	defer bound.Stop()
	go func() {
		select {
		case <-bound.C():
			cancel(errCheckpointTimeout)
		case <-attempt.Done():
		}
	}()
	run := &checkpointRun{s: s, client: s.client, rec: rec, limit: s.checkpointLimit.Load(), suffix: randomSuffix()}
	if run.limit <= 0 {
		run.limit = CheckpointFileLimit
	}
	refs, skipped, err := run.save(attempt)
	if ctx.Err() != nil {
		return nil, fmt.Errorf("the checkpoint of sandbox %s was abandoned: %w", rec.ID, ctx.Err())
	}
	run.cleanup(ctx)
	if err != nil && errors.Is(context.Cause(attempt), errCheckpointTimeout) {
		err = fmt.Errorf("%w: %v", errCheckpointTimeout, err)
	}
	st := &CheckpointState{At: s.deps.Now().UTC().Truncate(time.Second), OK: err == nil, Refs: refs}
	var text string
	switch {
	case err != nil:
		text = s.ScrubSandbox(ctx, rec.ID, err.Error())
		st.Error = &text
	case len(skipped) > 0:
		note := s.ScrubSandbox(ctx, rec.ID, "skipped: "+strings.Join(skipped, ", "))
		st.Error = &note
	}
	if werr := s.storeCheckpoint(ctx, rec.ID, st, actor, err != nil); werr != nil {
		return nil, werr
	}
	result := checkpointOK
	switch {
	case err != nil:
		result = checkpointFailed
	case len(st.Refs) == 0:
		result = checkpointEmpty
	}
	_ = s.deps.Recorder.Add(CheckpointsMetric, map[string]string{"result": result}, 1)
	if err != nil {
		s.logger().Warn("a sandbox checkpoint failed", "sandbox_id", rec.ID, "error", s.scrub(text))
		return st, &scrubbedError{text: text, err: err}
	}
	return st, nil
}

// storeCheckpoint stores st on the sandbox id with EventCheckpoint by actor,
// in one transaction; a failed checkpoint takes the refs of the stored one.
func (s *Service) storeCheckpoint(ctx context.Context, id string, st *CheckpointState, actor Actor, failed bool) error {
	return s.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		cur, err := Get(ctx, tx, id)
		if err != nil {
			return err
		}
		if failed {
			st.Refs = lastRefs(cur.Checkpoint)
		}
		raw, err := json.Marshal(st)
		if err != nil {
			return fmt.Errorf("the checkpoint of sandbox %s: %w", id, err)
		}
		if err := Update(ctx, tx, id, SetCheckpoint(raw), SetUpdatedAt(s.deps.Now())); err != nil {
			return err
		}
		_, err = s.deps.Events.Emit(ctx, tx, event.Input{
			Type: EventCheckpoint, ActorType: actor.Type, ActorID: actor.ID,
			ResourceType: "sandbox", ResourceID: id, Workspace: &cur.Workspace, SessionID: cur.SessionID,
			Metadata: map[string]any{"ok": st.OK, "refs": st.Refs, "error": st.Error},
		})
		if err != nil {
			return fmt.Errorf("event of sandbox %s: %w", id, err)
		}
		return nil
	})
}

// lastRefs are the refs of the stored checkpoint raw, those of the last
// successful one, since a failed one keeps them; none for no checkpoint.
func lastRefs(raw json.RawMessage) []CheckpointRef {
	var cp CheckpointState
	if raw == nil || json.Unmarshal(raw, &cp) != nil || cp.Refs == nil {
		return []CheckpointRef{}
	}
	return cp.Refs
}

// checkpointRun is one checkpoint attempt of a sandbox: the git commands it
// runs in the working directory and what it leaves there to remove.
type checkpointRun struct {
	s      *Service
	client runtime.Client
	rec    Record
	// limit is the size in bytes above which an untracked file is left out.
	limit int64
	// suffix makes the names of this attempt's files and ref its own.
	suffix string
	// gitDir is the clone's git directory relative to the working
	// directory, once known; made lists the files and ref made in it.
	gitDir  string
	made    []string
	madeRef string
}

// save stores the checkpoint refs of the working directory and returns
// them, sorted by branch, with the untracked files it left out.
func (r *checkpointRun) save(ctx context.Context) (refs []CheckpointRef, skipped []string, err error) {
	refs = []CheckpointRef{}
	if r.rec.Checkout == nil {
		return refs, nil, nil
	}
	if r.client == nil {
		return nil, nil, errors.New("the sandbox runtime is not connected")
	}
	repo, err := r.repository(ctx)
	if err != nil || !repo {
		return refs, nil, err
	}
	heads, current, err := r.heads(ctx)
	if err != nil {
		return nil, nil, err
	}
	hub, err := r.s.deps.Repos(ctx, r.rec.Workspace)
	if err != nil {
		return nil, nil, err
	}
	ids := make([]string, 0, len(heads))
	for _, id := range heads {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	missing, err := hub.Missing(ctx, slices.Compact(ids))
	if err != nil {
		return nil, nil, err
	}
	dirty, skipped, err := r.dirtyCommit(ctx, heads, current)
	if err != nil {
		return nil, nil, err
	}
	for branch, id := range heads {
		if slices.Contains(missing, id) && (dirty == "" || branch != current) {
			refs = append(refs, CheckpointRef{Branch: branch, SHA: id})
		}
	}
	if dirty != "" {
		refs = append(refs, CheckpointRef{Branch: current, SHA: dirty, Dirty: true})
	}
	slices.SortFunc(refs, func(a, b CheckpointRef) int { return strings.Compare(a.Branch, b.Branch) })
	if len(refs) == 0 {
		return refs, skipped, nil
	}
	if err := r.transfer(ctx, hub, refs); err != nil {
		return nil, nil, err
	}
	return refs, skipped, nil
}

// repository reports whether the working directory is a git work tree, and
// keeps its git directory. One that is not a repository is no failure.
func (r *checkpointRun) repository(ctx context.Context) (bool, error) {
	res, err := r.exec(ctx, gitCall{args: []string{"rev-parse", "--is-inside-work-tree", "--git-dir"}})
	if err != nil {
		return false, err
	}
	if res.ExitCode != 0 {
		if strings.Contains(string(res.Stderr), "not a git repository") {
			return false, nil
		}
		return false, exitError("git rev-parse", res)
	}
	lines := strings.Split(strings.TrimSpace(string(res.Stdout)), "\n")
	if len(lines) != 2 || lines[0] != "true" {
		return false, nil
	}
	// The files the checkpoint makes are named relative to the working
	// directory, where every command runs and which the runtime's file
	// transfer resolves the same way.
	dir := strings.TrimSpace(lines[1])
	if clean := path.Clean(dir); dir == "" || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return false, fmt.Errorf("the git directory %q is not inside the working directory", dir)
	}
	r.gitDir = dir
	return true, nil
}

// heads maps each local branch of the clone to its head, and names the
// checked-out branch, "HEAD" when none is.
func (r *checkpointRun) heads(ctx context.Context) (map[string]string, string, error) {
	out, err := r.git(ctx, gitCall{args: []string{"for-each-ref", "--format=%(HEAD) %(objectname) %(refname)", "refs/heads/"}})
	if err != nil {
		return nil, "", err
	}
	heads, current := map[string]string{}, "HEAD"
	for line := range strings.Lines(out) {
		// %(HEAD) is "*" for the checked-out branch and " " for the others.
		line = strings.TrimSuffix(line, "\n")
		mark, rest, _ := strings.Cut(line, " ")
		id, ref, _ := strings.Cut(strings.TrimPrefix(rest, " "), " ")
		branch, ok := strings.CutPrefix(ref, "refs/heads/")
		if !ok || (mark != "*" && mark != "") || !commitID.MatchString(id) || !gitrepo.ValidRefName(CheckpointRefPrefix+r.rec.ID+"/"+branch) {
			return nil, "", fmt.Errorf("git for-each-ref printed %q", line)
		}
		heads[branch] = id
		if mark == "*" {
			current = branch
		}
	}
	return heads, current, nil
}

// dirtyCommit makes the extra commit of the uncommitted changes, when the
// working tree has any, on the head of current, and returns it ("" for
// none) with the untracked files it left out. It builds the tree in a
// temporary index seeded from the clone's, so that the index, the branch
// and the working tree are not touched, and makes the commit with the
// hub's identity and no configuration of the clone's that could sign it,
// so that a clone without an identity works too.
func (r *checkpointRun) dirtyCommit(ctx context.Context, heads map[string]string, current string) (string, []string, error) {
	status, err := r.git(ctx, gitCall{args: []string{"--no-optional-locks", "status", "--porcelain", "--untracked-files=normal"}})
	if err != nil || strings.TrimSpace(status) == "" {
		return "", nil, err
	}
	skipped, err := r.largeUntracked(ctx)
	if err != nil {
		return "", nil, err
	}
	parent := heads[current]
	if current == "HEAD" {
		out, err := r.exec(ctx, gitCall{args: []string{"rev-parse", "--verify", "--quiet", "HEAD"}})
		switch {
		case err != nil:
			return "", nil, err
		case out.ExitCode == 0 && commitID.MatchString(strings.TrimSpace(string(out.Stdout))):
			parent = strings.TrimSpace(string(out.Stdout))
		case out.ExitCode != 1:
			return "", nil, exitError("git rev-parse", out)
		}
	}
	index := r.file("index")
	if _, err := r.run(ctx, []string{"cp", "--", path.Join(r.gitDir, "index"), index}); err != nil {
		return "", nil, err
	}
	env := map[string]string{"GIT_INDEX_FILE": index}
	pathspecs := ".\x00"
	for _, p := range skipped {
		pathspecs += ":(exclude,literal)" + p + "\x00"
	}
	if _, err := r.git(ctx, gitCall{env: env, stdin: []byte(pathspecs),
		args: []string{"add", "--all", "--pathspec-from-file=-", "--pathspec-file-nul"}}); err != nil {
		return "", nil, err
	}
	tree, err := r.git(ctx, gitCall{env: env, args: []string{"write-tree"}})
	if err != nil {
		return "", nil, err
	}
	tree = strings.TrimSpace(tree)
	args := []string{"-c", "commit.gpgsign=false", "commit-tree", tree, "-m", "parley checkpoint"}
	if parent != "" {
		// Only left-out files changed: the head holds all there is to save.
		same, err := r.git(ctx, gitCall{args: []string{"rev-parse", "--verify", "--end-of-options", parent + "^{tree}"}})
		if err != nil {
			return "", nil, err
		}
		if strings.TrimSpace(same) == tree {
			return "", skipped, nil
		}
		args = append(args, "-p", parent)
	}
	sig := gitrepo.HubSignature(r.s.deps.Now())
	date := "@" + strconv.FormatInt(sig.Time.Unix(), 10) + " +0000"
	commit, err := r.git(ctx, gitCall{args: args, env: map[string]string{
		"GIT_AUTHOR_NAME": sig.Name, "GIT_AUTHOR_EMAIL": sig.Email, "GIT_AUTHOR_DATE": date,
		"GIT_COMMITTER_NAME": sig.Name, "GIT_COMMITTER_EMAIL": sig.Email, "GIT_COMMITTER_DATE": date,
	}})
	if err != nil {
		return "", nil, err
	}
	commit = strings.TrimSpace(commit)
	if !commitID.MatchString(commit) {
		return "", nil, fmt.Errorf("git commit-tree printed %q, not a commit id", commit)
	}
	return commit, skipped, nil
}

// largeUntracked lists the untracked files git does not ignore that are
// larger than the limit, in git's order, reading their sizes through the
// runtime.
func (r *checkpointRun) largeUntracked(ctx context.Context) ([]string, error) {
	out, err := r.git(ctx, gitCall{args: []string{"ls-files", "--others", "--exclude-standard", "-z"}})
	if err != nil {
		return nil, err
	}
	var large []string
	for p := range strings.SplitSeq(out, "\x00") {
		if p == "" {
			continue
		}
		fi, err := r.client.Stat(ctx, r.rec.ID, path.Join(WorkDir, p))
		switch {
		case errors.Is(err, runtime.ErrNotFound):
			// Gone meanwhile: git adds nothing of it either.
		case err != nil:
			return nil, err
		case !fi.Dir && fi.Size > r.limit:
			large = append(large, p)
		}
	}
	return large, nil
}

// transfer brings the commits of refs to the hub as one git bundle, made in
// the sandbox and read through the runtime's file download, and stores the
// refs in the workspace's repository hub in one transaction. The bundle
// leaves out what the clone's remote-tracking branches hold, which came
// from the hub.
func (r *checkpointRun) transfer(ctx context.Context, hub *gitrepo.Repo, refs []CheckpointRef) error {
	revs := []string{}
	targets := map[string]string{}
	for _, ref := range refs {
		targets[CheckpointRefPrefix+r.rec.ID+"/"+ref.Branch] = ref.SHA
		if !ref.Dirty {
			revs = append(revs, "refs/heads/"+ref.Branch)
			continue
		}
		// A bundle carries named refs only, so the extra commit gets one
		// for a moment, outside the branches.
		r.madeRef = "refs/parley/" + checkpointName + r.suffix
		if _, err := r.git(ctx, gitCall{args: []string{"update-ref", r.madeRef, ref.SHA}}); err != nil {
			return err
		}
		revs = append(revs, r.madeRef)
	}
	bundle := r.file("bundle")
	if _, err := r.git(ctx, gitCall{args: append(append([]string{"bundle", "create", bundle}, revs...), "--not", "--remotes=origin")}); err != nil {
		return err
	}
	local, err := os.CreateTemp("", checkpointName+"*.bundle")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(local.Name()) }()
	err = r.download(ctx, path.Join(WorkDir, bundle), local)
	if cerr := local.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return hub.FetchBundle(ctx, local.Name(), targets)
}

// download copies the sandbox's file p into w.
func (r *checkpointRun) download(ctx context.Context, p string, w io.Writer) error {
	rc, err := r.client.Download(ctx, r.rec.ID, p)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	if _, err := io.Copy(w, rc); err != nil {
		return fmt.Errorf("read the checkpoint bundle: %w", err)
	}
	return nil
}

// file is the name, relative to the working directory, of the attempt's
// file of kind in the git directory; it is removed afterwards.
func (r *checkpointRun) file(kind string) string {
	name := path.Join(r.gitDir, checkpointName+r.suffix+"."+kind)
	r.made = append(r.made, name)
	return name
}

// cleanup removes what the attempt made in the sandbox, whether it
// succeeded or not; what cannot be removed is logged.
func (r *checkpointRun) cleanup(ctx context.Context) {
	if len(r.made) == 0 && r.madeRef == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, cleanupBudget)
	defer cancel()
	var errs []error
	if r.madeRef != "" {
		_, err := r.git(ctx, gitCall{args: []string{"update-ref", "-d", r.madeRef}})
		errs = append(errs, err)
	}
	if len(r.made) > 0 {
		_, err := r.run(ctx, append([]string{"rm", "-f", "--"}, r.made...))
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		text := r.s.scrub(r.s.ScrubSandbox(ctx, r.rec.ID, err.Error()))
		r.s.logger().Warn("a sandbox checkpoint left files in the sandbox", "sandbox_id", r.rec.ID, "error", text)
	}
}

// gitCall is a git command of a checkpoint: its arguments after git, the
// variables added to its environment, and its standard input.
type gitCall struct {
	args  []string
	env   map[string]string
	stdin []byte
}

// exec runs the git command c in the working directory and returns its
// result, whatever its exit code. No hook of the clone's runs: a
// reference-transaction hook would run for the checkpoint's own ref.
func (r *checkpointRun) exec(ctx context.Context, c gitCall) (runtime.ExecResult, error) {
	argv := append([]string{"git", "-c", "core.hooksPath=/dev/null"}, c.args...)
	return r.client.Exec(ctx, r.rec.ID, runtime.ExecRequest{Command: argv, WorkDir: WorkDir, Env: c.env, Stdin: c.stdin})
}

// git runs the git command c and returns its standard output; an exit code
// other than 0 is an error holding its standard error.
func (r *checkpointRun) git(ctx context.Context, c gitCall) (string, error) {
	res, err := r.exec(ctx, c)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", exitError("git "+subcommandOf(c.args), res)
	}
	return string(res.Stdout), nil
}

// run runs the command argv in the working directory; an exit code other
// than 0 is an error holding its standard error.
func (r *checkpointRun) run(ctx context.Context, argv []string) (string, error) {
	res, err := r.client.Exec(ctx, r.rec.ID, runtime.ExecRequest{Command: argv, WorkDir: WorkDir})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", exitError(argv[0], res)
	}
	return string(res.Stdout), nil
}

// exitError is the failure of the command what that ended with res.
func exitError(what string, res runtime.ExecResult) error {
	return fmt.Errorf("%s exited with %d: %s", what, res.ExitCode, strings.TrimSpace(string(res.Stderr)))
}

// subcommandOf is the git command args run, past the options before it.
func subcommandOf(args []string) string {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-c":
			i++
		case !strings.HasPrefix(args[i], "-"):
			return args[i]
		}
	}
	return ""
}

// randomSuffix is 16 random hexadecimal digits.
func randomSuffix() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b[:])
}
