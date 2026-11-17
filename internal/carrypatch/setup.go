package carrypatch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// DefaultIntegrationBranch is the integration branch of a carry-patch
// workspace created without one. The workspace create stores the field as
// given, null included; the set-up after the first clone writes this name
// into the record, and IntegrationBranch answers it for a record that
// still holds null.
const DefaultIntegrationBranch = "deploy"

// rerereKey is the key of the copy's git configuration that turns on
// remembered resolutions, which a rebuild replays.
const rerereKey = "rerere.enabled"

// ErrUpstreamFetch is matched by the error of a set-up whose upstream
// fetch failed, so that a caller that treats an upstream failure apart
// (a rebuild, a sync) can tell it from a failure of another step.
var ErrUpstreamFetch = errors.New("upstream fetch")

// IntegrationBranch is the integration branch of the carry-patch workspace
// w: its stored integration_branch, or DefaultIntegrationBranch while the
// stored value is null. Every piece of carry-patch work asks this function
// for the name, so that the fallback exists in one place.
func IntegrationBranch(w workspace.Workspace) string {
	if w.IntegrationBranch == nil || *w.IntegrationBranch == "" {
		return DefaultIntegrationBranch
	}
	return *w.IntegrationBranch
}

// RecordWriter writes the set-up's one field of a workspace record.
type RecordWriter interface {
	// DefaultIntegrationBranch stores branch as the integration_branch of
	// the workspace slug when the record holds none, and leaves a stored
	// value alone.
	DefaultIntegrationBranch(ctx context.Context, slug, branch string) error
}

// UpstreamFetcher fetches a workspace's upstream into its copy and
// resolves the upstream base from what the copy then holds. *Component
// is one.
type UpstreamFetcher interface {
	FetchUpstream(ctx context.Context, w workspace.Workspace) error
	UpstreamBase(ctx context.Context, w workspace.Workspace) (string, error)
}

// CopyWriter writes a workspace copy's configuration and refs.
// *gitrepo.Repo is one.
type CopyWriter interface {
	SetConfig(ctx context.Context, key, value string) error
	SetRef(ctx context.Context, ref, id string) error
	CompareAndSwapRef(ctx context.Context, ref, newID, expectedOld string) error
}

// Setup is the set-up of a carry-patch workspace's copy, in four steps:
// write DefaultIntegrationBranch into a record that holds no
// integration_branch; fetch upstream; turn on remembered resolutions; and
// write the integration branch at the upstream base when the fetch
// succeeded and a base resolves, else at the workspace's head_sha, the
// head of origin's default branch, so that the branch exists either way.
//
// Replace is the form of the integration branch's write: true writes it
// whatever it points at, as after a fresh clone, where a branch of that
// name brought from origin is replaced (refs/remotes/origin/* stay as the
// clone recorded them); false creates it only when it does not exist and
// never moves an existing one, as on a ready workspace.
//
// LeaveBranch leaves the last step out: the integration branch is neither
// created nor moved, as for a rebuild, which writes it at its publish alone
// so that an attempt that fails or stops before the publish leaves it as it
// was.
type Setup struct {
	Record      RecordWriter
	Upstream    UpstreamFetcher
	Copy        CopyWriter
	Replace     bool
	LeaveBranch bool
}

// Run runs the steps of the set-up of w, each whatever the earlier ones
// did, and returns nil or one error that names every step that failed; it matches ErrUpstreamFetch when the upstream fetch was one of
// them. Unless LeaveBranch is set, a fetch that succeeded after which no
// upstream base resolves is reported as a failure too, though the
// integration branch is then written at head_sha. The set-up writes neither upstream_head_sha nor
// clone_error and emits no event. Every git operation on the copy runs
// through RunGit, so that its error text is cleaned like the fetch's.
func (s Setup) Run(ctx context.Context, w workspace.Workspace) error {
	var failed []error
	if w.IntegrationBranch == nil {
		if err := s.Record.DefaultIntegrationBranch(ctx, w.Slug, DefaultIntegrationBranch); err != nil {
			failed = append(failed, fmt.Errorf("record integration_branch %s: %w", DefaultIntegrationBranch, err))
		}
	}

	fetchErr := s.Upstream.FetchUpstream(ctx, w)
	if fetchErr != nil {
		failed = append(failed, fmt.Errorf("%w: %w", ErrUpstreamFetch, fetchErr))
	}

	err := RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		return s.Copy.SetConfig(ctx, rerereKey, "true")
	})
	if err != nil {
		failed = append(failed, fmt.Errorf("turn on %s: %w", rerereKey, err))
	}

	if !s.LeaveBranch {
		failed = append(failed, s.branch(ctx, w, IntegrationBranch(w), fetchErr == nil)...)
	}

	if len(failed) == 0 {
		return nil
	}
	return &setupError{slug: w.Slug, failed: failed}
}

// branch is the last step of the set-up of w: it writes the integration
// branch named branch at the upstream base when fetched says the fetch
// succeeded and a base resolves, else at head_sha, and returns what
// failed.
func (s Setup) branch(ctx context.Context, w workspace.Workspace, branch string, fetched bool) []error {
	var failed []error
	ref := "refs/heads/" + branch
	target := ""
	if fetched {
		base, err := s.Upstream.UpstreamBase(ctx, w)
		if err != nil {
			failed = append(failed, fmt.Errorf("upstream base, %s set at origin's head instead: %w", ref, err))
		}
		target = base
	}
	if target == "" && w.HeadSHA != nil {
		target = *w.HeadSHA
	}
	if err := s.writeBranch(ctx, ref, target); err != nil {
		failed = append(failed, fmt.Errorf("write the integration branch %s: %w", ref, err))
	}
	return failed
}

// writeBranch writes the integration branch ref at target in the form
// Replace names.
func (s Setup) writeBranch(ctx context.Context, ref, target string) error {
	if target == "" {
		return errors.New("neither an upstream base nor a head_sha to set it at")
	}
	return RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		if s.Replace {
			return s.Copy.SetRef(ctx, ref, target)
		}
		err := s.Copy.CompareAndSwapRef(ctx, ref, target, "")
		if errors.Is(err, gitrepo.ErrRefMoved) {
			// The branch exists, or another writer is creating it: it is
			// never moved by a repeated set-up.
			return nil
		}
		return err
	})
}

// createIntegrationBranch creates the integration branch of w in repo at
// base, the upstream base, when it does not exist, and never moves an
// existing one: the last step of the set-up of a ready workspace, for a
// caller that took it out of the set-up to run it only once the base
// resolved.
func createIntegrationBranch(ctx context.Context, repo CopyWriter, w workspace.Workspace, base string) error {
	ref := "refs/heads/" + IntegrationBranch(w)
	if err := (Setup{Copy: repo}).writeBranch(ctx, ref, base); err != nil {
		return fmt.Errorf("write the integration branch %s: %w", ref, err)
	}
	return nil
}

// setupError is the error of a set-up: the failure of each step that
// failed, in step order.
type setupError struct {
	slug   string
	failed []error
}

func (e *setupError) Error() string {
	texts := make([]string, len(e.failed))
	for i, err := range e.failed {
		texts[i] = err.Error()
	}
	return "carry-patch set-up of " + e.slug + ": " + strings.Join(texts, "; ")
}

func (e *setupError) Unwrap() []error { return e.failed }

// PostClone is the post-clone step of the workspaces in mode carry_patch,
// which the clone work runs on the fresh copy repo of w before w is ready
// (the hub registers it with workspace.Service.RegisterPostCloneStep): the
// set-up in its replacing form. It first removes the workspace's
// remembered-resolution entries: a fresh copy starts with an empty
// rr-cache, so the entries no longer describe what git holds. The registry
// holds one step per mode, so this one step does both. Its error, already
// clean of credentials, is logged by the clone work and does not keep the
// workspace from becoming ready.
func (c *Component) PostClone(ctx context.Context, w workspace.Workspace, repo *gitrepo.Repo) error {
	var forget error
	if c.d.Store == nil {
		forget = errNotBound
	} else {
		forget = c.d.Store.Tx(ctx, func(tx *sql.Tx) error { return RemoveRerereEntries(ctx, tx, w.Slug) })
	}
	return errors.Join(forget, Setup{Record: c.record(), Upstream: c, Copy: repo, Replace: true}.Run(ctx, w))
}

// EnsureSetup repeats the set-up on the ready carry-patch workspace w, for
// a rebuild or a sync that finds part of it missing: it fetches upstream,
// turns remembered resolutions on and creates the integration branch only
// when it does not exist, never moving an existing one. It returns nil
// when every step succeeded, else the error Setup.Run describes.
func (c *Component) EnsureSetup(ctx context.Context, w workspace.Workspace) error {
	return c.ensureSetup(ctx, w, false, 0)
}

// ensureSetup is EnsureSetup, with the integration branch left alone when
// leaveBranch is set, and each git operation of the upstream fetch held to
// opLimit as well when it is positive.
func (c *Component) ensureSetup(ctx context.Context, w workspace.Workspace, leaveBranch bool, opLimit time.Duration) error {
	unlock, err := c.lockSetup(ctx, w.Slug)
	if err != nil {
		return err
	}
	defer unlock()
	return c.runSetup(ctx, w, leaveBranch, opLimit)
}

// lockSetup waits until no other repeated set-up of the workspace slug
// runs, or until ctx ends, which is then its error, and returns the
// function that lets the next one run.
func (c *Component) lockSetup(ctx context.Context, slug string) (unlock func(), err error) {
	unlock, err = c.setups.lock(ctx, slug)
	if err != nil {
		return nil, fmt.Errorf("wait for the other set-up of %s to end: %w", slug, err)
	}
	return unlock, nil
}

// runSetup is ensureSetup for a caller that holds the workspace's
// set-up lock.
func (c *Component) runSetup(ctx context.Context, w workspace.Workspace, leaveBranch bool, opLimit time.Duration) error {
	var cp CopyWriter
	repo, err := c.d.Git.Open(ctx, c.d.RepoPath(w.Slug))
	if err != nil {
		cp = brokenCopy{err: err}
	} else {
		cp = repo
	}
	var upstream UpstreamFetcher = c
	if opLimit > 0 {
		upstream = limitedUpstream{c: c, opLimit: opLimit}
	}
	return Setup{Record: c.record(), Upstream: upstream, Copy: cp, LeaveBranch: leaveBranch}.Run(ctx, w)
}

// limitedUpstream is the component's upstream fetch with each of its git
// operations held to opLimit as well: the set-up a rebuild attempt repeats
// is part of the attempt, whose every git operation has the attempt's
// deadline.
type limitedUpstream struct {
	c       *Component
	opLimit time.Duration
}

func (u limitedUpstream) FetchUpstream(ctx context.Context, w workspace.Workspace) error {
	return u.c.fetchUpstream(ctx, w, u.opLimit)
}

func (u limitedUpstream) UpstreamBase(ctx context.Context, w workspace.Workspace) (string, error) {
	return u.c.UpstreamBase(ctx, w)
}

// record is the RecordWriter of the component's store.
func (c *Component) record() RecordWriter { return storeRecord{c} }

// storeRecord writes the set-up's field into the workspace records of the
// component's store.
type storeRecord struct{ c *Component }

// DefaultIntegrationBranch is one conditional write, so that a value
// stored meanwhile is kept; like the clone's own field writes, it leaves
// updated_at alone.
func (r storeRecord) DefaultIntegrationBranch(ctx context.Context, slug, branch string) error {
	if r.c.d.Store == nil {
		return errors.New("no workspace store")
	}
	_, err := r.c.d.Store.Main().ExecContext(ctx,
		`UPDATE workspaces SET integration_branch = ? WHERE slug = ? AND integration_branch IS NULL`, branch, slug)
	return err
}

// brokenCopy is the copy of a workspace that could not be opened: every
// write fails with the error of the open.
type brokenCopy struct{ err error }

func (b brokenCopy) SetConfig(context.Context, string, string) error { return b.err }

func (b brokenCopy) SetRef(context.Context, string, string) error { return b.err }

func (b brokenCopy) CompareAndSwapRef(context.Context, string, string, string) error { return b.err }
