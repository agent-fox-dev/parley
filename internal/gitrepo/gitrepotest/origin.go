// Package gitrepotest is the test origin of the repository: a bare git
// repository the test builds commits, branches and tags in, served over TLS
// by net/http/httptest with git's file-based ("dumb") HTTP transport, or
// with its smart transport through git http-backend, which takes pushes too
// for an origin SmartOrigin builds. It is imported by test code only.
package gitrepotest

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// repoName is the last element of the origin's URL path.
const repoName = "origin.git"

// Origin is a bare repository served over HTTPS. Its zero value is not
// usable; NewOrigin builds one. The methods that change the repository
// fail the test on an error, so call them from the test's goroutine.
type Origin struct {
	t      testing.TB
	dir    string
	srv    *httptest.Server
	caFile string
	files  http.Handler

	// smart serves git's smart HTTP transport instead of the files.
	smart bool

	// mu guards what the handler reads while the test changes it.
	mu sync.Mutex
	// user and password, when user is not empty, are the Basic
	// authentication every request must carry.
	user, password string
	requests       int
	gate           chan struct{}
	refuse         int
	seq            int
	// untrusted, when set, is the certificate the origin serves instead of
	// the one CAFile holds.
	untrusted *tls.Certificate

	closeOnce sync.Once
}

// Option changes how NewOrigin builds an origin.
type Option func(*Origin)

// WithBasicAuth makes the origin answer 401 to every request that does not
// carry Basic authentication with user and password.
func WithBasicAuth(user, password string) Option {
	return func(o *Origin) { o.user, o.password = user, password }
}

// WithSmartHTTP makes the origin serve git's smart HTTP transport through
// git http-backend instead of the repository's files. Git cannot read a
// repository without refs over the file-based transport, so an empty origin
// needs this one to be cloned as a real server serves it.
func WithSmartHTTP() Option {
	return func(o *Origin) { o.smart = true }
}

// SmartOrigin builds an origin as NewOrigin does, with opts, that serves
// git's smart HTTP transport and takes pushes: git http-backend refuses a
// push from a client it knows no user of unless the repository enables
// receive-pack, which this one does.
func SmartOrigin(t testing.TB, opts ...Option) *Origin {
	t.Helper()
	o := NewOrigin(t, append(opts, WithSmartHTTP())...)
	// What git config http.receivepack true writes, without a git process.
	cfg, err := os.OpenFile(filepath.Join(o.dir, "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err == nil {
		_, err = cfg.WriteString("[http]\n\treceivepack = true\n")
		if cerr := cfg.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		t.Fatalf("origin: enable receive-pack: %v", err)
	}
	return o
}

// NewOrigin builds an empty bare repository whose default branch is main in
// a temporary directory of t, serves it over TLS and closes it when the test
// ends. A git that should trust it runs with the variables of Env.
func NewOrigin(t testing.TB, opts ...Option) *Origin {
	t.Helper()
	o := &Origin{t: t, dir: filepath.Join(t.TempDir(), repoName)}
	for _, opt := range opts {
		opt(o)
	}
	InitBare(t, o.dir, "main")
	o.Publish()
	o.files = http.StripPrefix("/"+repoName, http.FileServer(http.Dir(o.dir)))
	if o.smart {
		o.files = o.backend()
	}
	o.srv = httptest.NewUnstartedServer(http.HandlerFunc(o.serve))
	o.srv.TLS = &tls.Config{GetConfigForClient: o.tlsConfig}
	o.srv.StartTLS()
	o.caFile = filepath.Join(t.TempDir(), "origin-ca.pem")
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: o.srv.Certificate().Raw})
	if err := os.WriteFile(o.caFile, cert, 0o600); err != nil {
		t.Fatalf("write the origin's certificate: %v", err)
	}
	t.Cleanup(o.Close)
	return o
}

// URL is the origin's https URL, without user information.
func (o *Origin) URL() string { return o.srv.URL + "/" + repoName }

// CAFile is the PEM file of the certificate the origin serves, which git
// trusts through GIT_SSL_CAINFO.
func (o *Origin) CAFile() string { return o.caFile }

// Env are the variables, as NAME=value, that make git trust the origin.
func (o *Origin) Env() []string { return []string{"GIT_SSL_CAINFO=" + o.caFile} }

// Dir is the bare repository's directory.
func (o *Origin) Dir() string { return o.dir }

// Commit adds a commit with message msg on top of branch, creating the
// branch with a root commit when it does not exist, and returns its id.
// Every commit changes the content of the one file of the tree. It writes
// the commit in one run of git fast-import rather than one git process per
// object: a test process that starts git pays for every process it forks.
func (o *Origin) Commit(branch, msg string) string {
	o.t.Helper()
	o.mu.Lock()
	o.seq++
	content := fmt.Sprintf("%s\n%d\n", msg, o.seq)
	o.mu.Unlock()
	var stream strings.Builder
	fmt.Fprintf(&stream, "commit refs/heads/%s\nmark :1\ncommitter Origin <origin@example.invalid> now\n%s", branch, fastData(msg+"\n"))
	if parent := o.Head(branch); parent != "" {
		fmt.Fprintf(&stream, "from %s\n", parent)
	}
	// deleteall leaves README the tree's one file, whatever the parent's
	// tree holds.
	fmt.Fprintf(&stream, "deleteall\nM 100644 inline README\n%s\nget-mark :1\n", fastData(content))
	id := o.git(stream.String(), "fast-import", "--quiet", "--force", "--date-format=now")
	o.Publish()
	return id
}

// Branch creates or moves branch to the commit ref names (a branch or a
// commit id) and returns that commit's id.
func (o *Origin) Branch(branch, ref string) string {
	o.t.Helper()
	id := o.git("", "rev-parse", "--verify", ref+"^{commit}")
	SetRef(o.t, o.dir, "refs/heads/"+branch, id)
	o.Publish()
	return id
}

// Tag creates the lightweight tag name at the commit ref names and returns
// that commit's id.
func (o *Origin) Tag(name, ref string) string {
	o.t.Helper()
	id := o.git("", "rev-parse", "--verify", ref+"^{commit}")
	o.git("", "update-ref", "refs/tags/"+name, id)
	o.Publish()
	return id
}

// SetDefaultBranch points the origin's HEAD at branch.
func (o *Origin) SetDefaultBranch(branch string) {
	o.t.Helper()
	o.git("", "symbolic-ref", "HEAD", "refs/heads/"+branch)
	o.Publish()
}

// Head is the commit id branch points at, or "" when there is no such
// branch.
func (o *Origin) Head(branch string) string {
	o.t.Helper()
	if refs, ok := readRefs(o.dir, "refs/heads/"); ok {
		return refs[branch]
	}
	out, err := o.run("", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil {
		return ""
	}
	return out
}

// Accept makes the origin take, from now on, only the Basic authentication
// with user and password; an empty user makes it take every request.
func (o *Origin) Accept(user, password string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.user, o.password = user, password
}

// Requests is the number of HTTP requests the origin has received, those it
// refused or still holds at the gate included.
func (o *Origin) Requests() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.requests
}

// Block holds every request that arrives from now on, after it is counted,
// until Release or Close, or until its client goes away.
func (o *Origin) Block() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.gate == nil {
		o.gate = make(chan struct{})
	}
}

// Release lets the requests Block holds, and every later one, through.
func (o *Origin) Release() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.gate != nil {
		close(o.gate)
		o.gate = nil
	}
}

// Refuse makes the origin answer every request with status, without
// content; 0 makes it serve again.
func (o *Origin) Refuse(status int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.refuse = status
}

// Reject makes the origin's server-side hook refuse every update a push
// asks for, writing message, which the pushing git shows as origin's
// remote: lines; "" makes it take them again.
func (o *Origin) Reject(message string) {
	o.t.Helper()
	hook := filepath.Join(o.dir, "hooks", "pre-receive")
	if message == "" {
		if err := os.Remove(hook); err != nil && !os.IsNotExist(err) {
			o.t.Fatalf("origin: %v", err)
		}
		return
	}
	o.writeHook(hook, "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' "+shellQuote(message)+" >&2\nexit 1\n")
}

// RejectRef makes the origin's server-side hook refuse every push that
// updates ref, a full ref name such as refs/heads/patch/b, writing message,
// which the pushing git shows as origin's remote: lines, and take every
// other push. It replaces what Reject or an earlier RejectRef set; ""
// makes the origin take every push again.
func (o *Origin) RejectRef(ref, message string) {
	o.t.Helper()
	hook := filepath.Join(o.dir, "hooks", "pre-receive")
	if message == "" {
		o.Reject("")
		return
	}
	o.writeHook(hook, "#!/bin/sh\nrefuse=\nwhile read -r old new ref; do\n\t[ \"$ref\" = "+shellQuote(ref)+" ] && refuse=1\ndone\n"+
		"[ -z \"$refuse\" ] && exit 0\nprintf '%s\\n' "+shellQuote(message)+" >&2\nexit 1\n")
}

// writeHook writes the executable hook script.
func (o *Origin) writeHook(hook, script string) {
	o.t.Helper()
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		o.t.Fatalf("origin: %v", err)
	}
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		o.t.Fatalf("origin: %v", err)
	}
}

// ServeUntrusted makes the origin, on every connection from now on, serve
// a certificate of its own that the one in CAFile does not vouch for, so
// that a git that trusts the origin through Env refuses the connection;
// false makes it serve its usual certificate again.
func (o *Origin) ServeUntrusted(untrusted bool) {
	o.t.Helper()
	var cert *tls.Certificate
	if untrusted {
		cert = selfSigned(o.t)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.untrusted = cert
}

// tlsConfig is the TLS configuration of a connection: the server's own,
// or one with the untrusted certificate while ServeUntrusted holds one.
func (o *Origin) tlsConfig(*tls.ClientHelloInfo) (*tls.Config, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.untrusted == nil {
		return nil, nil
	}
	return &tls.Config{Certificates: []tls.Certificate{*o.untrusted}}, nil
}

// Close releases the held requests and shuts the server down, so that later
// requests find nothing listening. Calling it again does nothing.
func (o *Origin) Close() {
	o.closeOnce.Do(func() {
		o.Release()
		o.srv.Close()
	})
}

// serve counts the request, holds it at the gate, checks the Basic
// authentication and serves the repository's files.
func (o *Origin) serve(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	o.requests++
	gate, refuse := o.gate, o.refuse
	user, password := o.user, o.password
	o.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
	}
	if refuse != 0 {
		w.WriteHeader(refuse)
		return
	}
	if user != "" {
		gotUser, gotPassword, ok := r.BasicAuth()
		if !ok || gotUser != user || gotPassword != password {
			w.Header().Set("WWW-Authenticate", `Basic realm="origin"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}
	o.files.ServeHTTP(w, r)
}

// backend is git http-backend serving the origin's repository, which it
// finds below GIT_PROJECT_ROOT by the request's path.
func (o *Origin) backend() http.Handler {
	o.t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		o.t.Fatalf("origin: %v", err)
	}
	return &cgi.Handler{
		Path: git,
		Args: []string{"http-backend"},
		Dir:  filepath.Dir(o.dir),
		Env: []string{
			"GIT_PROJECT_ROOT=" + filepath.Dir(o.dir),
			"GIT_HTTP_EXPORT_ALL=1",
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_GLOBAL=" + os.DevNull,
		},
	}
}

// Publish writes the files git's file-based HTTP transport reads the refs
// from, as git update-server-info does, which a test calls after it wrote
// into the origin's repository with git itself; Commit, Branch, Tag and the
// other writers of Origin publish on their own. An origin that serves the
// smart transport has no use for the files. It writes them itself where it
// can rather than start git, since a test process pays for every process it
// forks.
func (o *Origin) Publish() {
	o.t.Helper()
	if o.smart || writeServerInfo(o.dir) {
		return
	}
	o.git("", "update-server-info")
}

// git runs git on the origin's repository with stdin as its input and
// returns its trimmed output, failing the test on an error.
func (o *Origin) git(stdin string, args ...string) string {
	o.t.Helper()
	out, err := o.run(stdin, args...)
	if err != nil {
		o.t.Fatalf("origin: %v", err)
	}
	return out
}

// run runs git on the origin's repository (see Run).
func (o *Origin) run(stdin string, args ...string) (string, error) {
	return Run(o.dir, stdin, args...)
}

// History builds on the empty origin main (the default branch) with three
// commits, dev with one more on top of the second, and the tag v1 on main's
// first commit. It returns the heads of main and dev and the tag's commit.
// It writes them all in one run of git fast-import, which costs a fraction
// of the dozens of git processes Commit, Branch and Tag would start.
func (o *Origin) History() (mainHead, devHead, v1 string) {
	o.t.Helper()
	var stream strings.Builder
	commit := func(mark int, branch, msg, from string) {
		o.mu.Lock()
		o.seq++
		content := fmt.Sprintf("%s\n%d\n", msg, o.seq)
		o.mu.Unlock()
		fmt.Fprintf(&stream, "commit refs/heads/%s\nmark :%d\ncommitter Origin <origin@example.invalid> now\ndata %d\n%s\n",
			branch, mark, len(msg), msg)
		if from != "" {
			fmt.Fprintf(&stream, "from %s\n", from)
		}
		fmt.Fprintf(&stream, "M 100644 inline README\ndata %d\n%s\n", len(content), content)
	}
	commit(1, "main", "first", "")
	commit(2, "main", "second", ":1")
	commit(3, "dev", "on dev", ":2")
	commit(4, "main", "third", ":2")
	stream.WriteString("reset refs/tags/v1\nfrom :1\n\n")
	o.git(stream.String(), "fast-import", "--quiet", "--date-format=now")
	heads := strings.Fields(o.git("", "rev-parse", "refs/heads/main", "refs/heads/dev", "refs/tags/v1"))
	if len(heads) != 3 {
		o.t.Fatalf("origin: the history's heads are %v, want three", heads)
	}
	o.Publish()
	return heads[0], heads[1], heads[2]
}

// selfSigned is a new self-signed certificate for the loopback addresses,
// which no other certificate vouches for.
func selfSigned(t testing.TB) *tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("origin: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big1,
		Subject:      pkix.Name{CommonName: "untrusted origin"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		DNSNames:     []string{"localhost"},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("origin: %v", err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// big1 is the serial number of the untrusted certificate.
var big1 = big.NewInt(1)

// shellQuote is s quoted for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Git runs git on the repository whose git directory is dir, such as a
// copy the code under test made, and returns its trimmed output, failing
// the test on an error.
func Git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	out, err := Run(dir, "", args...)
	if err != nil {
		t.Fatalf("in %s: %v", dir, err)
	}
	return out
}

// Refs maps every ref below prefix of the repository whose git directory is
// dir, by its name without prefix ("" for every ref, by its full name), to
// the object id it points at. It reads the ref files itself where it can
// rather than start git, since a test process pays for every process it
// forks (see readRefs).
func Refs(t testing.TB, dir, prefix string) map[string]string {
	t.Helper()
	if refs, ok := readRefs(dir, prefix); ok {
		return refs
	}
	args := []string{"for-each-ref", "--format=%(refname) %(objectname)"}
	if prefix != "" {
		args = append(args, prefix) // an empty pattern would match no ref
	}
	refs := map[string]string{}
	for line := range strings.Lines(Git(t, dir, args...)) {
		name, id, _ := strings.Cut(strings.TrimSpace(line), " ")
		refs[strings.TrimPrefix(name, prefix)] = id
	}
	return refs
}

// Pack is the pack git pack-objects --revs writes in the repository whose
// git directory is dir for revs, such as a commit and ^ a commit the
// receiver has: the bytes a client sends to push them.
func Pack(t testing.TB, dir string, revs ...string) string {
	t.Helper()
	cmd := exec.Command("git", "pack-objects", "--stdout", "--revs", "--quiet")
	cmd.Env = append(os.Environ(), "GIT_DIR="+dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	cmd.Stdin = strings.NewReader(strings.Join(revs, "\n") + "\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("pack %v in %s: %v", revs, dir, err)
	}
	return string(out)
}

// Quarantines lists the entries of the object directory of the repository
// whose git directory is dir that git does not keep there itself (pack,
// info and the two-character directories of loose objects): what a push
// left behind of its quarantine. A directory it cannot read is reported as
// its error, so that a test sees it.
func Quarantines(dir string) []string {
	entries, err := os.ReadDir(filepath.Join(dir, "objects"))
	if err != nil {
		return []string{err.Error()}
	}
	var strays []string
	for _, e := range entries {
		if n := e.Name(); n != "pack" && n != "info" && len(n) != 2 {
			strays = append(strays, n)
		}
	}
	return strays
}

// ObjectCounts is what git count-objects -v reports for the repository
// whose git directory is dir, by field: count is its loose objects, in-pack
// its packed ones.
func ObjectCounts(t testing.TB, dir string) map[string]string {
	t.Helper()
	counts := map[string]string{}
	for line := range strings.Lines(Git(t, dir, "count-objects", "-v")) {
		k, v, _ := strings.Cut(strings.TrimSpace(line), ": ")
		counts[k] = v
	}
	return counts
}

// PushCommit is the work of a client holding the main of the repository
// src: in a bare repository of its own, it commits README with content on
// top of main's head, then pushes that commit to branch of the repository
// dest, which receives the commit, its tree and its blob. It returns the
// commit's id.
func PushCommit(t testing.TB, src, dest, branch, content string) string {
	t.Helper()
	client := filepath.Join(t.TempDir(), "client.git")
	InitBare(t, client, "")
	Git(t, client, "fetch", "--quiet", src, "main:main")
	blob, err := Run(client, content, "hash-object", "-w", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := Run(client, "100644 blob "+blob+"\tREADME\n", "mktree")
	if err != nil {
		t.Fatal(err)
	}
	id := Git(t, client, "commit-tree", tree, "-p", "main", "-m", branch)
	Git(t, client, "push", "--quiet", dest, id+":refs/heads/"+branch)
	return id
}

// Run runs git on the repository whose git directory is dir with stdin as
// its input, away from the user's and the system's git configuration, with
// a fixed author, and returns its trimmed standard output, or an error that
// holds git's standard error: for a test that feeds git an input, or
// expects it to fail, where Git would fail the test.
func Run(dir, stdin string, args ...string) (string, error) {
	out, err := run(dir, stdin, args...)
	return strings.TrimSpace(string(out)), err
}

// Blob is the content of the blob rev, such as <commit>:<path>, of the
// repository whose git directory is dir, byte for byte, failing the test
// when there is none.
func Blob(t testing.TB, dir, rev string) []byte {
	t.Helper()
	out, err := run(dir, "", "cat-file", "blob", rev)
	if err != nil {
		t.Fatalf("in %s: %v", dir, err)
	}
	return out
}

// run is Run with git's standard output as it is.
func run(dir, stdin string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(),
		"GIT_DIR="+dir,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=Origin", "GIT_AUTHOR_EMAIL=origin@example.invalid",
		"GIT_COMMITTER_NAME=Origin", "GIT_COMMITTER_EMAIL=origin@example.invalid",
	)
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
