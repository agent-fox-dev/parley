package forge_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"testing"

	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The head commits of the fake forges' pull requests: the first page of a
// check run list is read from headSHA, and movedSHA is where a head moves.
const (
	headSHA  = "0123456789abcdef0123456789abcdef01234567"
	movedSHA = "fedcba9876543210fedcba9876543210fedcba98"
)

// githubPull is GitHub's open pull request number from feature into main,
// with the members of more added.
func githubPull(number int, more map[string]any) map[string]any {
	pull := map[string]any{
		"number": number, "title": "Change", "body": "text", "state": "open", "draft": false, "merged": false,
		"html_url": fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number), "user": map[string]any{"login": "kim"},
		"head": map[string]any{"sha": headSHA, "ref": "feature"}, "base": map[string]any{"sha": movedSHA, "ref": "main"},
		"created_at": second(0), "updated_at": second(60), "closed_at": nil, "merged_at": nil,
	}
	maps.Copy(pull, more)
	return pull
}

// gitlabMergeRequest is GitLab's opened merge request iid from feature
// into main, with the members of more added.
func gitlabMergeRequest(iid int, more map[string]any) map[string]any {
	mr := map[string]any{
		"id": 1000 + iid, "iid": iid, "title": "Change", "description": "text", "state": "opened", "draft": false,
		"web_url": fmt.Sprintf("https://gitlab.example.com/group/sub/proj/-/merge_requests/%d", iid),
		"author":  map[string]any{"username": "kim"}, "sha": headSHA, "source_branch": "feature", "target_branch": "main",
		"created_at": second(0), "updated_at": second(60), "closed_at": nil, "merged_at": nil,
	}
	maps.Copy(mr, more)
	return mr
}

// pullMembers are the members of a pull request as the hub answers it.
var pullMembers = []string{"author", "base_branch", "body", "closed_at", "created_at", "draft", "head_branch", "head_sha",
	"merged", "merged_at", "number", "state", "title", "updated_at", "url"}

func TestPullRequest(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.addWorkspace(t, workspace.Workspace{Slug: "gh", GitURL: new("https://github.com/acme/widgets.git"), Settings: w.github()})
	w.addWorkspace(t, workspace.Workspace{Slug: "gl", GitURL: new("https://gitlab.example.com/group/sub/proj.git"), Settings: w.gitlab()})
	ghPull, glMR := ghRepo+"/pulls/12", glProject+"/merge_requests/12"
	// object reads the JSON object path answers, failing the test unless
	// it is 200.
	object := func(t *testing.T, slug, path string) map[string]any {
		t.Helper()
		status, body, _ := w.get(t, slug, path, w.Token)
		var got map[string]any
		if status != http.StatusOK || json.Unmarshal(body, &got) != nil {
			t.Fatalf("GET %s%s = %d %.300s, want 200 with an object", slug, path, status, body)
		}
		return got
	}

	t.Run("answers each forge's pull request and its state as a projection of it", func(t *testing.T) {
		merged := map[string]any{"state": "closed", "merged": true, "merged_at": second(120), "closed_at": second(120)}
		cases := []struct {
			name, slug, path string
			forge            map[string]any
			want             string
		}{
			{"an open GitHub draft", "gh", ghPull, githubPull(12, map[string]any{"draft": true}),
				`{"number":12,"title":"Change","body":"text","state":"open","url":"https://github.com/acme/widgets/pull/12","draft":true,
				"merged":false,"head_sha":"` + headSHA + `","head_branch":"feature","base_branch":"main","author":{"login":"kim"},
				"created_at":"2026-10-01T08:00:00Z","updated_at":"2026-10-01T08:01:00Z","closed_at":null,"merged_at":null}`},
			{"a merged GitHub pull request", "gh", ghPull, githubPull(12, merged),
				`{"state":"merged","merged":true,"merged_at":"2026-10-01T08:02:00Z","closed_at":"2026-10-01T08:02:00Z"}`},
			{"a GitHub pull request with only merged_at", "gh", ghPull, githubPull(12, with(merged, "merged", nil)),
				`{"state":"merged","merged":true}`},
			{"a closed GitHub pull request", "gh", ghPull, githubPull(12, map[string]any{"state": "closed", "closed_at": second(120)}),
				`{"state":"closed","merged":false,"merged_at":null}`},
			{"an opened GitLab draft without a description", "gl", glMR, gitlabMergeRequest(12, map[string]any{"draft": true, "description": nil}),
				`{"number":12,"title":"Change","body":"","state":"open","url":"https://gitlab.example.com/group/sub/proj/-/merge_requests/12",
				"draft":true,"merged":false,"head_sha":"` + headSHA + `","head_branch":"feature","base_branch":"main","author":{"login":"kim"},
				"created_at":"2026-10-01T08:00:00Z","updated_at":"2026-10-01T08:01:00Z","closed_at":null,"merged_at":null}`},
			{"a merged GitLab merge request", "gl", glMR, gitlabMergeRequest(12, map[string]any{"state": "merged", "merged_at": second(120)}),
				`{"state":"merged","merged":true,"merged_at":"2026-10-01T08:02:00Z"}`},
			{"a locked GitLab merge request", "gl", glMR, gitlabMergeRequest(12, map[string]any{"state": "locked"}),
				`{"state":"open","merged":false}`},
			{"a closed GitLab merge request", "gl", glMR, gitlabMergeRequest(12, map[string]any{"state": "closed", "closed_at": second(120)}),
				`{"state":"closed","merged":false}`},
		}
		for _, c := range cases {
			w.fake.SetObject(c.path, c.forge)
			pull := object(t, c.slug, "/pulls/12")
			if keys := slices.Sorted(maps.Keys(pull)); !slices.Equal(keys, pullMembers) {
				t.Errorf("%s: the pull request has the members %v, want %v", c.name, keys, pullMembers)
			}
			var want map[string]any
			_ = json.Unmarshal([]byte(c.want), &want)
			for name, v := range want {
				if got, _ := json.Marshal(pull[name]); !sameJSON(got, mustMarshal(v)) {
					t.Errorf("%s: %s = %s, want %s", c.name, name, got, mustMarshal(v))
				}
			}
			if sha, _ := pull["head_sha"].(string); sha != headSHA {
				t.Errorf("%s: head_sha %q, want the full id %s", c.name, sha, headSHA)
			}
			if last := w.fake.Last(t); last.RawPath != c.path {
				t.Errorf("%s: the forge was asked for %s, want %s", c.name, last.RawPath, c.path)
			}
			state := object(t, c.slug, "/pulls/12/state")
			if keys := slices.Sorted(maps.Keys(state)); !slices.Equal(keys, []string{"head_sha", "merged", "number", "state"}) {
				t.Errorf("%s: the state has the members %v, want head_sha, merged, number and state", c.name, keys)
			}
			for name, v := range state {
				if fmt.Sprint(v) != fmt.Sprint(pull[name]) {
					t.Errorf("%s: the state's %s = %v, the pull request's %v", c.name, name, v, pull[name])
				}
			}
		}
	})

	t.Run("lists the files each forge says the pull request changes", func(t *testing.T) {
		var files []any
		var want []map[string]any
		for i, s := range [][2]string{{"added", "added"}, {"modified", "modified"}, {"removed", "deleted"}, {"renamed", "renamed"},
			{"copied", "added"}, {"changed", "modified"}, {"unchanged", "modified"}} {
			name := fmt.Sprintf("f%d.go", i)
			files = append(files, map[string]any{"sha": headSHA, "filename": name, "status": s[0], "additions": i, "deletions": i + 1, "changes": 2*i + 1})
			want = append(want, map[string]any{"filename": name, "status": s[1], "additions": i, "deletions": i + 1})
		}
		w.fake.SetList(ghPull+"/files", files...)
		w.fake.SetList(glMR+"/diffs",
			map[string]any{"old_path": "new.go", "new_path": "new.go", "new_file": true, "diff": "@@ -0,0 +1 @@\n+a\n"},
			map[string]any{"old_path": "gone.go", "new_path": "gone.go", "deleted_file": true, "diff": "@@ -1 +0,0 @@\n-a\n"},
			map[string]any{"old_path": "a.go", "new_path": "b.go", "renamed_file": true, "diff": ""},
			map[string]any{"old_path": "m.go", "new_path": "m.go", "diff": "@@ -1,2 +1,3 @@\n ctx\n-old\n+new\n+more\n--- dashes"},
			map[string]any{"old_path": "big.bin", "new_path": "big.bin", "too_large": true})
		for slug, items := range map[string]string{
			"gh": mustMarshal(want),
			"gl": `[{"filename":"new.go","status":"added","additions":1,"deletions":0},{"filename":"gone.go","status":"deleted","additions":0,"deletions":1},
				{"filename":"b.go","status":"renamed","additions":0,"deletions":0},{"filename":"m.go","status":"modified","additions":2,"deletions":2},
				{"filename":"big.bin","status":"modified","additions":0,"deletions":0}]`,
		} {
			l, _ := w.list(t, slug, "/pulls/12/files")
			if got, _ := json.Marshal(l.Items); !sameJSON(got, items) || l.NextCursor != nil {
				t.Errorf("GET %s/pulls/12/files = %s with next_cursor %v, want %s and null", slug, got, l.NextCursor, items)
			}
		}
	})

	t.Run("lists the check runs of the head the first page was read from", func(t *testing.T) {
		type run struct {
			forge      map[string]any
			status     string
			conclusion any
			summary    string
		}
		gh := []run{
			{map[string]any{"status": "queued"}, "queued", nil, ""},
			{map[string]any{"status": "in_progress"}, "in_progress", nil, ""},
			{map[string]any{"status": "pending"}, "queued", nil, ""},
			{map[string]any{"status": "completed", "conclusion": "success", "output": map[string]any{"summary": "ok"}}, "completed", "success", "ok"},
			{map[string]any{"status": "completed", "conclusion": "timed_out"}, "completed", "failure", ""},
			{map[string]any{"status": "completed", "conclusion": "stale"}, "completed", "cancelled", ""},
			{map[string]any{"status": "completed", "conclusion": "neutral"}, "completed", "neutral", ""},
		}
		gl := []run{
			{map[string]any{"status": "created"}, "queued", nil, ""},
			{map[string]any{"status": "manual"}, "queued", nil, ""},
			{map[string]any{"status": "running"}, "in_progress", nil, ""},
			{map[string]any{"status": "success"}, "completed", "success", ""},
			{map[string]any{"status": "failed"}, "completed", "failure", ""},
			{map[string]any{"status": "failed", "allow_failure": true}, "completed", "neutral", ""},
			{map[string]any{"status": "canceled"}, "completed", "cancelled", ""},
			{map[string]any{"status": "skipped"}, "completed", "skipped", ""},
		}
		// seed puts runs on the fake under the URL member urlName and
		// answers what the hub lists for them.
		seed := func(runs []run, urlName, urlFormat string) (forgeRuns []any, want string) {
			var items []map[string]any
			for i, r := range runs {
				u := fmt.Sprintf(urlFormat, i)
				forgeRuns = append(forgeRuns, with(r.forge, "id", i, "name", fmt.Sprintf("c%d", i), urlName, u))
				items = append(items, map[string]any{"name": fmt.Sprintf("c%d", i), "status": r.status, "conclusion": r.conclusion,
					"summary": r.summary, "url": u})
			}
			return forgeRuns, mustMarshal(items)
		}
		ghRuns, ghWant := seed(gh, "html_url", "https://github.com/acme/widgets/runs/%d")
		glJobs, glWant := seed(gl, "web_url", "https://gitlab.example.com/group/sub/proj/-/jobs/%d")
		w.fake.SetObject(ghPull, githubPull(12, nil))
		w.fake.SetListIn(ghRepo+"/commits/"+headSHA+"/check-runs", "check_runs", ghRuns...)
		w.fake.SetList(glMR+"/pipelines", map[string]any{"id": 501, "sha": headSHA}, map[string]any{"id": 400})
		w.fake.SetList(glProject+"/pipelines/501/jobs", glJobs...)
		w.fake.SetList(glProject+"/pipelines/400/jobs", with(glJobs[0].(map[string]any), "name", "old"))
		for slug, c := range map[string]struct{ want, path string }{
			"gh": {ghWant, ghRepo + "/commits/" + headSHA + "/check-runs"},
			"gl": {glWant, glProject + "/pipelines/501/jobs"},
		} {
			l, _ := w.list(t, slug, "/pulls/12/checks")
			if got, _ := json.Marshal(l.Items); !sameJSON(got, c.want) || l.NextCursor != nil {
				t.Errorf("GET %s/pulls/12/checks = %s with next_cursor %v, want %s and null", slug, got, l.NextCursor, c.want)
			}
			if last := w.fake.Last(t); last.RawPath != c.path {
				t.Errorf("GET %s/pulls/12/checks asked the forge last for %s, want %s", slug, last.RawPath, c.path)
			}
		}

		w.fake.SetObject(ghRepo+"/pulls/13", githubPull(13, nil))
		w.fake.SetObject(ghRepo+"/pulls/14", githubPull(14, map[string]any{"head": map[string]any{"ref": "gone"}}))
		w.fake.SetListIn(ghRepo+"/commits/"+headSHA+"/check-runs", "check_runs")
		w.fake.SetList(glProject + "/merge_requests/13/pipelines")
		for _, path := range []string{"gh/pulls/13/checks", "gh/pulls/14/checks", "gl/pulls/13/checks"} {
			if status, body, _ := w.get(t, path[:2], path[2:], w.Token); status != http.StatusOK || !sameJSON(body, `{"items":[],"next_cursor":null}`) {
				t.Errorf("GET %s of a head without check runs = %d %s, want 200 with no items and next_cursor null", path, status, body)
			}
		}

		w.fake.SetListIn(ghRepo+"/commits/"+headSHA+"/check-runs", "check_runs", ghRuns...)
		w.fake.SetListIn(ghRepo+"/commits/"+movedSHA+"/check-runs", "check_runs", ghRuns...)
		w.fake.SetList(glProject+"/pipelines/600/jobs", glJobs...)
		for slug, c := range map[string]struct {
			move func()
			path string
		}{
			"gh": {func() {
				w.fake.SetObject(ghPull, githubPull(12, map[string]any{"head": map[string]any{"sha": movedSHA}}))
			},
				ghRepo + "/commits/" + headSHA + "/check-runs"},
			"gl": {func() { w.fake.SetList(glMR+"/pipelines", map[string]any{"id": 600}, map[string]any{"id": 501}) },
				glProject + "/pipelines/501/jobs"},
		} {
			w.fake.SetObject(ghPull, githubPull(12, nil))
			w.fake.SetList(glMR+"/pipelines", map[string]any{"id": 501})
			p1, _ := w.list(t, slug, "/pulls/12/checks?limit=1")
			if p1.NextCursor == nil {
				t.Fatalf("GET %s/pulls/12/checks?limit=1 has next_cursor null", slug)
			}
			c.move()
			w.list(t, slug, "/pulls/12/checks?cursor="+url.QueryEscape(*p1.NextCursor))
			if last, q := w.fake.Last(t), w.lastQuery(t); last.RawPath != c.path || q.Get("page") != "2" {
				t.Errorf("%s: the second page after the head moved asked the forge for %s?%s, want %s page 2",
					slug, last.RawPath, last.RawQuery, c.path)
			}
		}
	})

	t.Run("lists the reviews oldest first with GitLab's approvals first and no page longer than limit", func(t *testing.T) {
		review := func(login, state string, at any) map[string]any {
			return map[string]any{"id": 1, "user": map[string]any{"login": login}, "state": state, "body": "b " + login, "submitted_at": at}
		}
		w.fake.SetList(ghPull+"/reviews", review("kim", "APPROVED", second(1)), with(review("lee", "PENDING", nil), "submitted_at", nil),
			review("ana", "CHANGES_REQUESTED", second(2)), review("bo", "COMMENTED", second(3)), review("cy", "DISMISSED", second(4)))
		l, _ := w.list(t, "gh", "/pulls/12/reviews")
		var states, times []string
		for _, item := range l.Items {
			states, times = append(states, fmt.Sprint(item["state"])), append(times, fmt.Sprint(item["submitted_at"]))
		}
		if fmt.Sprint(states) != "[approved changes_requested commented dismissed]" || !slices.IsSorted(times) {
			t.Errorf("GET gh/pulls/12/reviews: the states %v submitted at %v, want approved, changes_requested, commented and dismissed, oldest first",
				states, times)
		}
		if got, _ := json.Marshal(l.Items[0]); !sameJSON(got, `{"author":{"login":"kim"},"state":"approved","body":"b kim","submitted_at":"2026-10-01T08:00:01Z"}`) {
			t.Errorf("GET gh/pulls/12/reviews: the first is %s", got)
		}

		approvals := map[string]any{"approved_by": []any{
			map[string]any{"user": map[string]any{"username": "kim"}}, map[string]any{"user": map[string]any{"username": "lee"}}}}
		w.fake.SetObject(glMR+"/approvals", approvals)
		w.fake.SetList(glMR+"/notes", with(gitlabNote(1, "first", false), "author", map[string]any{"username": "ana"}),
			gitlabNote(2, "approved this merge request", true), gitlabNote(3, "second", false))
		l, _ = w.list(t, "gl", "/pulls/12/reviews")
		want := `[{"author":{"login":"kim"},"state":"approved","body":"","submitted_at":null},
			{"author":{"login":"lee"},"state":"approved","body":"","submitted_at":null},
			{"author":{"login":"ana"},"state":"commented","body":"first","submitted_at":"2026-10-01T08:00:01Z"},
			{"author":{"login":"kim"},"state":"commented","body":"second","submitted_at":"2026-10-01T08:00:03Z"}]`
		if got, _ := json.Marshal(l.Items); !sameJSON(got, want) || l.NextCursor != nil {
			t.Errorf("GET gl/pulls/12/reviews = %s with next_cursor %v, want %s and null", got, l.NextCursor, want)
		}
		if q := w.lastQuery(t); w.fake.Last(t).RawPath != glMR+"/notes" || q.Get("sort") != "asc" || q.Get("order_by") != "created_at" {
			t.Errorf("the notes were asked for at %s?%s, want %s with sort=asc and order_by=created_at",
				w.fake.Last(t).RawPath, w.fake.Last(t).RawQuery, glMR+"/notes")
		}

		w.fake.SetObject(glProject+"/merge_requests/13/approvals", approvals)
		var notes []any
		for i := 1; i <= 5; i++ {
			notes = append(notes, gitlabNote(i, fmt.Sprintf("n%d", i), false))
		}
		w.fake.SetList(glProject+"/merge_requests/13/notes", notes...)
		for limit, notesSize := range map[int]string{3: "1", 2: "2"} {
			var all []string
			path := fmt.Sprintf("/pulls/13/reviews?limit=%d", limit)
			for page := 1; ; page++ {
				before := len(w.fake.Requests())
				l, _ := w.list(t, "gl", path)
				for _, r := range w.fake.Requests()[before:] {
					if q, _ := url.ParseQuery(r.RawQuery); r.RawPath == glProject+"/merge_requests/13/notes" && q.Get("per_page") != notesSize {
						t.Errorf("limit %d, page %d: the notes were asked for with per_page=%s, want %s", limit, page, q.Get("per_page"), notesSize)
					}
				}
				if len(l.Items) > limit || page == 1 && len(l.Items) != limit {
					t.Errorf("limit %d: page %d holds %d reviews", limit, page, len(l.Items))
				}
				for _, item := range l.Items {
					author, _ := item["author"].(map[string]any)
					all = append(all, fmt.Sprintf("%v:%v", author["login"], item["body"]))
				}
				if l.NextCursor == nil || page == 10 {
					break
				}
				path = "/pulls/13/reviews?cursor=" + url.QueryEscape(*l.NextCursor)
			}
			if fmt.Sprint(all) != "[kim: lee: kim:n1 kim:n2 kim:n3 kim:n4 kim:n5]" {
				t.Errorf("limit %d: the pages hold %v, want the approvals of kim and lee then the notes n1 to n5", limit, all)
			}
		}
	})
}

// mustMarshal is v in JSON.
func mustMarshal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
