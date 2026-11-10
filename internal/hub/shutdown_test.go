package hub

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"io"
	"net"
	"net/http"
	"os/signal"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/store"
)

func TestShutdown_ASignalStopsWorkThenClosesTheStores(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			ctx, stop := signal.NotifyContext(context.Background(), sig)
			defer stop()
			var stopped, pinged atomic.Bool
			compose := withCompose(func(c *Composition) {
				c.AddWork("w", 2*time.Second, func(ctx context.Context, rt *Runtime) {
					<-ctx.Done()
					pinged.Store(rt.Store.Ping(context.Background()) == nil)
					stopped.Store(true)
				})
			})
			h := startHubWith(t, ctx, hubDir(t, loopbackConfig), nil, compose)
			rt := h.rt.Load()
			if err := syscall.Kill(syscall.Getpid(), sig); err != nil {
				t.Fatal(err)
			}
			if code := h.wait(time.Second); code != 0 {
				t.Fatalf("exit %d, want 0", code)
			}
			if !stopped.Load() || !pinged.Load() {
				t.Errorf("work stopped %v, its ping succeeded %v", stopped.Load(), pinged.Load())
			}
			if c, err := net.Dial("tcp", h.addr); err == nil {
				c.Close()
				t.Error("a connection was accepted after the hub stopped")
			}
			if err := rt.Store.Ping(context.Background()); !errors.Is(err, store.ErrClosed) {
				t.Errorf("Ping after the stop = %v, want ErrClosed", err)
			}
		})
	}
}

func TestShutdown_EndsWhenTheGracesPass(t *testing.T) {
	if DefaultRequestGrace != 15*time.Second || DefaultTotalGrace != 30*time.Second || DefaultLoopGrace != 15*time.Second {
		t.Errorf("defaults = %v %v %v, want 15 s, 30 s, 15 s", DefaultRequestGrace, DefaultTotalGrace, DefaultLoopGrace)
	}
	scaled := func(o *Options) { o.RequestGrace, o.TotalGrace = 150*time.Millisecond, 300*time.Millisecond }
	block := make(chan struct{})
	defer close(block)
	tests := []struct {
		name     string
		compose  func(*Composition)
		request  bool
		min, max time.Duration
	}{
		{"work ignoring its context, grace 100 ms", func(c *Composition) {
			c.AddWork("stubborn", 100*time.Millisecond, func(context.Context, *Runtime) { <-block })
		}, false, 100 * time.Millisecond, 1100 * time.Millisecond},
		{"work ignoring its context, grace 10 min", func(c *Composition) {
			c.AddWork("stubborn", 10*time.Minute, func(context.Context, *Runtime) { <-block })
		}, false, 300 * time.Millisecond, 1300 * time.Millisecond},
		{"a request that never ends", func(c *Composition) {
			c.OnServer(func(_ *Runtime, srv *server.Server) error {
				srv.HandleFunc("GET /stuck", func(http.ResponseWriter, *http.Request) { <-block })
				return nil
			})
		}, true, 150 * time.Millisecond, 1150 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startHub(t, hubDir(t, loopbackConfig), nil, scaled, withCompose(tt.compose))
			if tt.request {
				started := make(chan struct{})
				go func() {
					close(started)
					if resp, err := http.Get(h.url("/stuck")); err == nil {
						resp.Body.Close()
					}
				}()
				<-started
				time.Sleep(50 * time.Millisecond)
			}
			t0 := time.Now()
			h.cancel()
			if code := h.wait(5 * time.Second); code != 0 {
				t.Fatalf("exit %d, want 0", code)
			}
			if d := time.Since(t0); d < tt.min || d >= tt.max {
				t.Errorf("exit after %v, want within [%v, %v)", d, tt.min, tt.max)
			}
		})
	}
}

func TestShutdown_TheNotificationEndsALongLivedResponse(t *testing.T) {
	compose := withCompose(func(c *Composition) {
		c.OnServer(func(rt *Runtime, srv *server.Server) error {
			srv.HandleFunc("GET /stream", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
				io.WriteString(w, "first\n")
				http.NewResponseController(w).Flush()
				<-rt.ShuttingDown()
			})
			return nil
		})
	})
	h := startHub(t, hubDir(t, loopbackConfig), nil, compose)
	resp, err := http.Get(h.url("/stream"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	if line, err := br.ReadString('\n'); err != nil || line != "first\n" {
		t.Fatalf("first chunk = %q, %v", line, err)
	}
	t0 := time.Now()
	h.cancel()
	rest, err := io.ReadAll(br)
	if err != nil || resp.StatusCode != 200 {
		t.Errorf("rest of the stream = %q, %v (status %d), want a clean end", rest, err, resp.StatusCode)
	}
	if code := h.wait(2 * time.Second); code != 0 || time.Since(t0) >= time.Second {
		t.Errorf("exit %d after %v, want 0 within 1 s", code, time.Since(t0))
	}
}

func TestShutdown_CommittedWorkSurvivesTheStop(t *testing.T) {
	dir := hubDir(t, loopbackConfig)
	var hookCalls atomic.Int32
	compose := withCompose(func(c *Composition) {
		c.AddStep(store.Step{Name: "test.a", Target: store.Main, Apply: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `CREATE TABLE a (v INTEGER)`)
			return err
		}})
		c.AddStartHook("count", func(context.Context, *Runtime) error { hookCalls.Add(1); return nil })
	})
	h := startHub(t, dir, nil, compose)
	st := h.rt.Load().Store
	for i := range 100 {
		if err := st.Tx(context.Background(), func(tx *sql.Tx) error {
			_, err := tx.Exec(`INSERT INTO a VALUES (?)`, i)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if code := h.stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	h2 := startHub(t, dir, nil, compose)
	var n int
	if err := h2.rt.Load().Store.DB().QueryRow(`SELECT count(*) FROM a`).Scan(&n); err != nil || n != 100 {
		t.Errorf("rows after the restart = %d, %v; want 100", n, err)
	}
	if hookCalls.Load() != 2 {
		t.Errorf("the start hook ran %d times, want 2", hookCalls.Load())
	}
}
