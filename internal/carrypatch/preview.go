package carrypatch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The predicted outcomes of a patch in a preview.
const (
	previewSucceed  = "would_succeed"
	previewConflict = "would_conflict"
	previewSkip     = "would_skip"
)

// previewAnswer is the answer of a preview: the upstream base the
// prediction started from and one result per tested patch, in position
// order, [] when there is none.
type previewAnswer struct {
	UpstreamHeadSHA string          `json:"upstream_head_sha"`
	PatchResults    []previewResult `json:"patch_results"`
}

// previewResult is the predicted outcome of one patch: conflict_files is
// [] and conflict null unless the patch would conflict.
type previewResult struct {
	PatchID       string          `json:"patch_id"`
	BranchName    string          `json:"branch_name"`
	Position      int             `json:"position"`
	Status        string          `json:"status"`
	ConflictFiles []string        `json:"conflict_files"`
	Conflict      *ConflictReport `json:"conflict"`
}

// previewRoute answers GET /workspaces/:slug/rebuild-preview: what a
// rebuild would do with each patch, predicted against the upstream base as
// the last upstream fetch left it. Unlike the other read routes it reads
// the copy, so after the guard it refuses a workspace that is not ready as
// a write route does. A copy with no upstream base is 409
// upstream_unknown. Nothing is fetched and nothing is changed: no ref, no
// patch, no job.
func (c *Component) previewRoute(w http.ResponseWriter, r *http.Request) error {
	ws, err := c.Guard(r)
	if err != nil {
		return err
	}
	if err := workspace.RequireReady(ws); err != nil {
		return err
	}
	answer, err := c.preview(r.Context(), ws)
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, answer)
	return nil
}

// preview predicts the outcome of each patch of ws that a rebuild applies,
// the patches neither deleted, disabled nor merged upstream, in position
// order. The predicted state starts at the upstream base; the tip of each
// patch branch, read once before the first merge, is merged onto it in a
// trial merge, whatever the workspace's rebuild strategy: a missing branch
// would be skipped, a clean merge would succeed and its result becomes the
// state, and a conflict is reported with the rebuild's conflict report and
// leaves the state as it was. A git failure that is not a conflict is an
// error, never a result.
func (c *Component) preview(ctx context.Context, ws workspace.Workspace) (previewAnswer, error) {
	if c.d.Store == nil {
		return previewAnswer{}, errNotBound
	}
	base, err := c.UpstreamBase(ctx, ws)
	if errors.Is(err, ErrNoUpstreamBase) && ctx.Err() == nil {
		return previewAnswer{}, server.NewError("upstream_unknown", http.StatusConflict,
			fmt.Sprintf("workspace %q has no upstream base: its copy holds no upstream branch to start from; sync it first", ws.Slug))
	}
	if err != nil {
		return previewAnswer{}, err
	}
	live, err := ListLivePatches(ctx, c.d.Store.Main(), ws.Slug)
	if err != nil {
		return previewAnswer{}, fmt.Errorf("preview of %s: list the patches: %w", ws.Slug, err)
	}
	tested := slices.DeleteFunc(live, func(p Patch) bool {
		return p.Status == StatusDisabled || p.Status == StatusMergedUpstream
	})
	repo, err := c.d.Git.Open(ctx, c.d.RepoPath(ws.Slug))
	if err != nil {
		return previewAnswer{}, fmt.Errorf("preview of %s: %w", ws.Slug, err)
	}
	refs := make([]string, 0, len(tested))
	for _, p := range tested {
		refs = append(refs, headsPrefix+p.BranchName)
	}
	var tips map[string]string
	err = previewGit(ctx, func(ctx context.Context) (err error) {
		tips, err = repo.RefValues(ctx, refs)
		return err
	})
	if err != nil {
		return previewAnswer{}, fmt.Errorf("preview of %s: read the patch branches: %w", ws.Slug, err)
	}

	answer := previewAnswer{UpstreamHeadSHA: base, PatchResults: make([]previewResult, 0, len(tested))}
	state := base
	for _, p := range tested {
		res := previewResult{PatchID: p.ID, BranchName: p.BranchName, Status: previewSkip, ConflictFiles: []string{}}
		if p.Position != nil {
			res.Position = *p.Position
		}
		if tip, ok := tips[headsPrefix+p.BranchName]; ok {
			next, err := c.predict(ctx, repo, p, state, tip, &res)
			if err != nil {
				return previewAnswer{}, fmt.Errorf("preview of %s: %s: %w", ws.Slug, p.BranchName, err)
			}
			state = next
		}
		answer.PatchResults = append(answer.PatchResults, res)
	}
	return answer, nil
}

// predict merges tip, the tip of the branch of the patch p, onto the
// predicted state in a trial merge, records the outcome in res, and
// returns the state the next patch is tested against: on a clean merge a
// commit of the merged tree whose parents are state and tip, made by the
// landing identity and pointed at by no ref, or state itself when it holds
// tip already; on a conflict state unchanged.
func (c *Component) predict(ctx context.Context, repo *gitrepo.Repo, p Patch, state, tip string, res *previewResult) (string, error) {
	merged, err := c.trialMerge(ctx, repo, state, tip)
	if err != nil {
		return "", err
	}
	if len(merged.Conflicts) > 0 {
		paths := make([]string, 0, len(merged.Conflicts))
		for _, f := range merged.Conflicts {
			paths = append(paths, f.Path)
		}
		slices.Sort(paths)
		paths = slices.Compact(paths)
		var report ConflictReport
		err := previewGit(ctx, func(ctx context.Context) (err error) {
			report, err = c.BuildConflictReport(ctx, repo, PatchConflict{
				PatchID: p.ID, Source: tip, Target: state, Commit: tip, Merge: true, Paths: paths,
			})
			return err
		})
		if err != nil {
			return "", fmt.Errorf("report the conflict: %w", err)
		}
		res.Status, res.ConflictFiles, res.Conflict = previewConflict, paths, &report
		return state, nil
	}
	res.Status = previewSucceed
	if merged.Base == tip {
		// The state holds the tip already: the merge changes nothing.
		return state, nil
	}
	identity := c.committer()
	var next string
	err = previewGit(ctx, func(ctx context.Context) (err error) {
		next, err = repo.CreateCommit(ctx, gitrepo.CommitSpec{
			Tree: merged.Tree, Parents: []string{state, tip},
			Message: "Merge branch '" + p.BranchName + "'", Author: identity, Committer: identity,
		})
		return err
	})
	if err != nil {
		return "", fmt.Errorf("write the predicted state: %w", err)
	}
	return next, nil
}

// trialMerge merges theirs into ours in a trial merge of repo, which
// changes no ref and neither reads nor records a remembered resolution:
// git merge-tree needs no working tree and never consults rr-cache. It is
// Deps.TrialMerge when that is set. Its failure, a timeout included, is an
// error of the whole preview, never a conflict.
func (c *Component) trialMerge(ctx context.Context, repo *gitrepo.Repo, ours, theirs string) (gitrepo.TrialMergeResult, error) {
	var merged gitrepo.TrialMergeResult
	err := previewGit(ctx, func(ctx context.Context) (err error) {
		if c.d.TrialMerge != nil {
			merged, err = c.d.TrialMerge(ctx, repo, ours, theirs)
		} else {
			merged, err = repo.TrialMerge(ctx, ours, theirs)
		}
		return err
	})
	return merged, err
}

// previewGit runs op, one git operation of a preview, through RunGit,
// under the deadline of every carry-patch git operation and with no
// credential: a preview reads and writes the copy alone.
func previewGit(ctx context.Context, op func(ctx context.Context) error) error {
	return RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error { return op(ctx) })
}
