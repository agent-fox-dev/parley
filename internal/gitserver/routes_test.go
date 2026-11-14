package gitserver_test

import (
	"net/http"
	"testing"

	"github.com/agent-fox-dev/parley/internal/hubtest"
)

func TestRoutes_RefusesRefDiscoveryWithoutAValidService(t *testing.T) {
	t.Parallel()
	w := newWorld(t, hubtest.Config{})
	for _, q := range []string{"", "service=", "service=git-archive", "service=upload-pack", "svc=git-upload-pack"} {
		t.Run(q, func(t *testing.T) {
			a := w.call(t, http.MethodGet, "/git/acme/demo.git/info/refs?"+q, basic("x", w.ownerKey))
			if a.status != http.StatusBadRequest || a.body != "invalid or missing service parameter\n" {
				t.Errorf("answer = %d %q, want 400 \"invalid or missing service parameter\\n\"", a.status, a.body)
			}
		})
	}
	if n := w.service.served(); n != 0 {
		t.Errorf("%d refused requests reached the service, want none", n)
	}
	a := w.call(t, http.MethodGet, "/git/acme/demo.git/info/refs?service=git-upload-pack&service=git-receive-pack", basic("x", w.ownerKey))
	if a.status != http.StatusOK {
		t.Fatalf("two service values = %d %q, want 200", a.status, a.body)
	}
	if x := w.service.last(t); x.Service != "git-upload-pack" || x.Need != "read" {
		t.Errorf("exchange = service %q need %q, want git-upload-pack read", x.Service, x.Need)
	}
}
