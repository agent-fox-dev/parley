package gitserver_test

import (
	"cmp"
	"maps"
	"net/http"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/gitserver"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
)

// grown is the series of the counter that after holds more of than before,
// with how much more.
func grown(before, after map[gitserver.RequestLabels]uint64) map[gitserver.RequestLabels]uint64 {
	d := map[gitserver.RequestLabels]uint64{}
	for k, n := range after {
		if n > before[k] {
			d[k] = n - before[k]
		}
	}
	return d
}

// The hub admits one request a minute per key and each row starts a new
// window, so that the row over the limit, which sends its request twice, is
// the only one refused for it. The workspace empty is ready and has no
// repository on disk. The processor answers every push 200, so that the
// result it reports is the only thing that tells its pushes apart.
func TestMetrics_CountsEveryRequestOnceByItsServiceAndResult(t *testing.T) {
	t.Parallel()
	processor := &fakeProcessor{}
	w := newGitWorld(t, hubtest.Config{TOML: "[limits]\nrequests_per_minute = 1", Compose: func(c *hub.Composition) {
		c.AdjustGitServer(func(d *gitserver.Deps) { d.Push = processor })
	}})
	w.AddWorkspace(t, access.Record{Slug: "empty", OwnerID: w.owner, OrgID: w.acme, Kind: access.KindRepository, Status: access.StatusReady})
	noGit, _ := w.pat(t, w.owner, "", "changes:read")
	owner := basic("x", w.ownerKey)
	const (
		get, post                  = http.MethodGet, http.MethodPost
		refs, fetch, push          = gitserver.LabelInfoRefs, gitserver.LabelUploadPack, gitserver.LabelReceivePack
		ok, refused, failed        = gitserver.ResultOK, gitserver.ResultRefused, gitserver.ResultError
		archive, emptyRefs, nosuch = "/git/acme/demo.git/info/refs?service=git-archive", "/git/acme/empty.git/info/refs?service=git-upload-pack", "/git/acme/nosuch.git/git-upload-pack"
	)
	rows := []struct {
		name, method, path, auth, body string
		status                         string // demo's status, ready when ""
		twice                          bool
		report                         gitserver.Result
		service                        string
		result                         gitserver.Result
	}{
		{"served ref discovery", get, demoRefs, owner, "", "", false, "", refs, ok},
		{"served fetch", post, uploadPack, owner, fetchRequest(w.head), "", false, "", fetch, ok},
		{"401 no credential", get, demoRefs, "", "", "", false, "", refs, refused},
		{"403 pat without git:read", post, uploadPack, basic("x", noGit), "", "", false, "", fetch, refused},
		{"404 unknown repository", post, nosuch, owner, "", "", false, "", fetch, refused},
		{"404 push to an unknown repository", post, nosuchPush, owner, "", "", false, "", push, refused},
		{"400 invalid service", get, archive, owner, "", "", false, "", refs, failed},
		{"409 workspace not ready", post, uploadPack, owner, "", access.StatusFailed, false, "", fetch, failed},
		{"429 over the limit", get, demoRefs, owner, "", "", true, "", refs, failed},
		{"500 no repository on disk", get, emptyRefs, owner, "", "", false, "", refs, failed},
		{"empty fetch request", post, uploadPack, owner, "", "", false, "", fetch, failed},
		{"fetch request git refuses", post, uploadPack, owner, "0009junk", "", false, "", fetch, failed},
		{"push the processor takes", post, receivePack, owner, "0000", "", false, ok, push, ok},
		{"push the processor refuses", post, receivePack, owner, "0000", "", false, refused, push, refused},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			w.advance(time.Minute)
			w.setStatus(t, "demo", cmp.Or(row.status, access.StatusReady))
			processor.report(row.report)
			if row.twice {
				w.send(t, row.method, row.path, row.auth, nil, row.body)
			}
			before := w.git.Requests()
			a := w.send(t, row.method, row.path, row.auth, nil, row.body)
			want := map[gitserver.RequestLabels]uint64{{Service: row.service, Result: row.result}: 1}
			if got := grown(before, w.git.Requests()); !maps.Equal(got, want) {
				t.Errorf("after an answer of %d the counter grew by %v, want %v", a.status, got, want)
			}
		})
	}
}
