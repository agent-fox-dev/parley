package webhook_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/secret"
)

// secretShape is the form of a signing secret as a create shows it.
var secretShape = regexp.MustCompile(`^whsec_[0-9a-f]{64}$`)

// eventCreate is the type of the event a create writes.
const eventCreate = "parley.webhook.create"

func TestCreate_StoresTheSubscriptionAndShowsItsSecretOnce(t *testing.T) {
	t.Parallel()
	w := newWorld(t, options{})
	const url = "https://ci.example/hooks/parley?t=1"
	for _, tc := range []struct {
		name       string
		credential string
		body       map[string]any
		types      []any
		forks      bool
		desc       string
		actorType  string
		createdBy  any
	}{
		{
			name: "the owner with the url alone gets the defaults", credential: w.ownerKey,
			body:  map[string]any{"url": url},
			types: []any{"*"}, forks: true, desc: "", actorType: "api_key", createdBy: w.owner,
		},
		{
			name: "the admin token with every field gets those values", credential: w.Token,
			body:  map[string]any{"url": url, "types": []string{"parley.change.*", "parley.git.push"}, "include_forks": false, "description": "CI"},
			types: []any{"parley.change.*", "parley.git.push"}, forks: false, desc: "CI", actorType: "admin_token", createdBy: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(w.events(t, eventCreate))
			status, raw := w.create(t, tc.credential, demo, tc.body)
			if status != http.StatusCreated {
				t.Fatalf("create = %d %s, want 201", status, raw)
			}
			got := decode(t, raw)
			id, _ := got["id"].(string)
			if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(id) {
				t.Errorf("id = %v, want a UUID", got["id"])
			}
			want := map[string]any{
				"id": id, "workspace": demo, "url": url, "types": tc.types, "include_forks": tc.forks,
				"description": tc.desc, "state": "active", "created_by": tc.createdBy,
				"created_at": w.clock.stamp(), "updated_at": w.clock.stamp(),
				"last_delivery_at": nil, "last_status": nil, "failing_since": nil, "secret": got["secret"],
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("answer = %v\nwant     %v", got, want)
			}
			s1, _ := got["secret"].(string)
			if !secretShape.MatchString(s1) {
				t.Errorf("secret = %q, want whsec_ and 64 lower-case hexadecimal characters", s1)
			}

			created := w.events(t, eventCreate)
			if len(created) != before+1 {
				t.Fatalf("the log holds %d create events, want %d", len(created), before+1)
			}
			e := created[len(created)-1]
			if e.Workspace.String != demo || e.ActorType != tc.actorType || e.ResourceType != "event_webhook" || e.ResourceID != id {
				t.Errorf("event = %+v, want workspace %s, actor %s, resource event_webhook %s", e, demo, tc.actorType, id)
			}
			if tc.createdBy == nil && e.ActorID.Valid || tc.createdBy != nil && e.ActorID.String != tc.createdBy {
				t.Errorf("event actor id = %v, want %v", e.ActorID, tc.createdBy)
			}
			wantMeta := map[string]any{"webhook_id": id, "host": "ci.example", "types": tc.types, "include_forks": tc.forks}
			if !reflect.DeepEqual(e.Metadata, wantMeta) {
				t.Errorf("event metadata = %v, want %v", e.Metadata, wantMeta)
			}
			if meta := fmt.Sprint(e.Metadata); strings.Contains(meta, "/hooks/parley") || strings.Contains(meta, "t=1") {
				t.Errorf("event metadata %s holds the url's path or query", meta)
			}
			var position int64
			if err := w.Store.Main().QueryRow(`SELECT position FROM webhook_subscriptions WHERE id = ?`, id).Scan(&position); err != nil {
				t.Fatalf("read the position: %v", err)
			}
			if position != e.Seq {
				t.Errorf("position = %d, want the seq of the create event, %d", position, e.Seq)
			}

			// A second create shows a secret of its own.
			again := w.mustCreate(t, demo, url)
			if again["secret"] == s1 {
				t.Errorf("two creates showed the same secret %q", s1)
			}
		})
	}
}

func TestCreate_RefusesABodyThatBreaksAFieldRule(t *testing.T) {
	t.Parallel()
	urlOf := func(n int) string {
		const prefix = "https://ci.example/"
		return prefix + strings.Repeat("a", n-len(prefix))
	}
	entries := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("parley.t%d", i)
		}
		return out
	}
	const ok = "https://ci.example/x"
	// field is the field the message must name, "" for a body that is
	// created; insecure runs the row on a hub with allow_insecure.
	type row struct {
		name     string
		body     map[string]any
		insecure bool
		field    string
	}
	rows := []row{
		{"no url", map[string]any{"description": "x"}, false, "url"},
		{"a url that is not a string", map[string]any{"url": 7}, false, "url"},
		{"a url that is not absolute", map[string]any{"url": "ci.example/x"}, false, "url"},
		{"an http url", map[string]any{"url": "http://ci.example/x"}, false, "url"},
		{"an http url to a loopback address without allow_insecure", map[string]any{"url": "http://127.0.0.1/x"}, false, "url"},
		{"an http url to a public host with allow_insecure", map[string]any{"url": "http://ci.example/x"}, true, "url"},
		{"a url without a host", map[string]any{"url": "https:///x"}, false, "url"},
		{"a url with userinfo", map[string]any{"url": "https://u:p@ci.example/"}, false, "url"},
		{"a url with a fragment", map[string]any{"url": "https://ci.example/#f"}, false, "url"},
		{"a url of 2,049 characters", map[string]any{"url": urlOf(2049)}, false, "url"},
		{"types that are not a list", map[string]any{"url": ok, "types": "x"}, false, "types"},
		{"types that are not strings", map[string]any{"url": ok, "types": []any{1}}, false, "types"},
		{"an empty types list", map[string]any{"url": ok, "types": []string{}}, false, "types"},
		{"21 types entries", map[string]any{"url": ok, "types": entries(21)}, false, "types"},
		{"a types entry with * inside", map[string]any{"url": ok, "types": []string{"parley.*.land"}}, false, "types"},
		{"an empty types entry", map[string]any{"url": ok, "types": []string{""}}, false, "types"},
		{"include_forks that is not a bool", map[string]any{"url": ok, "include_forks": "yes"}, false, "include_forks"},
		{"a description that is not a string", map[string]any{"url": ok, "description": 7}, false, "description"},
		{"a description of 1,025 characters", map[string]any{"url": ok, "description": strings.Repeat("d", 1025)}, false, "description"},
		{"a url of exactly 2,048 characters", map[string]any{"url": urlOf(2048)}, false, ""},
		{"20 types entries", map[string]any{"url": ok, "types": entries(20)}, false, ""},
		{"a description of 1,024 characters", map[string]any{"url": ok, "description": strings.Repeat("d", 1024)}, false, ""},
		{"an unknown member", map[string]any{"url": ok, "color": "red"}, false, ""},
		{"an http url to a loopback address with allow_insecure", map[string]any{"url": "http://127.0.0.1:9/x"}, true, ""},
		{"an http url to localhost with allow_insecure", map[string]any{"url": "http://localhost:9/x"}, true, ""},
	}
	for _, insecure := range []bool{false, true} {
		t.Run(fmt.Sprintf("allow_insecure %t", insecure), func(t *testing.T) {
			t.Parallel()
			opts := options{}
			if insecure {
				opts.toml = "[webhooks]\nallow_insecure = true\n"
			}
			w := newWorld(t, opts)
			for _, tc := range rows {
				if tc.insecure != insecure {
					continue
				}
				t.Run(tc.name, func(t *testing.T) {
					count, seq := w.count(t, ""), w.lastSeq(t)
					status, raw := w.create(t, w.ownerKey, demo, tc.body)
					if tc.field == "" {
						if status != http.StatusCreated {
							t.Errorf("create = %d %s, want 201", status, raw)
						}
						return
					}
					if status != http.StatusBadRequest {
						t.Fatalf("create = %d %s, want 400", status, raw)
					}
					e := errorOf(t, raw)
					if e["error_type"] != "invalid_request" {
						t.Errorf("error_type = %v, want invalid_request", e["error_type"])
					}
					if msg, _ := e["message"].(string); !strings.Contains(msg, tc.field) {
						t.Errorf("message %q does not name %s", msg, tc.field)
					}
					if got := w.count(t, ""); got != count || count != 0 {
						t.Errorf("subscriptions = %d after the refusal, want %d and none", got, count)
					}
					if got := w.lastSeq(t); got != seq {
						t.Errorf("the newest seq moved from %d to %d", seq, got)
					}
				})
			}
		})
	}
}

func TestCreate_RefusesPastTheWorkspaceLimit(t *testing.T) {
	t.Parallel()
	const url = "https://ci.example/x"
	for _, tc := range []struct {
		name  string
		max   int
		setup func(t *testing.T, w *world)
		slug  string
		want  []int
	}{
		{
			name: "an active and a disabled subscription fill a limit of 2", max: 2,
			setup: func(t *testing.T, w *world) {
				w.mustCreate(t, demo, url)
				disabled := w.mustCreate(t, demo, url)
				if _, err := w.Store.Main().Exec(`UPDATE webhook_subscriptions SET state = 'disabled' WHERE id = ?`, disabled["id"]); err != nil {
					t.Fatal(err)
				}
			},
			slug: demo, want: []int{http.StatusTooManyRequests},
		},
		{
			name: "one subscription under a limit of 2", max: 2,
			setup: func(t *testing.T, w *world) { w.mustCreate(t, demo, url) },
			slug:  demo, want: []int{http.StatusCreated, http.StatusTooManyRequests},
		},
		{
			name: "a limit of 0", max: 0,
			setup: func(*testing.T, *world) {},
			slug:  demo, want: []int{http.StatusTooManyRequests},
		},
		{
			name: "another workspace's subscriptions do not count", max: 2,
			setup: func(t *testing.T, w *world) { w.mustCreate(t, demo, url); w.mustCreate(t, demo, url) },
			slug:  other, want: []int{http.StatusCreated},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t, options{toml: fmt.Sprintf("[webhooks]\nmax_per_workspace = %d\n", tc.max)})
			tc.setup(t, w)
			for i, want := range tc.want {
				count, created := w.count(t, tc.slug), len(w.events(t, eventCreate))
				status, raw := w.create(t, w.ownerKey, tc.slug, map[string]any{"url": url})
				if status != want {
					t.Fatalf("create %d = %d %s, want %d", i+1, status, raw, want)
				}
				if want != http.StatusTooManyRequests {
					continue
				}
				if e := errorOf(t, raw); e["error_type"] != "webhook_limit_reached" {
					t.Errorf("error_type = %v, want webhook_limit_reached", e["error_type"])
				}
				if got := w.count(t, tc.slug); got != count {
					t.Errorf("subscriptions of %s = %d after the refusal, want %d", tc.slug, got, count)
				}
				if got := len(w.events(t, eventCreate)); got != created {
					t.Errorf("create events = %d after the refusal, want %d", got, created)
				}
			}
		})
	}
}

func TestCreate_ReplaysTheFirstAnswerWithoutTheSecret(t *testing.T) {
	t.Parallel()
	w := newWorld(t, options{})
	body := map[string]any{"url": "https://ci.example/x", "description": "CI"}
	header := http.Header{"Idempotency-Key": {"k1"}}
	s1, r1 := w.CallWith(t, http.MethodPost, hooksPath(demo), w.ownerKey, body, header)
	s2, r2 := w.CallWith(t, http.MethodPost, hooksPath(demo), w.ownerKey, body, header)
	if s1 != http.StatusCreated || s2 != http.StatusOK {
		t.Fatalf("statuses = %d, %d (%s), want 201, 200", s1, s2, r2)
	}
	first, replay := decode(t, r1), decode(t, r2)
	if _, ok := replay["secret"]; ok {
		t.Errorf("the replay %s holds the secret", r2)
	}
	if _, ok := first["secret"]; !ok {
		t.Errorf("the first answer %s holds no secret", r1)
	}
	delete(first, "secret")
	if !reflect.DeepEqual(replay, first) {
		t.Errorf("replay = %v\nwant     %v", replay, first)
	}
	if n := w.count(t, ""); n != 1 {
		t.Errorf("subscriptions = %d, want 1", n)
	}
	if n := len(w.events(t, eventCreate)); n != 1 {
		t.Errorf("create events = %d, want 1", n)
	}
}

func TestUpdate_RefusesABodyThatBreaksAFieldRule(t *testing.T) {
	t.Parallel()
	w := newWorld(t, options{})
	id, _ := w.mustCreate(t, demo, "https://ci.example/x")["id"].(string)
	for _, tc := range []struct {
		name string
		body any
	}{
		{"an empty body", map[string]any{}},
		{"an unknown member", map[string]any{"color": "red"}},
		{"a valid field and an unknown member", map[string]any{"description": "x", "color": "red"}},
		{"active that is not a bool", map[string]any{"active": "yes"}},
		{"an http url", map[string]any{"url": "http://ci.example/x"}},
		{"an empty types list", map[string]any{"types": []string{}}},
		{"include_forks that is not a bool", map[string]any{"include_forks": 1}},
		{"a description of 1,025 characters", map[string]any{"description": strings.Repeat("d", 1025)}},
		{"a body that is not an object", []string{"description"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, seq := w.configured(t), w.lastSeq(t)
			status, raw := w.Call(t, http.MethodPatch, hookPath(demo, id), w.ownerKey, tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("PATCH = %d %s, want 400", status, raw)
			}
			if e := errorOf(t, raw); e["error_type"] != "invalid_request" {
				t.Errorf("error_type = %v, want invalid_request", e["error_type"])
			}
			if got := w.configured(t); !reflect.DeepEqual(got, rows) {
				t.Errorf("rows = %v\nwant   %v", got, rows)
			}
			if got := w.lastSeq(t); got != seq {
				t.Errorf("the newest seq moved from %d to %d", seq, got)
			}
		})
	}
}

func TestSetURL_RefusesADestinationInTheHubsNetworks(t *testing.T) {
	t.Parallel()
	const (
		refused = "webhook_destination_refused"
		invalid = "invalid_request"
	)
	// row is a url and the error_type its create and change are refused
	// with, "" for a url they accept.
	type row struct{ url, want string }
	https := func(want string, hosts ...string) []row {
		out := make([]row, len(hosts))
		for i, h := range hosts {
			out[i] = row{"https://" + h + "/h", want}
		}
		return out
	}
	byDefault := append(https(refused,
		"127.0.0.1", "127.8.9.10", "localhost", "169.254.1.1", "169.254.169.254", "10.0.0.1", "172.16.0.1",
		"172.31.255.255", "192.168.1.1", "0.0.0.0", "224.0.0.1", "[::1]", "[fe80::1]", "[fc00::1]",
		"[fd00:ec2::254]", "[::]", "[ff02::1]", "[::ffff:10.0.0.1]", "inside.example", "mixed.example"),
		https("", "203.0.113.10", "172.32.0.1", "[2001:db8::1]", "ci.example", "nowhere.example")...)
	withPrivate := append(https("", "127.0.0.1", "10.0.0.1", "169.254.169.254", "[::1]", "[fc00::1]", "inside.example"),
		row{"http://10.0.0.1/x", invalid})
	withInsecure := []row{
		{"http://127.0.0.1:8080/h", ""},
		{"http://[::1]:8080/h", ""},
		{"http://localhost:8080/h", ""},
		{"http://ci.example/h", invalid},
		{"http://10.0.0.1/h", invalid},
		{"https://127.0.0.1/h", refused},
	}

	for _, tc := range []struct {
		name string
		toml string
		rows []row
	}{
		{"by default", "", byDefault},
		{"with allow_private", "[webhooks]\nallow_private = true\n", withPrivate},
		{"with allow_insecure", "[webhooks]\nallow_insecure = true\n", withInsecure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t, options{toml: tc.toml})
			id, _ := w.mustCreate(t, demo, "https://ci.example/keep")["id"].(string)
			for _, r := range tc.rows {
				t.Run(r.url, func(t *testing.T) {
					for _, op := range []struct {
						name   string
						send   func() (int, []byte)
						status int
					}{
						{"create", func() (int, []byte) { return w.create(t, w.ownerKey, demo, map[string]any{"url": r.url}) }, http.StatusCreated},
						{"PATCH url", func() (int, []byte) {
							return w.Call(t, http.MethodPatch, hookPath(demo, id), w.ownerKey, map[string]any{"url": r.url})
						}, http.StatusOK},
					} {
						rows, seq := w.configured(t), w.lastSeq(t)
						status, raw := op.send()
						if r.want == "" {
							if status != op.status {
								t.Errorf("%s = %d %s, want %d", op.name, status, raw, op.status)
							}
							continue
						}
						if status != http.StatusBadRequest {
							t.Errorf("%s = %d %s, want 400", op.name, status, raw)
							continue
						}
						if e := errorOf(t, raw); e["error_type"] != r.want {
							t.Errorf("%s error_type = %v, want %s", op.name, e["error_type"], r.want)
						}
						if got := w.configured(t); !reflect.DeepEqual(got, rows) {
							t.Errorf("%s changed the subscriptions:\n%v\nwant\n%v", op.name, got, rows)
						}
						if got := w.lastSeq(t); got != seq {
							t.Errorf("%s moved the newest seq from %d to %d", op.name, seq, got)
						}
					}
				})
			}
		})
	}
}

func TestChange_RefusesAnArchivedWorkspace(t *testing.T) {
	t.Parallel()
	w := newWorld(t, options{})
	id, _ := w.mustCreate(t, demo, "https://ci.example/x")["id"].(string)
	w.archive(t, demo)
	for _, tc := range []struct {
		name, method, path string
		body               any
	}{
		{"a create", http.MethodPost, hooksPath(demo), map[string]any{"url": "https://ci.example/y"}},
		{"a change of the description", http.MethodPatch, hookPath(demo, id), map[string]any{"description": "x"}},
		{"a switch off", http.MethodPatch, hookPath(demo, id), map[string]any{"active": false}},
		{"a rotation of the secret", http.MethodPost, hookPath(demo, id) + "/rotate-secret", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, seq := w.configured(t), w.lastSeq(t)
			status, raw := w.Call(t, tc.method, tc.path, w.ownerKey, tc.body)
			if status != http.StatusConflict {
				t.Fatalf("%s %s = %d %s, want 409", tc.method, tc.path, status, raw)
			}
			if e := errorOf(t, raw); e["error_type"] != "workspace_archived" {
				t.Errorf("error_type = %v, want workspace_archived", e["error_type"])
			}
			if got := w.configured(t); !reflect.DeepEqual(got, rows) {
				t.Errorf("rows = %v\nwant   %v", got, rows)
			}
			if got := w.lastSeq(t); got != seq {
				t.Errorf("the newest seq moved from %d to %d", seq, got)
			}
		})
	}
}

func TestSecretUse_RefusesWithoutAKey(t *testing.T) {
	t.Parallel()
	w := newWorld(t, options{})
	id, _ := w.mustCreate(t, demo, "https://ci.example/x")["id"].(string)
	// The hub starts again without the operator's key.
	w.RestartEnv(t, nil)
	for _, tc := range []struct {
		name, path string
		body       any
	}{
		{"a create", hooksPath(demo), map[string]any{"url": "https://ci.example/y"}},
		{"a rotation of a secret stored with a key", hookPath(demo, id) + "/rotate-secret", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, seq := w.configured(t), w.lastSeq(t)
			status, raw := w.Call(t, http.MethodPost, tc.path, w.ownerKey, tc.body)
			if status != http.StatusServiceUnavailable {
				t.Fatalf("POST %s = %d %s, want 503", tc.path, status, raw)
			}
			if e := errorOf(t, raw); e["error_type"] != "secrets_unavailable" {
				t.Errorf("error_type = %v, want secrets_unavailable", e["error_type"])
			}
			if got := w.configured(t); !reflect.DeepEqual(got, rows) {
				t.Errorf("rows = %v\nwant   %v", got, rows)
			}
			if got := w.lastSeq(t); got != seq {
				t.Errorf("the newest seq moved from %d to %d", seq, got)
			}
		})
	}
}

func TestDelete_RemovesTheSubscriptionWithItsDeliveriesInAnyStatus(t *testing.T) {
	t.Parallel()
	w := newWorld(t, options{toml: insecure})
	for _, tc := range []struct {
		status string
		slug   string
	}{
		{"ready", demo},
		{"archived", other},
	} {
		t.Run(tc.status, func(t *testing.T) {
			failing, healthy := failingInbox(t), newInbox(t, answerOK)
			id := w.subscribe(t, tc.slug, map[string]any{"url": failing.URL + "/hooks/parley?t=1", "types": testTypes})
			keep := w.subscribe(t, tc.slug, map[string]any{"url": healthy.url(), "types": testTypes})
			// A delivery that succeeded of each, as the delivery loop leaves
			// them, and a pending one of the subscription to delete, whose
			// retry the loop has scheduled.
			now := w.clock.Now().UnixNano()
			deliveryID := func(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%s%012d", tc.status[:1], i) }
			for i, sub := range []string{id, keep} {
				if _, err := w.Store.Main().Exec(`INSERT INTO webhook_deliveries
					(id, subscription_id, seq, type, status, attempts, created_at) VALUES (?, ?, ?, 'parley.change.open', 'succeeded', 1, ?)`,
					deliveryID(i), sub, i+1, now); err != nil {
					t.Fatal(err)
				}
			}
			w.emit(t, tc.slug, "parley.test.one", "")
			w.retrying(t, id, 1)
			var sealed []byte
			if err := w.Store.Main().QueryRow(`SELECT secret_sealed FROM webhook_subscriptions WHERE id = ?`, id).Scan(&sealed); err != nil {
				t.Fatal(err)
			}
			if tc.status == "archived" {
				w.archive(t, tc.slug)
			}
			deletes := len(w.events(t, "parley.webhook.delete"))

			status, raw := w.Call(t, http.MethodDelete, hookPath(tc.slug, id), w.ownerKey, nil)
			if status != http.StatusNoContent || len(raw) != 0 {
				t.Fatalf("DELETE = %d %q, want 204 and no body", status, raw)
			}
			status, raw = w.Call(t, http.MethodGet, hookPath(tc.slug, id), w.ownerKey, nil)
			if status != http.StatusNotFound || errorOf(t, raw)["error_type"] != "webhook_not_found" {
				t.Errorf("GET after the delete = %d %s, want 404 webhook_not_found", status, raw)
			}
			var deliveries, holding, kept int
			db := w.Store.Main()
			if err := db.QueryRow(`SELECT count(*) FROM webhook_deliveries WHERE subscription_id = ?`, id).Scan(&deliveries); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT count(*) FROM webhook_subscriptions WHERE secret_sealed = ?`, sealed).Scan(&holding); err != nil {
				t.Fatal(err)
			}
			// The other subscription's loop may record deliveries of its own
			// meanwhile; the one stored above must stay.
			if err := db.QueryRow(`SELECT count(*) FROM webhook_deliveries WHERE subscription_id = ? AND id = ?`, keep, deliveryID(1)).Scan(&kept); err != nil {
				t.Fatal(err)
			}
			if deliveries != 0 || holding != 0 {
				t.Errorf("after the delete %d deliveries and %d rows with its ciphertext remain, want none", deliveries, holding)
			}
			if kept != 1 || w.count(t, tc.slug) != 1 {
				t.Errorf("the other subscription kept %d deliveries and the workspace %d subscriptions, want 1 and 1", kept, w.count(t, tc.slug))
			}
			got := w.events(t, "parley.webhook.delete")
			if len(got) != deletes+1 {
				t.Fatalf("the log holds %d delete events, want %d", len(got), deletes+1)
			}
			e := got[len(got)-1]
			if e.Workspace.String != tc.slug || e.ActorType != "api_key" || e.ActorID.String != w.owner ||
				e.ResourceType != "event_webhook" || e.ResourceID != id {
				t.Errorf("event = %+v, want workspace %s, the owner's api_key, resource event_webhook %s", e, tc.slug, id)
			}
			if want := map[string]any{"webhook_id": id, "host": "127.0.0.1"}; !reflect.DeepEqual(e.Metadata, want) {
				t.Errorf("event metadata = %v, want %v", e.Metadata, want)
			}

			// Its deliveries stop: an hour on, past the scheduled retry, the
			// receiver has had no request since the delete, while the other
			// subscription delivers the next event.
			sent := len(failing.requests())
			w.clock.advance(time.Hour)
			seq := w.emit(t, tc.slug, "parley.test.two", "")
			waitFor(t, "the other subscription to deliver the next event", func() bool { return slices.Contains(healthy.seqs(), seq) })
			time.Sleep(20 * time.Millisecond)
			if n := len(failing.requests()); n != sent {
				t.Errorf("the receiver of the deleted subscription got %d requests after the delete, want none", n-sent)
			}
		})
	}
}

func TestUpdate_AppliesTheChangeFromTheNextAttemptAndKeepsThePendingDelivery(t *testing.T) {
	t.Parallel()
	w := newWorld(t, options{toml: insecure})
	failing := newInbox(t, func(delivered, <-chan struct{}) int { return http.StatusInternalServerError })
	healthy := newInbox(t, answerOK)
	id := w.subscribe(t, demo, map[string]any{"url": failing.url(), "types": []string{"*"}})
	e1 := w.emit(t, demo, "parley.change.open", "")
	pending := w.retrying(t, id, 1)
	e2 := w.emit(t, demo, "parley.git.push", "")
	e3 := w.emit(t, demo, "parley.change.open", "")

	status, raw := w.Call(t, http.MethodPatch, hookPath(demo, id), w.ownerKey,
		map[string]any{"url": healthy.url(), "types": []string{"parley.change.*"}, "description": "d"})
	if status != http.StatusOK {
		t.Fatalf("PATCH = %d %s, want 200", status, raw)
	}
	before := len(failing.requests())
	obj := decode(t, raw)
	if obj["url"] != healthy.url() || obj["description"] != "d" || obj["include_forks"] != true ||
		!reflect.DeepEqual(obj["types"], []any{"parley.change.*"}) || obj["updated_at"] != w.clock.stamp() {
		t.Errorf("PATCH answered %v, want the new url, types and description, include_forks true as before, updated_at %s", obj, w.clock.stamp())
	}
	updates := w.events(t, "parley.webhook.update")
	if len(updates) != 1 {
		t.Fatalf("the log holds %d update events, want 1", len(updates))
	}
	if m := updates[0].Metadata; !reflect.DeepEqual(m["fields"], []any{"url", "types", "description"}) || m["state"] != "active" {
		t.Errorf("update event metadata = %v, want fields [url types description] and state active", m)
	}
	if n := len(healthy.requests()); n != 0 {
		t.Errorf("the new url got %d requests before the next attempt was due, want 0", n)
	}

	// The clock moves on until the loop settles, not once: the PATCH and its
	// event wake the loop, and one that reads the clock just before a single
	// move arms its timer a whole wait past the move, with no wake-up left
	// to come.
	w.drive(t, "the loop to settle at the new url", 10*time.Second, func() bool { return w.settled(t, id) })
	got := healthy.requests()
	if seqs := healthy.seqs(); !reflect.DeepEqual(seqs, []int64{e1, e3}) {
		t.Errorf("the new url got seqs %v, want e1 %d then e3 %d (never e2 %d)", seqs, e1, e3, e2)
	}
	if len(got) > 0 && got[0].delivery != pending.id {
		t.Errorf("the new url got e1 under delivery %s, want the pending delivery's %s", got[0].delivery, pending.id)
	}
	if n := len(failing.requests()); n != before {
		t.Errorf("the old url got %d requests after the PATCH, want none", n-before)
	}
}

// signedWith reports whether the request d carries the signature of its
// timestamp and body under the signing secret key.
func signedWith(d delivered, key string) bool {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(d.timestamp + "." + string(d.body)))
	return hmac.Equal([]byte(d.signature), []byte("sha256="+hex.EncodeToString(mac.Sum(nil))))
}

func TestRotate_SignsTheNextAttemptOfAPendingDeliveryWithTheNewSecret(t *testing.T) {
	t.Parallel()
	w := newWorld(t, options{toml: insecure})
	in := newInbox(t, func(delivered, <-chan struct{}) int { return http.StatusInternalServerError })
	created := w.mustCreate(t, demo, in.url())
	id, _ := created["id"].(string)
	s1, _ := created["secret"].(string)
	w.emit(t, demo, "parley.change.open", "")
	w.retrying(t, id, 1)

	status, raw := w.Call(t, http.MethodPost, hookPath(demo, id)+"/rotate-secret", w.ownerKey, nil)
	if status != http.StatusOK {
		t.Fatalf("rotate = %d %s, want 200", status, raw)
	}
	s2, _ := decode(t, raw)["secret"].(string)
	if !secretShape.MatchString(s2) || s2 == s1 {
		t.Errorf("rotated secret = %q, want a new whsec_ secret other than %q", s2, s1)
	}
	if n := len(in.requests()); n != 1 {
		t.Errorf("the receiver got %d requests before the next attempt was due, want 1", n)
	}

	w.clock.advance(10 * time.Second)
	w.retrying(t, id, 2)
	got := in.requests()
	if len(got) != 2 {
		t.Fatalf("the receiver got %d requests, want 2", len(got))
	}
	if !signedWith(got[0], s1) || signedWith(got[1], s1) || !signedWith(got[1], s2) {
		t.Errorf("signatures: request 1 under s1 %t; request 2 under s1 %t, under s2 %t; want true, false, true",
			signedWith(got[0], s1), signedWith(got[1], s1), signedWith(got[1], s2))
	}
	if got[0].delivery != got[1].delivery {
		t.Errorf("the attempts carry deliveries %s and %s, want one", got[0].delivery, got[1].delivery)
	}
	updates := w.events(t, "parley.webhook.update")
	if len(updates) != 1 || !reflect.DeepEqual(updates[0].Metadata["fields"], []any{"secret"}) {
		t.Errorf("update events = %+v, want one with fields [secret]", updates)
	}
}

// cellsOf is every cell of every row query returns, as text, a BLOB as its
// bytes.
func (w *world) cellsOf(t *testing.T, query string) []string {
	t.Helper()
	rows, err := w.Store.Main().Query(query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for _, v := range vals {
			if b, ok := v.([]byte); ok {
				out = append(out, string(b))
			} else {
				out = append(out, fmt.Sprint(v))
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSecret_IsShownOnlyByCreateAndRotateAndStoredSealed(t *testing.T) {
	t.Parallel()
	w := newWorld(t, options{toml: insecure})
	in := newInbox(t, answerOK)
	body := map[string]any{"url": in.url(), "types": testTypes}
	header := http.Header{"Idempotency-Key": {"k1"}}
	status, raw := w.CallWith(t, http.MethodPost, hooksPath(demo), w.ownerKey, body, header)
	if status != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", status, raw)
	}
	s0, _ := decode(t, raw)["secret"].(string)
	status, replay := w.CallWith(t, http.MethodPost, hooksPath(demo), w.ownerKey, body, header)
	if status != http.StatusOK {
		t.Fatalf("the replay of the create = %d %s, want 200", status, replay)
	}
	created := w.mustCreateWith(t, demo, body)
	id, _ := created["id"].(string)
	s1, _ := created["secret"].(string)
	status, raw = w.Call(t, http.MethodPost, hookPath(demo, id)+"/rotate-secret", w.ownerKey, nil)
	if status != http.StatusOK {
		t.Fatalf("rotate = %d %s, want 200", status, raw)
	}
	s2, _ := decode(t, raw)["secret"].(string)
	secrets := []string{s0, s1, s2}
	for _, s := range secrets {
		if !secretShape.MatchString(s) {
			t.Fatalf("the secrets shown are %q, want three signing secrets", secrets)
		}
	}
	w.emit(t, demo, "parley.test.one", "")
	waitFor(t, "the event to be delivered", func() bool { return w.settled(t, id) })
	delivery := w.deliveries(t, id)[0].id

	answers := map[string][]byte{"replay": replay}
	for _, call := range []struct {
		name, method, path string
		body               any
	}{
		{"list", http.MethodGet, hooksPath(demo), nil},
		{"get", http.MethodGet, hookPath(demo, id), nil},
		{"update", http.MethodPatch, hookPath(demo, id), map[string]any{"description": "x"}},
		{"ping", http.MethodPost, hookPath(demo, id) + "/ping", nil},
		{"deliveries", http.MethodGet, hookPath(demo, id) + "/deliveries", nil},
		{"redeliver", http.MethodPost, redeliverPath(id, delivery), nil},
	} {
		status, raw := w.Call(t, call.method, call.path, w.ownerKey, call.body)
		if status != http.StatusOK {
			t.Fatalf("%s = %d %s, want 200", call.name, status, raw)
		}
		answers[call.name] = raw
	}
	holds := func(text string) bool {
		return slices.ContainsFunc(secrets, func(s string) bool { return strings.Contains(text, s) })
	}
	for name, raw := range answers {
		if holds(string(raw)) || strings.Contains(string(raw), `"secret":`) {
			t.Errorf("the answer of %s holds a secret: %s", name, raw)
		}
	}
	for _, cell := range w.cellsOf(t, `SELECT * FROM hub_events`) {
		if holds(cell) {
			t.Errorf("an event holds a secret: %s", cell)
		}
	}
	for _, table := range []string{"webhook_subscriptions", "webhook_deliveries"} {
		for _, cell := range w.cellsOf(t, `SELECT * FROM `+table) {
			if holds(cell) {
				t.Errorf("a column of %s holds a secret as text", table)
			}
		}
	}
	var sealed []byte
	if err := w.Store.Main().QueryRow(`SELECT secret_sealed FROM webhook_subscriptions WHERE id = ?`, id).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if got, err := secret.OpenValue(secret.Keys{Current: w.key}, "webhook:"+id, sealed); err != nil || got != s2 {
		t.Errorf("the stored ciphertext opens to %q, %v, want the rotated secret", got, err)
	}

	// The hub's log scrubs a secret from a line that would hold it, from
	// the create on and, after a restart, once the secret has been opened.
	w.logger.Warn("webhook secrecy probe", "value", "a line holding "+s2+" and "+s0)
	if text := w.Log.String(); !strings.Contains(text, "webhook secrecy probe") || holds(text) {
		t.Errorf("the hub's log holds a secret or lacks the probe line:\n%s", text)
	}
	w.Restart(t)
	if status, raw := w.Call(t, http.MethodPost, hookPath(demo, id)+"/ping", w.ownerKey, nil); status != http.StatusOK {
		t.Fatalf("ping after the restart = %d %s, want 200", status, raw)
	}
	w.logger.Warn("webhook secrecy probe after the restart", "value", "a line holding "+s2)
	if text := w.Log.String(); !strings.Contains(text, "webhook secrecy probe after the restart") || holds(text) {
		t.Errorf("after the restart the hub's log holds a secret or lacks the probe line:\n%s", text)
	}
}
