package hub_test

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/store"
)

// The tests that send SIGTERM or SIGINT signal the test process, and every
// hub that serves in it catches them, so they are not parallel: the testing
// package holds every parallel test of the package at its t.Parallel call
// until the sequential tests have ended, so no other hub runs while they
// signal. A signal is sent only after the listening line, when Run has
// registered for it; before that it would end the test binary.

// sendSignal sends sig to the test process.
func sendSignal(t *testing.T, sig syscall.Signal) {
	t.Helper()
	if err := syscall.Kill(os.Getpid(), sig); err != nil {
		t.Fatalf("send %v: %v", sig, err)
	}
}

// scaledLimits are time limits a test can wait for: the request grace, the
// stop limit and the loop's grace, all well under a second.
func scaledLimits(opts hub.Options) hub.Options {
	opts.RequestGrace = 150 * time.Millisecond
	opts.StopLimit = 300 * time.Millisecond
	opts.LoopGrace = 150 * time.Millisecond
	return opts
}

// ignoringWork registers background work that ignores its context and runs
// until release is closed.
func ignoringWork(grace time.Duration, release <-chan struct{}) func(*hub.Composition) {
	return func(c *hub.Composition) {
		c.AddBackgroundWork("ignores its context", grace, func(context.Context, *hub.Runtime) { <-release })
	}
}

// released returns a channel that is closed when the test and its hubs have
// ended, for work and handlers that a stop abandons.
func released(t *testing.T) <-chan struct{} {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	return release
}

func TestRun_StopsOnASignalAfterTheBackgroundWorkHasEnded(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			var rt *hub.Runtime
			pinged := make(chan error, 1)
			r := startHub(t, hub.Options{WorkDir: workDir(t, localConfig), Compose: func(c *hub.Composition) {
				rt = c.Runtime()
				c.AddBackgroundWork("w", 2*time.Second, func(ctx context.Context, rt *hub.Runtime) {
					<-ctx.Done()
					pinged <- rt.Store.Ping(context.Background())
				})
			}})

			sendSignal(t, sig)
			if code := r.wait(t, time.Second); code != 0 {
				t.Errorf("Run returned %d, want 0", code)
			}
			select {
			case err := <-pinged:
				if err != nil {
					t.Errorf("the background work's ping after its cancellation = %v, want the stores still open", err)
				}
			default:
				t.Error("the background work had not ended when Run returned")
			}
			if conn, err := net.Dial("tcp", r.addr); err == nil {
				conn.Close()
				t.Errorf("a connection to %s was accepted after Run returned", r.addr)
			}
			if err := rt.Store.Ping(context.Background()); !errors.Is(err, store.ErrClosed) {
				t.Errorf("Ping after Run returned = %v, want %v", err, store.ErrClosed)
			}
		})
	}
}

func TestRun_StopsWaitingWhenAGraceOrTheStopLimitHasPassed(t *testing.T) {
	t.Parallel()
	for name, limit := range map[string]struct{ got, want time.Duration }{
		"request grace": {hub.DefaultRequestGrace, 15 * time.Second},
		"stop limit":    {hub.DefaultStopLimit, 30 * time.Second},
		"loop grace":    {hub.DefaultLoopGrace, 15 * time.Second},
	} {
		if limit.got != limit.want {
			t.Errorf("default %s = %v, want %v", name, limit.got, limit.want)
		}
	}

	for _, tc := range []struct {
		name     string
		compose  func(c *hub.Composition, release <-chan struct{}, entered chan<- struct{})
		request  bool
		min, max time.Duration
		// abandoned is the work the stop logs it abandoned, if any.
		abandoned string
	}{
		{
			name: "background work ignores its context past its grace",
			compose: func(c *hub.Composition, release <-chan struct{}, _ chan<- struct{}) {
				ignoringWork(100*time.Millisecond, release)(c)
			},
			min: 100 * time.Millisecond, max: 1100 * time.Millisecond, abandoned: "ignores its context",
		},
		{
			name: "background work with a grace of ten minutes",
			compose: func(c *hub.Composition, release <-chan struct{}, _ chan<- struct{}) {
				ignoringWork(10*time.Minute, release)(c)
			},
			min: 300 * time.Millisecond, max: 1300 * time.Millisecond, abandoned: "ignores its context",
		},
		{
			name: "a request handler that never returns",
			compose: func(c *hub.Composition, release <-chan struct{}, entered chan<- struct{}) {
				c.Handle("GET /block", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
					entered <- struct{}{}
					<-release
				}))
			},
			request: true,
			min:     150 * time.Millisecond, max: 1150 * time.Millisecond,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			release := released(t)
			entered := make(chan struct{}, 1)
			r := startHub(t, scaledLimits(hub.Options{WorkDir: workDir(t, localConfig), Compose: func(c *hub.Composition) {
				tc.compose(c, release, entered)
			}}))
			if tc.request {
				go func() {
					if res, err := http.Get(r.url("/block")); err == nil {
						res.Body.Close()
					}
				}()
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("the request did not reach its handler within 5 s")
				}
			}

			t0 := time.Now()
			code := r.stop(t)
			elapsed := time.Since(t0)
			if code != 0 {
				t.Errorf("Run returned %d, want 0", code)
			}
			if elapsed < tc.min || elapsed >= tc.max {
				t.Errorf("Run returned %v after the stop began, want within [%v, %v)", elapsed, tc.min, tc.max)
			}
			var abandoned []any
			for _, rec := range r.stderr.records(t) {
				if rec["msg"] == "abandoned at the stop" {
					if rec["level"] != "WARN" {
						t.Errorf("level of %v = %v, want WARN", rec, rec["level"])
					}
					abandoned = append(abandoned, rec["work"])
				}
			}
			var want []any
			if tc.abandoned != "" {
				want = []any{tc.abandoned}
			}
			if !reflect.DeepEqual(abandoned, want) {
				t.Errorf("the stop logged it abandoned %v, want %q", abandoned, tc.abandoned)
			}
		})
	}
}

func TestRun_EndsALongLivedResponseAtTheShutdownNotification(t *testing.T) {
	t.Parallel()
	r := startHub(t, hub.Options{WorkDir: workDir(t, localConfig), Compose: func(c *hub.Composition) {
		c.Handle("GET /stream", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "first\n")
			_ = http.NewResponseController(w).Flush()
			<-c.ShuttingDown()
		}))
	}})

	res, err := http.Get(r.url("/stream"))
	if err != nil {
		t.Fatalf("GET /stream: %v", err)
	}
	defer res.Body.Close()
	body := bufio.NewReader(res.Body)
	if first, err := body.ReadString('\n'); err != nil || first != "first\n" || res.StatusCode != http.StatusOK {
		t.Fatalf("GET /stream = %d, first chunk %q (error %v), want 200 and \"first\\n\"", res.StatusCode, first, err)
	}

	t0 := time.Now()
	r.cancel()
	if rest, err := io.ReadAll(body); err != nil || len(rest) != 0 {
		t.Errorf("the rest of the stream = %q, error %v; want its end with no error", rest, err)
	}
	if code := r.wait(t, time.Second); code != 0 {
		t.Errorf("Run returned %d, want 0", code)
	}
	if elapsed := time.Since(t0); elapsed >= time.Second {
		t.Errorf("Run returned %v after the stop began, want under 1 s", elapsed)
	}
}

func TestRun_KeepsCommittedRowsAcrossAStopAndCallsTheStartHooksAgain(t *testing.T) {
	t.Parallel()
	dir := workDir(t, localConfig)
	hookCalls := 0
	start := func() (*running, *hub.Runtime) {
		runtimes := make(chan *hub.Runtime, 1)
		r := startHub(t, hub.Options{WorkDir: dir, Compose: func(c *hub.Composition) {
			c.AddSchemaStep(store.Step{Name: "test_a", Target: store.Main, Apply: func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, "CREATE TABLE a (n INTEGER NOT NULL)")
				return err
			}})
			c.AddStartHook("count", func(_ context.Context, rt *hub.Runtime) error {
				hookCalls++
				runtimes <- rt
				return nil
			})
		}})
		return r, <-runtimes
	}

	first, rt := start()
	ctx := context.Background()
	for i := range 100 {
		if err := rt.Store.Tx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "INSERT INTO a (n) VALUES (?)", i)
			return err
		}); err != nil {
			t.Fatalf("commit row %d: %v", i, err)
		}
	}
	if code := first.stop(t); code != 0 {
		t.Fatalf("the first hub returned %d, want 0", code)
	}

	_, rt = start()
	var n int
	if err := rt.Store.Main().QueryRowContext(ctx, "SELECT count(*) FROM a").Scan(&n); err != nil {
		t.Fatalf("count the rows: %v", err)
	}
	if n != 100 {
		t.Errorf("the second hub reads %d rows, want 100", n)
	}
	if hookCalls != 2 {
		t.Errorf("the start hook was called %d times, want 2", hookCalls)
	}
}

func TestRun_GoesOnWithTheStopWhenASecondSignalArrives(t *testing.T) {
	release := released(t)
	r := startHub(t, scaledLimits(hub.Options{WorkDir: workDir(t, localConfig), Compose: ignoringWork(200*time.Millisecond, release)}))

	t0 := time.Now()
	sendSignal(t, syscall.SIGTERM)
	time.Sleep(50 * time.Millisecond)
	sendSignal(t, syscall.SIGINT)
	code := r.wait(t, 5*time.Second)
	elapsed := time.Since(t0)
	if code != 0 {
		t.Errorf("Run returned %d, want 0", code)
	}
	if elapsed < 200*time.Millisecond || elapsed >= 1200*time.Millisecond {
		t.Errorf("Run returned %v after the first signal, want within [200ms, 1.2s)", elapsed)
	}
}
