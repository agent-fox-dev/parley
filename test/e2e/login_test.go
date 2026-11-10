package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/agent-fox-dev/parley/internal/cli"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/store"
)

// fakeGitHub is an identity provider: an authorize page that redirects to
// the given redirect_uri with a fresh code and the given state, a token URL
// that turns a code into an access token once, and a user URL.
type fakeGitHub struct {
	*httptest.Server
	mu     sync.Mutex
	codes  map[string]bool
	serial atomic.Int32
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	g := &fakeGitHub{codes: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		code := fmt.Sprintf("code-%d", g.serial.Add(1))
		g.mu.Lock()
		g.codes[code] = true
		g.mu.Unlock()
		q := r.URL.Query()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		code := r.PostForm.Get("code")
		g.mu.Lock()
		ok := g.codes[code]
		delete(g.codes, code)
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			io.WriteString(w, `{"error":"bad_verification_code"}`)
			return
		}
		io.WriteString(w, `{"access_token":"at-`+code+`"}`)
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":123,"login":"Alice.Smith","name":"Alice Smith","email":"a@x"}`)
	})
	g.Server = httptest.NewServer(mux)
	t.Cleanup(g.Close)
	return g
}

// browse follows the URL and its redirects, as a browser does.
func browse(raw string) error {
	resp, err := http.Get(raw)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// loginHub starts a hub whose github provider is the fake.
func loginHub(t *testing.T, gh *fakeGitHub) (string, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	conf := fmt.Sprintf("[server]\nbind = \"127.0.0.1\"\nport = 0\n[[oauth.providers]]\nname = \"github\"\nclient_id = \"gh-id\"\nclient_secret = \"gh-secret\"\nauthorize_url = %q\ntoken_url = %q\nuserinfo_url = %q\n",
		gh.URL+"/authorize", gh.URL+"/token", gh.URL+"/user")
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	var st atomic.Pointer[store.Store]
	addr, _ := inProcessHub(t, dir, whoamiRoute, func(rt *hub.Runtime) { st.Store(rt.Store) })
	return "http://" + addr, st.Load()
}

func cliLogin(t *testing.T, configDir, hubURL string) (map[string]any, map[string]any) {
	t.Helper()
	t.Setenv("PARLEY_CONFIG_DIR", configDir)
	t.Setenv("PARLEY_ENDPOINT_URL", "")
	var out, errb bytes.Buffer
	code := cli.RunWith(context.Background(), []string{"login", "--endpoint-url", hubURL}, cli.Options{Opener: browse, LoginWait: 10 * time.Second}, &out, &errb)
	if code != 0 {
		t.Fatalf("parley login exit %d: %s", code, errb.String())
	}
	var user map[string]any
	if err := json.Unmarshal(out.Bytes(), &user); err != nil {
		t.Fatalf("stdout %q: %v", out.String(), err)
	}
	b, err := os.ReadFile(filepath.Join(configDir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := toml.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	return user, cfg
}

func count(t *testing.T, st *store.Store, query string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestLogin_AFirstLoginLeavesAWorkingKey(t *testing.T) {
	gh := newFakeGitHub(t)
	hubURL, st := loginHub(t, gh)
	cfgDir := filepath.Join(t.TempDir(), "a")
	user, cfg := cliLogin(t, cfgDir, hubURL)
	if user["username"] != "Alice.Smith" || user["status"] != "active" {
		t.Errorf("user %v", user)
	}
	fi, _ := os.Stat(filepath.Join(cfgDir, "config.toml"))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %o", fi.Mode().Perm())
	}
	key, _ := cfg["api_key"].(string)
	if cfg["endpoint_url"] != hubURL || cfg["user_id"] != user["id"] || !regexp.MustCompile(`^parley_[0-9A-Za-z]{8}_[0-9A-Za-z]{32}$`).MatchString(key) {
		t.Errorf("config %v", cfg)
	}
	status, who := getJSON(t, hubURL+"/api/v1/test/whoami", "Bearer "+key)
	whoUser, _ := who["user"].(map[string]any)
	if status != 200 || who["type"] != "api_key" || whoUser["id"] != user["id"] {
		t.Errorf("whoami with the key: %d %v", status, who)
	}
	if status, doc := getJSON(t, hubURL+"/api/v1/test/whoami", ""); status != 401 || errorMessage(doc) != "authentication required" {
		t.Errorf("whoami without a key: %d %v", status, doc)
	}
	var slug, owner string
	if err := st.DB().QueryRow(`SELECT slug, owner_id FROM orgs`).Scan(&slug, &owner); err != nil {
		t.Fatal(err)
	}
	if slug != "alice-smith" || owner != user["id"] || count(t, st, `SELECT count(*) FROM users`) != 1 || count(t, st, `SELECT count(*) FROM memberships`) != 1 {
		t.Errorf("org %s owned by %s", slug, owner)
	}
}

func TestLogin_ASecondLoginReplacesTheKey(t *testing.T) {
	gh := newFakeGitHub(t)
	hubURL, st := loginHub(t, gh)
	dirA, dirB := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	userA, cfgA := cliLogin(t, dirA, hubURL)
	userB, cfgB := cliLogin(t, dirB, hubURL)
	k1, k2 := cfgA["api_key"].(string), cfgB["api_key"].(string)
	if k1 == k2 || userA["id"] != userB["id"] || cfgA["user_id"] != userB["id"] {
		t.Errorf("users %v %v keys equal %v", userA["id"], userB["id"], k1 == k2)
	}
	status, doc := getJSON(t, hubURL+"/api/v1/test/whoami", "Bearer "+k1)
	if e, _ := doc["error"].(map[string]any); status != 401 || e["error_type"] != "unauthenticated" || e["message"] != "credential revoked" {
		t.Errorf("the first key: %d %v", status, doc)
	}
	if status, _ := getJSON(t, hubURL+"/api/v1/test/whoami", "Bearer "+k2); status != 200 {
		t.Errorf("the second key: %d", status)
	}
	if count(t, st, `SELECT count(*) FROM users`) != 1 || count(t, st, `SELECT count(*) FROM orgs`) != 1 ||
		count(t, st, `SELECT count(*) FROM api_keys`) != 2 || count(t, st, `SELECT count(*) FROM api_keys WHERE revoked_at IS NOT NULL`) != 1 {
		t.Error("the store does not hold one user, one org and two keys, one revoked")
	}
}
