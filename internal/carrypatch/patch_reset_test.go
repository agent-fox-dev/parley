package carrypatch_test

import (
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The refs of patch/a in a reset case's copy.
const (
	headA   = "refs/heads/patch/a"
	backupA = carrypatch.ReplacedRefPrefix + "patch/a"
)

// commitOn writes a commit with the message msg and an empty tree on top of
// parent into the repository dir and returns its id.
func commitOn(t *testing.T, dir, parent, msg string) string {
	t.Helper()
	tree := gitrepotest.Git(t, dir, "hash-object", "-t", "tree", "-w", "--stdin")
	return gitrepotest.Git(t, dir, "commit-tree", tree, "-p", parent, "-m", msg)
}

// diverge gives the copy's patch/a a commit on top of origin's tip and
// origin a different one, and returns the copy's new tip.
func (rc *resetCase) diverge(t *testing.T) string {
	t.Helper()
	hub := commitOn(t, rc.copy, refIn(rc.copy, headA), "on the hub")
	gitrepotest.Git(t, rc.copy, "update-ref", headA, hub)
	rc.origin.Commit("patch/a", "on origin")
	return hub
}

// resetState is a state of patch/a in the hub's copy and on origin that a
// reset is tried from, with the action it takes and the backup ref it
// leaves.
type resetState struct {
	name string
	// prepare brings the copy and origin into the state.
	prepare func(t *testing.T, rc *resetCase)
	action  string
	// backup is the commit the backup ref is at after the reset: "old"
	// for the copy's tip before it, "K" for the commit an earlier replace
	// saved, "" for no backup ref.
	backup string
}

// resetStates are the states a reset is tried from.
var resetStates = []resetState{
	{"the hub has no branch", func(t *testing.T, rc *resetCase) {
		gitrepotest.Git(t, rc.copy, "update-ref", "-d", headA)
	}, "created", ""},
	{"the tips are equal", func(*testing.T, *resetCase) {}, "none", ""},
	{"origin is ahead by 2 commits", func(_ *testing.T, rc *resetCase) {
		rc.origin.Commit("patch/a", "one")
		rc.origin.Commit("patch/a", "two")
	}, "fast_forwarded", ""},
	{"both sides have a commit the other lacks", func(t *testing.T, rc *resetCase) { rc.diverge(t) }, "replaced", "old"},
	{"the hub is ahead of origin by 1 commit", func(t *testing.T, rc *resetCase) {
		gitrepotest.Git(t, rc.copy, "update-ref", headA, commitOn(t, rc.copy, refIn(rc.copy, headA), "on the hub"))
	}, "replaced", "old"},
	{"diverged with an earlier backup ref", func(t *testing.T, rc *resetCase) {
		gitrepotest.Git(t, rc.copy, "update-ref", backupA, commitIn(t, rc.copy, "K"))
		rc.diverge(t)
	}, "replaced", "old"},
	{"the tips are equal with an earlier backup ref", func(t *testing.T, rc *resetCase) {
		gitrepotest.Git(t, rc.copy, "update-ref", backupA, commitIn(t, rc.copy, "K"))
	}, "none", "K"},
}

func TestResetToOrigin_AnswersPatchWithOutcome(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	members := append(slices.Clone(patchMembers), "action", "rebuild_triggered")
	slices.Sort(members)
	for _, status := range []string{carrypatch.StatusActive, carrypatch.StatusConflict, carrypatch.StatusDisabled} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			rc := h.resetCarry(t, "reset-"+status, nil)
			h.alter(t, rc.slug, rc.patch.ID, func(p *carrypatch.Patch) { p.Status = status })
			tip := rc.origin.Commit("patch/a", "ahead")
			otherBefore := refIn(rc.copy, "refs/remotes/origin/other")
			rc.origin.Commit("other", "ahead too")

			a := h.resetToOrigin(t, rc.slug, rc.patch.ID)
			if a.status != http.StatusOK {
				t.Fatalf("reset = %d %s, want 200", a.status, a.raw)
			}
			if got := slices.Sorted(maps.Keys(a.patch)); !slices.Equal(got, members) {
				t.Errorf("reset answered the members %v, want %v", got, members)
			}
			if _, isBool := a.patch["rebuild_triggered"].(bool); !isBool || a.patch["id"] != rc.patch.ID ||
				a.patch["action"] != "fast_forwarded" || a.patch["status"] != status {
				t.Errorf("reset = %s, want the patch %s, status %s, action fast_forwarded and rebuild_triggered a boolean",
					a.raw, rc.patch.ID, status)
			}
			if head, tracking := refIn(rc.copy, headA), refIn(rc.copy, "refs/remotes/origin/patch/a"); head != tip || tracking != tip {
				t.Errorf("the copy's patch/a = %s and its origin head %s, want origin's tip %s", head, tracking, tip)
			}
			if other := refIn(rc.copy, "refs/remotes/origin/other"); other != otherBefore {
				t.Errorf("the copy's origin head of other = %s, want it left at %s: only patch/a is fetched", other, otherBefore)
			}
			if n := h.origins.readsOf(rc.slug); n != 1 {
				t.Errorf("the origin credential was read %d times, want once", n)
			}
		})
	}
	t.Run("git sees no credential and no branch name as an option", func(t *testing.T) {
		t.Parallel()
		rc := h.resetCarry(t, "reset-args", nil)
		rc.origin.Commit("patch/a", "ahead")
		if a := h.resetToOrigin(t, rc.slug, rc.patch.ID); a.status != http.StatusOK {
			t.Fatalf("reset = %d %s, want 200", a.status, a.raw)
		}
		if arg := h.gitArgHolding(originToken); arg != "" {
			t.Errorf("a git argument holds the origin token: %q", arg)
		}
		h.argsMu.Lock()
		defer h.argsMu.Unlock()
		for _, args := range h.gitArgs {
			if i := slices.Index(args, "--"); slices.Contains(args, "patch/a") || (i < 0 && slices.Contains(args, rc.origin.URL())) {
				t.Errorf("git ran with the branch name bare or the URL before the end of its options: %q", args)
			}
		}
	})
}

func TestResetToOrigin_Actions(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	for i, s := range resetStates {
		for _, source := range []string{"hub", "origin"} {
			for _, policy := range []string{"replace", "report"} {
				t.Run(fmt.Sprintf("%s/%s/%s", s.name, source, policy), func(t *testing.T) {
					t.Parallel()
					rc := h.resetCarry(t, fmt.Sprintf("act-%d-%s-%s", i, source, policy), withSettings(source, policy))
					s.prepare(t, rc)
					old, earlier := refIn(rc.copy, headA), refIn(rc.copy, backupA)
					refsBefore := gitrepotest.Refs(t, rc.copy, "")

					a := h.resetToOrigin(t, rc.slug, rc.patch.ID)
					if a.status != http.StatusOK || a.patch["action"] != s.action {
						t.Fatalf("reset = %d %s, want 200 with action %s", a.status, a.raw, s.action)
					}
					if head, tip := refIn(rc.copy, headA), rc.origin.Head("patch/a"); head != tip {
						t.Errorf("the copy's patch/a = %s, want origin's tip %s", head, tip)
					}
					want := map[string]string{"": "", "old": old, "K": earlier}[s.backup]
					if backup := refIn(rc.copy, backupA); backup != want {
						t.Errorf("the backup ref = %q, want %q", backup, want)
					}
					if replaced, ok := a.patch["replaced_sha"]; ok != (want != "") || (ok && replaced != want) {
						t.Errorf("the answer's replaced_sha = %v (present %v), want %q", replaced, ok, want)
					}
					if s.action == "none" {
						if refs := gitrepotest.Refs(t, rc.copy, ""); !maps.Equal(refs, refsBefore) {
							t.Errorf("a reset with nothing to do changed the copy's refs to %v, want %v", refs, refsBefore)
						}
					}
					if old != "" {
						if _, err := gitrepotest.Run(rc.copy, "", "cat-file", "-e", old+"^{commit}"); err != nil {
							t.Errorf("the hub's old tip %s is gone from the copy: %v", old, err)
						}
					}
				})
			}
		}
	}
}

func TestResetToOrigin_RecordsOriginState(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	oldSHA, synced := strings.Repeat("1", 40), at(9, 30)
	cases := []struct {
		name, source string
		stored       func(p *carrypatch.Patch)
		prepare      func(t *testing.T, rc *resetCase)
		action       string
		// recorded tells the three fields set by the reset from those left
		// as stored.
		recorded bool
	}{
		{"origin, stored diverged, tips diverged", "origin", func(p *carrypatch.Patch) {
			p.OriginSyncState, p.OriginSHA, p.OriginSyncedAt = new("diverged"), &oldSHA, &synced
		}, func(t *testing.T, rc *resetCase) { rc.diverge(t) }, "replaced", true},
		{"origin, stored missing on origin, branch created", "origin", func(p *carrypatch.Patch) {
			p.OriginSyncState, p.OriginSyncedAt = new("missing_on_origin"), &synced
		}, func(t *testing.T, rc *resetCase) { gitrepotest.Git(t, rc.copy, "update-ref", "-d", headA) }, "created", true},
		{"origin, nothing to do", "origin", func(p *carrypatch.Patch) {
			p.OriginSyncState, p.OriginSHA, p.OriginSyncedAt = new("in_sync"), &oldSHA, &synced
		}, func(*testing.T, *resetCase) {}, "none", true},
		{"hub, fast-forward", "hub", func(*carrypatch.Patch) {}, func(_ *testing.T, rc *resetCase) {
			rc.origin.Commit("patch/a", "ahead")
		}, "fast_forwarded", false},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rc := h.resetCarry(t, fmt.Sprintf("state-%d", i), withSettings(c.source, ""))
			h.alter(t, rc.slug, rc.patch.ID, c.stored)
			c.prepare(t, rc)
			before := h.get(t, "/workspaces/"+rc.slug+"/patches/"+rc.patch.ID)

			a := h.resetToOrigin(t, rc.slug, rc.patch.ID)
			if a.status != http.StatusOK || a.patch["action"] != c.action {
				t.Fatalf("reset = %d %s, want 200 with action %s", a.status, a.raw, c.action)
			}
			want := map[string]any{"origin_sync_state": "in_sync", "origin_sha": rc.origin.Head("patch/a"), "origin_synced_at": h.stamp()}
			if !c.recorded {
				for k := range want {
					want[k] = before.patch[k]
				}
			}
			got := h.get(t, "/workspaces/"+rc.slug+"/patches/"+rc.patch.ID)
			for k, v := range want {
				if a.patch[k] != v || got.patch[k] != v {
					t.Errorf("%s = %v in the answer and %v in GET, want %v", k, a.patch[k], got.patch[k], v)
				}
			}
		})
	}
}

func TestResetToOrigin_EmitsEvents(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	for _, action := range []string{"none", "created", "fast_forwarded", "replaced"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			i := slices.IndexFunc(resetStates, func(s resetState) bool { return s.action == action })
			rc := h.resetCarry(t, "events-"+strings.ReplaceAll(action, "_", "-"), nil)
			resetStates[i].prepare(t, rc)
			local := refIn(rc.copy, headA)

			if a := h.resetToOrigin(t, rc.slug, rc.patch.ID); a.status != http.StatusOK || a.patch["action"] != action {
				t.Fatalf("reset = %d %s, want 200 with action %s", a.status, a.raw, action)
			}
			tip := rc.origin.Head("patch/a")
			var localSHA, replacedSHA any // null unless set below
			if local != "" {
				localSHA = local
			}
			if action == "replaced" {
				replacedSHA = local
			}
			e := h.patchEvents(t, rc.slug, carrypatch.EventPatchReset)
			if len(e) != 1 {
				t.Fatalf("the reset wrote %d parley.patch.reset events, want 1", len(e))
			}
			wantMeta := map[string]any{"branch_name": "patch/a", "action": action, "local_sha": localSHA, "origin_sha": tip, "replaced_sha": replacedSHA}
			if e[0].ResourceType != "patch" || e[0].ResourceID != rc.patch.ID || str(e[0].ActorID) != h.owner ||
				!reflect.DeepEqual(e[0].Metadata, wantMeta) {
				t.Errorf("parley.patch.reset = %+v (actor %s), want resource patch %s, actor %s and metadata %v",
					e[0], str(e[0].ActorID), rc.patch.ID, h.owner, wantMeta)
			}
			p := h.patchEvents(t, rc.slug, carrypatch.EventPatchReplace)
			if action != "replaced" {
				if len(p) != 0 {
					t.Errorf("a reset that did not replace wrote %d parley.patch.replace events, want none", len(p))
				}
				return
			}
			want := map[string]any{"branch_name": "patch/a", "replaced_sha": local, "origin_sha": tip, "trigger": "reset_to_origin"}
			if len(p) != 1 || !reflect.DeepEqual(p[0].Metadata, want) || p[0].ResourceID != rc.patch.ID {
				t.Errorf("parley.patch.replace = %+v, want one for %s with metadata %v", p, rc.patch.ID, want)
			}
		})
	}
}

func TestResetToOrigin_RefusesByPatchStatus(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	deleted := at(10, 0)
	cases := map[string]func(p *carrypatch.Patch){
		carrypatch.StatusMergedUpstream: func(p *carrypatch.Patch) { p.Status = carrypatch.StatusMergedUpstream },
		carrypatch.StatusDeleted: func(p *carrypatch.Patch) {
			p.Status, p.Position, p.DeletedAt = carrypatch.StatusDeleted, nil, &deleted
		},
	}
	for status, edit := range cases {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			rc := h.resetCarry(t, "refused-"+strings.ReplaceAll(status, "_", "-"), nil)
			h.alter(t, rc.slug, rc.patch.ID, edit)
			rc.origin.Commit("patch/a", "ahead")
			refs, requests := gitrepotest.Refs(t, rc.copy, ""), rc.origin.Requests()

			a := h.resetToOrigin(t, rc.slug, rc.patch.ID)
			if a.status != http.StatusConflict || a.Error.ErrorType != "patch_status_invalid" {
				t.Errorf("reset of a %s patch = %d %s, want 409 patch_status_invalid", status, a.status, a.raw)
			}
			if n, reads := rc.origin.Requests()-requests, h.origins.readsOf(rc.slug); n != 0 || reads != 0 {
				t.Errorf("origin got %d requests and the credential was read %d times, want neither", n, reads)
			}
			if after := gitrepotest.Refs(t, rc.copy, ""); !maps.Equal(after, refs) {
				t.Errorf("the copy's refs = %v, want them as before %v", after, refs)
			}
			if e := h.patchEvents(t, rc.slug, ""); len(e) != 0 {
				t.Errorf("the refusal wrote %d parley.patch.* events, want none", len(e))
			}
		})
	}
}

func TestResetToOrigin_MissingOnOrigin(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	rc := h.resetCarry(t, "missing", withSettings("origin", ""))
	h.alter(t, rc.slug, rc.patch.ID, func(p *carrypatch.Patch) {
		p.OriginSyncState, p.OriginSHA = new("in_sync"), new(refIn(rc.copy, headA))
	})
	gitrepotest.Git(t, rc.origin.Dir(), "update-ref", "-d", headA)
	gitrepotest.Git(t, rc.origin.Dir(), "update-server-info")
	path := "/workspaces/" + rc.slug + "/patches/" + rc.patch.ID
	before, refs := h.get(t, path), gitrepotest.Refs(t, rc.copy, "")

	a := h.resetToOrigin(t, rc.slug, rc.patch.ID)
	if a.status != http.StatusConflict || a.Error.ErrorType != "missing_on_origin" {
		t.Errorf("reset of a branch origin lacks = %d %s, want 409 missing_on_origin", a.status, a.raw)
	}
	if after := gitrepotest.Refs(t, rc.copy, ""); !maps.Equal(after, refs) || refIn(rc.copy, backupA) != "" {
		t.Errorf("the copy's refs = %v, want them as before %v, without a backup ref", after, refs)
	}
	if got := h.get(t, path); !reflect.DeepEqual(got.patch, before.patch) {
		t.Errorf("the patch is %s, want it as before %s", got.raw, before.raw)
	}
	if e := h.patchEvents(t, rc.slug, ""); len(e) != 0 {
		t.Errorf("the refusal wrote %d parley.patch.* events, want none", len(e))
	}
	if n := h.rebuilds.calls(); n != 0 {
		t.Errorf("the refusal asked for %d rebuilds, want none", n)
	}
}

func TestResetToOrigin_FetchFailures(t *testing.T) {
	t.Parallel()
	if carrypatch.DefaultBranchFetchDeadline != 2*time.Minute {
		t.Errorf("DefaultBranchFetchDeadline = %v, want 2 minutes", carrypatch.DefaultBranchFetchDeadline)
	}
	// The deadline leaves git time to start and reach origin also on a
	// loaded machine; the bound on the answer leaves the hub more.
	const deadline = time.Second
	h := startPatchHub(t, func(d *carrypatch.Deps) { d.BranchFetchDeadline = deadline })
	const wrongToken = "wrong-token-5e2b"
	cases := []struct {
		name string
		// gitURL, when set, is the git_url the workspace has instead of
		// url, its origin's, with the number of requests the server there
		// still holds when it can tell.
		gitURL func(t *testing.T, url string) (string, *atomic.Int32)
		// token and err, when one is set, are what the credential resolver
		// gives instead of originToken.
		token string
		err   error
	}{
		{name: "origin is stopped", gitURL: func(t *testing.T, _ string) (string, *atomic.Int32) {
			o := tokenOrigin(t)
			o.Close()
			return o.URL(), nil
		}},
		{name: "origin refuses the token the resolver gives", gitURL: func(_ *testing.T, url string) (string, *atomic.Int32) {
			return strings.Replace(url, "https://", "https://user:s3cret@", 1), nil
		}, token: wrongToken},
		{name: "the credential cannot be resolved", err: errors.New("the secret store cannot be read")},
		{name: "origin never answers", gitURL: func(t *testing.T, _ string) (string, *atomic.Int32) {
			// Takes each request and never answers it: a request ends only
			// when the git process holding its connection is gone.
			var open atomic.Int32
			srv := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				open.Add(1)
				defer open.Add(-1)
				<-r.Context().Done()
			}))
			t.Cleanup(srv.Close)
			ca := filepath.Join(t.TempDir(), "ca.pem")
			if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
				t.Fatal(err)
			}
			h.trust(t, ca)
			return srv.URL + "/origin.git", &open
		}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var open *atomic.Int32
			rc := h.resetCarry(t, fmt.Sprintf("fails-%d", i), func(w *workspace.Workspace) {
				withSettings("origin", "")(w)
				if c.gitURL != nil {
					url, held := c.gitURL(t, *w.GitURL)
					w.GitURL, open = &url, held
				}
			})
			if c.token != "" || c.err != nil {
				h.origins.set(rc.slug, c.token, c.err)
			}
			rc.origin.Commit("patch/a", "ahead")
			path := "/workspaces/" + rc.slug + "/patches/" + rc.patch.ID
			before, refs := h.get(t, path), gitrepotest.Refs(t, rc.copy, "")

			start := time.Now()
			a := h.resetToOrigin(t, rc.slug, rc.patch.ID)
			if d := time.Since(start); d > deadline+5*time.Second {
				t.Errorf("the reset answered after %v, want within 5 s of the fetch deadline %v", d, deadline)
			}
			if a.status != http.StatusBadGateway || a.Error.ErrorType != "origin_fetch_failed" {
				t.Errorf("reset = %d %s, want 502 origin_fetch_failed", a.status, a.raw)
			}
			for _, secret := range []string{"s3cret", originToken, wrongToken} {
				if strings.Contains(string(a.raw), secret) {
					t.Errorf("the answer shows the credential %q: %s", secret, a.raw)
				}
			}
			if after := gitrepotest.Refs(t, rc.copy, ""); !maps.Equal(after, refs) {
				t.Errorf("the copy's refs = %v, want them as before %v", after, refs)
			}
			if got := h.get(t, path); !reflect.DeepEqual(got.patch, before.patch) {
				t.Errorf("the patch is %s, want it as before %s", got.raw, before.raw)
			}
			if e := h.patchEvents(t, rc.slug, ""); len(e) != 0 {
				t.Errorf("the refusal wrote %d parley.patch.* events, want none", len(e))
			}
			for wait := time.Now(); open != nil && open.Load() != 0; time.Sleep(10 * time.Millisecond) {
				if time.Since(wait) > 5*time.Second {
					t.Fatal("a request to origin is still open 5 s after the answer: the fetch was not stopped")
				}
			}
		})
	}
	if n := h.rebuilds.calls(); n != 0 {
		t.Errorf("the refusals asked for %d rebuilds, want none", n)
	}
}

func TestResetToOrigin_LostRaceIsRefMoved(t *testing.T) {
	t.Parallel()
	// moves maps a workspace to the commit the other writer puts its
	// branch at between the reset's read and its compare-and-swap.
	var moves sync.Map
	var h *patchHub
	h = startPatchHub(t, func(d *carrypatch.Deps) {
		d.BeforeRefSwap = func(slug, branch string) {
			if w, ok := moves.Load(slug); ok {
				if _, err := gitrepotest.Run(h.repoPath(slug), "", "update-ref", "refs/heads/"+branch, w.(string)); err != nil {
					t.Errorf("the other writer could not move %s of %s: %v", branch, slug, err)
				}
			}
		}
	})
	cases := []struct {
		name    string
		prepare func(t *testing.T, rc *resetCase)
		// keepsBackup tells a case whose reset writes the backup ref
		// before its compare-and-swap.
		keepsBackup bool
	}{
		{"tips diverged", func(t *testing.T, rc *resetCase) { rc.diverge(t) }, true},
		{"origin ahead", func(_ *testing.T, rc *resetCase) { rc.origin.Commit("patch/a", "ahead") }, false},
		{"the hub has no branch", func(t *testing.T, rc *resetCase) { gitrepotest.Git(t, rc.copy, "update-ref", "-d", headA) }, false},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rc := h.resetCarry(t, fmt.Sprintf("race-%d", i), nil)
			c.prepare(t, rc)
			compared, w := refIn(rc.copy, headA), commitIn(t, rc.copy, "the other writer's")
			moves.Store(rc.slug, w)
			path := "/workspaces/" + rc.slug + "/patches/" + rc.patch.ID
			before := h.get(t, path)

			a := h.resetToOrigin(t, rc.slug, rc.patch.ID)
			var details struct {
				Error struct {
					Details struct {
						CurrentSHA string `json:"current_sha"`
					} `json:"details"`
				} `json:"error"`
			}
			_ = json.Unmarshal(a.raw, &details)
			if a.status != http.StatusConflict || a.Error.ErrorType != "ref_moved" || details.Error.Details.CurrentSHA != w {
				t.Errorf("reset = %d %s, want 409 ref_moved with details.current_sha %s", a.status, a.raw, w)
			}
			if head := refIn(rc.copy, headA); head != w {
				t.Errorf("the copy's patch/a = %s, want the other writer's %s", head, w)
			}
			wantBackup := ""
			if c.keepsBackup {
				wantBackup = compared
			}
			if backup := refIn(rc.copy, backupA); backup != wantBackup {
				t.Errorf("the backup ref = %q, want %q", backup, wantBackup)
			}
			// replaced_sha shows the backup ref, which the reset leaves
			// written; the patch's own fields are as they were.
			got := h.get(t, path)
			delete(got.patch, "replaced_sha")
			if !reflect.DeepEqual(got.patch, before.patch) {
				t.Errorf("the patch is %s, want it as before %s", got.raw, before.raw)
			}
			if e := h.patchEvents(t, rc.slug, ""); len(e) != 0 {
				t.Errorf("the lost race wrote %d parley.patch.* events, want none", len(e))
			}

			moves.Delete(rc.slug)
			if a := h.resetToOrigin(t, rc.slug, rc.patch.ID); a.status != http.StatusOK {
				t.Errorf("the repeated reset = %d %s, want 200", a.status, a.raw)
			}
		})
	}
	if n := h.rebuilds.calls(); n != 0 {
		t.Errorf("the lost races asked for %d rebuilds, want none", n)
	}
}
