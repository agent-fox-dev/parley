package carrypatch_test

import (
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
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
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The refs of patch/a in a reset case's copy.
const (
	headA   = "refs/heads/patch/a"
	backupA = carrypatch.ReplacedRefPrefix + "patch/a"
)

// commitOn writes a commit with the message msg and an empty tree on top of
// parent into the repository dir and returns its id, in one run of git
// fast-import; deleteall empties the tree whatever parent's holds.
func commitOn(t *testing.T, dir, parent, msg string) string {
	t.Helper()
	const scratch = "refs/carrypatch-test/commit"
	stream := fmt.Sprintf("commit %s\nmark :1\ncommitter Origin <origin@example.invalid> now\ndata %d\n%s\n\nfrom %s\ndeleteall\n\nget-mark :1\nreset %s\n\n",
		scratch, len(msg)+1, msg, parent, scratch)
	id, err := gitrepotest.Run(dir, stream, "fast-import", "--quiet", "--date-format=now")
	if err != nil {
		t.Fatalf("write a commit on %s into %s: %v", parent, dir, err)
	}
	return id
}

// diverge gives the copy's patch/a a commit on top of origin's tip and
// origin a different one, and returns the copy's new tip.
func (rc *resetCase) diverge(t *testing.T) string {
	t.Helper()
	hub := commitOn(t, rc.copy, refIn(rc.copy, headA), "on the hub")
	gitrepotest.SetRef(t, rc.copy, headA, hub)
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
		gitrepotest.SetRef(t, rc.copy, headA, commitOn(t, rc.copy, refIn(rc.copy, headA), "on the hub"))
	}, "replaced", "old"},
	{"diverged with an earlier backup ref", func(t *testing.T, rc *resetCase) {
		rootsAt(t, rc.copy, map[string]string{backupA: "K"})
		rc.diverge(t)
	}, "replaced", "old"},
	{"the tips are equal with an earlier backup ref", func(t *testing.T, rc *resetCase) {
		rootsAt(t, rc.copy, map[string]string{backupA: "K"})
	}, "none", "K"},
}

// resetTo brings rc into the first of resetStates whose reset takes action.
func resetTo(t *testing.T, rc *resetCase, action string) {
	t.Helper()
	i := slices.IndexFunc(resetStates, func(s resetState) bool { return s.action == action })
	resetStates[i].prepare(t, rc)
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
			rc := h.resetCarry(t, "events-"+strings.ReplaceAll(action, "_", "-"), nil)
			resetTo(t, rc, action)
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
	rc.origin.Publish()
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
			if n := len(h.rebuilds.callsOf(rc.slug)); n != 0 {
				t.Errorf("the refusal asked for %d rebuilds, want none", n)
			}
			for wait := time.Now(); open != nil && open.Load() != 0; time.Sleep(10 * time.Millisecond) {
				if time.Since(wait) > 5*time.Second {
					t.Fatal("a request to origin is still open 5 s after the answer: the fetch was not stopped")
				}
			}
		})
	}
}

func TestResetToOrigin_LosesToAnotherWriterMeanwhile(t *testing.T) {
	t.Parallel()
	// meanwhile maps a workspace to what another writer does between the
	// reset's compare of the tips and its record and move.
	var meanwhile sync.Map
	h := startPatchHub(t, func(d *carrypatch.Deps) {
		d.BeforeRefSwap = func(slug, _ string) {
			if other, ok := meanwhile.Load(slug); ok {
				other.(func())()
			}
		}
	})
	// The other writers: one moves, creates or deletes the branch, the
	// others delete the patch or mark it merged upstream through the routes.
	const (
		moveBranch  = "move the branch"
		deletePatch = "delete the patch"
		mergePatch  = "mark the patch merged upstream"
	)
	cases := []struct {
		name    string
		prepare func(t *testing.T, rc *resetCase)
		other   string
		// errorType is the reset's refusal; keepsBackup tells a case whose
		// reset writes the backup ref before it loses.
		errorType   string
		keepsBackup bool
	}{
		{"tips diverged", func(t *testing.T, rc *resetCase) { rc.diverge(t) }, moveBranch, "ref_moved", true},
		{"origin ahead", func(_ *testing.T, rc *resetCase) { rc.origin.Commit("patch/a", "ahead") }, moveBranch, "ref_moved", false},
		{"the hub has no branch", func(t *testing.T, rc *resetCase) { gitrepotest.Git(t, rc.copy, "update-ref", "-d", headA) }, moveBranch, "ref_moved", false},
		{"tips diverged, the patch deleted", func(t *testing.T, rc *resetCase) { rc.diverge(t) }, deletePatch, "patch_status_invalid", false},
		{"tips equal, the patch deleted", func(*testing.T, *resetCase) {}, deletePatch, "patch_status_invalid", false},
		{"origin ahead, the patch merged upstream", func(_ *testing.T, rc *resetCase) { rc.origin.Commit("patch/a", "ahead") }, mergePatch, "patch_status_invalid", false},
	}
	for i, c := range cases {
		t.Run(c.other+"/"+c.name, func(t *testing.T) {
			t.Parallel()
			rc := h.resetCarry(t, fmt.Sprintf("race-%d", i), withSettings("origin", ""))
			c.prepare(t, rc)
			compared, backupBefore := refIn(rc.copy, headA), refIn(rc.copy, backupA)
			w := commitIn(t, rc.copy, "the other writer's")
			path := "/workspaces/" + rc.slug + "/patches/" + rc.patch.ID
			// left is the patch as the other writer leaves it.
			left := make(chan answer, 1)
			meanwhile.Store(rc.slug, func() {
				switch c.other {
				case moveBranch:
					if _, err := gitrepotest.Run(rc.copy, "", "update-ref", headA, w); err != nil {
						t.Errorf("the other writer could not move patch/a: %v", err)
					}
				case deletePatch:
					if a := h.remove(t, rc.slug, rc.patch.ID, nil); a.status != http.StatusNoContent {
						t.Errorf("DELETE meanwhile = %d %s, want 204", a.status, a.raw)
					}
				case mergePatch:
					if a := h.update(t, rc.slug, rc.patch.ID, map[string]any{"status": "merged_upstream"}); a.status != http.StatusOK {
						t.Errorf("PATCH meanwhile = %d %s, want 200", a.status, a.raw)
					}
				}
				left <- h.get(t, path)
			})

			a := h.resetToOrigin(t, rc.slug, rc.patch.ID)
			if a.status != http.StatusConflict || a.Error.ErrorType != c.errorType {
				t.Errorf("reset = %d %s, want 409 %s", a.status, a.raw, c.errorType)
			}
			wantHead, wantBackup := compared, backupBefore
			if c.other == moveBranch {
				var details struct {
					Error struct {
						Details struct {
							CurrentSHA string `json:"current_sha"`
						} `json:"details"`
					} `json:"error"`
				}
				_ = json.Unmarshal(a.raw, &details)
				if details.Error.Details.CurrentSHA != w {
					t.Errorf("reset = %s, want details.current_sha %s", a.raw, w)
				}
				wantHead = w
			}
			if c.keepsBackup {
				wantBackup = compared
			}
			if head := refIn(rc.copy, headA); head != wantHead {
				t.Errorf("the copy's patch/a = %s, want %s", head, wantHead)
			}
			if backup := refIn(rc.copy, backupA); backup != wantBackup {
				t.Errorf("the backup ref = %q, want %q", backup, wantBackup)
			}
			// replaced_sha shows the backup ref, which a lost swap leaves
			// written; the patch's own fields are as the other writer left
			// them.
			var want answer
			select {
			case want = <-left:
			default:
				t.Fatal("the other writer did not run while the reset waited")
			}
			got := h.get(t, path)
			delete(want.patch, "replaced_sha")
			delete(got.patch, "replaced_sha")
			if !reflect.DeepEqual(got.patch, want.patch) {
				t.Errorf("the patch is %s, want it as the other writer left it %s", got.raw, want.raw)
			}
			for _, typ := range []string{carrypatch.EventPatchReset, carrypatch.EventPatchReplace} {
				if e := h.patchEvents(t, rc.slug, typ); len(e) != 0 {
					t.Errorf("the lost reset wrote %d %s events, want none", len(e), typ)
				}
			}
			if n := len(h.rebuilds.callsOf(rc.slug)); n != 0 {
				t.Errorf("the lost reset asked for %d rebuilds, want none", n)
			}

			meanwhile.Delete(rc.slug)
			retry, wantRetry := h.resetToOrigin(t, rc.slug, rc.patch.ID), http.StatusOK
			if c.other != moveBranch {
				wantRetry = http.StatusConflict
			}
			if retry.status != wantRetry {
				t.Errorf("the repeated reset = %d %s, want %d", retry.status, retry.raw, wantRetry)
			}
		})
	}
}

func TestResetToOrigin_FailedRecordMovesNothing(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	for _, action := range []string{"created", "fast_forwarded", "replaced"} {
		// Serial: the failing event log is the hub's, turned on by each row.
		t.Run(action, func(t *testing.T) {
			rc := h.resetCarry(t, "unrecorded-"+strings.ReplaceAll(action, "_", "-"), withSettings("origin", ""))
			resetTo(t, rc, action)
			old, backup := refIn(rc.copy, headA), refIn(rc.copy, backupA)
			path := "/workspaces/" + rc.slug + "/patches/" + rc.patch.ID
			before := h.get(t, path)

			h.failEvents.Store(true)
			a := h.resetToOrigin(t, rc.slug, rc.patch.ID)
			h.failEvents.Store(false)
			if a.status != http.StatusInternalServerError {
				t.Errorf("reset with a failing event log = %d %s, want 500", a.status, a.raw)
			}
			if head, now := refIn(rc.copy, headA), refIn(rc.copy, backupA); head != old || now != backup {
				t.Errorf("the copy's patch/a = %q and its backup ref %q, want them as before: %q and %q", head, now, old, backup)
			}
			if got := h.get(t, path); !reflect.DeepEqual(got.patch, before.patch) {
				t.Errorf("the patch is %s, want it as before %s", got.raw, before.raw)
			}
			if n := len(h.rebuilds.callsOf(rc.slug)); n != 0 {
				t.Errorf("the failed reset asked for %d rebuilds, want none", n)
			}

			if a := h.resetToOrigin(t, rc.slug, rc.patch.ID); a.status != http.StatusOK || a.patch["action"] != action {
				t.Fatalf("the repeated reset = %d %s, want 200 with action %s", a.status, a.raw, action)
			}
			wantReplace := 0
			if action == "replaced" {
				wantReplace = 1
			}
			if r, p := h.patchEvents(t, rc.slug, carrypatch.EventPatchReset), h.patchEvents(t, rc.slug, carrypatch.EventPatchReplace); len(r) != 1 || len(p) != wantReplace {
				t.Errorf("the repeated reset wrote %d reset and %d replace events, want 1 and %d", len(r), len(p), wantReplace)
			}
		})
	}
}

func TestResetToOrigin_QueuesRebuild(t *testing.T) {
	t.Parallel()
	h := startPatchHub(t)
	// reset resets rc as the caller credential, whose reset event names the
	// actor the enqueue must be given, and checks the answer and the one
	// call of the enqueue.
	reset := func(t *testing.T, rc *resetCase, credential, action, submitter string) {
		t.Helper()
		job := "job-" + rc.slug
		h.rebuilds.answer(rc.slug, carrypatch.RebuildResult{Queued: true, JobID: job}, nil)

		a := h.send(t, credential, http.MethodPost, "/workspaces/"+rc.slug+"/patches/"+rc.patch.ID+"/reset-to-origin", nil)
		if a.status != http.StatusOK || a.patch["action"] != action || a.patch["rebuild_triggered"] != true || a.patch["rebuild_job_id"] != job {
			t.Fatalf("reset = %d %s, want 200 with action %s, rebuild_triggered true and rebuild_job_id %s", a.status, a.raw, action, job)
		}
		e := h.patchEvents(t, rc.slug, carrypatch.EventPatchReset)
		if len(e) != 1 {
			t.Fatalf("the reset wrote %d parley.patch.reset events, want 1", len(e))
		}
		// The rebuild is asked for once the branch is at origin's tip and
		// the reset is recorded.
		want := []rebuildCall{{slug: rc.slug,
			opts: carrypatch.RebuildOptions{SubmittedBy: submitter, ActorType: e[0].ActorType, ActorID: e[0].ActorID},
			seen: rebuildSeen{head: rc.origin.Head("patch/a"), resets: 1}}}
		if got := h.rebuilds.callsOf(rc.slug); !reflect.DeepEqual(got, want) {
			t.Errorf("the enqueue got %+v, want %+v", got, want)
		}
	}
	for _, action := range []string{"created", "fast_forwarded", "replaced"} {
		for _, status := range []string{carrypatch.StatusActive, carrypatch.StatusConflict} {
			t.Run(action+"/"+status, func(t *testing.T) {
				t.Parallel()
				rc := h.resetCarry(t, "queue-"+strings.ReplaceAll(action, "_", "-")+"-"+status, autoRebuild(true))
				h.alter(t, rc.slug, rc.patch.ID, func(p *carrypatch.Patch) { p.Status = status })
				resetTo(t, rc, action)
				reset(t, rc, h.key, action, h.owner)
			})
		}
	}
	t.Run("the admin token, the setting at its default", func(t *testing.T) {
		t.Parallel()
		rc := h.resetCarry(t, "queue-admin", nil)
		rc.origin.Commit("patch/a", "ahead")
		reset(t, rc, h.Token, "fast_forwarded", event.ActorAdminToken)
	})
}

func TestResetToOrigin_DoesNotQueueRebuild(t *testing.T) {
	t.Parallel()
	// turnOff maps the workspaces whose auto_rebuild_after_sync is turned
	// off through the hub while their reset runs, after the reset read the
	// workspace, to their test.
	var turnOff sync.Map
	var h *patchHub
	h = startPatchHub(t, func(d *carrypatch.Deps) {
		d.BeforeRefSwap = func(slug, _ string) {
			v, ok := turnOff.Load(slug)
			if !ok {
				return
			}
			t := v.(*testing.T)
			body := map[string]any{"settings": map[string]any{"auto_rebuild_after_sync": false}}
			if status, raw := h.Call(t, http.MethodPatch, "/api/v1/workspaces/"+slug, h.key, body); status != http.StatusOK {
				t.Errorf("turn auto_rebuild_after_sync of %s off = %d %s, want 200", slug, status, raw)
			}
		}
	})
	cases := []struct {
		name, action, status string
		setting, turnedOff   bool
	}{
		{"nothing to do", "none", carrypatch.StatusActive, true, false},
		{"disabled patch fast-forwarded", "fast_forwarded", carrypatch.StatusDisabled, true, false},
		{"disabled patch replaced", "replaced", carrypatch.StatusDisabled, true, false},
		{"setting off, fast-forwarded", "fast_forwarded", carrypatch.StatusActive, false, false},
		{"setting off, conflict patch replaced", "replaced", carrypatch.StatusConflict, false, false},
		{"setting turned off while the reset runs", "fast_forwarded", carrypatch.StatusActive, true, true},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rc := h.resetCarry(t, fmt.Sprintf("no-queue-%d", i), autoRebuild(c.setting))
			h.alter(t, rc.slug, rc.patch.ID, func(p *carrypatch.Patch) { p.Status = c.status })
			resetTo(t, rc, c.action)
			h.rebuilds.answer(rc.slug, carrypatch.RebuildResult{Queued: true, JobID: "job-" + rc.slug}, nil)
			if c.turnedOff {
				turnOff.Store(rc.slug, t)
			}

			a := h.resetToOrigin(t, rc.slug, rc.patch.ID)
			if _, job := a.patch["rebuild_job_id"]; a.status != http.StatusOK || a.patch["action"] != c.action ||
				a.patch["rebuild_triggered"] != false || job {
				t.Errorf("reset = %d %s, want 200 with action %s, rebuild_triggered false and no rebuild_job_id", a.status, a.raw, c.action)
			}
			if calls := h.rebuilds.callsOf(rc.slug); len(calls) != 0 {
				t.Errorf("the reset asked for the rebuilds %+v, want none", calls)
			}
		})
	}
}

func TestResetToOrigin_KeepsResetWhenNoRebuildIsQueued(t *testing.T) {
	t.Parallel()
	var log hubtest.LogBuffer
	h := startPatchHub(t, func(d *carrypatch.Deps) { d.Logger = slog.New(slog.NewJSONHandler(&log, nil)) })
	cases := []struct {
		name   string
		result carrypatch.RebuildResult
		err    error
		// logged is how many error records name the workspace.
		logged int
	}{
		{"the workspace has a rebuild waiting already", carrypatch.RebuildResult{ExistingJobID: "job-waiting"}, nil, 0},
		{"the enqueue fails", carrypatch.RebuildResult{}, errors.New("the job store refuses the job"), 1},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rc := h.resetCarry(t, fmt.Sprintf("kept-%d", i), nil)
			tip := rc.origin.Commit("patch/a", "ahead")
			h.rebuilds.answer(rc.slug, c.result, c.err)

			a := h.resetToOrigin(t, rc.slug, rc.patch.ID)
			if _, job := a.patch["rebuild_job_id"]; a.status != http.StatusOK || a.patch["action"] != "fast_forwarded" ||
				a.patch["rebuild_triggered"] != false || job {
				t.Errorf("reset = %d %s, want 200 with action fast_forwarded, rebuild_triggered false and no rebuild_job_id", a.status, a.raw)
			}
			if head := refIn(rc.copy, headA); head != tip {
				t.Errorf("the copy's patch/a = %s, want origin's tip %s", head, tip)
			}
			if e := h.patchEvents(t, rc.slug, carrypatch.EventPatchReset); len(e) != 1 {
				t.Errorf("the reset wrote %d parley.patch.reset events, want 1", len(e))
			}
			if n := len(h.rebuilds.callsOf(rc.slug)); n != 1 {
				t.Errorf("the reset asked for %d rebuilds, want 1", n)
			}
			logged := 0
			for line := range strings.SplitSeq(log.String(), "\n") {
				var rec map[string]any
				if json.Unmarshal([]byte(line), &rec) == nil && rec["level"] == "ERROR" && rec["slug"] == rc.slug {
					logged++
					if c.err != nil && rec["error"] != c.err.Error() {
						t.Errorf("the error record %s does not hold the enqueue's error %q", line, c.err)
					}
				}
			}
			if logged != c.logged {
				t.Errorf("the log holds %d error records naming %s, want %d:\n%s", logged, rc.slug, c.logged, log.String())
			}
		})
	}
}
