package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
)

func TestPrintError_WritesTheCLIsOwnEnvelope(t *testing.T) {
	tests := []struct {
		err error
		typ string
	}{
		{UsageError("bad flag %s", "x"), "usage_error"},
		{ClientError("no hub"), "client_error"},
		{WaitFailed("ended dead"), "wait_failed"},
		{WaitTimeout("too slow"), "wait_timeout"},
		{ConditionNotMet("n is odd"), "condition_not_met"},
		{errors.New("something else"), "client_error"},
	}
	for _, tt := range tests {
		var buf bytes.Buffer
		PrintError(&buf, tt.err)
		var doc map[string]map[string]any
		if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
			t.Fatalf("%v: %q is not JSON", tt.err, buf.String())
		}
		e := doc["error"]
		var keys []string
		for k := range e {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(doc) != 1 || strings.Join(keys, ",") != "code,error_type,message" {
			t.Errorf("%v: members %v", tt.err, keys)
		}
		if e["code"] != float64(0) || e["message"] == "" || e["error_type"] != tt.typ {
			t.Errorf("%v: %v", tt.err, e)
		}
		var want bytes.Buffer
		json.Indent(&want, bytes.TrimSpace(buf.Bytes()), "", "  ")
		if buf.String() != want.String()+"\n" {
			t.Errorf("%v: not indented by 2 spaces: %q", tt.err, buf.String())
		}
	}
}

func TestExitCode_MapsEveryOutcomeToZeroToThree(t *testing.T) {
	tests := []struct {
		err  error
		want int
	}{
		{nil, 0},
		{&HubError{Status: 404, Body: []byte(`{"error":{}}`)}, 1},
		{&HubError{Status: 500, Body: []byte(`{"error":{}}`)}, 1},
		{WaitFailed("x"), 1},
		{WaitTimeout("x"), 1},
		{UsageError("x"), 2},
		{ClientError("x"), 2},
		{errors.New("other"), 2},
		{ConditionNotMet("x"), 3},
	}
	for _, tt := range tests {
		if got := ExitCode(tt.err); got != tt.want {
			t.Errorf("ExitCode(%v) = %d, want %d", tt.err, got, tt.want)
		}
	}
}
