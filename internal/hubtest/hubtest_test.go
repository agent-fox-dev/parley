package hubtest_test

import (
	"net/http"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/agent-fox-dev/parley/internal/hubtest"
)

// A call the client may not retry on another connection, such as a DELETE,
// goes to the restarted hub and is answered, even when the client has not
// yet seen the stopped hub close the connections it kept.
func TestRestart_ACallRightAfterARestartIsAnswered(t *testing.T) {
	// One busy processor delays the client's reading of the close, as a
	// loaded machine does; the binary runs no other test at the same time.
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	busy := func() (stop func()) {
		var done atomic.Bool
		t.Cleanup(func() { done.Store(true) })
		go func() {
			for !done.Load() {
			}
		}()
		return func() { done.Store(true) }
	}
	h := hubtest.Start(t, nil)
	for range 20 {
		if status, body := h.Call(t, http.MethodGet, "/readyz", "", nil); status != http.StatusOK {
			t.Fatalf("GET /readyz = %d %s, want 200", status, body)
		}
		stop := busy()
		h.Restart(t)
		status, body := h.Call(t, http.MethodDelete, "/readyz", "", nil)
		stop()
		if status != http.StatusMethodNotAllowed {
			t.Fatalf("DELETE /readyz = %d %s, want 405 from the restarted hub", status, body)
		}
	}
}
