package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The variables that describe the OpenShell gateway beyond its URL, which
// Gateway's variable gives. TokenFileVar names a file holding the bearer
// token the hub sends the gateway; TLSDirVar instead names a directory
// holding ca.crt, tls.crt and tls.key, the gateway's CA and the client
// certificate and key the hub authenticates with by mutual TLS, as the
// openshell CLI keeps them. ImageVar names the image every profile runs. HubHostVar is the host name the gateway's sandboxes reach this
// machine at, for the hub and for Page; without it both are on the
// loopback address, which only a gateway on this machine shares.
// ProviderProfileVar names a provider profile the gateway holds, which the
// credential of the profile cred uses; anthropic without it.
const (
	TokenFileVar       = "PARLEY_ACCEPTANCE_OPENSHELL_TOKEN_FILE"
	TLSDirVar          = "PARLEY_ACCEPTANCE_OPENSHELL_TLS_DIR"
	ImageVar           = "PARLEY_ACCEPTANCE_SANDBOX_IMAGE"
	HubHostVar         = "PARLEY_ACCEPTANCE_SANDBOX_HUB_HOST"
	ProviderProfileVar = "PARLEY_ACCEPTANCE_OPENSHELL_PROVIDER_PROFILE"
)

// sandboxProfiles are the profiles a scenario may name, each the keys of
// its table after its name and image. None names a policy file: the
// built-in policy lets a sandbox reach the hub and nothing else.
var sandboxProfiles = map[string]string{
	"default": "default = true\nttl = \"1h\"\n",
	// cred attaches the secret MODEL_KEY as a credential and exposes none.
	"cred": "ttl = \"1h\"\ncredentials = [\"model\"]\n",
	// exposing exposes DEPLOY_KEY where the creator may read it, and
	// leaves it out otherwise, rather than refusing the creation.
	"exposing": "ttl = \"1h\"\noptional_expose_secrets = [\"DEPLOY_KEY\"]\n",
	// needs requires a secret nobody stores.
	"needs": "ttl = \"1h\"\nexpose_secrets = [\"ABSENT_KEY\"]\n",
	"quick": "ttl = \"3m\"\nidle_timeout = \"1m\"\n",
}

// SandboxOptions are the Options of a hub connected to the gateway the
// gateway variables describe: a [sandbox] section with settings, extra
// keys of the section such as "max_per_workspace = 1", and the profiles
// named, among default, cred, exposing, needs and quick, each running the
// image ImageVar names. tb is skipped unless Gateway's variable is set.
// The gateway variable alone gates, so that a gateway set up halfway fails
// the scenario rather than skipping it: without ImageVar the scenario
// fails at once, and without TokenFileVar or TLSDirVar the hub sends a
// token the gateway refuses.
func SandboxOptions(tb testing.TB, settings string, profiles ...string) Options {
	tb.Helper()
	gateway := Require(tb, Gateway)
	token := "no-token-file-was-given"
	auth := "token_file = \"gateway-token\""
	if dir := os.Getenv(TLSDirVar); dir != "" {
		auth = fmt.Sprintf("tls_ca = %q\ntls_cert = %q\ntls_key = %q",
			filepath.Join(dir, "ca.crt"), filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
	} else if file := os.Getenv(TokenFileVar); file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			tb.Fatalf("read %s: %v", TokenFileVar, err)
		}
		token = string(raw)
	}
	image := os.Getenv(ImageVar)
	if image == "" {
		tb.Fatalf("%s is set, %s is not: set it to the image the sandboxes run", Gateway.Var, ImageVar)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[sandbox]\ngateway_url = %q\nopenshell_workspace = %q\n%s\n%s\n", gateway, gatewayWorkspace(tb), auth, settings)
	for _, name := range profiles {
		keys, ok := sandboxProfiles[name]
		if !ok {
			tb.Fatalf("the harness has no sandbox profile %q", name)
		}
		fmt.Fprintf(&b, "[[sandbox.profiles]]\nname = %q\nimage = %q\n%s\n", name, image, keys)
		if name == "cred" {
			provider := os.Getenv(ProviderProfileVar)
			if provider == "" {
				provider = "anthropic"
			}
			fmt.Fprintf(&b, "[[sandbox.credentials]]\nname = \"model\"\nprovider_profile = %q\nsecret = \"MODEL_KEY\"\n\n", provider)
		}
	}
	return Options{
		TOML: b.String(), Files: map[string]string{"gateway-token": token}, PublicHost: os.Getenv(HubHostVar),
	}
}

// gatewayWorkspace is the OpenShell workspace of the test's hub. A hub owns
// its workspace: it installs its own provider profile for the hub
// credential there, which names its port, so hubs that run at once each
// need one. It is named after the test, so that a run uses the
// workspaces of the run before.
func gatewayWorkspace(tb testing.TB) string {
	sum := sha256.Sum256([]byte(tb.Name()))
	return "pat-" + hex.EncodeToString(sum[:6])
}

// Page serves a page that the gateway's sandboxes can ask for, on the host
// HubHostVar names (the loopback address without it), until the test
// ends, and returns its URL and that host.
func Page(tb testing.TB) (pageURL, host string) {
	tb.Helper()
	host, bind := os.Getenv(HubHostVar), "0.0.0.0"
	if host == "" {
		host, bind = "127.0.0.1", "127.0.0.1"
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(bind, "0"))
	if err != nil {
		tb.Fatal(err)
	}
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "reached\n")
	})}}
	srv.Start()
	tb.Cleanup(srv.Close)
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	return "http://" + net.JoinHostPort(host, port) + "/", host
}

// readyLimit is how long a sandbox may take to become ready.
const readyLimit = 120 * time.Second

// Sandbox is a sandbox a scenario created, which is deleted when the test
// ends, also when it failed, so that a shared gateway is left as it was.
type Sandbox struct {
	hub *Hub
	// ID is the sandbox's id; Workspace is the slug of the workspace it
	// is attached to; Credential created it, and the harness acts in it
	// and deletes it with it.
	ID, Workspace, Credential string
	// Body is the sandbox object the creation answered.
	Body map[string]any
}

// TrySandbox asks for a sandbox on workspace with credential and the
// create's body, and returns the hub's answer, with the sandbox when it was
// created (nil otherwise), failing the test when no answer came. It does
// not wait for the sandbox to be ready.
func (h *Hub) TrySandbox(credential, workspace string, body map[string]any) (*Sandbox, Response) {
	h.t.Helper()
	r := h.Call(http.MethodPost, "/api/v1/workspaces/"+workspace+"/sandboxes", credential, body)
	if r.Err != nil {
		h.t.Fatalf("create a sandbox on %s: no answer (status %d): %v", workspace, r.Status, r.Err)
	}
	if r.Status != http.StatusCreated {
		return nil, r
	}
	return h.AdoptSandbox(credential, workspace, r.Body), r
}

// Sandbox creates a sandbox on workspace with credential and the create's
// body, failing the test unless it is created and becomes ready.
func (h *Hub) Sandbox(credential, workspace string, body map[string]any) *Sandbox {
	h.t.Helper()
	s, r := h.TrySandbox(credential, workspace, body)
	if s == nil {
		h.t.Fatalf("create a sandbox on %s: answer %d, want 201: %s", workspace, r.Status, r.Raw)
	}
	s.WaitReady()
	return s
}

// AdoptSandbox is the sandbox object obj, on workspace, created with
// credential by another call than TrySandbox, such as a fork's creation:
// it is deleted when the test ends like those the harness creates.
func (h *Hub) AdoptSandbox(credential, workspace string, obj map[string]any) *Sandbox {
	h.t.Helper()
	s := &Sandbox{hub: h, ID: Str(obj["id"]), Workspace: workspace, Credential: credential, Body: obj}
	if s.ID == "" {
		h.t.Fatalf("the sandbox object of %s has no id: %v", workspace, obj)
	}
	h.t.Cleanup(s.delete)
	return s
}

// delete deletes the sandbox without a checkpoint, with its credential, or
// with the admin token when that is refused; a sandbox already deleted
// answers 204 too.
func (s *Sandbox) delete() {
	h := s.hub
	if h.PID() == 0 {
		h.t.Errorf("sandbox %s of %s was not deleted, since its hub is not running: delete it on the gateway", s.ID, s.Workspace)
		return
	}
	var r Response
	for _, credential := range []string{s.Credential, h.AdminToken} {
		if r = h.Call(http.MethodDelete, s.Path("?checkpoint=false"), credential, nil); r.Status == http.StatusNoContent {
			return
		}
	}
	h.t.Errorf("delete sandbox %s of %s: answer %d %s, want 204: delete it on the gateway", s.ID, s.Workspace, r.Status, r.Raw)
}

// Path is the API path of the sandbox followed by suffix, such as /exec.
func (s *Sandbox) Path(suffix string) string {
	return "/api/v1/workspaces/" + s.Workspace + "/sandboxes/" + s.ID + suffix
}

// Get reads the sandbox with its credential, failing the test unless the
// answer is 200.
func (s *Sandbox) Get() Response {
	s.hub.t.Helper()
	return s.hub.Get(s.Credential, s.Path(""))
}

// WaitReady waits until the sandbox is ready and returns its object,
// failing the test when it ends another way or is not ready in time.
func (s *Sandbox) WaitReady() map[string]any {
	h := s.hub
	h.t.Helper()
	var obj map[string]any
	EventuallyWithin(h.t, readyLimit, "sandbox "+s.ID+" to be ready", func() bool {
		obj = h.Get(s.Credential, s.Path("?wait_for=ready&wait=10")).Body
		switch obj["state"] {
		case "ready":
			return true
		case "provisioning":
			return false
		}
		h.t.Fatalf("sandbox %s is %v (%v: %s), want ready: %v", s.ID, obj["state"], obj["state_reason"], s.failure(), obj)
		return false
	})
	return obj
}

// failure is the error the parley.sandbox.fail event of the sandbox
// reports, which the sandbox object does not carry, or a note that no such
// event arrived within a few seconds.
func (s *Sandbox) failure() string {
	stream := s.hub.Events(s.hub.AdminToken, "types=parley.sandbox.fail&after=0")
	defer stream.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range stream.All() {
			if e.Record["resource_id"] == s.ID {
				meta, _ := e.Record["metadata"].(map[string]any)
				return Str(meta["error"])
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return "no parley.sandbox.fail event of it arrived"
}

// WaitState polls the sandbox until it is in state and returns its object,
// failing the test when it is not within limit.
func (s *Sandbox) WaitState(limit time.Duration, state string) map[string]any {
	h := s.hub
	h.t.Helper()
	var obj map[string]any
	EventuallyWithin(h.t, limit, "sandbox "+s.ID+" to be "+state, func() bool {
		obj = h.Get(s.Credential, s.Path("")).Body
		return obj["state"] == state
	})
	return obj
}

// Exec is how a command run in a sandbox ended, with its output collected.
type Exec struct {
	ExitCode       int
	Stdout, Stderr string
}

// Lines are the lines of the standard output, without their ends.
func (e Exec) Lines() []string { return Result{Stdout: e.Stdout}.Lines() }

// Exec runs command in the sandbox's working directory with its
// credential, waits for it and returns how it ended, failing the test
// unless the hub ran it.
func (s *Sandbox) Exec(command ...string) Exec {
	h := s.hub
	h.t.Helper()
	r := h.Call(http.MethodPost, s.Path("/exec"), s.Credential, map[string]any{"command": command}).Want(h.t, http.StatusOK)
	return Exec{ExitCode: Num(r.Body["exit_code"]), Stdout: Str(r.Body["stdout"]), Stderr: Str(r.Body["stderr"])}
}
