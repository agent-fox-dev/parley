package webhook_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"
	"time"
)

// pings are the requests of d that are pings.
func pings(ds []delivered) []delivered {
	var out []delivered
	for _, d := range ds {
		if d.typ == "parley.webhook.ping" {
			out = append(out, d)
		}
	}
	return out
}

// closedURL is a webhook URL on 127.0.0.1 whose port nobody listens on.
func closedURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	return srv.URL + "/hook"
}

func TestPing_SendsOneSignedTestDeliveryAndChangesNothingElse(t *testing.T) {
	t.Parallel()
	w := newWorld(t, options{toml: insecure})
	for i, state := range []string{"active", "failing", "disabled"} {
		for _, answer := range []struct {
			name string
			// status is what the receiver answers a ping, 0 for a closed
			// port.
			status int
			// outcome, code and failure are the ping's delivery record.
			outcome string
			code    any
			failure any
		}{
			{"200", http.StatusOK, "succeeded", float64(200), nil},
			{"500", http.StatusInternalServerError, "failed", float64(500), nil},
			{"a closed port", 0, "failed", nil, "connection_failed"},
		} {
			t.Run(state+"/"+answer.name, func(t *testing.T) {
				// The receiver fails every event, so that the subscription
				// has a pending delivery of the loop, and answers a ping as
				// the row says.
				in := newInbox(t, func(d delivered, _ <-chan struct{}) int {
					if d.typ == "parley.webhook.ping" {
						return answer.status
					}
					return http.StatusInternalServerError
				})
				url := in.url()
				if answer.status == 0 {
					url = closedURL(t)
				}
				typ := fmt.Sprintf("parley.test.ping%d%s", i, answer.name)
				created := w.mustCreateWith(t, demo, map[string]any{"url": url, "types": []string{typ}})
				id, _ := created["id"].(string)
				key, _ := created["secret"].(string)
				w.emit(t, demo, typ, "")
				pending := w.retrying(t, id, 1)
				switch state {
				case "failing":
					if _, err := w.Store.Main().Exec(`UPDATE webhook_subscriptions SET state = 'failing' WHERE id = ?`, id); err != nil {
						t.Fatal(err)
					}
				case "disabled":
					if status, raw := w.Call(t, http.MethodPatch, hookPath(demo, id), w.ownerKey, map[string]any{"active": false}); status != http.StatusOK {
						t.Fatalf("switch off = %d %s", status, raw)
					}
					pending = deliveryRow{}
				}
				row, rows, seq, before := w.row(t, id), w.configured(t), w.lastSeq(t), w.deliveries(t, id)
				result := map[string]string{"succeeded": "ok", "failed": "failed"}[answer.outcome]
				counted := w.metrics.deliveries(result)

				status, raw := w.Call(t, http.MethodPost, hookPath(demo, id)+"/ping", w.ownerKey, nil)
				if status != http.StatusOK {
					t.Fatalf("ping = %d %s, want 200", status, raw)
				}
				got := decode(t, raw)
				pingID, _ := got["id"].(string)
				want := map[string]any{
					"id": pingID, "seq": float64(0), "type": "parley.webhook.ping", "status": answer.outcome,
					"attempts": float64(1), "response_code": answer.code, "error": answer.failure,
					"duration_ms": got["duration_ms"], "created_at": w.clock.stamp(),
					"last_attempt_at": w.clock.stamp(), "next_attempt_at": nil,
				}
				if !reflect.DeepEqual(got, want) || pingID == "" {
					t.Errorf("ping record = %v\nwant          %v", got, want)
				}
				if ms, ok := got["duration_ms"].(float64); !ok || ms < 0 {
					t.Errorf("duration_ms = %v, want a number >= 0", got["duration_ms"])
				}

				if got := w.metrics.deliveries(result); got != counted+1 {
					t.Errorf("deliveries reported %s = %v, want %v", result, got, counted+1)
				}

				if answer.status != 0 {
					ps := pings(in.requests())
					if len(ps) != 1 {
						t.Fatalf("the receiver got %d pings, want 1", len(ps))
					}
					p := ps[0]
					var body map[string]any
					if err := json.Unmarshal(p.body, &body); err != nil {
						t.Fatalf("ping body %s: %v", p.body, err)
					}
					for name, v := range map[string]any{
						"type": "parley.webhook.ping", "seq": float64(0), "workspace": demo,
						"actor_type": "api_key", "actor_id": w.owner, "resource_type": "event_webhook",
						"resource_id": id, "action": "ping", "session_id": nil, "metadata": map[string]any{},
					} {
						if !reflect.DeepEqual(body[name], v) {
							t.Errorf("ping body %s = %v, want %v", name, body[name], v)
						}
					}
					if eid, _ := body["id"].(string); eid == "" || eid == pingID {
						t.Errorf("ping body id = %q, want a new id of its own", eid)
					}
					if p.seq != 0 || p.delivery != pingID || !signedWith(p, key) {
						t.Errorf("ping headers seq %d delivery %s signed %v, want 0, %s, true", p.seq, p.delivery, signedWith(p, key), pingID)
					}
				}

				if got := w.row(t, id); got != row {
					t.Errorf("subscription = %+v, want it unchanged %+v", got, row)
				}
				if got := w.configured(t); !reflect.DeepEqual(got, rows) {
					t.Errorf("subscriptions = %v\nwant them unchanged %v", got, rows)
				}
				if got := w.lastSeq(t); got != seq {
					t.Errorf("the newest seq moved from %d to %d", seq, got)
				}
				after := w.deliveries(t, id)
				if len(after) != len(before)+1 || !reflect.DeepEqual(after[1:], before) || after[0].id != pingID {
					t.Errorf("deliveries = %+v, want the ping's then %+v unchanged", after, before)
				}
				if pending.id != "" && before[0] != pending {
					t.Errorf("the pending delivery = %+v, want %+v", before[0], pending)
				}

				w.clock.advance(time.Hour)
				if pending.id != "" {
					w.retrying(t, id, 2)
				}
				if n := len(pings(in.requests())); answer.status != 0 && n != 1 {
					t.Errorf("the receiver got %d pings an hour later, want 1", n)
				}
			})
		}
	}
}

func TestPingAndRedeliver_RecordTheReceiversAnswerWhenTheCallerLeaves(t *testing.T) {
	t.Parallel()
	w := newWorld(t, options{toml: insecure})
	for i, call := range []string{"ping", "redeliver"} {
		t.Run(call, func(t *testing.T) {
			h := newHolder(t)
			typ := "parley.test.leave" + strconv.Itoa(i)
			id := w.subscribe(t, demo, map[string]any{"url": h.url(), "types": []string{typ}})
			path := hookPath(demo, id) + "/ping"
			if call == "redeliver" {
				w.emit(t, demo, typ, "")
				waitFor(t, "the event to be delivered", func() bool { return w.settled(t, id) })
				path = redeliverPath(id, w.deliveries(t, id)[0].id)
			}
			before := len(w.deliveries(t, id))
			h.hold.Store(true)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL(path), nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+w.ownerKey)
			done := make(chan struct{})
			go func() {
				defer close(done)
				if resp, err := http.DefaultClient.Do(req); err == nil {
					resp.Body.Close()
				}
			}()
			h.waitArrived(t)
			cancel()
			<-done
			// The hub would end the attempt soon after the caller left; give
			// it the time to, then let the receiver answer.
			select {
			case <-h.aborted:
			case <-time.After(500 * time.Millisecond):
			}
			h.release()

			var got deliveryRow
			waitFor(t, "the attempt to be recorded", func() bool {
				for _, d := range w.deliveries(t, id) {
					if (call == "ping" && d.typ == "parley.webhook.ping" && len(w.deliveries(t, id)) > before) ||
						(call == "redeliver" && d.attempts == 2) {
						got = d
						return true
					}
				}
				return false
			})
			if got.status != "succeeded" || got.responseCode.Int64 != http.StatusOK || got.err.Valid {
				t.Errorf("the %s's record = %+v, want succeeded with the receiver's 200", call, got)
			}
		})
	}
}

func TestPingAndRedeliver_AnswerNotFoundWhenTheSubscriptionGoesMeanwhile(t *testing.T) {
	t.Parallel()
	w := newWorld(t, options{toml: insecure})
	for i, tc := range []struct {
		name, call string
		// meanwhile is what happens while the receiver holds the attempt:
		// the subscription is deleted, or its ended records are pruned.
		meanwhile string
		errorType string
	}{
		{"a ping of a subscription deleted meanwhile", "ping", "delete", "webhook_not_found"},
		{"a redelivery of a subscription deleted meanwhile", "redeliver", "delete", "webhook_not_found"},
		{"a redelivery of a record pruned meanwhile", "redeliver", "prune", "delivery_not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHolder(t)
			typ := "parley.test.gone" + strconv.Itoa(i)
			id := w.subscribe(t, demo, map[string]any{"url": h.url(), "types": []string{typ}})
			path := hookPath(demo, id) + "/ping"
			if tc.call == "redeliver" {
				w.emit(t, demo, typ, "")
				waitFor(t, "the event to be delivered", func() bool { return w.settled(t, id) })
				path = redeliverPath(id, w.deliveries(t, id)[0].id)
			}
			h.hold.Store(true)
			type answer struct {
				status int
				raw    []byte
			}
			answered := make(chan answer, 1)
			go func() {
				status, raw := w.Call(t, http.MethodPost, path, w.ownerKey, nil)
				answered <- answer{status, raw}
			}()
			h.waitArrived(t)
			switch tc.meanwhile {
			case "delete":
				if status, raw := w.Call(t, http.MethodDelete, hookPath(demo, id), w.ownerKey, nil); status != http.StatusNoContent && status != http.StatusOK {
					t.Fatalf("delete = %d %s", status, raw)
				}
			case "prune":
				if _, err := w.hooks.PruneDeliveries(context.Background(), w.clock.Now().Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			h.release()
			got := <-answered
			if got.status != http.StatusNotFound || errorOf(t, got.raw)["error_type"] != tc.errorType {
				t.Errorf("%s = %d %s, want 404 %s", tc.call, got.status, got.raw, tc.errorType)
			}
		})
	}
}
