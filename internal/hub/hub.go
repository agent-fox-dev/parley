// Package hub composes the parleyd process. Run parses parleyd's arguments,
// loads the configuration, opens the stores, runs the start hooks, starts the
// maintenance loop and serves until it is stopped, in that fixed order.
//
// It is the one place where the hub's packages plug in: each registers its
// configuration keys, schema steps, start hooks, background work,
// maintenance tasks, routes and pipeline checks on a Composition, from a file
// of its own in this package (see
// docs/adr/01-compose-the-hub-in-one-package.md).
package hub

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/maintenance"
	"github.com/agent-fox-dev/parley/internal/scrub"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/store"
)

// DefaultRequestGrace is how long in-flight requests get to finish on a stop.
const DefaultRequestGrace = 15 * time.Second

// DefaultStopLimit is the longest a stop waits for anything, counted from
// the signal; then the stores are closed and Run returns.
const DefaultStopLimit = 30 * time.Second

// DefaultLoopGrace is how long a stop waits for the maintenance loop to end
// once its context is cancelled.
const DefaultLoopGrace = 15 * time.Second

// parts compose the hub's own packages, in this order, before
// Options.Compose. A package that plugs into the hub writes its function in a
// file of its own in this package and appends it here, so that the order of
// every package's schema steps, start hooks and maintenance tasks is written
// down in one place.
var parts = []func(*Composition){
	composeIdempotency,
	composeBootstrap,
	composeIdentity,
}

// Options are the inputs of Run. The process gives Args, Env, WorkDir,
// Stdout and Stderr; the other fields let tests control what they must
// without changing what the hub does, and are zero in parleyd.
type Options struct {
	// Args are the command-line arguments, without the program name.
	Args []string
	// Env looks up environment variables; nil is an empty environment.
	Env config.Env
	// WorkDir is the working directory, where config.toml and the default
	// data directory are looked for when the environment does not say.
	WorkDir string
	// Stdout receives the usage for --help; nil discards it.
	Stdout io.Writer
	// Stderr receives the log lines and usage errors; nil discards them.
	Stderr io.Writer

	// Compose, when set, adds parts after the hub's own.
	Compose func(*Composition)
	// Port, when set, replaces [server].port.
	Port *int
	// Now is the hub's clock; nil means time.Now.
	Now func() time.Time
	// RequestGrace replaces DefaultRequestGrace when it is not zero.
	RequestGrace time.Duration
	// MaintenanceInterval replaces maintenance.Interval, the time between two
	// maintenance passes, when it is not zero.
	MaintenanceInterval time.Duration
	// StopLimit replaces DefaultStopLimit, the longest a stop waits, when it
	// is not zero.
	StopLimit time.Duration
	// LoopGrace replaces DefaultLoopGrace, how long a stop waits for the
	// maintenance loop, when it is not zero.
	LoopGrace time.Duration
}

// Runtime is what the hub hands the parts it runs: the loaded configuration,
// the open stores, the logger, the scrubber and the clock.
type Runtime struct {
	Config   *config.Config
	Store    *store.Store
	Logger   *slog.Logger
	Scrubber *scrub.Scrubber
	Now      func() time.Time
}

// namedFunc is a start hook.
type namedFunc struct {
	name string
	fn   func(context.Context, *Runtime) error
}

type mount struct {
	pattern string
	handler http.Handler
}

// Composition collects what the hub's packages register before Run starts
// the hub. Its methods are called from the parts, one goroutine, before the
// configuration is loaded. A registration that is refused is remembered and
// stops the start with exit status 1, before anything is opened.
type Composition struct {
	registry      *config.Registry
	steps         []store.Step
	hooks         []namedFunc
	tasks         *maintenance.Registry
	routes        []server.Route
	mounts        []mount
	authenticator server.Authenticator
	bindingGuard  server.Check
	scopeCheck    server.Check
	accessChecks  []server.Check
	works         []backgroundWork
	runtime       *Runtime
	shutdown      chan struct{}
	errs          []error
}

func newComposition() *Composition {
	return &Composition{registry: config.NewRegistry(), tasks: maintenance.NewRegistry(), runtime: &Runtime{}, shutdown: make(chan struct{})}
}

// Runtime returns the runtime the hub fills once the stores are open, before
// the first start hook runs and before any request is served. A route
// handler or a check keeps it and reads it when it is called.
func (c *Composition) Runtime() *Runtime { return c.runtime }

// RegisterKey adds a configuration key (see config.Registry.Register).
func (c *Composition) RegisterKey(k config.Key) {
	if err := c.registry.Register(k); err != nil {
		c.errs = append(c.errs, err)
	}
}

// RegisterSection adds a configuration section checked as a whole (see
// config.Registry.RegisterSection).
func (c *Composition) RegisterSection(name string, validate func(map[string]any) error) {
	if err := c.registry.RegisterSection(name, validate); err != nil {
		c.errs = append(c.errs, err)
	}
}

// AddSchemaStep adds a schema step; the stores apply the steps in the order
// they were added (see store.Step).
func (c *Composition) AddSchemaStep(s store.Step) {
	c.steps = append(c.steps, s)
}

// AddStartHook adds a start hook. The hooks run in the order they were added,
// each to completion, after the stores are open and before the listener is
// bound; an error from one stops the start, and no later hook runs. The
// error is logged with the hook's name.
func (c *Composition) AddStartHook(name string, fn func(context.Context, *Runtime) error) {
	switch {
	case name == "":
		c.errs = append(c.errs, errors.New("a start hook has no name"))
		return
	case fn == nil:
		c.errs = append(c.errs, fmt.Errorf("start hook %q has no function", name))
		return
	}
	for _, h := range c.hooks {
		if h.name == name {
			c.errs = append(c.errs, fmt.Errorf("start hook %q is registered twice", name))
			return
		}
	}
	c.hooks = append(c.hooks, namedFunc{name, fn})
}

// AddMaintenanceTask adds a maintenance task under name (see
// maintenance.Registry.Register). The hub's maintenance loop runs one pass
// right after the start hooks and one every hour; a pass runs the tasks one
// after the other, those under the names the maintenance package knows in its
// fixed order, then the others in the order they were added. A task's error
// or panic is logged with its name and does not stop the others; its context
// is cancelled when the hub stops.
func (c *Composition) AddMaintenanceTask(name string, fn func(context.Context, *Runtime) error) {
	var task maintenance.Task
	if fn != nil {
		rt := c.runtime
		task = func(ctx context.Context) error { return fn(ctx, rt) }
	}
	if err := c.tasks.Register(name, task); err != nil {
		c.errs = append(c.errs, err)
	}
}

// AddRoute adds a REST API route (see server.Server.Register). An invalid or
// duplicate route stops the start.
func (c *Composition) AddRoute(r server.Route) {
	c.routes = append(c.routes, r)
}

// Handle mounts a handler outside the REST API (see server.Server.Handle).
func (c *Composition) Handle(pattern string, h http.Handler) {
	c.mounts = append(c.mounts, mount{pattern, h})
}

// SetAuthenticator installs the pipeline's authenticator.
func (c *Composition) SetAuthenticator(a server.Authenticator) { c.authenticator = a }

// SetBindingGuard installs the pipeline's binding guard.
func (c *Composition) SetBindingGuard(check server.Check) { c.bindingGuard = check }

// SetScopeCheck installs the pipeline's scope check.
func (c *Composition) SetScopeCheck(check server.Check) { c.scopeCheck = check }

// AddAccessCheck adds an access check after those added before it.
func (c *Composition) AddAccessCheck(check server.Check) {
	c.accessChecks = append(c.accessChecks, check)
}

// install registers the routes, mounts and checks on srv. server.Server
// panics on an invalid or duplicate route, a programming error of the part
// that added it; here it is a start error.
func (c *Composition) install(srv *server.Server) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("compose the routes: %v", p)
		}
	}()
	for _, r := range c.routes {
		srv.Register(r)
	}
	for _, m := range c.mounts {
		srv.Handle(m.pattern, m.handler)
	}
	if c.authenticator != nil {
		srv.SetAuthenticator(c.authenticator)
	}
	if c.bindingGuard != nil {
		srv.SetBindingGuard(c.bindingGuard)
	}
	if c.scopeCheck != nil {
		srv.SetScopeCheck(c.scopeCheck)
	}
	for _, check := range c.accessChecks {
		srv.AddAccessCheck(check)
	}
	return nil
}

// check returns the registrations the composition refused, and installs the
// routes on a server of its own so that a bad route stops the start before
// anything is opened.
func (c *Composition) check() error {
	if err := errors.Join(c.errs...); err != nil {
		return err
	}
	return c.install(server.New(server.Options{Logger: slog.New(slog.DiscardHandler)}))
}

// ServerOptions returns the options of the hub's HTTP server that come from
// the configuration and the stores: the body limit, the rate limit, the
// health-probe logging, one readiness probe that fails while either store
// does not answer a query, and the idempotency records kept in the
// transactional store.
func ServerOptions(cfg *config.Config, st *store.Store) server.Options {
	return server.Options{
		MaxBodySize:       cfg.Server.MaxBodySize,
		RequestsPerMinute: cfg.Limits.RequestsPerMinute,
		// The server reads a RequestsPerMinute of 0 as its default; a
		// configured 0 turns the limit off.
		RateLimitDisabled: cfg.Limits.RequestsPerMinute == 0,
		LogHealthProbes:   cfg.Logging.LogHealthProbes,
		ReadinessProbes:   []server.Probe{st.Ping},
		IdempotencyStore:  newIdempotencyStore(st),
	}
}

const usage = `Usage: parleyd [flags]
       parleyd backup <directory>

parleyd is the Parley hub. It reads config.toml from $XDG_CONFIG_HOME or the
working directory, opens its data, listens for HTTP on [server].bind and
[server].port, and runs until it receives SIGTERM or SIGINT.

Commands:
  backup <directory>   write a snapshot of the hub's data into an empty directory

Flags:
  --admin-email <address>   on a new hub, store the admin's e-mail, write an
                            admin token file and exit
  --reset-admin-token       replace the admin token, write it to the token
                            file and exit
  -h, --help                print this help and exit
`

// command is what the arguments ask for.
type command struct {
	adminEmail      string
	resetAdminToken bool
	backup          bool
	backupDir       string
}

// parseArgs reads the arguments. It reports false when Run must return
// status at once: after the usage for --help, or after a usage error.
func parseArgs(args []string, stdout, stderr io.Writer) (cmd command, status int, ok bool) {
	fs := flag.NewFlagSet("parleyd", flag.ContinueOnError)
	// The flag package's own messages name a flag without the dashes it was
	// given with, so parseArgs writes every message itself.
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	fs.StringVar(&cmd.adminEmail, "admin-email", "", "")
	fs.BoolVar(&cmd.resetAdminToken, "reset-admin-token", false, "")
	err := fs.Parse(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprint(stdout, usage)
		return cmd, 0, false
	case err != nil:
		fmt.Fprintf(stderr, "parleyd: argument %q: %v\n\n%s", rejectedArgument(args, fs.Args()), err, usage)
		return cmd, 2, false
	}
	rest := fs.Args()
	switch {
	case len(rest) == 0:
		return cmd, 0, true
	case rest[0] != "backup":
		fmt.Fprintf(stderr, "parleyd: unexpected argument %q\n\n%s", rest[0], usage)
		return cmd, 2, false
	case len(rest) != 2:
		fmt.Fprintf(stderr, "parleyd: backup takes one directory, got %d arguments\n\n%s", len(rest)-1, usage)
		return cmd, 2, false
	case cmd.adminEmail != "" || cmd.resetAdminToken:
		fmt.Fprintf(stderr, "parleyd: backup does not take --admin-email or --reset-admin-token\n\n%s", usage)
		return cmd, 2, false
	}
	cmd.backup, cmd.backupDir = true, rest[1]
	return cmd, 0, true
}

// rejectedArgument is the argument a failed fs.Parse stopped at. Parse drops
// each argument it reads, the rejected flag included, so it is the last one
// read; when Parse read none, it is the first.
func rejectedArgument(args, rest []string) string {
	if n := len(args) - len(rest); n > 0 {
		return args[n-1]
	}
	return args[0]
}

// run is one call of Run.
type run struct {
	opts     Options
	cmd      command
	logger   *slog.Logger
	level    *slog.LevelVar
	scrubber *scrub.Scrubber
}

// Run is parleyd: it parses opts.Args and starts the hub, or with the
// arguments backup <directory> writes a snapshot of the hub's data into the
// directory, and returns the exit status: 0 after --help, after the bootstrap
// wrote the admin token file, after a stop, or after a written backup; 1
// when the start or the backup fails; 2 for a usage error. A usage error
// loads, opens and creates nothing.
//
// The start runs in this order and stops at the first failure: load the
// configuration, open the stores, the bootstrap (which may end the run
// after writing the admin token file), the start hooks in the order
// they were added, bind the listener, start the maintenance loop and the
// background work, then write the listening line and serve. A failure is one
// error line on Stderr; what was opened is closed. The hub serves until ctx
// is cancelled or the process receives SIGTERM or SIGINT. Then it stops
// accepting connections, closes the shutdown notification (see
// Composition.ShuttingDown), cancels the maintenance loop and the
// background work, and waits for all of them and for the requests in flight
// at the same time, each up to its grace and nothing beyond the stop limit;
// it closes the stores last and returns 0. Further signals during the stop
// change nothing.
func Run(ctx context.Context, opts Options) int {
	stdout, stderr := orDiscard(opts.Stdout), orDiscard(opts.Stderr)
	cmd, status, ok := parseArgs(opts.Args, stdout, stderr)
	if !ok {
		return status
	}
	// Once, so that every path resolved against it is absolute: a relative
	// directory would make the checks of where a path lies fail open. When
	// it cannot be made absolute, the configuration load reports it.
	if wd, err := filepath.Abs(opts.WorkDir); err == nil {
		opts.WorkDir = wd
	}
	r := &run{opts: opts, cmd: cmd, level: new(slog.LevelVar), scrubber: scrub.New()}
	// The logger exists before the configuration is loaded, at info, so that
	// a configuration error is a JSON line like every other.
	r.logger = newLogger(stderr, r.level, r.scrubber)
	slog.SetDefault(r.logger)
	if cmd.backup {
		return r.backup(ctx)
	}
	return r.serve(ctx)
}

func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

// fail writes the one error line of a failed start and returns its status.
func (r *run) fail(err error) int {
	r.logger.Error("cannot start", "error", err.Error())
	return 1
}

// configure composes the hub and loads the configuration with the keys and
// sections the composition registered, then sets the logger's level and
// hands the scrubber the configured secrets.
func (r *run) configure() (*Composition, *config.Config, error) {
	c := newComposition()
	for _, part := range parts {
		part(c)
	}
	if r.opts.Compose != nil {
		r.opts.Compose(c)
	}
	if err := c.check(); err != nil {
		return nil, nil, err
	}
	cfg, err := c.registry.Load(r.opts.Env, r.opts.WorkDir)
	if err != nil {
		return nil, nil, err
	}
	r.level.Set(parseLevel(cfg.Logging.Level))
	for _, p := range cfg.OAuth.Providers {
		r.scrubber.Add(p.ClientSecret)
	}
	return c, cfg, nil
}

// serve composes the hub, loads the configuration and opens the stores, and
// closes them once the hub has stopped or its start has failed.
func (r *run) serve(ctx context.Context) int {
	c, cfg, err := r.configure()
	if err != nil {
		return r.fail(err)
	}
	st, err := store.Open(ctx, store.Options{DatabasePath: cfg.Database.Path, WorkspaceRoot: cfg.Workspace.Path}, c.steps)
	if err != nil {
		return r.fail(err)
	}
	status := r.start(ctx, c, cfg, st)
	if err := st.Close(); err != nil {
		r.logger.Error("cannot close the stores", "error", err.Error())
	}
	return status
}

// start runs the stages after the stores are open and serves.
func (r *run) start(ctx context.Context, c *Composition, cfg *config.Config, st *store.Store) int {
	now := r.opts.Now
	if now == nil {
		now = time.Now
	}
	rt := c.runtime
	*rt = Runtime{Config: cfg, Store: st, Logger: r.logger, Scrubber: r.scrubber, Now: now}

	done, err := bootstrap(ctx, rt, r.cmd)
	if err != nil {
		return r.fail(err)
	}
	if done {
		return 0
	}
	for _, h := range c.hooks {
		if err := h.fn(ctx, rt); err != nil {
			return r.fail(fmt.Errorf("start hook %q: %w", h.name, err))
		}
	}

	// Registered before the bind, so that a signal that arrives once the
	// listening line is out stops the hub rather than the process, and kept
	// until Run returns, so that a further signal during the stop does not
	// end the process either: signal.NotifyContext would restore the default
	// action on the first one. Nothing reads the channel after the first
	// signal, so the later ones are dropped and change nothing.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, os.Interrupt)
	defer signal.Stop(signals)

	port := cfg.Server.Port
	if r.opts.Port != nil {
		port = *r.opts.Port
	}
	addr := net.JoinHostPort(cfg.Server.Bind, strconv.Itoa(port))
	// The listener is bound before the maintenance loop starts, so that a
	// bind failure stops the start before any background work has run.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		r.logger.Error("cannot listen", "addr", addr, "error", err.Error())
		return 1
	}

	opts := ServerOptions(cfg, st)
	opts.Addr = addr
	opts.Listener = ln
	opts.ShutdownGrace = r.opts.RequestGrace
	if opts.ShutdownGrace == 0 {
		opts.ShutdownGrace = DefaultRequestGrace
	}
	opts.Now = now
	opts.Logger = r.logger
	opts.OnListen = func(bound net.Addr) {
		r.logger.Info("listening", "addr", bound.String(), "config", cfg.Path, "data_dir", cfg.DataDir())
	}
	srv := server.New(opts)
	if err := c.install(srv); err != nil {
		_ = ln.Close()
		return r.fail(err)
	}

	// The maintenance loop, the background work and the server share one
	// context, so that the stop cancels them at the same moment.
	serveCtx, stop := context.WithCancel(ctx)
	defer stop()
	waits := []wait{{"maintenance loop", r.startMaintenance(serveCtx, c.tasks), r.loopGrace()}}
	waits = append(waits, startBackgroundWork(serveCtx, c.works, rt)...)
	served := make(chan struct{})
	var serveErr error
	go func() {
		defer close(served)
		serveErr = srv.Serve(serveCtx)
	}()

	select {
	case <-signals:
	case <-serveCtx.Done():
	case <-served:
	}
	// The server gives in-flight requests the request grace itself and
	// closes their connections when it has passed, so the stop waits for it
	// only up to the stop limit.
	r.stop(stop, c.shutdown, append([]wait{{"requests", served, r.stopLimit()}}, waits...))
	select {
	case <-served:
		if serveErr != nil {
			r.logger.Error("server failed", "error", serveErr.Error())
			return 1
		}
	default:
	}
	return 0
}

// startMaintenance starts the maintenance loop over tasks in the background,
// without waiting for its first pass, with a ticker of maintenance.Interval
// or Options.MaintenanceInterval. The returned channel is closed once the
// loop has returned after ctx is cancelled.
func (r *run) startMaintenance(ctx context.Context, tasks *maintenance.Registry) <-chan struct{} {
	interval := r.opts.MaintenanceInterval
	if interval <= 0 {
		interval = maintenance.Interval
	}
	loop := maintenance.New(tasks, r.logger)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		loop.Run(ctx, ticker.C)
	}()
	return done
}
