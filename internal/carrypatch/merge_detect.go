package carrypatch

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

// squashMergeDetectionKey is the workspace setting that chooses the tests
// a sync finds a patch upstream merged by: ancestry_only, content_based or
// both.
const squashMergeDetectionKey = "squash_merge_detection"

// prWindow is how many of the newest upstream commits a sync examines for
// a patch's pull request number when it cannot tell which commits are new:
// on the first sync, and after upstream was force-pushed.
const prWindow = 50

// pullNumber finds the pull request number in a patch's upstream_pr_url.
var pullNumber = regexp.MustCompile(`/pull/([0-9]+)`)

// mergeTests are the tests the effective squash_merge_detection of the
// sync's workspace selects; a missing key or registry reads as the
// setting's default, both.
type mergeTests struct {
	ancestry, content bool
}

func (s *syncPass) mergeTests() mergeTests {
	mode := ""
	if s.c.d.Settings != nil {
		w := s.run.Workspace
		mode, _ = s.c.d.Settings.EffectiveSettings(w.Settings, w.DefaultBranch)[squashMergeDetectionKey].(string)
	}
	switch mode {
	case "ancestry_only":
		return mergeTests{ancestry: true}
	case "content_based":
		return mergeTests{content: true}
	}
	return mergeTests{ancestry: true, content: true}
}

// detectMerges finds the active and conflict patches, in position order,
// that upstream has merged, marks each merged_upstream and lists its branch
// in patches_merged. It runs only when something it looks at changed:
// upstream advanced, or the origin refresh created or moved a patch
// branch; otherwise nothing a test reads is new and patches_merged stays
// empty. A patch whose branch the hub lacks has nothing to compare and is
// left out, as are disabled, merged and deleted patches, so that a sync
// never changes their status. Any git or store failure stops the sync.
func (s *syncPass) detectMerges(ctx context.Context) error {
	if !s.report.UpstreamAdvanced && len(s.moved) == 0 {
		return nil
	}
	slug := s.run.Workspace.Slug
	live, err := ListLivePatches(ctx, s.c.d.Store.Main(), slug)
	if err != nil {
		return fmt.Errorf("list the patches to examine for upstream merges: %w", err)
	}
	var candidates []Patch
	var refs []string
	for _, p := range live {
		if p.Status == StatusActive || p.Status == StatusConflict {
			candidates = append(candidates, p)
			refs = append(refs, headsPrefix+p.BranchName)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	var tips map[string]string
	if err := RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) (err error) {
		tips, err = s.repo.RefValues(ctx, refs)
		return err
	}); err != nil {
		return fmt.Errorf("read the patch branches to examine for upstream merges: %w", err)
	}
	d := mergeDetection{s: s, tests: s.mergeTests()}
	for _, p := range candidates {
		tip, exists := tips[headsPrefix+p.BranchName]
		if !exists {
			continue
		}
		merged, err := d.merged(ctx, p, tip)
		if err != nil {
			return fmt.Errorf("examine the patch branch %q for an upstream merge: %w", p.BranchName, err)
		}
		if !merged {
			continue
		}
		marked, err := s.markMerged(ctx, p.ID)
		if err != nil {
			return fmt.Errorf("mark the patch branch %q merged upstream: %w", p.BranchName, err)
		}
		if marked {
			s.report.PatchesMerged = append(s.report.PatchesMerged, p.BranchName)
		}
	}
	return nil
}

// mergeDetection is the merge detection of one sync: the tests it runs and
// the upstream subjects the pull request number test reads, read once, on
// first use.
type mergeDetection struct {
	s        *syncPass
	tests    mergeTests
	subjects []string
	read     bool
}

// merged reports whether upstream merged the patch p whose branch is at
// tip, by the first of the selected tests that finds it: the tip is the
// upstream base or its ancestor; the patch's pull request number ends the
// subject of an examined upstream commit; every commit of the branch that
// is not a merge and not in the base has an equivalent in upstream.
func (d *mergeDetection) merged(ctx context.Context, p Patch, tip string) (bool, error) {
	base := d.s.base
	if d.tests.ancestry {
		var ancestor bool
		if err := RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) (err error) {
			ancestor, err = d.s.repo.IsAncestor(ctx, tip, base)
			return err
		}); err != nil || ancestor {
			return ancestor, err
		}
	}
	if !d.tests.content {
		return false, nil
	}
	if n := prNumber(p); n != "" {
		subjects, err := d.upstreamSubjects(ctx)
		if err != nil {
			return false, err
		}
		for _, subject := range subjects {
			if strings.HasSuffix(strings.TrimSpace(subject), "(#"+n+")") {
				return true, nil
			}
		}
	}
	var commits []gitrepo.CherryCommit
	if err := RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) (err error) {
		commits, err = d.s.repo.Cherry(ctx, base, tip)
		return err
	}); err != nil {
		return false, err
	}
	for _, c := range commits {
		if !c.Equivalent {
			return false, nil
		}
	}
	return len(commits) > 0, nil
}

// prNumber is the pull request number of p's upstream_pr_url, "" when it
// names none.
func prNumber(p Patch) string {
	if p.UpstreamPRURL == nil {
		return ""
	}
	if m := pullNumber.FindStringSubmatch(*p.UpstreamPRURL); m != nil {
		return m[1]
	}
	return ""
}

// upstreamSubjects are the subjects of the upstream commits this sync
// examines for pull request numbers: those the new base reaches and the
// stored upstream head does not, or, on the first sync and after a
// force-push, when there is no stored head to start from, the prWindow
// newest of the new base. A sync that saw upstream stand still examines
// none.
func (d *mergeDetection) upstreamSubjects(ctx context.Context) ([]string, error) {
	if d.read {
		return d.subjects, nil
	}
	stored := d.s.run.Workspace.UpstreamHeadSHA
	from, limit := "", prWindow
	switch {
	case !d.s.report.UpstreamAdvanced:
		d.read = true
		return nil, nil
	case stored != nil && *stored != "" && !d.s.report.ForcePushDetected:
		from, limit = *stored, 0
	}
	if err := RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) (err error) {
		d.subjects, err = d.s.repo.Subjects(ctx, from, d.s.base, limit)
		return err
	}); err != nil {
		return nil, fmt.Errorf("read the subjects of the new upstream commits: %w", err)
	}
	d.read = true
	return d.subjects, nil
}

// markMerged marks the patch id merged_upstream in one store transaction
// with its event: the status, no conflict files, no origin fields, since a
// merged patch's branch is no longer compared with origin, and
// parley.patch.update with the sync's caller as actor. It reports whether
// the patch was marked: one no longer active or conflict since it was
// listed is left as it is.
func (s *syncPass) markMerged(ctx context.Context, id string) (bool, error) {
	slug := s.run.Workspace.Slug
	var marked bool
	err := s.c.d.Store.Tx(ctx, func(tx *sql.Tx) error {
		marked = false
		p, found, err := ReadPatch(ctx, tx, slug, id)
		if err != nil {
			return err
		}
		if !found || (p.Status != StatusActive && p.Status != StatusConflict) {
			return nil
		}
		next := p
		next.Status, next.ConflictFiles = StatusMergedUpstream, []string{}
		next.UpdatedAt = s.c.now().UTC().Truncate(time.Second)
		if err := writeFields(ctx, tx, next); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE patches SET origin_sync_state = NULL, origin_sha = NULL, origin_synced_at = NULL
WHERE workspace_slug = ? AND id = ?`, slug, id); err != nil {
			return fmt.Errorf("clear the origin fields: %w", err)
		}
		if _, err := s.c.d.Events.Emit(ctx, tx, event.Input{
			Type: EventPatchUpdate, Workspace: &slug, ActorType: s.by.typ, ActorID: s.by.id,
			ResourceType: "patch", ResourceID: p.ID,
			Metadata: map[string]any{"branch_name": p.BranchName, "changed": []string{"status"},
				"from": p.Status, "to": StatusMergedUpstream, "reason": "merged_upstream"},
		}); err != nil {
			return fmt.Errorf("emit %s: %w", EventPatchUpdate, err)
		}
		marked = true
		return nil
	})
	return marked, err
}
