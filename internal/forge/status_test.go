package forge_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/forge"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// hubURL is the external URL of the hubs of the status tests.
const hubURL = "https://hub.example.com"

// externalURL is the [server] line that sets external_url to u, none for "".
func externalURL(u string) string {
	if u == "" {
		return ""
	}
	return fmt.Sprintf("external_url = %q", u)
}

// counting counts the requests its transport is given.
type counting struct {
	http.RoundTripper
	n *atomic.Int64
}

func (c counting) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return c.RoundTripper.RoundTrip(r)
}

// newStatusWorld is a world whose hub has the external URL external and
// whose forge client counts in sent every request it sends, to any host.
func newStatusWorld(t *testing.T, external string) (*world, *atomic.Int64) {
	t.Helper()
	sent := new(atomic.Int64)
	w := newWorldWith(t, hubtest.Config{Server: externalURL(external)}, func(_ *world, d *forge.Deps) {
		d.Client.Transport = counting{d.Client.Transport, sent}
	})
	return w, sent
}

// members decodes a diagnostic document into its members.
func members(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("the answer %s is no document: %v", body, err)
	}
	return doc
}

// jsonValue decodes the JSON text s.
func jsonValue(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("decode %s: %v", s, err)
	}
	return v
}

// The members of a document that describe github.com/acme/widgets.
const githubWidgets = `"forge":"github","api_url":"https://api.github.com",` +
	`"repository":{"host":"github.com","owner":"acme","name":"widgets"}`

func TestStatus_DescribesTheForgeWithoutCallingIt(t *testing.T) {
	t.Parallel()
	w, sent := newStatusWorld(t, hubURL)
	gh := new("https://github.com/acme/widgets.git")
	carry := workspace.ModeCarryPatch
	w.addWorkspace(t, workspace.Workspace{Slug: "widgets", GitURL: gh})
	w.putSecret(t, "widgets", workspace.SecretGitPAT, "t1")
	w.addWorkspace(t, workspace.Workspace{Slug: "one-segment", GitURL: new("https://github.com/widgets")})
	w.addWorkspace(t, workspace.Workspace{Slug: "unknown", GitURL: new("https://code.example.com/acme/widgets.git")})
	w.addWorkspace(t, workspace.Workspace{Slug: "carry", Mode: carry, GitURL: gh, UpstreamURL: new("https://gitlab.com/group/sub/proj.git")})
	w.putSecret(t, "carry", "UPSTREAM_GIT_PAT", "u1")
	w.addWorkspace(t, workspace.Workspace{Slug: "carry-no-upstream", Mode: carry, GitURL: gh})
	w.addWorkspace(t, workspace.Workspace{Slug: "upstream-setting-unused", GitURL: gh,
		Settings: settings(forge.SettingUpstreamAPIURL, "https://u:p@x.example.com")})

	t.Run("the document", func(t *testing.T) {
		cases := []struct {
			name, slug, query string
			// want are the members but webhook.
			want string
		}{
			{"a GitHub workspace", "widgets", "", `{` + githubWidgets + `,"credential":"pat","upstream":null}`},
			{"a target given", "widgets", "?target=upstream", `{` + githubWidgets + `,"credential":"pat","upstream":null}`},
			{"a remote naming no repository", "one-segment", "",
				`{"forge":"unknown","api_url":null,"repository":null,"credential":"none","upstream":null}`},
			{"a forge of unknown kind", "unknown", "",
				`{"forge":"unknown","api_url":null,"repository":{"host":"code.example.com","owner":"acme","name":"widgets"},"credential":"none","upstream":null}`},
			{"a carry-patch workspace's upstream", "carry", "", `{` + githubWidgets + `,"credential":"none","upstream":{"forge":"gitlab",` +
				`"api_url":"https://gitlab.com/api/v4","repository":{"host":"gitlab.com","owner":"group/sub","name":"proj"},"credential":"pat"}}`},
			{"a carry-patch workspace without upstream_url", "carry-no-upstream", "", `{` + githubWidgets + `,"credential":"none","upstream":null}`},
			{"an upstream setting of a standard workspace", "upstream-setting-unused", "", `{` + githubWidgets + `,"credential":"none","upstream":null}`},
		}
		for _, c := range cases {
			status, body, _ := w.get(t, c.slug, "/forge"+c.query, w.ownerKey)
			if status != http.StatusOK {
				t.Errorf("%s: GET %s/forge%s = %d %s, want 200", c.name, c.slug, c.query, status, body)
				continue
			}
			got := members(t, body)
			webhook, _ := got["webhook"].(map[string]any)
			delete(got, "webhook")
			if want := jsonValue(t, c.want); !reflect.DeepEqual(got, want) {
				t.Errorf("%s: GET %s/forge%s = %s, want %s and a webhook", c.name, c.slug, c.query, body, c.want)
			}
			if keys := slices.Sorted(maps.Keys(webhook)); !slices.Equal(keys, []string{"configured", "last_delivery_at", "url"}) {
				t.Errorf("%s: webhook has the members %v, want configured, url and last_delivery_at", c.name, keys)
			}
			if strings.Contains(string(body), "t1") || strings.Contains(string(body), "u1") {
				t.Errorf("%s: the document %s holds a token", c.name, body)
			}
		}
		if n, m := sent.Load(), w.fake.Count(); n != 0 || m != 0 {
			t.Errorf("the forge client sent %d requests, the fake received %d, want none", n, m)
		}
	})

	t.Run("a task fork answers its parent's document", func(t *testing.T) {
		w.addWorkspace(t, workspace.Workspace{Slug: "p", GitURL: gh})
		w.putSecret(t, "p", workspace.SecretGitPAT, "parent-token")
		w.putSecret(t, "p", forge.SecretWebhook, "hook")
		w.addWorkspace(t, workspace.Workspace{Slug: "f", ParentSlug: new("p")})
		delivered := time.Date(2026, 10, 21, 9, 12, 3, 0, time.UTC)
		w.forge.SetLastDeliveryProvider(func(_ context.Context, slug string) (time.Time, bool, error) {
			return delivered, slug == "p", nil
		})
		defer w.forge.SetLastDeliveryProvider(nil)
		fs, fb, _ := w.get(t, "f", "/forge", w.ownerKey)
		ps, pb, _ := w.get(t, "p", "/forge", w.ownerKey)
		if fs != http.StatusOK || ps != http.StatusOK || string(fb) != string(pb) {
			t.Fatalf("GET f/forge = %d %s, GET p/forge = %d %s, want 200 with the same document", fs, fb, ps, pb)
		}
		want := jsonValue(t, `{"configured":true,"url":"https://hub.example.com/hooks/forge/p","last_delivery_at":"2026-10-21T09:12:03Z"}`)
		if got := members(t, fb)["webhook"]; !reflect.DeepEqual(got, want) {
			t.Errorf("the fork's webhook = %v, want the parent's, %v", got, want)
		}
	})

	t.Run("refuses an invalid API URL setting that applies", func(t *testing.T) {
		w.addWorkspace(t, workspace.Workspace{Slug: "bad-api", GitURL: gh, Settings: settings(forge.SettingAPIURL, "http://x.example.com")})
		w.addWorkspace(t, workspace.Workspace{Slug: "bad-upstream-api", Mode: carry, GitURL: gh,
			UpstreamURL: new("https://gitlab.com/group/sub/proj.git"), Settings: settings(forge.SettingUpstreamAPIURL, "https://u:p@x.example.com")})
		w.addWorkspace(t, workspace.Workspace{Slug: brokenSlug, GitURL: gh})
		cases := []struct {
			slug, errorType, contains string
			status                    int
		}{
			{"bad-api", forge.ErrorTypeAPIURLInvalid, forge.SettingAPIURL, http.StatusUnprocessableEntity},
			{"bad-upstream-api", forge.ErrorTypeAPIURLInvalid, forge.SettingUpstreamAPIURL, http.StatusUnprocessableEntity},
			{brokenSlug, "internal", "", http.StatusInternalServerError},
		}
		for _, c := range cases {
			status, body, _ := w.get(t, c.slug, "/forge", w.ownerKey)
			if e := errorOf(t, body); status != c.status || e.Type != c.errorType || !strings.Contains(e.Message, c.contains) {
				t.Errorf("GET %s/forge = %d %s, want %d %s naming %q", c.slug, status, body, c.status, c.errorType, c.contains)
			}
		}
	})
}

func TestStatus_ReportsTheWebhook(t *testing.T) {
	t.Parallel()
	reportsNone := func(context.Context, string) (time.Time, bool, error) { return time.Time{}, false, nil }
	reportsOne := func(context.Context, string) (time.Time, bool, error) {
		return time.Date(2026, 10, 21, 9, 12, 3, 700_000_000, time.UTC), true, nil
	}
	fails := func(context.Context, string) (time.Time, bool, error) {
		return time.Time{}, false, errors.New("the delivery log cannot be read")
	}
	const (
		none      = `{"configured":false,"url":null,"last_delivery_at":null}`
		noneYet   = `{"configured":true,"url":"https://hub.example.com/hooks/forge/widgets","last_delivery_at":null}`
		delivered = `{"configured":true,"url":"https://hub.example.com/hooks/forge/widgets","last_delivery_at":"2026-10-21T09:12:03Z"}`
	)
	cases := []struct {
		name, external, slug string
		provider             forge.LastDeliveryProvider
		// want is the member webhook; "" wants 500 internal.
		want string
	}{
		{"no secret, no external URL, no provider", "", "unset", nil, none},
		{"a secret stored empty", "", "empty", nil, none},
		{"a provider reporting no delivery", hubURL, "widgets", reportsNone, noneYet},
		{"an external URL with a trailing slash", hubURL + "/", "widgets", nil, noneYet},
		{"a provider reporting a delivery", hubURL, "widgets", reportsOne, delivered},
		{"a provider failing", hubURL, "widgets", fails, ""},
	}
	worlds := map[string]*world{}
	for _, c := range cases {
		w := worlds[c.external]
		if w == nil {
			w, _ = newStatusWorld(t, c.external)
			for _, slug := range []string{"unset", "empty", "widgets"} {
				w.addWorkspace(t, workspace.Workspace{Slug: slug, GitURL: new("https://github.com/acme/widgets.git")})
			}
			w.putSecret(t, "empty", forge.SecretWebhook, "")
			w.putSecret(t, "widgets", forge.SecretWebhook, "hook-secret")
			worlds[c.external] = w
		}
		w.forge.SetLastDeliveryProvider(c.provider)
		status, body, _ := w.get(t, c.slug, "/forge", w.ownerKey)
		switch {
		case c.want == "":
			if status != http.StatusInternalServerError || errorOf(t, body).Type != "internal" {
				t.Errorf("%s: GET %s/forge = %d %s, want 500 internal", c.name, c.slug, status, body)
			}
		case status != http.StatusOK:
			t.Errorf("%s: GET %s/forge = %d %s, want 200", c.name, c.slug, status, body)
		default:
			if got, want := members(t, body)["webhook"], jsonValue(t, c.want); !reflect.DeepEqual(got, want) {
				t.Errorf("%s: webhook = %v, want %s", c.name, got, c.want)
			}
		}
		if strings.Contains(string(body), "hook-secret") {
			t.Errorf("%s: the answer %s holds the webhook secret", c.name, body)
		}
	}
}
