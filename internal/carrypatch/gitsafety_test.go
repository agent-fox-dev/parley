package carrypatch_test

import (
	"encoding/base64"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/hub"
)

// echoingUpstream is an https remote that refuses every request with 401
// and a plain-text body that echoes the Authorization header it received,
// decoded, as a server that reflects what it was sent would, and which git
// shows as the remote's message.
type echoingUpstream struct {
	srv  *httptest.Server
	cert []byte
	// echoedSecret counts the answers whose body held secret.
	echoedSecret atomic.Int64
	secret       string
}

// startEchoingUpstream starts an echoing upstream, closed when the test
// ends, that counts the answers echoing secret.
func startEchoingUpstream(t *testing.T, secret string) *echoingUpstream {
	t.Helper()
	u := &echoingUpstream{secret: secret}
	u.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if enc, ok := strings.CutPrefix(auth, "Basic "); ok {
			if dec, err := base64.StdEncoding.DecodeString(enc); err == nil {
				auth = string(dec)
			}
		}
		if strings.Contains(auth, u.secret) {
			u.echoedSecret.Add(1)
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="upstream"`)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("refused the credentials " + auth + "\n"))
	}))
	t.Cleanup(u.srv.Close)
	u.cert = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: u.srv.Certificate().Raw})
	return u
}

// URL is the URL of a repository on the echoing upstream.
func (u *echoingUpstream) URL() string { return u.srv.URL + "/upstream.git" }

func TestGitFailure_KeepsTheUpstreamCredentialOutOfEveryText(t *testing.T) {
	t.Parallel()
	const token = "s3cr3t-Upstream-Token-91"
	up := startEchoingUpstream(t, token)
	f := startFixture(t, fixtureOptions{
		deps:    func(d *carrypatch.Deps) { d.Git.Env = trusting(t, d.Git.Env, up.cert) },
		compose: func(c *hub.Composition) { addUpstreamFetchProbe(c, 0) },
	})
	f.addPatch(t, "p1")
	f.secrets.set(carrypatch.SecretUpstreamGitPAT, token)
	// From now on the upstream answers the attempt's fetch with the
	// credential it was sent.
	if _, err := f.Store.Main().Exec(`UPDATE workspaces SET upstream_url = ? WHERE slug = ?`, up.URL(), f.slug); err != nil {
		t.Fatal(err)
	}

	out, id := f.attempt(t, nil)
	if out.Kind == attemptCompleted {
		t.Fatalf("the attempt = %+v, want it to fail on the upstream's refusal", out)
	}
	status, body := f.Call(t, http.MethodPost, "/api/v1/workspaces/"+f.slug+"/upstream-fetch-probe", f.key, nil)

	if up.echoedSecret.Load() == 0 {
		t.Fatalf("the upstream never received the credential, so it echoed none")
	}
	jobErr := str(f.job(t, id).Error)
	if jobErr == "" || !strings.Contains(jobErr, "fetch") {
		t.Errorf("the job's error = %q, want it to name the fetch's failure", jobErr)
	}
	var events []string
	rows, err := f.Store.Main().Query(`SELECT json_object('id', id, 'type', type, 'workspace', workspace, 'actor_id', actor_id,
		'resource_id', resource_id, 'action', action, 'metadata', metadata) FROM hub_events ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			t.Fatal(err)
		}
		events = append(events, doc)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(events, "\n"), carrypatch.EventRebuildFail) {
		t.Errorf("the events %v hold no %s", events, carrypatch.EventRebuildFail)
	}
	for _, c := range []struct{ what, text string }{
		{"the job's error", jobErr},
		{"an event", strings.Join(events, "\n")},
		{"the hub's log", f.Log.String()},
		{"the answer of the request", string(body)},
	} {
		if strings.Contains(c.text, token) {
			t.Errorf("%s holds the upstream credential: %s", c.what, c.text)
		}
	}
	if status == http.StatusNoContent {
		t.Errorf("the request's fetch = %d %s, want it to fail on the upstream's refusal", status, body)
	}
}
