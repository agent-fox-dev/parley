package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// defaultProbe is 'probe get <name>' with the default run step.
func defaultProbe(extra ...func(*Spec)) *cobra.Command {
	s := Spec{Use: "get", Description: "gets a thing", Method: "GET", Path: "/things/:name",
		Args: []Arg{{Name: "name", Required: true}}, Auth: AuthUser}
	for _, e := range extra {
		e(&s)
	}
	return Group("probe", "test commands", Declare(s))
}

func reindent(t *testing.T, body string) string {
	t.Helper()
	var compact, out bytes.Buffer
	if err := json.Compact(&compact, []byte(body)); err != nil {
		t.Fatal(err)
	}
	json.Indent(&out, compact.Bytes(), "", "  ")
	return out.String() + "\n"
}

func TestOutput_AJSONResultIsReindentedAsReceived(t *testing.T) {
	bodies := []struct {
		status int
		body   string
	}{
		{200, `{"b":1,"a":{"z":[1,2],"y":null}}`},
		{201, `{"id":"x","n":1.50,"big":12345678901234567890}`},
		{200, `{"items":[],"next_cursor":null}`},
		{200, `[]`},
		{202, "{\n\t\"tabbed\": true\n}"},
	}
	for _, b := range bodies {
		cliEnv(t)
		hub := newRecordingHub(t, b.status, b.body)
		r := runProbe(t, []*cobra.Command{defaultProbe()}, "probe", "get", "x", "--endpoint-url", hub.URL, "--api-key", "k")
		if r.code != 0 || r.stdout != reindent(t, b.body) || r.stderr != "" {
			t.Errorf("%s: exit %d stdout %q stderr %q", b.body, r.code, r.stdout, r.stderr)
		}
	}
}

func TestOutput_ASuccessWithoutABodyPrintsEmptyOrTheConfirmation(t *testing.T) {
	plain := Group("probe", "test commands", Declare(Spec{Use: "delete", Description: "deletes", Method: "DELETE", Path: "/things/:name",
		Args: []Arg{{Name: "name", Required: true}}, Auth: AuthUser}))
	confirm := Group("probe", "test commands", Declare(Spec{Use: "remove", Description: "removes", Method: "DELETE", Path: "/things/:name",
		Args: []Arg{{Name: "slug", Required: true}}, Auth: AuthUser,
		Confirm: func(args map[string]string) any {
			return struct {
				Status string `json:"status"`
				Slug   string `json:"slug"`
			}{"deleted", args["slug"]}
		}}))
	tests := []struct {
		cmd    *cobra.Command
		words  []string
		status int
		want   string
	}{
		{plain, []string{"probe", "delete", "demo"}, 204, "{}\n"},
		{plain, []string{"probe", "delete", "demo"}, 200, "{}\n"},
		{confirm, []string{"probe", "remove", "demo"}, 204, "{\n  \"status\": \"deleted\",\n  \"slug\": \"demo\"\n}\n"},
	}
	for _, tt := range tests {
		cliEnv(t)
		hub := newRecordingHub(t, tt.status, "")
		r := runProbe(t, []*cobra.Command{tt.cmd}, append(tt.words, "--endpoint-url", hub.URL, "--api-key", "k")...)
		if r.code != 0 || r.stdout != tt.want || r.stderr != "" {
			t.Errorf("%v %d: exit %d stdout %q stderr %q", tt.words, tt.status, r.code, r.stdout, r.stderr)
		}
	}
}

func TestOutput_AHubErrorIsPrintedAsReceived(t *testing.T) {
	sha := strings.Repeat("a", 40)
	answers := []struct {
		status int
		body   string
	}{
		{404, `{"error":{"code":404,"message":"workspace not found","error_type":"not_found"}}`},
		{401, `{"error":{"code":401,"message":"invalid credentials","error_type":"unauthenticated"}}`},
		{403, `{"error":{"code":403,"message":"scope","error_type":"insufficient_scope"}}`},
		{409, `{"error":{"code":409,"message":"m","error_type":"ref_moved","details":{"current_sha":"` + sha + `"}}}`},
		{410, `{"error":{"code":410,"message":"m","error_type":"cursor_expired","oldest_seq":42}}`},
		{503, `{"error":{"code":503,"message":"m","error_type":"unavailable"}}`},
	}
	for _, a := range answers {
		cliEnv(t)
		hub := newRecordingHub(t, a.status, a.body)
		r := runProbe(t, []*cobra.Command{defaultProbe()}, "probe", "get", "x", "--endpoint-url", hub.URL, "--api-key", "k")
		if r.code != 1 || r.stdout != "" || r.stderr != reindent(t, a.body) {
			t.Errorf("%d: exit %d stdout %q stderr %q", a.status, r.code, r.stdout, r.stderr)
		}
	}
}

func TestOutput_AShownOnceSecretIsWarnedAbout(t *testing.T) {
	cliEnv(t)
	hub := newRecordingHub(t, 201, `{"id":"t1","token":"parley_pat_abc"}`)
	probe := defaultProbe(func(s *Spec) { s.ShowOnce = true })
	r := runProbe(t, []*cobra.Command{probe}, "probe", "get", "x", "--endpoint-url", hub.URL, "--api-key", "k")
	var doc map[string]any
	json.Unmarshal([]byte(r.stdout), &doc)
	if r.code != 0 || doc["token"] != "parley_pat_abc" || r.stderr != "warning: this value is shown once and will not be shown again\n" {
		t.Errorf("exit %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	hub.mu.Lock()
	hub.status, hub.body = 403, `{"error":{"code":403,"message":"no","error_type":"forbidden"}}`
	hub.mu.Unlock()
	r = runProbe(t, []*cobra.Command{defaultProbe(func(s *Spec) { s.ShowOnce = true })}, "probe", "get", "x", "--endpoint-url", hub.URL, "--api-key", "k")
	if r.code != 1 || strings.Contains(r.stderr, "shown once") {
		t.Errorf("on an error: exit %d stderr %q", r.code, r.stderr)
	}
}

func TestOutput_AFailOnConditionPrintsTheResultThenExitsThree(t *testing.T) {
	failOnOdd := func(s *Spec) {
		s.FailOn = []FailOn{{Name: "fail-on-odd", Usage: "exit 3 when n is odd",
			Holds: func(result []byte) bool {
				var d struct{ N int }
				json.Unmarshal(result, &d)
				return d.N%2 == 1
			},
			Message: func(result []byte) string {
				var d struct{ N int }
				json.Unmarshal(result, &d)
				return fmt.Sprintf("n is %d, which is odd", d.N)
			}}}
	}
	tests := []struct {
		flag bool
		n    int
		code int
	}{{true, 3, 3}, {true, 2, 0}, {false, 3, 0}}
	for _, tt := range tests {
		cliEnv(t)
		body := fmt.Sprintf(`{"n": %d}`, tt.n)
		hub := newRecordingHub(t, 200, body)
		args := []string{"probe", "get", "x", "--endpoint-url", hub.URL, "--api-key", "k"}
		if tt.flag {
			args = append(args, "--fail-on-odd")
		}
		r := runProbe(t, []*cobra.Command{defaultProbe(failOnOdd)}, args...)
		if r.code != tt.code || r.stdout != reindent(t, body) {
			t.Errorf("%+v: exit %d stdout %q", tt, r.code, r.stdout)
			continue
		}
		if tt.code == 3 {
			if e := r.envelope(t); e["error_type"] != "condition_not_met" || !strings.Contains(e["message"].(string), "3") {
				t.Errorf("%+v: %v", tt, e)
			}
		} else if r.stderr != "" {
			t.Errorf("%+v: stderr %q", tt, r.stderr)
		}
	}
}
