package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// The tests in this file that run a command set environment variables, so
// they do not run in parallel.

func TestOutput_PrintsTheHubsBodyIndentedWithItsOrderAndNumbersAsReceived(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"nested object", 200, `{"b":1,"a":{"z":[1,2],"y":null}}`},
		{"number text", 201, `{"id":"x","n":1.50,"big":12345678901234567890}`},
		{"empty list", 200, `{"items":[],"next_cursor":null}`},
		{"array", 200, `[]`},
		{"already indented with tabs", 202, "{\n\t\"b\": \"x\",\n\t\"a\": [\n\t\t1.0,\n\t\t2e3\n\t]\n}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := newStubHub(t).answer(http.MethodGet, "/api/v1/things/x", tt.status, tt.body)
			probeEnv(t, hub.URL)

			code, stdout, stderr := runProbes(t, "probe", "get", "x")

			if code != 0 {
				t.Fatalf("exit status = %d, want 0; stderr %q", code, stderr)
			}
			if want := indented(t, tt.body); stdout != want {
				t.Errorf("stdout = %q, want %q", stdout, want)
			}
			var got, want bytes.Buffer
			json.Compact(&got, []byte(stdout))
			json.Compact(&want, []byte(tt.body))
			if got.String() != want.String() {
				t.Errorf("stdout holds %s, want the members in order and the numbers as received, %s", got.String(), want.String())
			}
			var doc any
			oneJSONDocument(t, stdout, &doc)
			if stderr != "" {
				t.Errorf("stderr = %q, want it empty", stderr)
			}
		})
	}
}

// deleteProbes builds "probe delete <name>" and "probe remove <name>", both
// DELETE /things/:name; remove confirms with {status, slug}.
func deleteProbes() []func() *cobra.Command {
	del := func() *cobra.Command {
		return declare(command{
			Name: "delete", Description: "Delete a thing",
			Method: http.MethodDelete, Path: "/things/:name",
			Args: []argSpec{{Name: "name", Required: true}}, Auth: authUser,
		})
	}
	remove := func() *cobra.Command {
		return declare(command{
			Name: "remove", Description: "Remove a thing and confirm it",
			Method: http.MethodDelete, Path: "/things/:name",
			Args: []argSpec{{Name: "name", Required: true}}, Auth: authUser,
			Confirm: func(c *call) any {
				return struct {
					Status string `json:"status"`
					Slug   string `json:"slug"`
				}{"deleted", c.args["name"]}
			},
		})
	}
	return []func() *cobra.Command{del, remove}
}

func TestOutput_PrintsAnEmptyObjectOrTheConfirmationForASuccessWithoutABody(t *testing.T) {
	tests := []struct {
		name    string
		command string
		status  int
		want    string
	}{
		{"204 without a confirmation", "delete", 204, "{}\n"},
		{"200 with an empty body without a confirmation", "delete", 200, "{}\n"},
		{"204 with a confirmation", "remove", 204, "{\n  \"status\": \"deleted\",\n  \"slug\": \"demo\"\n}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := newStubHub(t).answer(http.MethodDelete, "/api/v1/things/demo", tt.status, "")
			probeEnv(t, hub.URL)

			code, stdout, stderr := runCLIWith(t, withProbes(defaultOptions(), deleteProbes()...), "probe", tt.command, "demo")

			if code != 0 {
				t.Fatalf("exit status = %d, want 0; stderr %q", code, stderr)
			}
			if stdout != tt.want {
				t.Errorf("stdout = %q, want %q", stdout, tt.want)
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want it empty", stderr)
			}
			if got := hub.recorded(); len(got) != 1 || got[0].method != http.MethodDelete {
				t.Errorf("the hub got %v, want one DELETE", got)
			}
		})
	}
}

func TestOutput_PrintsTheHubsErrorEnvelopeOnStderrAsReceived(t *testing.T) {
	sha := strings.Repeat("0123456789", 4)
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"not found", 404, `{"error":{"code":404,"message":"workspace not found","error_type":"not_found"}}`},
		{"unauthenticated", 401, `{"error":{"code":401,"message":"invalid credentials","error_type":"unauthenticated"}}`},
		{"insufficient scope", 403, `{"error":{"code":403,"message":"the token lacks the scope users:read","error_type":"insufficient_scope"}}`},
		{"details", 409, `{"error":{"code":409,"message":"m","error_type":"ref_moved","details":{"current_sha":"` + sha + `"}}}`},
		{"oldest_seq", 410, `{"error":{"code":410,"message":"m","error_type":"cursor_expired","oldest_seq":42}}`},
		{"unavailable", 503, `{"error":{"code":503,"message":"the hub is shutting down","error_type":"unavailable"}}` + "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := newStubHub(t).answer(http.MethodGet, "/api/v1/things/x", tt.status, tt.body)
			probeEnv(t, hub.URL)

			code, stdout, stderr := runProbes(t, "probe", "get", "x")

			if code != 1 {
				t.Errorf("exit status = %d, want 1", code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want it empty", stdout)
			}
			var got, want any
			oneJSONDocument(t, stderr, &got)
			json.Unmarshal([]byte(tt.body), &want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("stderr holds %v, want the hub's envelope %v", got, want)
			}
			if want := indented(t, tt.body); stderr != want {
				t.Errorf("stderr = %q, want %q", stderr, want)
			}
		})
	}
}

func TestOutput_WarnsAfterAResultThatShowsASecretOnce(t *testing.T) {
	mint := func() *cobra.Command {
		return declare(command{
			Name: "mint", Description: "Mint a token",
			Method: http.MethodPost, Path: "/tokens", Auth: authUser,
			ShowsOnce: true,
		})
	}
	opts := withProbes(defaultOptions(), mint)

	t.Run("a result", func(t *testing.T) {
		hub := newStubHub(t).answer(http.MethodPost, "/api/v1/tokens", 201, `{"id":"t1","token":"parley_pat_abc"}`)
		probeEnv(t, hub.URL)

		code, stdout, stderr := runCLIWith(t, opts, "probe", "mint")

		if code != 0 {
			t.Fatalf("exit status = %d, want 0; stderr %q", code, stderr)
		}
		var doc struct{ ID, Token string }
		oneJSONDocument(t, stdout, &doc)
		if doc.Token != "parley_pat_abc" {
			t.Errorf("token = %q, want the hub's parley_pat_abc", doc.Token)
		}
		if want := "warning: this value is shown once and will not be shown again\n"; stderr != want {
			t.Errorf("stderr = %q, want %q", stderr, want)
		}
	})

	t.Run("an error", func(t *testing.T) {
		envelope := `{"error":{"code":403,"message":"the token lacks the scope tokens:write","error_type":"insufficient_scope"}}`
		hub := newStubHub(t).answer(http.MethodPost, "/api/v1/tokens", 403, envelope)
		probeEnv(t, hub.URL)

		code, stdout, stderr := runCLIWith(t, opts, "probe", "mint")

		if code != 1 || stdout != "" {
			t.Errorf("exit status = %d, stdout %q, want 1 and nothing", code, stdout)
		}
		if want := indented(t, envelope); stderr != want {
			t.Errorf("stderr = %q, want the envelope alone, %q", stderr, want)
		}
		if strings.Contains(stderr, "shown once") {
			t.Errorf("stderr = %q, want no show-once warning", stderr)
		}
	})
}

// tallyProbe builds "probe tally" (GET /tally) with --fail-on-odd, whose
// condition is the result's n being odd.
func tallyProbe() *cobra.Command {
	return declare(command{
		Name: "tally", Description: "Tally the things",
		Method: http.MethodGet, Path: "/tally", Auth: authUser,
		FailOn: []failOn{{
			Name:        "odd",
			Description: "fail when the tally is odd",
			Check: func(result []byte) (bool, string) {
				var doc struct{ N int }
				json.Unmarshal(result, &doc)
				return doc.N%2 == 1, fmt.Sprintf("n is %d, an odd number", doc.N)
			},
		}},
	})
}

func TestOutput_PrintsTheResultThenFailsWhenAFailOnConditionHolds(t *testing.T) {
	tests := []struct {
		name     string
		flag     bool
		n        int
		wantCode int
	}{
		{"flag given, condition holds", true, 3, 3},
		{"flag given, condition does not hold", true, 2, 0},
		{"flag not given, condition holds", false, 3, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"n": %d}`, tt.n)
			hub := newStubHub(t).answer(http.MethodGet, "/api/v1/tally", 200, body)
			probeEnv(t, hub.URL)
			args := []string{"probe", "tally"}
			if tt.flag {
				args = append(args, "--fail-on-odd")
			}

			code, stdout, stderr := runCLIWith(t, withProbes(defaultOptions(), tallyProbe), args...)

			if code != tt.wantCode {
				t.Errorf("exit status = %d, want %d; stderr %q", code, tt.wantCode, stderr)
			}
			if want := indented(t, body); stdout != want {
				t.Errorf("stdout = %q, want the whole result %q", stdout, want)
			}
			if tt.wantCode == 0 {
				if stderr != "" {
					t.Errorf("stderr = %q, want it empty", stderr)
				}
				return
			}
			errCode, message, errorType := clientEnvelope(t, stderr)
			if errCode != 0 || errorType != "condition_not_met" {
				t.Errorf("envelope code %v, error_type %q, want 0 and condition_not_met", errCode, errorType)
			}
			if !strings.Contains(message, "3") {
				t.Errorf("message = %q, want it to name n, 3", message)
			}
		})
	}
}

func TestWriters_WriteOnePlainLineOnStderr(t *testing.T) {
	t.Parallel()
	var w bytes.Buffer

	warn(&w, "cannot read %s", "the file")
	progress(&w, "waiting: %s", "running")

	if want := "warning: cannot read the file\nwaiting: running\n"; w.String() != want {
		t.Errorf("written %q, want %q", w.String(), want)
	}
}
