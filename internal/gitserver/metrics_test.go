package gitserver_test

import (
	"maps"
	"net/http"
	"testing"

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

// The refusals of the checks are counted in
// TestRoutes_AnswersEveryEarlyRefusalWithOneLineOfText. The processor
// answers every push 200, so that the result it reports is the only thing
// that tells its pushes apart.
func TestMetrics_CountsAnExchangeOnceByItsServiceAndResult(t *testing.T) {
	t.Parallel()
	processor := &fakeProcessor{}
	w := newGitWorld(t, hubtest.Config{Compose: func(c *hub.Composition) {
		c.AdjustGitServer(func(d *gitserver.Deps) { d.Push = processor })
	}})
	const (
		refs, fetch, push   = gitserver.LabelInfoRefs, gitserver.LabelUploadPack, gitserver.LabelReceivePack
		ok, refused, failed = gitserver.ResultOK, gitserver.ResultRefused, gitserver.ResultError
	)
	rows := []struct {
		name, method, path, body string
		report                   gitserver.Result
		service                  string
		result                   gitserver.Result
	}{
		{"served ref discovery", http.MethodGet, demoRefs, "", "", refs, ok},
		{"served fetch", http.MethodPost, uploadPack, fetchRequest(w.head), "", fetch, ok},
		{"empty fetch request", http.MethodPost, uploadPack, "", "", fetch, failed},
		{"fetch request git refuses", http.MethodPost, uploadPack, "0009junk", "", fetch, failed},
		{"push the processor takes", http.MethodPost, receivePack, "0000", ok, push, ok},
		{"push the processor refuses", http.MethodPost, receivePack, "0000", refused, push, refused},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			processor.report(row.report)
			before := w.git.Requests()
			a := w.send(t, row.method, row.path, basic("x", w.ownerKey), nil, row.body)
			want := map[gitserver.RequestLabels]uint64{{Service: row.service, Result: row.result}: 1}
			if got := grown(before, w.git.Requests()); !maps.Equal(got, want) {
				t.Errorf("after an answer of %d the counter grew by %v, want %v", a.status, got, want)
			}
		})
	}
}
