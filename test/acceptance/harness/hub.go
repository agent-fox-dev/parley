// Package harness starts a Parley hub from its built binaries and drives it
// the way a user does: over HTTP, through the parley binary and through
// stock git. It also serves what surrounds a hub in a scenario: an identity
// provider, git origins, a webhook receiver and a forge. It reads nothing
// under a hub's data directory and imports no package of the hub.
package harness

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// AdminEmail is the e-mail the hub's bootstrap names: the user who signs in
// with it first, User("admin"), becomes an admin.
const AdminEmail = "admin@acceptance.test"

// How long a hub may take to start and to stop. A stop gives in-flight
// requests 15 s and the whole stop 30 s.
const (
	startLimit = 30 * time.Second
	stopLimit  = 35 * time.Second
)

// The variables a test process hands to a go test it runs itself: childVar
// marks the child, buildVar names the parent's build so the child does not
// build the binaries again.
const (
	childVar = "PARLEY_ACCEPTANCE_CHILD"
	buildVar = "PARLEY_ACCEPTANCE_BUILD_DIR"
)

// built is what every hub of the test process shares, made once: the two
// binaries, a PATH holding only git, and the certificate every httptest TLS
// server serves, which git origins and the forge use alike.
var built struct {
	once                                  sync.Once
	dir, parleyd, parley, gitPath, caFile string
	owned                                 bool
	err                                   error
}

// Main runs the suite's tests at most four at a time unless the command
// line says otherwise, since every scenario's hub and git compete for the
// cores with the rest of the repository's tests, and removes the build
// afterwards. TestMain returns its result to os.Exit.
func Main(m *testing.M) int {
	limitParallel(4)
	code := m.Run()
	if built.owned {
		os.RemoveAll(built.dir)
	}
	return code
}

// limitParallel sets -test.parallel to n unless the command line gave it.
func limitParallel(n int) {
	if !flag.Parsed() {
		flag.Parse()
	}
	given := false
	flag.Visit(func(f *flag.Flag) { given = given || f.Name == "test.parallel" })
	parallel := flag.Lookup("test.parallel")
	if given || parallel == nil {
		return
	}
	if current, err := strconv.Atoi(parallel.Value.String()); err == nil && current > n {
		_ = parallel.Value.Set(strconv.Itoa(n))
	}
}

// build makes what built holds the first time a scenario needs it, so that
// a run of skipped scenarios builds nothing.
func build(tb testing.TB) {
	tb.Helper()
	built.once.Do(func() {
		if dir := os.Getenv(buildVar); dir != "" {
			built.dir = dir
		} else {
			built.dir, built.err = os.MkdirTemp("", "parley-acceptance-")
			if built.err != nil {
				return
			}
			built.owned = true
			built.err = buildInto(built.dir)
		}
		built.parleyd = filepath.Join(built.dir, "parleyd")
		built.parley = filepath.Join(built.dir, "parley")
		built.gitPath = filepath.Join(built.dir, "path")
		built.caFile = filepath.Join(built.dir, "ca.pem")
	})
	if built.err != nil {
		tb.Fatalf("build the binaries: %v", built.err)
	}
}

// buildInto builds parleyd and parley into dir, with a directory path/
// holding only a link to git and the file ca.pem.
func buildInto(dir string) error {
	gomod, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return fmt.Errorf("go env GOMOD: %w", err)
	}
	cmd := exec.Command("go", "build", "-o", dir+string(filepath.Separator), "./cmd/parleyd", "./cmd/parley")
	cmd.Dir = filepath.Dir(strings.TrimSpace(string(gomod)))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build: %w\n%s", err, out)
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(dir, "path"), 0o755); err != nil {
		return err
	}
	if err := os.Symlink(git, filepath.Join(dir, "path", "git")); err != nil {
		return err
	}
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	return os.WriteFile(filepath.Join(dir, "ca.pem"), certPEM(srv), 0o644)
}

// certPEM is the certificate srv serves, PEM-encoded.
func certPEM(srv *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
}

// Binaries returns the paths of the built parleyd and parley.
func Binaries(tb testing.TB) (parleyd, parley string) {
	tb.Helper()
	build(tb)
	return built.parleyd, built.parley
}

// IsChild reports whether this test process is a go test that a scenario
// runs itself, with ChildEnv.
func IsChild() bool { return os.Getenv(childVar) != "" }

// ChildEnv is the environment for a go test of the suite that a scenario
// runs itself: the process's own without any PARLEY_ACCEPTANCE_ variable,
// marked as a child, sharing this process's build, plus extra.
func ChildEnv(tb testing.TB, extra ...string) []string {
	tb.Helper()
	build(tb)
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PARLEY_ACCEPTANCE_") {
			env = append(env, kv)
		}
	}
	return append(append(env, childVar+"=1", buildVar+"="+built.dir), extra...)
}

// Options are what a scenario adds to the harness's hub.
type Options struct {
	// Server is added inside [server].
	Server string
	// TOML is added after the harness's configuration and begins with a
	// table header. A [limits] or [changes] table in it replaces the
	// harness's, which turns the rate limit off and makes mergeability
	// immediate.
	TOML string
	// Env are NAME=value variables added to parleyd's environment.
	Env []string
	// Files are written to the configuration directory, by name.
	Files map[string]string
	// OwnBinary runs a link to parleyd placed alone in the working
	// directory, instead of the shared build.
	OwnBinary bool
	// PublicHost, when set, is the host name other machines reach this
	// one at, such as the sandboxes of an OpenShell gateway: the hub then
	// listens on every address and its external URL names that host. The
	// harness itself still calls it on the loopback address.
	PublicHost string
}

// Hub is a parleyd process the harness started, with its directories. Its
// helpers fail the test it was started in, or the one With names.
type Hub struct {
	t testing.TB
	*hubState
}

// hubState is what every view of a hub (With) shares.
type hubState struct {
	opts Options
	// Port is the hub's loopback port; ListeningAddr is the address its
	// listening line named at the last start.
	Port          int
	ListeningAddr string
	// ConfigDir holds config.toml; DataDir is the data directory; WorkDir
	// is the process's working directory; Home and TmpDir are its HOME and
	// TMPDIR.
	ConfigDir, DataDir, WorkDir, Home, TmpDir string
	// AdminToken is the token the bootstrap wrote.
	AdminToken string

	bin, secretsKey string
	env             []string
	// gitHome and cliHome are the HOME of the scenario's git and parley;
	// cliConfig is parley's configuration directory.
	gitHome, cliHome, cliConfig string
	log                         *logBuffer

	mu       sync.Mutex
	cmd      *exec.Cmd
	exited   chan struct{}
	exitCode int
	// started is set once the hub listened for the first time.
	started bool

	usersMu sync.Mutex
	users   map[string]*User
}

// Start starts a hub on fresh directories and a free loopback port, with
// the first Options given, bootstraps its admin token and waits until it
// answers GET /readyz with 200. The hub is stopped when the test ends, and
// its log is attached to a test that failed.
func Start(tb testing.TB, opts ...Options) *Hub {
	tb.Helper()
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	h := newHub(tb, o, tb.TempDir(), base64.StdEncoding.EncodeToString(key))
	if o.OwnBinary {
		h.bin = filepath.Join(h.WorkDir, "parleyd")
		if err := os.Link(built.parleyd, h.bin); err != nil {
			tb.Fatalf("link parleyd: %v", err)
		}
	}
	boot := h.Parleyd("--admin-email", AdminEmail)
	tokenFile := filepath.Join(h.ConfigDir, "admin_token")
	token, err := os.ReadFile(tokenFile)
	if boot.Code != 0 || err != nil {
		tb.Fatalf("parleyd --admin-email exited %d (%v):\n%s%s", boot.Code, err, boot.Stdout, boot.Stderr)
	}
	// A bootstrapped hub refuses to start while the token file exists.
	if err := os.Remove(tokenFile); err != nil {
		tb.Fatal(err)
	}
	h.AdminToken = string(token)
	h.Start()
	return h
}

// StartOn starts a second hub with h's options and secrets key on dataDir,
// such as a backup of h, with its own configuration directory and port.
// dataDir is already bootstrapped: h's admin token and the users h signed
// in stay valid on it.
func (h *Hub) StartOn(dataDir string) *Hub {
	h.t.Helper()
	h2 := newHub(h.t, h.opts, dataDir, h.secretsKey)
	h2.AdminToken = h.AdminToken
	h.usersMu.Lock()
	for name, u := range h.users {
		h2.users[name] = u
	}
	h.usersMu.Unlock()
	h2.Start()
	return h2
}

// newHub prepares a hub on dataDir that is not started yet: its
// directories, its port, its configuration and its environment.
func newHub(tb testing.TB, o Options, dataDir, secretsKey string) *Hub {
	tb.Helper()
	build(tb)
	h := &Hub{t: tb, hubState: &hubState{
		opts: o, Port: freePort(tb), DataDir: dataDir, secretsKey: secretsKey,
		ConfigDir: tb.TempDir(), WorkDir: tb.TempDir(), Home: tb.TempDir(), TmpDir: tb.TempDir(),
		gitHome: tb.TempDir(), cliHome: tb.TempDir(), cliConfig: tb.TempDir(),
		bin: built.parleyd, log: &logBuffer{}, users: map[string]*User{},
	}}
	h.writeConfig()
	h.env = append([]string{
		"XDG_CONFIG_HOME=" + h.ConfigDir, "XDG_DATA_HOME=" + h.DataDir, "PARLEY_SECRETS_KEY=" + secretsKey,
		"GIT_SSL_CAINFO=" + built.caFile, "HOME=" + h.Home, "PATH=" + built.gitPath, "TMPDIR=" + h.TmpDir,
	}, o.Env...)
	tb.Cleanup(func() {
		h.mu.Lock()
		running := h.cmd != nil
		h.mu.Unlock()
		if running {
			h.signal(syscall.SIGTERM)
		}
		if tb.Failed() {
			tb.Logf("parleyd log (%s):\n%s", h.URL(""), h.Log())
		}
	})
	return h
}

func freePort(tb testing.TB) int {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// writeConfig writes config.toml and the files of the options.
func (h *Hub) writeConfig() {
	h.t.Helper()
	bind, external := "127.0.0.1", h.URL("")
	if host := h.opts.PublicHost; host != "" {
		bind, external = "0.0.0.0", "http://"+net.JoinHostPort(host, strconv.Itoa(h.Port))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[server]\nbind = %q\nport = %d\nexternal_url = %q\n%s\n", bind, h.Port, external, h.opts.Server)
	// The default rate limit throttles a scenario's own traffic, git's
	// unauthenticated probes included, and mergeability otherwise lags 5 s.
	if !hasTable(h.opts.TOML, "limits") {
		b.WriteString("[limits]\nrequests_per_minute = 0\n\n")
	}
	if !hasTable(h.opts.TOML, "changes") {
		b.WriteString("[changes]\nmergeable_debounce = \"0s\"\n\n")
	}
	idp := identityProvider(h.t)
	fmt.Fprintf(&b, "[[oauth.providers]]\nname = \"github\"\nclient_id = \"acceptance\"\nclient_secret = \"acceptance\"\n"+
		"token_url = %q\nuserinfo_url = %q\n\n%s", idp.URL+"/token", idp.URL+"/user", h.opts.TOML)
	files := map[string]string{"config.toml": b.String()}
	for name, content := range h.opts.Files {
		files[name] = content
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(h.ConfigDir, name), []byte(content), 0o600); err != nil {
			h.t.Fatal(err)
		}
	}
}

func hasTable(toml, name string) bool {
	for _, line := range strings.Split(toml, "\n") {
		if strings.TrimSpace(line) == "["+name+"]" {
			return true
		}
	}
	return false
}

// With is the same hub with helpers that fail tb instead, such as a
// subtest of the test that started it.
func (h *Hub) With(tb testing.TB) *Hub { return &Hub{t: tb, hubState: h.hubState} }

// URL is the hub's address followed by path.
func (h *Hub) URL(path string) string { return "http://127.0.0.1:" + strconv.Itoa(h.Port) + path }

// Log is everything the hub wrote to its standard error and output, over
// every start.
func (h *Hub) Log() string { return h.log.String() }

// Parleyd runs the hub's parleyd binary with args in the hub's environment
// and working directory, such as parleyd backup, and waits for it.
func (h *Hub) Parleyd(args ...string) Result {
	ctx, cancel := context.WithTimeout(context.Background(), callLimit)
	defer cancel()
	return run(ctx, h.WorkDir, h.env, h.bin, args...)
}

// Start starts the hub again on its directories and port after Stop or
// Kill, and waits until it is ready. A start again keeps the port, which
// the hub's external URL and every remote of its clients name, so another
// process that took the port while the hub was down fails it; that is
// inherent in restarting a hub where its clients look for it. The first
// start of a hub moves to a fresh port when its own was taken, since the
// port was only known free when freePort closed its listener.
func (h *Hub) Start() {
	h.t.Helper()
	for try := 1; ; try++ {
		from := h.log.size()
		exited := h.launch()
		if exited == nil {
			break
		}
		h.mu.Lock()
		code := h.exitCode
		h.cmd = nil
		h.mu.Unlock()
		if h.started || try == 3 || !strings.Contains(h.log.since(from), "address already in use") {
			h.t.Fatalf("parleyd exited %d before it listened (its log follows the test)", code)
		}
		h.Port = freePort(h.t)
		h.writeConfig()
	}
	h.started = true
	deadline := time.Now().Add(startLimit)
	for {
		if r := h.Call(http.MethodGet, "/readyz", "", nil); r.Status == http.StatusOK {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("GET /readyz did not answer 200 within %v", startLimit)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// launch starts parleyd and waits for its listening line. It returns nil
// once the hub listens, and the channel closed at its exit when it exited
// first.
func (h *Hub) launch() <-chan struct{} {
	h.t.Helper()
	cmd := exec.Command(h.bin)
	cmd.Dir, cmd.Env = h.WorkDir, h.env
	// A writer, not a pipe the harness could stop reading: parleyd dies of
	// SIGPIPE on its next log line once its standard error is closed.
	cmd.Stdout, cmd.Stderr = h.log, h.log
	cmd.WaitDelay = 5 * time.Second
	listening := h.log.expectListening()
	if err := cmd.Start(); err != nil {
		h.t.Fatalf("start parleyd: %v", err)
	}
	exited := make(chan struct{})
	h.mu.Lock()
	h.cmd, h.exited = cmd, exited
	h.mu.Unlock()
	go func() {
		_ = cmd.Wait()
		h.mu.Lock()
		h.exitCode = cmd.ProcessState.ExitCode()
		h.mu.Unlock()
		close(exited)
	}()
	select {
	case h.ListeningAddr = <-listening:
		return nil
	case <-exited:
		return exited
	case <-time.After(startLimit):
		h.t.Fatalf("parleyd wrote no listening line within %v", startLimit)
		return nil
	}
}

// Stop sends SIGTERM to the hub and fails the test unless it exits with
// status 0 within the stop's limit.
func (h *Hub) Stop() {
	h.t.Helper()
	if code := h.signal(syscall.SIGTERM); code != 0 {
		h.t.Fatalf("parleyd exited %d after SIGTERM, want 0", code)
	}
}

// Kill sends SIGKILL to the hub and waits until it has gone.
func (h *Hub) Kill() {
	h.t.Helper()
	h.signal(syscall.SIGKILL)
}

// signal sends sig to the running hub, waits for its exit and returns its
// status; a hub that outlives the stop's limit is killed and fails the test.
func (h *Hub) signal(sig syscall.Signal) int {
	h.t.Helper()
	h.mu.Lock()
	cmd, exited := h.cmd, h.exited
	h.cmd = nil
	h.mu.Unlock()
	if cmd == nil {
		h.t.Fatal("the hub is not running")
	}
	_ = cmd.Process.Signal(sig)
	select {
	case <-exited:
	case <-time.After(stopLimit):
		_ = cmd.Process.Kill()
		<-exited
		h.t.Errorf("parleyd did not exit within %v of %v", stopLimit, sig)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.exitCode
}

// PID is the running hub's process id, 0 when it is not running.
func (h *Hub) PID() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cmd == nil {
		return 0
	}
	return h.cmd.Process.Pid
}

// Children are the process ids of the running hub's child processes.
func (h *Hub) Children() []int {
	h.t.Helper()
	pid := h.PID()
	if pid == 0 {
		return nil
	}
	out, err := exec.Command("pgrep", "-P", strconv.Itoa(pid)).Output()
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
		return nil // pgrep found none
	} else if err != nil {
		h.t.Fatalf("pgrep: %v", err)
	}
	var pids []int
	for _, f := range strings.Fields(string(out)) {
		if n, err := strconv.Atoi(f); err == nil {
			pids = append(pids, n)
		}
	}
	return pids
}

// logBuffer keeps what a hub writes and tells the harness the address of
// the listening line of the current start.
type logBuffer struct {
	mu        sync.Mutex
	all       bytes.Buffer
	line      []byte
	listening chan string
}

// expectListening returns the channel the address of the next listening
// line is sent on.
func (l *logBuffer) expectListening() <-chan string {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.listening = make(chan string, 1)
	return l.listening
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.all.Write(p)
	l.line = append(l.line, p...)
	for {
		i := bytes.IndexByte(l.line, '\n')
		if i < 0 {
			break
		}
		var rec struct{ Msg, Addr string }
		if l.listening != nil && json.Unmarshal(l.line[:i], &rec) == nil && rec.Msg == "listening" {
			l.listening <- rec.Addr
			l.listening = nil
		}
		l.line = append(l.line[:0], l.line[i+1:]...)
	}
	return len(p), nil
}

// size is the length of everything written so far.
func (l *logBuffer) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.all.Len()
}

// since is what was written after the first n bytes.
func (l *logBuffer) since(n int) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return string(l.all.Bytes()[n:])
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.all.String()
}

// Eventually polls cond every 100 ms until it holds, and fails the test
// when it does not within 30 s.
func Eventually(tb testing.TB, what string, cond func() bool) {
	tb.Helper()
	EventuallyWithin(tb, 30*time.Second, what, cond)
}

// EventuallyWithin is Eventually with another limit.
func EventuallyWithin(tb testing.TB, limit time.Duration, what string, cond func() bool) {
	tb.Helper()
	deadline := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(deadline) {
			tb.Fatalf("waited %v for %s", limit, what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Release runs fn(0) to fn(n-1), each in a goroutine of its own, held at a
// start gate until all n are ready and then let go at the same moment, so
// that their requests reach the hub together. It returns once they are let
// go; wait returns once every fn has returned. fn must not fail the test
// itself: it records what it saw for the test goroutine to judge.
func Release(n int, fn func(i int)) (wait func()) {
	var ready, done sync.WaitGroup
	gate := make(chan struct{})
	ready.Add(n)
	done.Add(n)
	for i := range n {
		go func() {
			defer done.Done()
			ready.Done()
			<-gate
			fn(i)
		}()
	}
	ready.Wait()
	close(gate)
	return done.Wait
}

// SignIn signs the user name in to the hub again, as a person who logs in
// once more does: the user gets a new API key and its earlier keys are
// revoked. User returns the user with the new key from then on.
func (h *Hub) SignIn(name string) *User {
	h.t.Helper()
	h.usersMu.Lock()
	delete(h.users, name)
	h.usersMu.Unlock()
	return h.User(name)
}
