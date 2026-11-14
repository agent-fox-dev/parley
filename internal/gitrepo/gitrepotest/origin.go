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
	o.git("", "config", "http.receivepack", "true")
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
	o.git("", "init", "--quiet", "--bare", "--initial-branch=main")
	o.publish()
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
// Every commit changes the content of the one file of the tree.
func (o *Origin) Commit(branch, msg string) string {
	o.t.Helper()
	o.mu.Lock()
	o.seq++
	content := fmt.Sprintf("%s\n%d\n", msg, o.seq)
	o.mu.Unlock()
	blob := o.git(content, "hash-object", "-w", "--stdin")
	tree := o.git("100644 blob "+blob+"\tREADME\n", "mktree")
	args := []string{"commit-tree", tree, "-m", msg}
	if parent := o.Head(branch); parent != "" {
		args = append(args, "-p", parent)
	}
	id := o.git("", args...)
	o.git("", "update-ref", "refs/heads/"+branch, id)
	o.publish()
	return id
}

// Branch creates or moves branch to the commit ref names (a branch or a
// commit id) and returns that commit's id.
func (o *Origin) Branch(branch, ref string) string {
	o.t.Helper()
	id := o.git("", "rev-parse", "--verify", ref+"^{commit}")
	o.git("", "update-ref", "refs/heads/"+branch, id)
	o.publish()
	return id
}

// Tag creates the lightweight tag name at the commit ref names and returns
// that commit's id.
func (o *Origin) Tag(name, ref string) string {
	o.t.Helper()
	id := o.git("", "rev-parse", "--verify", ref+"^{commit}")
	o.git("", "update-ref", "refs/tags/"+name, id)
	o.publish()
	return id
}

// SetDefaultBranch points the origin's HEAD at branch.
func (o *Origin) SetDefaultBranch(branch string) {
	o.t.Helper()
	o.git("", "symbolic-ref", "HEAD", "refs/heads/"+branch)
	o.publish()
}

// Head is the commit id branch points at, or "" when there is no such
// branch.
func (o *Origin) Head(branch string) string {
	o.t.Helper()
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
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' " + shellQuote(message) + " >&2\nexit 1\n"
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

// publish writes the files git's file-based HTTP transport reads the refs
// from.
func (o *Origin) publish() {
	o.t.Helper()
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

// run runs git on the origin's repository (see gitIn).
func (o *Origin) run(stdin string, args ...string) (string, error) {
	return gitIn(o.dir, stdin, args...)
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
	o.publish()
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
	out, err := gitIn(dir, "", args...)
	if err != nil {
		t.Fatalf("in %s: %v", dir, err)
	}
	return out
}

// Refs maps every ref below prefix of the repository whose git directory is
// dir, by its name without prefix ("" for every ref, by its full name), to
// the object id it points at.
func Refs(t testing.TB, dir, prefix string) map[string]string {
	t.Helper()
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

// gitIn runs git on the repository whose git directory is dir with stdin as
// its input, away from the user's and the system's git configuration, with
// a fixed author, and returns its trimmed standard output.
func gitIn(dir, stdin string, args ...string) (string, error) {
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
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}
