package hubtest_test

import (
	"net"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// A restart serves the hub on the port it had, even when another listener
// tries to take that port while the hub is down, as a test binary running
// beside it may: whether the restart stops the hub, or the test stopped it
// before and waits to restart it.
func TestRestart_HoldsThePortWhileTheHubIsDown(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stopFirst bool
	}{
		{"the restart stops the hub", false},
		{"the test stopped the hub before", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := hubtest.Start(t, nil)
			addr, _ := h.Listening["addr"].(string)
			done := make(chan struct{})
			rival := make(chan net.Listener, 1)
			var stop sync.Once
			t.Cleanup(func() {
				stop.Do(func() { close(done) })
				select {
				case ln := <-rival:
					ln.Close()
				default:
				}
			})
			go func() {
				for {
					select {
					case <-done:
						return
					default:
					}
					if ln, err := net.Listen("tcp", addr); err == nil {
						rival <- ln
						return
					}
				}
			}()
			if tc.stopFirst {
				h.Stop()
				if code := h.ExitCode(t, 10*time.Second); code != 0 {
					t.Fatalf("the hub stopped with %d, want 0", code)
				}
				if ln, err := net.Listen("tcp", addr); err == nil {
					ln.Close()
					t.Fatalf("another listener bound %s after the test stopped the hub", addr)
				}
			}
			h.Restart(t)
			stop.Do(func() { close(done) })
			select {
			case ln := <-rival:
				ln.Close()
				t.Fatalf("another listener bound %s while the hub was down", addr)
			default:
			}
			if got, _ := h.Listening["addr"].(string); got != addr {
				t.Errorf("the restarted hub listens on %s, want %s", got, addr)
			}
			if status, body := h.Call(t, http.MethodGet, "/readyz", "", nil); status != http.StatusOK {
				t.Errorf("GET /readyz after the restart = %d %s, want 200", status, body)
			}
		})
	}
}
