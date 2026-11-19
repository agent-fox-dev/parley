package webhook_test

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/secret"
)

// newKey is a new secret key and its form in the environment.
func newKey() (secret.Key, string) {
	k := make(secret.Key, 32)
	_, _ = rand.Read(k)
	return k, base64.StdEncoding.EncodeToString(k)
}

// reencrypted waits for the line of a re-encryption of the current run
// that reports converted and remaining, and fails the test when none comes
// within 10 s. Each run of the world's hub passes its maintenance at the
// start and then at every interval.
func (w *world) reencrypted(t *testing.T, converted, remaining int) {
	t.Helper()
	waitFor(t, fmt.Sprintf("a re-encryption that converted %d and left %d", converted, remaining), func() bool {
		for line := range strings.SplitSeq(w.Log.String(), "\n") {
			var rec struct {
				Msg       string  `json:"msg"`
				Converted float64 `json:"converted"`
				Remaining float64 `json:"remaining"`
			}
			if json.Unmarshal([]byte(line), &rec) != nil || rec.Msg != "secret re-encryption" {
				continue
			}
			if int(rec.Converted) == converted && int(rec.Remaining) == remaining {
				return true
			}
		}
		return false
	})
}

// sealedSecrets is the sealed signing secret of each subscription.
func (w *world) sealedSecrets(t *testing.T) map[string]string {
	t.Helper()
	rows, err := w.Store.Main().Query(`SELECT id, secret_sealed FROM webhook_subscriptions`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id string
		var sealed []byte
		if err := rows.Scan(&id, &sealed); err != nil {
			t.Fatal(err)
		}
		out[id] = string(sealed)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// withoutSecrets is every row of the webhook tables with the sealed
// signing secret left out.
func (w *world) withoutSecrets(t *testing.T) []string {
	t.Helper()
	return w.cellsOf(t, `SELECT id, workspace, url, types, include_forks, description, state, created_by, created_at,
		updated_at, last_delivery_at, last_status, failing_since, position FROM webhook_subscriptions ORDER BY id`)
}

func TestReencrypt_ResealsTheSigningSecretsUnderThePreviousKey(t *testing.T) {
	t.Parallel()
	k1, env1 := newKey()
	_, env2 := newKey()
	w := newWorld(t, options{toml: insecure, env: map[string]string{secret.EnvCurrentKey: env1}, maintenance: 100 * time.Millisecond})
	in := newInbox(t, answerOK)
	body := map[string]any{"url": in.url(), "types": testTypes}

	// Three subscriptions under K1, and one whose ciphertext no key opens.
	shown := map[string]string{}
	var underK1 []string
	for range 3 {
		created := w.mustCreateWith(t, demo, body)
		id, _ := created["id"].(string)
		shown[id], _ = created["secret"].(string)
		underK1 = append(underK1, id)
	}
	broken := w.subscribe(t, demo, body)
	garbage := make([]byte, 60)
	_, _ = rand.Read(garbage)
	if _, err := w.Store.Main().Exec(`UPDATE webhook_subscriptions SET secret_sealed = ? WHERE id = ?`,
		append([]byte(secret.KeyID(k1)), garbage...), broken); err != nil {
		t.Fatal(err)
	}

	// K2 alone: nothing is converted, and all four are not under it. One
	// subscription is created under K2.
	w.RestartEnv(t, map[string]string{secret.EnvCurrentKey: env2})
	w.reencrypted(t, 0, 4)
	w.subscribe(t, demo, body)
	all := func() bool {
		for id := range w.sealedSecrets(t) {
			if !w.settled(t, id) {
				return false
			}
		}
		return true
	}
	waitFor(t, "every subscription to examine every event", all)
	rows, sealed, seq := w.withoutSecrets(t), w.sealedSecrets(t), w.lastSeq(t)

	// K2 with K1 as the previous key: the three under K1 are re-sealed.
	w.RestartEnv(t, map[string]string{secret.EnvCurrentKey: env2, secret.EnvPreviousKey: env1})
	w.reencrypted(t, 3, 1)
	w.reencrypted(t, 0, 1)
	if got := w.withoutSecrets(t); !reflect.DeepEqual(got, rows) {
		t.Errorf("the re-encryption changed the subscriptions:\n%v\nwant\n%v", got, rows)
	}
	if got := w.lastSeq(t); got != seq {
		t.Errorf("the newest event is %d after the re-encryption, want %d", got, seq)
	}
	after := w.sealedSecrets(t)
	for id, before := range sealed {
		_, converted := shown[id]
		if changed := after[id] != before; changed != converted {
			t.Errorf("the ciphertext of %s changed = %t, want %t", id, changed, converted)
		}
	}
	unchanged := maps.Clone(after)
	for _, id := range underK1 {
		delete(unchanged, id)
	}
	if len(unchanged) != 2 {
		t.Errorf("%d ciphertexts were left as they were, want the one under K2 and the broken one", len(unchanged))
	}

	// K2 alone again: each converted subscription signs with the secret
	// its create showed.
	w.RestartEnv(t, map[string]string{secret.EnvCurrentKey: env2})
	w.reencrypted(t, 0, 1)
	for _, id := range underK1 {
		status, raw := w.Call(t, http.MethodPost, hookPath(demo, id)+"/ping", w.ownerKey, nil)
		if status != http.StatusOK || decode(t, raw)["status"] != "succeeded" {
			t.Fatalf("ping %s under K2 alone = %d %s, want 200 succeeded", id, status, raw)
		}
	}
	got := pings(in.requests())
	if len(got) != len(underK1) {
		t.Fatalf("the receiver got %d pings, want %d", len(got), len(underK1))
	}
	for i, id := range underK1 {
		if !signedWith(got[i], shown[id]) {
			t.Errorf("the ping of %s is not signed with the secret its create showed", id)
		}
	}
}
