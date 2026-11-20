package acceptance

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/test/acceptance/harness"
)

// webhooks is the path of demo's event webhooks.
const webhooks = "/api/v1/workspaces/demo/webhooks"

// webhookHub starts a hub that may send to a plain-HTTP receiver on
// loopback, with alice signed in, the workspace demo she owns, and a
// receiver answering 200.
func webhookHub(t *testing.T) (*harness.Hub, *harness.Workspace, *harness.Receiver) {
	t.Helper()
	hub := harness.Start(t, harness.Options{TOML: "[webhooks]\nallow_insecure = true\n"})
	return hub, hub.Workspace(hub.User("alice"), "demo"), harness.NewReceiver(t)
}

// subscribe subscribes url to demo's events with parley webhook create, run
// with credential, and returns the subscription's id and the signing
// secret the create showed.
func subscribe(t *testing.T, hub *harness.Hub, credential, url string) (id, secret string) {
	t.Helper()
	w := hub.CLI(credential, "webhook", "create", "demo", "--url", url).Must(t).JSON()
	id, secret = harness.Str(w["id"]), harness.Str(w["secret"])
	if id == "" || secret == "" {
		t.Fatalf("parley webhook create printed the id %q and the secret %q, want both", id, secret)
	}
	return id, secret
}

// claimSeq is the seq of the parley.claim.create event of the claim id.
func claimSeq(t *testing.T, hub *harness.Hub, credential, id string) int64 {
	t.Helper()
	for _, e := range eventLog(t, hub, credential, "workspace=demo&types=parley.claim.create", 0) {
		if e["resource_id"] == id {
			return seqOf(e)
		}
	}
	t.Fatalf("the log holds no parley.claim.create of the claim %s", id)
	return 0
}

// seqsOf are the Parley-Seq values of the requests.
func seqsOf(requests []harness.Delivery) []int64 {
	seqs := []int64{}
	for _, r := range requests {
		seqs = append(seqs, r.Seq())
	}
	return seqs
}

func TestWebhook_DeliversSignedPostsInSeqOrder(t *testing.T) {
	t.Parallel()
	hub, demo, receiver := webhookHub(t)
	key := demo.Owner.Key
	id, secret := subscribe(t, hub, key, receiver.URL)
	if got := hub.Get(key, webhooks+"/"+id); got.Body["secret"] != nil || got.Body["url"] != receiver.URL {
		t.Errorf("the subscription reads %s, want its url and no secret", got.Raw)
	}
	// The subscription starts after the event of its creation.
	created := eventLog(t, hub, key, "workspace=demo&types=parley.webhook.create", 0)
	if len(created) != 1 {
		t.Fatalf("demo's log holds %d parley.webhook.create events, want 1", len(created))
	}
	before := seqOf(created[0])

	var last string
	for range 10 {
		last = newClaim(t, hub, key)
	}
	until := claimSeq(t, hub, key, last)
	requests := receiver.WaitUntilSeq(until, 20*time.Second)
	var want []int64
	for _, e := range eventLog(t, hub, key, "workspace=demo", before) {
		if s := seqOf(e); s <= until {
			want = append(want, s)
		}
	}
	if len(want) < 10 {
		t.Fatalf("demo's log holds %d events after the subscription, want the ten claims' at least", len(want))
	}
	if got := seqsOf(requests); !slices.Equal(got, want) {
		t.Errorf("the receiver got the Parley-Seq values %v, want the log's %v", got, want)
	}
	for _, r := range requests {
		var body struct{ Type string }
		_ = json.Unmarshal(r.Body, &body)
		if r.Method != http.MethodPost || !harness.VerifySignature(secret, r) || body.Type != r.Header.Get("Parley-Event") || body.Type == "" {
			t.Errorf("the delivery of seq %d is %s with Parley-Event %q, body type %q and signature %q, want a POST of the body's type signed with the secret",
				r.Seq(), r.Method, r.Header.Get("Parley-Event"), body.Type, r.Header.Get("Parley-Signature"))
		}
	}
}

func TestWebhook_FailingReceiverHoldsOrderAndLosesNothing(t *testing.T) {
	t.Parallel()
	hub, demo, receiver := webhookHub(t)
	key := demo.Owner.Key
	id, secret := subscribe(t, hub, key, receiver.URL)
	receiver.WaitUntilSeq(claimSeq(t, hub, key, newClaim(t, hub, key)), 20*time.Second)
	mark := len(receiver.Deliveries())

	receiver.SetStatus(http.StatusInternalServerError)
	var held []int64
	for range 5 {
		held = append(held, claimSeq(t, hub, key, newClaim(t, hub, key)))
	}
	a := receiver.Wait(mark+2, 40*time.Second)[mark:]
	if a[0].Header.Get("Parley-Delivery") == "" || a[1].Header.Get("Parley-Delivery") != a[0].Header.Get("Parley-Delivery") ||
		a[0].Seq() != held[0] || a[1].Seq() != held[0] {
		t.Errorf("the two first attempts carry the deliveries %q and %q with the seqs %d and %d, want one delivery of the seq %d",
			a[0].Header.Get("Parley-Delivery"), a[1].Header.Get("Parley-Delivery"), a[0].Seq(), a[1].Seq(), held[0])
	}
	if gap := a[1].At.Sub(a[0].At); gap < 10*time.Second {
		t.Errorf("the retry came %v after the first attempt, want at least 10 s", gap)
	}
	for _, r := range receiver.Deliveries()[mark:] {
		if r.Seq() != held[0] {
			t.Errorf("while the receiver fails it got the seq %d, want only the held %d", r.Seq(), held[0])
		}
	}

	receiver.SetStatus(http.StatusOK)
	all := receiver.WaitUntilSeq(held[4], 60*time.Second)[mark:]
	var ok []harness.Delivery
	for _, r := range all {
		if r.Status == http.StatusOK {
			ok = append(ok, r)
		} else if r.Seq() != held[0] {
			t.Errorf("a failed attempt carries the seq %d, want only the held %d", r.Seq(), held[0])
		}
		if !harness.VerifySignature(secret, r) {
			t.Errorf("the delivery of seq %d has the signature %q, which does not verify", r.Seq(), r.Header.Get("Parley-Signature"))
		}
	}
	if got := seqsOf(ok); !slices.Equal(got, held) {
		t.Errorf("the receiver accepted the seqs %v, want the held %v in order", got, held)
	}
	if ok[0].Header.Get("Parley-Delivery") != a[0].Header.Get("Parley-Delivery") {
		t.Errorf("the accepted delivery of seq %d is %q, want the retried %q", held[0], ok[0].Header.Get("Parley-Delivery"), a[0].Header.Get("Parley-Delivery"))
	}
	if state := hub.Get(key, webhooks+"/"+id).Body["state"]; state != "active" {
		t.Errorf("the subscription is %v, want active", state)
	}
}

func TestWebhook_PrivateDestinationIsRefused(t *testing.T) {
	t.Parallel()
	hub, demo, receiver := webhookHub(t)
	key := demo.Owner.Key
	for _, url := range []string{"https://10.0.0.5/hook", "https://192.168.1.1/hook", "https://169.254.169.254/latest", "https://[fd00::1]/hook"} {
		t.Run(url, func(t *testing.T) {
			r := hub.Call(http.MethodPost, webhooks, key, map[string]any{"url": url})
			if r.Status != http.StatusBadRequest || r.ErrorType() != "webhook_destination_refused" {
				t.Errorf("POST %s with the url %s answered %d %s, want 400 webhook_destination_refused", webhooks, url, r.Status, r.Raw)
			}
		})
	}
	if items := hub.Get(key, webhooks+"?limit=200").Items(); len(items) != 0 {
		t.Errorf("demo lists the subscriptions %v after the refusals, want none", items)
	}
	c := hub.CLI(key, "webhook", "create", "demo", "--url", "https://10.0.0.5/hook")
	var refusal struct {
		Error struct {
			ErrorType string `json:"error_type"`
		}
	}
	if err := json.Unmarshal([]byte(c.Stderr), &refusal); c.Code != 1 || err != nil || refusal.Error.ErrorType != "webhook_destination_refused" {
		t.Errorf("parley webhook create to 10.0.0.5 exited %d with %s, want 1 and webhook_destination_refused", c.Code, c.Stderr)
	}

	id, _ := subscribe(t, hub, key, receiver.URL)
	r := hub.Call(http.MethodPatch, webhooks+"/"+id, key, map[string]any{"url": "https://10.0.0.5/hook"})
	if r.Status != http.StatusBadRequest || r.ErrorType() != "webhook_destination_refused" {
		t.Errorf("the update to 10.0.0.5 answered %d %s, want 400 webhook_destination_refused", r.Status, r.Raw)
	}
	if url := hub.Get(key, webhooks+"/"+id).Body["url"]; url != receiver.URL {
		t.Errorf("after the refused update the subscription's url is %v, want %s", url, receiver.URL)
	}
}

func TestWebhook_PingAndRedeliveryArriveSigned(t *testing.T) {
	t.Parallel()
	hub, demo, receiver := webhookHub(t)
	key := demo.Owner.Key
	id, secret := subscribe(t, hub, key, receiver.URL)
	for range 3 {
		newClaim(t, hub, key)
	}
	deliveries := webhooks + "/" + id + "/deliveries"
	var events []map[string]any
	harness.Eventually(t, "three deliveries to have succeeded", func() bool {
		events = hub.Get(key, deliveries+"?status=succeeded").Items()
		return len(events) == 3
	})

	p := hub.CLI(key, "webhook", "ping", "demo", id)
	if p.Code != 0 || p.JSON()["status"] != "succeeded" {
		t.Errorf("parley webhook ping exited %d with %s%s, want 0 and the status succeeded", p.Code, p.Stdout, p.Stderr)
	}
	if r := receiver.Last(); r.Header.Get("Parley-Event") != "parley.webhook.ping" || !harness.VerifySignature(secret, r) {
		t.Errorf("the ping arrived with Parley-Event %q and the signature %q, want parley.webhook.ping signed with the secret",
			r.Header.Get("Parley-Event"), r.Header.Get("Parley-Signature"))
	}

	// The deliveries are listed newest first; the ping is one of them now,
	// so the event's is picked by its type.
	var d map[string]any
	for _, it := range hub.Get(key, deliveries).Items() {
		if it["type"] != "parley.webhook.ping" && it["seq"] == events[1]["seq"] {
			d = it
		}
	}
	if d == nil {
		t.Fatalf("the deliveries no longer list the one of seq %v", events[1]["seq"])
	}
	delivery := harness.Str(d["id"])
	var orig *harness.Delivery
	for _, r := range receiver.Deliveries() {
		if r.Header.Get("Parley-Delivery") == delivery {
			orig = &r
			break
		}
	}
	if orig == nil {
		t.Fatalf("the receiver got no request of the delivery %s", delivery)
	}
	hub.Call(http.MethodPost, deliveries+"/"+delivery+"/redeliver", key, nil).Want(t, http.StatusOK)
	r := receiver.Last()
	if r.Header.Get("Parley-Delivery") != delivery || string(r.Body) != string(orig.Body) || !harness.VerifySignature(secret, r) {
		t.Errorf("the redelivery arrived as delivery %q with the body %s, want %s with the body %s, signed with the secret",
			r.Header.Get("Parley-Delivery"), r.Body, delivery, orig.Body)
	}
	if n := len(receiver.Deliveries()); n != 5 {
		t.Errorf("the receiver got %d requests, want 5: three events, the ping and the redelivery", n)
	}
}
