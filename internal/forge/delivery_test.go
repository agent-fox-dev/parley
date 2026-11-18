package forge_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/forge"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// deliveryTime is the hub's clock in the delivery tests, on a whole second.
var deliveryTime = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

// newDeliveryWorld is a world whose hub's clock is now, with the
// workspace demo, whose origin is acme/widgets on GitHub, and the
// workspace other, whose origin is acme/other.
func newDeliveryWorld(t *testing.T, now time.Time, adjust func(w *world, d *forge.Deps)) *world {
	t.Helper()
	w := newWorldWith(t, hubtest.Config{Now: func() time.Time { return now }}, adjust)
	w.addWorkspace(t, workspace.Workspace{Slug: "demo", GitURL: new("https://github.com/acme/widgets.git")})
	w.addWorkspace(t, workspace.Workspace{Slug: "other", GitURL: new("https://github.com/acme/other.git")})
	return w
}

// record records the delivery, failing the test on an error, and is its
// outcome.
func (w *world) record(t *testing.T, slug string, kind forge.ForgeKind, id, eventName string, body []byte) forge.Outcome {
	t.Helper()
	out, err := w.forge.RecordDelivery(context.Background(), slug, kind, id, eventName, body)
	if err != nil {
		t.Fatalf("record delivery %s of %s: %v", id, slug, err)
	}
	return out
}

// delivery is the recorded delivery id of slug, failing the test when
// there is none.
func (w *world) delivery(t *testing.T, slug, id string) forge.Delivery {
	t.Helper()
	d, ok, err := forge.GetDelivery(context.Background(), w.Store.Main(), slug, id)
	if err != nil || !ok {
		t.Fatalf("read delivery %s of %s: found %v, %v", id, slug, ok, err)
	}
	return d
}

// deliveryRows are the rows of the delivery table of slug, each column
// as its text.
func (w *world) deliveryRows(t *testing.T, slug string) [][]string {
	t.Helper()
	rows, err := w.Store.Main().Query(`SELECT * FROM forge_deliveries WHERE workspace = ?`, slug)
	if err != nil {
		t.Fatalf("read the delivery table: %v", err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out [][]string
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		row := make([]string, len(cols))
		for i, v := range values {
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			row[i] = fmt.Sprint(v)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// ghIssue is a GitHub issues payload of the action for acme/widgets, for
// issue 7, with the members of extra.
func ghIssue(action string, extra map[string]any) []byte {
	return ghPayload(action, "acme/widgets", overlay(map[string]any{"issue": map[string]any{"number": 7}}, extra))
}

// glLabelUpdate is a GitLab Issue Hook update of issue 12 of the
// repository that changes the title and swaps the label a for b, which
// relays three events: edited, unlabeled a, labeled b.
func glLabelUpdate(repo string) []byte {
	return glPayload("update", repo, overlay(map[string]any{"object_attributes": map[string]any{"iid": 12, "title": "ZZMARKER"}},
		map[string]any{"changes": overlay(labelChanges([]string{"a"}, []string{"b"}), map[string]any{"title": map[string]any{}})}))
}

var labelUpdateTypes = []string{"parley.forge.issue.edited", "parley.forge.issue.unlabeled", "parley.forge.issue.labeled"}

// types are the types of the events.
func types(records []event.Record) []string {
	out := make([]string, 0, len(records))
	for _, r := range records {
		out = append(out, r.Type)
	}
	return out
}

func TestRecordDelivery_StoresItsFieldsWithoutThePayloadOrTheSecret(t *testing.T) {
	t.Parallel()
	// The clock is past a whole second, which the record drops.
	w := newDeliveryWorld(t, deliveryTime.Add(400*time.Millisecond), nil)
	const marker, secretValue = "ZZMARKER", "the-webhook-secret-0123"
	w.putSecret(t, "demo", forge.SecretWebhook, secretValue)
	body := ghIssue("labeled", map[string]any{"label": map[string]any{"name": "bug"},
		"issue": map[string]any{"number": 7, "title": "a title with " + marker, "body": marker}})
	if out := w.record(t, "demo", forge.KindGitHub, "d1", "issues", body); out != forge.OutcomeRelayed {
		t.Fatalf("outcome %q, want relayed", out)
	}
	longEvent := strings.Repeat("e", 80)
	w.record(t, "demo", forge.KindGitHub, "d2", longEvent, ghIssue("opened", nil))
	if out := w.record(t, "demo", forge.KindGitHub, "d3", "ping", []byte(`{"zen":"`+marker+`"}`)); out != forge.OutcomeIgnored {
		t.Fatalf("ping outcome %q, want ignored", out)
	}

	cases := []struct {
		id   string
		want forge.Delivery
	}{
		{"d1", forge.Delivery{DeliveryID: "d1", Forge: forge.KindGitHub, Event: "issues", Action: "labeled", Target: new(forge.TargetOrigin),
			Outcome: forge.OutcomeRelayed, Relayed: []string{"parley.forge.issue.labeled"}, ReceivedAt: deliveryTime}},
		{"d2", forge.Delivery{DeliveryID: "d2", Forge: forge.KindGitHub, Event: longEvent[:64], Action: "opened", Target: new(forge.TargetOrigin),
			Outcome: forge.OutcomeIgnored, Relayed: []string{}, ReceivedAt: deliveryTime}},
		{"d3", forge.Delivery{DeliveryID: "d3", Forge: forge.KindGitHub, Event: "ping", Action: "",
			Outcome: forge.OutcomeIgnored, Relayed: []string{}, ReceivedAt: deliveryTime}},
	}
	for _, c := range cases {
		got := w.delivery(t, "demo", c.id)
		c.want.Seq = got.Seq
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("delivery %s = %+v, want %+v", c.id, got, c.want)
		}
	}

	rows := w.deliveryRows(t, "demo")
	if len(rows) != len(cases) {
		t.Fatalf("the delivery table holds %d rows of demo, want %d", len(rows), len(cases))
	}
	for _, row := range rows {
		for _, col := range row {
			if strings.Contains(col, marker) || strings.Contains(col, secretValue) {
				t.Errorf("the stored delivery %v holds the payload or the secret", row)
			}
		}
	}
	for _, r := range w.eventsOf(t, "parley.forge.", "demo") {
		if j := jsonOf(r); strings.Contains(j, marker) || strings.Contains(j, secretValue) {
			t.Errorf("the stored event %s holds the payload or the secret", j)
		}
	}
}

func TestRecordDelivery_AnswersAnIdTheWorkspaceRecordedAsADuplicate(t *testing.T) {
	t.Parallel()
	w := newDeliveryWorld(t, deliveryTime, nil)
	if out := w.record(t, "demo", forge.KindGitHub, "d1", "issues", ghIssue("opened", nil)); out != forge.OutcomeRelayed {
		t.Fatalf("first outcome %q, want relayed", out)
	}
	if out := w.record(t, "demo", forge.KindGitHub, "d1", "issues", ghIssue("closed", nil)); out != forge.OutcomeDuplicate {
		t.Fatalf("repeated outcome %q, want duplicate", out)
	}
	if n := len(w.deliveryRows(t, "demo")); n != 1 {
		t.Errorf("demo holds %d deliveries, want 1", n)
	}
	if got := w.delivery(t, "demo", "d1"); got.Action != "opened" {
		t.Errorf("the first delivery's action is now %q, want opened", got.Action)
	}
	if got := types(w.eventsOf(t, "parley.forge.", "demo")); !reflect.DeepEqual(got, []string{"parley.forge.issue.opened"}) {
		t.Errorf("the events of demo are %v, want the first delivery's only", got)
	}

	other := ghPayload("opened", "acme/other", map[string]any{"issue": map[string]any{"number": 7}})
	if out := w.record(t, "other", forge.KindGitHub, "d1", "issues", other); out != forge.OutcomeRelayed {
		t.Errorf("the same id in other: outcome %q, want relayed", out)
	}
	if n := len(w.deliveryRows(t, "other")); n != 1 {
		t.Errorf("other holds %d deliveries, want 1", n)
	}
}

func TestRecordDelivery_StoresTheRecordAndItsEventsTogetherAlsoWhenTheCallerLeft(t *testing.T) {
	t.Parallel()
	w := newDeliveryWorld(t, deliveryTime, nil)
	if out := w.record(t, "demo", forge.KindGitLab, "d1", "Issue Hook", glLabelUpdate("acme/widgets")); out != forge.OutcomeRelayed {
		t.Fatalf("outcome %q, want relayed", out)
	}
	if got := w.delivery(t, "demo", "d1").Relayed; !reflect.DeepEqual(got, labelUpdateTypes) {
		t.Errorf("relayed %v, want %v", got, labelUpdateTypes)
	}
	events := w.eventsOf(t, "parley.forge.", "demo")
	if got := types(events); !reflect.DeepEqual(got, labelUpdateTypes) {
		t.Errorf("the events of demo are %v, want %v", got, labelUpdateTypes)
	}
	for _, r := range events {
		if id := (recorded{Record: r}).meta(t)["delivery_id"]; id != "d1" {
			t.Errorf("event %s has delivery_id %v, want d1", r.Type, id)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := w.forge.RecordDelivery(ctx, "demo", forge.KindGitHub, "d9", "issues", ghIssue("opened", nil))
	if err != nil || out != forge.OutcomeRelayed {
		t.Fatalf("record with a cancelled context = %q, %v; want relayed", out, err)
	}
	if got := w.delivery(t, "demo", "d9"); got.Outcome != forge.OutcomeRelayed {
		t.Errorf("delivery d9 outcome %q, want relayed", got.Outcome)
	}
	if got := types(w.eventsOf(t, "parley.forge.", "demo")); len(got) != 4 || got[3] != "parley.forge.issue.opened" {
		t.Errorf("the events of demo are %v, want the event of d9 last", got)
	}
}

// errAppendFailed is the failure of failingLog's event.
var errAppendFailed = errors.New("the event log refused the event")

// failingLog is the event log, except that the nth event it is asked to
// record, counted from 1, fails while it is armed.
type failingLog struct {
	next  forge.TxEmitter
	nth   int64
	armed atomic.Bool
	calls atomic.Int64
}

func (l *failingLog) Emit(ctx context.Context, tx *sql.Tx, in event.Input) (event.Record, error) {
	if l.calls.Add(1) == l.nth && l.armed.Load() {
		return event.Record{}, errAppendFailed
	}
	return l.next.Emit(ctx, tx, in)
}

func TestRecordDelivery_KeepsNothingWhenAnEventCannotBeRecorded(t *testing.T) {
	t.Parallel()
	log := &failingLog{nth: 2}
	log.armed.Store(true)
	w := newDeliveryWorld(t, deliveryTime, func(_ *world, d *forge.Deps) { log.next = d.Log; d.Log = log })
	const secretValue = "the-webhook-secret-0123"
	w.putSecret(t, "demo", forge.SecretWebhook, secretValue)
	body := glLabelUpdate("acme/widgets")
	out, err := w.forge.RecordDelivery(context.Background(), "demo", forge.KindGitLab, "d1", "Issue Hook", body)
	if err == nil {
		t.Fatalf("record with a failing event = %q, want an error", out)
	}
	if msg := err.Error(); strings.Contains(msg, "ZZMARKER") || strings.Contains(msg, secretValue) {
		t.Errorf("the error %q holds the payload or the secret", msg)
	}
	if n := len(w.deliveryRows(t, "demo")); n != 0 {
		t.Errorf("demo holds %d deliveries after the failure, want none", n)
	}
	if n := len(w.eventsOf(t, "parley.forge.", "demo")); n != 0 {
		t.Errorf("demo holds %d events after the failure, want none", n)
	}

	log.armed.Store(false)
	if out := w.record(t, "demo", forge.KindGitLab, "d1", "Issue Hook", body); out != forge.OutcomeRelayed {
		t.Errorf("recorded again: outcome %q, want relayed", out)
	}
	if got := types(w.eventsOf(t, "parley.forge.", "demo")); !reflect.DeepEqual(got, labelUpdateTypes) {
		t.Errorf("the events of demo are %v, want %v", got, labelUpdateTypes)
	}
}

func TestRecordDelivery_RecordsOneOfConcurrentCopies(t *testing.T) {
	t.Parallel()
	w := newDeliveryWorld(t, deliveryTime, nil)
	const copies = 16
	outcomes := make([]forge.Outcome, copies)
	errs := make([]error, copies)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range copies {
		wg.Go(func() {
			<-start
			outcomes[i], errs[i] = w.forge.RecordDelivery(context.Background(), "demo", forge.KindGitHub, "d1", "issues", ghIssue("opened", nil))
		})
	}
	close(start)
	wg.Wait()
	count := map[forge.Outcome]int{}
	for i := range copies {
		if errs[i] != nil {
			t.Errorf("copy %d: %v", i, errs[i])
		}
		count[outcomes[i]]++
	}
	if count[forge.OutcomeRelayed] != 1 || count[forge.OutcomeDuplicate] != copies-1 {
		t.Errorf("outcomes %v, want one relayed and %d duplicate", count, copies-1)
	}
	if n := len(w.deliveryRows(t, "demo")); n != 1 {
		t.Errorf("demo holds %d deliveries, want 1", n)
	}
	if n := len(w.eventsOf(t, "parley.forge.issue.opened", "demo")); n != 1 {
		t.Errorf("demo holds %d opened events, want 1", n)
	}
}
