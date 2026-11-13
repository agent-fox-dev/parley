package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/agent-fox-dev/parley/internal/cli"
	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
)

// fakeGitHub is GitHub's sign-in as a browser and the hub meet it: an
// authorize page that sends the browser back to redirect_uri with a fresh
// code and the state, a token URL that turns each code into a token once,
// and a user URL that answers the one user for a token it issued.
type fakeGitHub struct {
	*httptest.Server

	mu     sync.Mutex
	issued int
	codes  map[string]bool
	tokens map[string]bool
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	g := &fakeGitHub{codes: map[string]bool{}, tokens: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		back, err := url.Parse(r.URL.Query().Get("redirect_uri"))
		if err != nil || back.Host == "" {
			http.Error(w, "bad redirect_uri", http.StatusBadRequest)
			return
		}
		g.mu.Lock()
		g.issued++
		code := fmt.Sprintf("code-%d", g.issued)
		g.codes[code] = true
		g.mu.Unlock()
		back.RawQuery = url.Values{"code": {code}, "state": {r.URL.Query().Get("state")}}.Encode()
		http.Redirect(w, r, back.String(), http.StatusFound)
	})
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		code := r.PostFormValue("code")
		g.mu.Lock()
		fresh := g.codes[code]
		delete(g.codes, code)
		if fresh {
			g.tokens["token-"+code] = true
		}
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !fresh {
			fmt.Fprint(w, `{"error":"bad_verification_code"}`)
			return
		}
		fmt.Fprintf(w, `{"access_token":%q,"token_type":"bearer"}`, "token-"+code)
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		known := g.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		g.mu.Unlock()
		if !known {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":123,"login":"Alice.Smith","name":"Alice Smith","email":"a@x"}`)
	})
	g.Server = httptest.NewServer(mux)
	t.Cleanup(g.Close)
	return g
}

// startLoginHub serves a hub whose one identity provider, github, is a fake
// GitHub, with GET /api/v1/test/whoami added. It returns the hub's URL and
// the path of its database file.
func startLoginHub(t *testing.T) (hubURL, dbPath string) {
	t.Helper()
	dir := t.TempDir()
	gh := newFakeGitHub(t)
	writeConfig(t, dir, fmt.Sprintf("[server]\nbind = \"127.0.0.1\"\nport = 0\n\n"+
		"[[oauth.providers]]\nname = \"github\"\nclient_id = \"gh-id\"\nclient_secret = \"gh-secret\"\n"+
		"authorize_url = %q\ntoken_url = %q\nuserinfo_url = %q\n",
		gh.URL+"/login/oauth/authorize", gh.URL+"/login/oauth/access_token", gh.URL+"/user"))
	env := config.MapEnv(map[string]string{"XDG_CONFIG_HOME": dir, "XDG_DATA_HOME": dir})
	s := hubtest.Serve(t, hub.Options{Env: env, WorkDir: dir, Compose: addWhoami})
	return s.URL(""), filepath.Join(dir, "parley.db")
}

// browserLogin runs parley login against hubURL with configDir as
// PARLEY_CONFIG_DIR and a browser that follows the provider's redirects. It
// fails the test unless the login succeeds, and returns the printed User
// and the saved configuration.
func browserLogin(t *testing.T, hubURL, configDir string) (user, saved map[string]any) {
	t.Helper()
	t.Setenv("PARLEY_CONFIG_DIR", configDir)
	browse := func(u string) error {
		res, err := http.Get(u)
		if err != nil {
			return err
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("the sign-in ended with %s", res.Status)
		}
		return nil
	}
	var stdout, stderr bytes.Buffer
	code := cli.RunWith(context.Background(), []string{"login", "--endpoint-url", hubURL}, &stdout, &stderr,
		cli.Hooks{OpenBrowser: browse, LoginWait: 10 * time.Second})
	if code != 0 {
		t.Fatalf("parley login returned %d, want 0; stderr = %q", code, stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &user); err != nil {
		t.Fatalf("stdout is not a JSON object: %v: %q", err, stdout.String())
	}
	data, err := os.ReadFile(filepath.Join(configDir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := toml.Unmarshal(data, &saved); err != nil {
		t.Fatalf("config.toml is not TOML: %v", err)
	}
	return user, saved
}

var apiKeyFormat = regexp.MustCompile(`^parley_[0-9A-Za-z]{8}_[0-9A-Za-z]{32}$`)

func TestLogin_ADeveloperSignsInForTheFirstTimeAndTheSavedKeyWorks(t *testing.T) {
	hubURL, db := startLoginHub(t)
	configDir := filepath.Join(t.TempDir(), "parley")

	user, saved := browserLogin(t, hubURL, configDir)

	if user["username"] != "Alice.Smith" || user["status"] != "active" {
		t.Errorf("printed user %v, want username Alice.Smith and status active", user)
	}
	if fi, err := os.Stat(filepath.Join(configDir, "config.toml")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("config.toml: %v, want mode 0600", fi)
	}
	key, _ := saved["api_key"].(string)
	if saved["endpoint_url"] != hubURL || saved["user_id"] != user["id"] || !apiKeyFormat.MatchString(key) {
		t.Errorf("saved %v, want endpoint_url %s, user_id %v and an API key", saved, hubURL, user["id"])
	}
	whoami := hubURL + "/api/v1/test/whoami"
	code, body := getAuthorized(t, whoami, "Bearer "+key)
	if who, _ := body["user"].(map[string]any); code != http.StatusOK || body["type"] != "api_key" || who["id"] != user["id"] {
		t.Errorf("whoami with the saved key: %d %v, want 200 with type api_key and user %v", code, body, user["id"])
	}
	code, body = getAuthorized(t, whoami, "")
	if e, _ := body["error"].(map[string]any); code != http.StatusUnauthorized || e["message"] != "authentication required" {
		t.Errorf("whoami without a credential: %d %v, want 401 %q", code, body, "authentication required")
	}
	if n := countRows(t, db, "users"); n != 1 {
		t.Errorf("%d users, want 1", n)
	}
	if n := countRows(t, db, "memberships"); n != 1 {
		t.Errorf("%d memberships, want 1", n)
	}
	if n, own := countRows(t, db, "orgs"), countRows(t, db, fmt.Sprintf("orgs WHERE slug = 'alice-smith' AND owner_id = '%s'", user["id"])); n != 1 || own != 1 {
		t.Errorf("%d orgs, %d of them alice-smith owned by the user, want 1 and 1", n, own)
	}
}

func TestLogin_ASecondLoginReplacesTheDevelopersKey(t *testing.T) {
	hubURL, db := startLoginHub(t)
	first, second := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	user1, saved1 := browserLogin(t, hubURL, first)

	user2, saved2 := browserLogin(t, hubURL, second)

	if user2["id"] != user1["id"] || saved1["user_id"] != user1["id"] {
		t.Errorf("the logins printed the users %v and %v, want the same", user1["id"], user2["id"])
	}
	if saved1["api_key"] == saved2["api_key"] {
		t.Errorf("both logins saved the key %v, want two keys", saved1["api_key"])
	}
	whoami := hubURL + "/api/v1/test/whoami"
	code, body := getAuthorized(t, whoami, fmt.Sprint("Bearer ", saved1["api_key"]))
	if e, _ := body["error"].(map[string]any); code != http.StatusUnauthorized || e["error_type"] != "unauthenticated" || e["message"] != "credential revoked" {
		t.Errorf("the first key: %d %v, want 401 unauthenticated credential revoked", code, body)
	}
	if code, _ := getAuthorized(t, whoami, fmt.Sprint("Bearer ", saved2["api_key"])); code != http.StatusOK {
		t.Errorf("the second key: %d, want 200", code)
	}
	for table, want := range map[string]int{"users": 1, "orgs": 1, "api_keys": 2, "api_keys WHERE revoked_at IS NOT NULL": 1} {
		if n := countRows(t, db, table); n != want {
			t.Errorf("%d rows in %s, want %d", n, table, want)
		}
	}
}
