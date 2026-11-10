// Package hub composes parleyd: it reads the configuration, opens the data,
// runs the bootstrap and the start hooks, starts the maintenance loop and
// the background work, serves, and stops within a bounded time. Every
// feature package plugs into the Composition — its configuration keys,
// schema steps, start hooks, background work, maintenance tasks, routes and
// the checks of the request pipeline — in a file of its own in this
// package; cmd/parleyd only calls Run.
package hub

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/maintenance"
	"github.com/agent-fox-dev/parley/internal/scrub"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/store"
)

// DefaultRequestGrace is how long in-flight requests get to finish once the
// hub is told to stop.
const DefaultRequestGrace = 15 * time.Second

// Options are the inputs of Run. Args, Env, WorkDir, Stdout and Stderr are
// the process's; the rest let tests control what they must without
// changing behaviour.
type Options struct {
	Args    []string
	Env     config.LookupEnv
	WorkDir string
	Stdout  io.Writer
	Stderr  io.Writer

	// Compose adds registrations after the features' own.
	Compose func(*Composition)
	// Port replaces [server].port.
	Port *int
	// Now is the hub's clock; nil means time.Now.
	Now func() time.Time
	// RequestGrace replaces DefaultRequestGrace.
	RequestGrace time.Duration
	// OnRuntime is called once the stores are open, before the bootstrap.
	OnRuntime func(*Runtime)
	// MaintenanceInterval replaces maintenance.Interval between passes.
	MaintenanceInterval time.Duration
	// TotalGrace replaces DefaultTotalGrace, LoopGrace DefaultLoopGrace.
	TotalGrace time.Duration
	LoopGrace  time.Duration
}

// Option changes Options; cmd/parleyd's tests pass them through its run
// function.
type Option func(*Options)

// Runtime is what a running hub hands to the features' hooks, tasks and
// server functions.
type Runtime struct {
	Config   *config.Config
	Store    *store.Store
	Server   *server.Server
	Logger   *slog.Logger
	Scrubber *scrub.Scrubber
	Now      func() time.Time

	shutdown chan struct{}
}

// StartHook is a piece of durable work resumed at start, before the
// listener is bound.
type StartHook struct {
	Name string
	Run  func(ctx context.Context, rt *Runtime) error
}

// Task is a maintenance task.
type Task struct {
	Name string
	Run  func(ctx context.Context, rt *Runtime) error
}

// Composition is what the features register on before the hub starts.
type Composition struct {
	// Config is the configuration key registry.
	Config *config.Registry

	steps   []store.Step
	hooks   []StartHook
	tasks   []Task
	work    []Work
	servers []func(*Runtime, *server.Server) error
}

func newComposition() *Composition {
	return &Composition{Config: config.NewRegistry()}
}

// AddStep registers a schema step.
func (c *Composition) AddStep(s store.Step) { c.steps = append(c.steps, s) }

// AddStartHook registers a start hook; hooks run in registration order,
// each to completion, after the bootstrap and before the listener is bound,
// and the first error stops the start.
func (c *Composition) AddStartHook(name string, fn func(ctx context.Context, rt *Runtime) error) {
	c.hooks = append(c.hooks, StartHook{Name: name, Run: fn})
}

// AddTask registers a maintenance task under one of the names of
// maintenance.Order, or another name that then runs after them; a name
// registered twice stops the start.
func (c *Composition) AddTask(name string, fn func(ctx context.Context, rt *Runtime) error) {
	c.tasks = append(c.tasks, Task{Name: name, Run: fn})
}

// OnServer registers a function that mounts routes and installs the checks
// of the request pipeline on the HTTP server before it listens.
func (c *Composition) OnServer(fn func(rt *Runtime, srv *server.Server) error) {
	c.servers = append(c.servers, fn)
}

// compose registers what the hub itself needs; each feature package adds
// one call here to its own compose function.
func compose(c *Composition) {
	c.AddStep(idempotencyStep)
	c.AddStep(identity.AdminSettingsStep)
	composeIdentity(c)
}

const usage = `Usage: parleyd [flags]
       parleyd backup <directory>

parleyd is the Parley hub server. It reads config.toml, opens its data,
serves until it receives SIGTERM or SIGINT, and stops within 30 seconds.

Flags:
  --admin-email <address>   on a new hub, store the admin's e-mail, write a
                            first admin token to admin_token and exit
  --reset-admin-token       write a new admin token, replacing the old one,
                            and exit
  -h, --help                print this help and exit

Commands:
  backup <directory>        write a consistent snapshot of the data into an
                            empty directory while the hub runs
`

// flags are the parsed arguments.
type flags struct {
	adminEmail string
	resetToken bool
	backup     string // the target directory of parleyd backup, or ""
}

// parseArgs parses the arguments; a non-nil exit status means the run ends
// with it, having printed usage.
func parseArgs(args []string, stdout, stderr io.Writer) (flags, *int) {
	var f flags
	fs := flag.NewFlagSet("parleyd", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	fs.StringVar(&f.adminEmail, "admin-email", "", "")
	fs.BoolVar(&f.resetToken, "reset-admin-token", false, "")
	exit := func(code int) *int { return &code }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, usage)
			return f, exit(0)
		}
		fmt.Fprintf(stderr, "parleyd: %v\n\n%s", err, usage)
		return f, exit(2)
	}
	rest := fs.Args()
	switch {
	case len(rest) == 0:
		return f, nil
	case rest[0] != "backup":
		fmt.Fprintf(stderr, "parleyd: unexpected argument %q\n\n%s", rest[0], usage)
		return f, exit(2)
	case len(rest) != 2:
		fmt.Fprintf(stderr, "parleyd: backup takes exactly one directory\n\n%s", usage)
		return f, exit(2)
	}
	if f.adminEmail != "" || f.resetToken {
		fmt.Fprintf(stderr, "parleyd: --admin-email and --reset-admin-token cannot be used with backup\n\n%s", usage)
		return f, exit(2)
	}
	f.backup = rest[1]
	return f, nil
}

// hubRun is one run of the hub.
type hubRun struct {
	opts     Options
	flags    flags
	comp     *Composition
	scrubber *scrub.Scrubber
	logger   *slog.Logger
	cfg      *config.Config
	st       *store.Store
	rt       *Runtime
	tasks    *maintenance.Registry
	now      func() time.Time
}

// Run runs parleyd with the given options until it ends by itself or ctx is
// cancelled (the signal), and returns the exit status: 0 for a clean run, 1
// for a failure, 2 for a usage error.
func Run(ctx context.Context, opts Options) int {
	f, exit := parseArgs(opts.Args, opts.Stdout, opts.Stderr)
	if exit != nil {
		return *exit
	}
	r := &hubRun{opts: opts, flags: f, comp: newComposition(), scrubber: scrub.New(), now: opts.Now}
	if r.now == nil {
		r.now = time.Now
	}
	r.logger = NewLogger(opts.Stderr, "info", r.scrubber)
	if r.flags.backup != "" {
		return r.backup(ctx)
	}
	return r.serve(ctx)
}

// fail ends a start that did not reach the listener: it closes what was
// opened and writes the one error line.
func (r *hubRun) fail(err error) int {
	if r.st != nil {
		_ = r.st.Close() // the start already failed; its cause is the line
	}
	r.logger.Error("parleyd cannot start", "error", err.Error())
	return 1
}

// setUp loads and validates the configuration and builds the logger.
func (r *hubRun) setUp() error {
	compose(r.comp)
	if r.opts.Compose != nil {
		r.opts.Compose(r.comp)
	}
	if err := r.registerTasks(); err != nil {
		return err
	}
	cfg, err := r.comp.Config.Load(r.opts.Env, r.opts.WorkDir)
	if err != nil {
		return err
	}
	for _, p := range cfg.OAuth.Providers {
		r.scrubber.Add(p.ClientSecret)
	}
	if r.opts.Port != nil {
		cfg.Server.Port = *r.opts.Port
	}
	r.cfg = cfg
	r.logger = NewLogger(r.opts.Stderr, cfg.Logging.Level, r.scrubber)
	slog.SetDefault(r.logger)
	return nil
}

// serve runs the stages in their fixed order — configuration, data,
// bootstrap, start hooks, maintenance, listener — and stops at the first
// failure.
func (r *hubRun) serve(ctx context.Context) int {
	if err := r.setUp(); err != nil {
		return r.fail(err)
	}
	st, err := store.Open(ctx, store.Options{DatabasePath: r.cfg.Database.Path, WorkspaceRoot: r.cfg.Workspace.Path}, r.comp.steps)
	if err != nil {
		return r.fail(err)
	}
	r.st = st
	r.rt = &Runtime{Config: r.cfg, Store: st, Logger: r.logger, Scrubber: r.scrubber, Now: r.now, shutdown: make(chan struct{})}
	if r.opts.OnRuntime != nil {
		r.opts.OnRuntime(r.rt)
	}
	if done, code, err := r.bootstrap(ctx); err != nil {
		return r.fail(err)
	} else if done {
		r.closeStore()
		return code
	}
	for _, h := range r.comp.hooks {
		if err := h.Run(ctx, r.rt); err != nil {
			return r.fail(fmt.Errorf("start hook %s failed: %w", h.Name, err))
		}
	}

	workCtx, stopWork := context.WithCancel(context.WithoutCancel(ctx))
	defer stopWork()
	pieces := []running{r.startMaintenance(workCtx)}
	pieces = append(pieces, r.startWork(workCtx)...)

	srv, err := r.buildServer()
	if err != nil {
		stopWork()
		r.waitStopped(time.Now(), pieces)
		return r.fail(err)
	}

	// The signal (ctx) fires the shutdown notification, cancels the
	// background work and the loop, and then stops the server, which stops
	// accepting connections and gives requests their grace.
	serveCtx, stopServe := context.WithCancel(context.WithoutCancel(ctx))
	defer stopServe()
	signalled := make(chan time.Time, 1)
	go func() {
		select {
		case <-ctx.Done():
		case <-serveCtx.Done():
			return
		}
		at := time.Now()
		close(r.rt.shutdown)
		stopWork()
		stopServe()
		signalled <- at
	}()
	err = srv.Serve(serveCtx, func(bound net.Addr) {
		_, port, _ := net.SplitHostPort(bound.String())
		r.logger.Info("listening",
			"addr", net.JoinHostPort(r.cfg.Server.Bind, port),
			"config", r.cfg.Path,
			"data_dir", r.cfg.DataDir())
	})
	if err != nil {
		stopServe()
		stopWork()
		r.waitStopped(time.Now(), pieces)
		return r.fail(fmt.Errorf("cannot listen on %s: %w", listenAddr(r.cfg), err))
	}
	r.waitStopped(<-signalled, pieces)
	r.closeStore()
	return 0
}

// closeStore closes the stores, last, and logs a failure to do so.
func (r *hubRun) closeStore() {
	if err := r.st.Close(); err != nil {
		r.logger.Error("closing the stores failed", "error", err.Error())
	}
}

// startMaintenance starts the maintenance loop in the background, after the
// start hooks and without waiting for its first pass.
func (r *hubRun) startMaintenance(ctx context.Context) running {
	interval := maintenance.Interval
	if r.opts.MaintenanceInterval > 0 {
		interval = r.opts.MaintenanceInterval
	}
	ticker := time.NewTicker(interval)
	loop := running{name: "maintenance", grace: r.loopGrace(), done: make(chan struct{})}
	go func() {
		defer close(loop.done)
		defer ticker.Stop()
		maintenance.New(r.tasks, r.logger).Run(ctx, ticker.C)
	}()
	return loop
}

// registerTasks puts the composition's tasks into the loop's registry; a
// refused registration is a start error.
func (r *hubRun) registerTasks() error {
	r.tasks = maintenance.NewRegistry()
	for _, t := range r.comp.tasks {
		run := t.Run
		if err := r.tasks.Register(t.Name, func(ctx context.Context) error { return run(ctx, r.rt) }); err != nil {
			return err
		}
	}
	return nil
}

func listenAddr(cfg *config.Config) string {
	return net.JoinHostPort(cfg.Server.Bind, strconv.Itoa(cfg.Server.Port))
}

// ServerOptions builds the HTTP server's options from the configuration:
// the listen address, the body limit, the requests per minute, health-probe
// logging, and one readiness probe that fails when either store does not
// answer a query.
func ServerOptions(cfg *config.Config, st *store.Store) server.Options {
	opts := server.DefaultOptions()
	opts.Addr = listenAddr(cfg)
	opts.BodyLimit = cfg.Server.MaxBodySize
	opts.RequestsPerMinute = cfg.Limits.RequestsPerMinute
	opts.LogHealthProbes = cfg.Logging.LogHealthProbes
	opts.ReadinessProbes = []server.Probe{st.Ping}
	return opts
}

// buildServer builds the HTTP server and lets every registered function
// mount its routes and checks.
func (r *hubRun) buildServer() (*server.Server, error) {
	opts := ServerOptions(r.cfg, r.st)
	opts.ShutdownGrace = r.requestGrace()
	opts.Now = r.now
	opts.Logger = r.logger
	opts.IdempotencyStore = idempotencyStore{st: r.st}
	srv := server.New(opts)
	r.rt.Server = srv
	for _, fn := range r.comp.servers {
		if err := fn(r.rt, srv); err != nil {
			return nil, err
		}
	}
	return srv, nil
}
