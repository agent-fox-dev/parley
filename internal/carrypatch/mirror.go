package carrypatch

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// EventPatchMirrorFailed is the event of a mirror of a patch branch to
// origin that failed.
const EventPatchMirrorFailed = "parley.patch.mirror_failed"

// reasonOriginMoved is the reason a mirror fails with when origin's branch
// is no longer at the head the hub recorded for it, the push's lease.
const reasonOriginMoved = "origin moved"

// mirrorOn reports whether the effective settings ask the hub to mirror
// its patch branches to origin: the hub is their authority and
// push_patches_to_origin is on. A missing or mistyped value reads as the
// key's default, hub and off.
func mirrorOn(settings map[string]any) bool {
	source, _ := settings[patchBranchSourceKey].(string)
	on, _ := settings[pushPatchesToOriginKey].(bool)
	return source != "origin" && on
}

// mirror pushes the branch of each of patches, in order, to the origin of
// the workspace w: the head the branch has in the hub's copy when the
// mirror starts, which may be newer than the request's, as a forced push
// under the lease of the head the hub last recorded for origin's branch, or
// that origin has no such branch when it recorded none, so that it never
// overwrites what another writer put there. Each push runs within the
// forward limit. One that succeeds records the pushed head as origin's;
// one that fails is reported with one parley.patch.mirror_failed event,
// and the mirror goes on with the next branch. A branch the copy no longer
// has is skipped. The origin credential is read once, at the start, and so
// are the heads, in one git.
//
// A mirror cut off by ctx, which ends when the hub stops, reports nothing
// and pushes no further branch: the stop is not origin's failure.
func (c *Component) mirror(ctx context.Context, w workspace.Workspace, patches []Patch) {
	log := c.logger().With("workspace", w.Slug)
	cred, credErr := c.originCredential(ctx, w.Slug)
	if credErr != nil {
		log.Warn("mirror to origin: the origin credential cannot be read", "error", c.scrub(credErr.Error()))
	}
	repo, heads, headsErr := c.mirrorHeads(ctx, w.Slug, patches)
	if headsErr != nil && ctx.Err() == nil {
		log.Warn("mirror to origin: the heads of the hub's copy cannot be read", "error", headsErr)
	}
	for _, p := range patches {
		if ctx.Err() != nil {
			log.Info("mirror to origin cut off", "branch", p.BranchName, "error", ctx.Err())
			return
		}
		var reason string
		head, found := heads[headsPrefix+p.BranchName]
		switch {
		case headsErr != nil:
			reason = reasonPushFailed
		case !found:
			log.Debug("mirror to origin skipped a branch the hub no longer has", "branch", p.BranchName)
			continue
		case credErr != nil:
			reason = msgNoOriginCredential
		case w.GitURL == nil || *w.GitURL == "":
			log.Warn("mirror to origin failed: the workspace has no origin", "branch", p.BranchName)
			reason = reasonPushFailed
		default:
			lease := heads[originPrefix+p.BranchName]
			reason = c.mirrorBranch(ctx, log, repo, *w.GitURL, p.BranchName, head, lease, cred)
			if reason == "" {
				continue
			}
		}
		if ctx.Err() != nil {
			log.Info("mirror to origin cut off", "branch", p.BranchName, "error", ctx.Err())
			return
		}
		c.mirrorFailed(ctx, log, w.Slug, p, c.scrub(reason, cred.Username, cred.Password))
	}
}

// originPrefix is where the hub's copy records origin's heads.
const originPrefix = "refs/remotes/origin/"

// originCredential is the origin credential of the workspace slug, read
// from its secrets now.
func (c *Component) originCredential(ctx context.Context, slug string) (gitrepo.Credential, error) {
	if c.d.Secrets == nil {
		return gitrepo.Credential{}, errors.New("no secret reader to read the origin credential with")
	}
	return workspace.ResolveOriginCredential(ctx, c.d.Secrets, slug)
}

// mirrorHeads opens the hub's copy of the workspace slug and reads, in one
// git, the head of each branch of patches and the origin head it records
// for it, by full ref name; a ref that does not exist is absent.
func (c *Component) mirrorHeads(ctx context.Context, slug string, patches []Patch) (*gitrepo.Repo, map[string]string, error) {
	if c.d.RepoPath == nil {
		return nil, nil, errNotBound
	}
	repo, err := c.d.Git.Open(ctx, c.d.RepoPath(slug))
	if err != nil {
		return nil, nil, err
	}
	refs := make([]string, 0, 2*len(patches))
	for _, p := range patches {
		refs = append(refs, headsPrefix+p.BranchName, originPrefix+p.BranchName)
	}
	var heads map[string]string
	err = RunGit(ctx, refLimit, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		heads, err = repo.RefValues(ctx, refs)
		return err
	})
	return repo, heads, err
}

// mirrorBranch pushes head from repo, the hub's copy, to branch on origin
// at url with cred under lease, "" for none, records it as origin's head
// once origin took it, unless that is the lease already, and returns ""
// then, else the reason it failed for.
func (c *Component) mirrorBranch(ctx context.Context, log *slog.Logger, repo *gitrepo.Repo, url, branch, head, lease string, cred gitrepo.Credential) string {
	log = log.With("branch", branch)
	req := gitrepo.PushRequest{URL: url, Credential: cred, Commit: head, Branch: branch, Force: true, Lease: lease}
	err := RunGit(ctx, c.ForwardLimit(), cred, func(ctx context.Context, _ time.Duration) error {
		_, err := repo.Push(ctx, req)
		return err
	})
	if err != nil {
		log.Info("mirror to origin failed", "error", c.scrub(err.Error(), cred.Username, cred.Password))
		return mirrorReason(ctx, err)
	}
	if head == lease {
		return ""
	}
	// Origin holds the head now: the record is written even when the
	// caller has gone meanwhile, and one that fails leaves the push
	// standing, with the next mirror's lease meeting the branch where the
	// hub did not see it.
	err = RunGit(context.WithoutCancel(ctx), refLimit, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		return repo.SetOriginHead(ctx, branch, head)
	})
	if err != nil {
		log.Warn("mirror to origin succeeded and origin's head could not be recorded", "error", err)
	}
	return ""
}

// mirrorReason is the reason a mirror whose push failed with err is
// reported with: origin moved for a lease origin's branch no longer
// matched, else the reason a forward fails with.
func mirrorReason(ctx context.Context, err error) string {
	if errors.Is(err, gitrepo.ErrOriginMoved) && !errors.Is(err, ErrGitTimeout) {
		return reasonOriginMoved
	}
	return forwardReason(ctx, err)
}

// mirrorFailed writes parley.patch.mirror_failed for the patch p, whose
// branch the mirror could not push for reason, already scrubbed, in a store
// transaction of its own. An event that cannot be written is logged, and
// the mirror goes on.
func (c *Component) mirrorFailed(ctx context.Context, log *slog.Logger, slug string, p Patch, reason string) {
	log = log.With("branch", p.BranchName, "reason", reason)
	if c.d.Store == nil || c.d.Events == nil {
		log.Warn("mirror to origin failed and cannot be reported: no event log")
		return
	}
	in := event.Input{
		Type: EventPatchMirrorFailed, Workspace: &slug, ActorType: event.ActorSystem,
		ResourceType: "patch", ResourceID: p.ID,
		Metadata: map[string]any{"branch_name": p.BranchName, "error": reason},
	}
	err := c.d.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := c.d.Events.Emit(ctx, tx, in)
		return err
	})
	if err != nil {
		log.Warn("mirror to origin failed and could not be reported", "error", err)
	}
}
