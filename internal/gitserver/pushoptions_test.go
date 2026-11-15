package gitserver_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitserver"
)

// sessionStub is a session linker that notes each check and refuses with
// err, or accepts the id in lower case when err is nil.
type sessionStub struct {
	err   error
	calls []sessionCheck
}

type sessionCheck struct {
	caller        gitserver.PushCaller
	id, workspace string
}

func (s *sessionStub) Check(_ context.Context, caller gitserver.PushCaller, id, workspace string) (string, error) {
	s.calls = append(s.calls, sessionCheck{caller, id, workspace})
	if s.err != nil {
		return "", s.err
	}
	return strings.ToLower(id), nil
}

func ptr(s string) *string { return &s }

// numbered returns n options prefix1 .. prefixN.
func numbered(prefix string, n int) []string {
	opts := make([]string, n)
	for i := range opts {
		opts[i] = fmt.Sprintf("%s%d", prefix, i+1)
	}
	return opts
}

const sessionID = "3F2C8A1E-5B7D-4C9E-8F10-2A3B4C5D6E7F"

func TestValidatePushOptions_AcceptsWellFormedOptions(t *testing.T) {
	title200 := strings.Repeat("é", 10) + strings.Repeat("a", 190)
	intent := strings.Repeat("i", 1024-len("parley.intent="))
	all := append([]string{
		"parley.change", "parley.target=release", "parley.title=t",
		"parley.intent=i", "parley.draft", "parley.session=" + sessionID,
	}, numbered("x", 10)...)
	for _, tc := range []struct {
		name string
		opts []string
		want gitserver.PushOptions
	}{
		{"none", nil, gitserver.PushOptions{}},
		{"change", []string{"parley.change"}, gitserver.PushOptions{Change: true}},
		{"target implies change", []string{"parley.target=release"}, gitserver.PushOptions{Change: true, Target: ptr("release")}},
		{"draft and change", []string{"parley.draft", "parley.change"}, gitserver.PushOptions{Change: true, Draft: true}},
		{"value split at the first =", []string{"parley.title=a=b"}, gitserver.PushOptions{Title: ptr("a=b")}},
		{"title of 200 characters", []string{"parley.title=" + title200}, gitserver.PushOptions{Title: ptr(title200)}},
		{"option of 1024 bytes", []string{"parley.intent=" + intent}, gitserver.PushOptions{Intent: ptr(intent)}},
		{"other options ignored", []string{"ci.skip", "other=1", "parley.change"}, gitserver.PushOptions{Change: true}},
		{"16 options", all, gitserver.PushOptions{
			Change: true, Target: ptr("release"), Title: ptr("t"), Intent: ptr("i"),
			Draft: true, SessionID: ptr(strings.ToLower(sessionID)),
		}},
		{"session the linker accepts", []string{"parley.session=" + sessionID}, gitserver.PushOptions{SessionID: ptr(strings.ToLower(sessionID))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, refusal, _ := gitserver.ValidatePushOptions(context.Background(), tc.opts, gitserver.PushCaller{UserID: "U"}, "demo", &sessionStub{})
			if refusal != "" {
				t.Fatalf("refused: %q", refusal)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("options = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestValidatePushOptions_RefusesTheFirstInvalidOption(t *testing.T) {
	const tooMany = "too many push options: at most 16 are accepted"
	for _, tc := range []struct {
		opts []string
		want string
	}{
		{[]string{"parley.titel=x"}, "parley.titel: unknown option"},
		{[]string{"parley.change", "parley.change"}, "parley.change: given more than once"},
		{[]string{"parley.title=a", "parley.title=b"}, "parley.title: given more than once"},
		{[]string{"parley.change=1"}, "parley.change: takes no value"},
		{[]string{"parley.draft="}, "parley.draft: takes no value"},
		{[]string{"parley.target"}, "parley.target: value required"},
		{[]string{"parley.target="}, "parley.target: value required"},
		{[]string{"parley.title="}, "parley.title: value required"},
		{[]string{"parley.intent"}, "parley.intent: value required"},
		{[]string{"parley.session="}, "parley.session: value required"},
		{[]string{"parley.target=a..b"}, "parley.target: not a valid branch name"},
		{[]string{"parley.target=bad name"}, "parley.target: not a valid branch name"},
		{[]string{"parley.target=x.lock"}, "parley.target: not a valid branch name"},
		{[]string{"parley.title=" + strings.Repeat("a", 201)}, "parley.title: longer than 200 characters"},
		{[]string{"parley.draft=1", "parley.bogus"}, "parley.draft: takes no value"},
		{[]string{"parley.intent=" + strings.Repeat("i", 1025-len("parley.intent="))}, "parley.intent: longer than 1024 bytes"},
		{[]string{"note=" + strings.Repeat("n", 1021)}, "note: longer than 1024 bytes"},
		{numbered("x", 17), tooMany},
		{append([]string{"parley.bogus"}, numbered("x", 16)...), tooMany},
	} {
		want := tc.want
		if want != tooMany {
			want = "invalid push option " + want
		}
		t.Run(want, func(t *testing.T) {
			got, refusal, _ := gitserver.ValidatePushOptions(context.Background(), tc.opts, gitserver.PushCaller{UserID: "U"}, "demo", &sessionStub{})
			if refusal != want {
				t.Errorf("refusal = %q, want %q", refusal, want)
			}
			if !reflect.DeepEqual(got, gitserver.PushOptions{}) {
				t.Errorf("a refused push has options %+v", got)
			}
		})
	}
}

func TestValidatePushOptions_RefusesASessionTheLinkerDoesNotAccept(t *testing.T) {
	caller := gitserver.PushCaller{CredentialType: gitserver.CredentialAPIKey, CredentialID: "k1", UserID: "U"}
	for _, tc := range []struct {
		name   string
		id     string
		linker *sessionStub // nil for no linker registered
		want   string
	}{
		{"refused", sessionID, &sessionStub{err: fmt.Errorf("no such session: %w", gitserver.ErrUnknownSession)}, "invalid push option parley.session: unknown session"},
		{"not an id", "not-a-uuid", &sessionStub{err: gitserver.ErrUnknownSession}, "invalid push option parley.session: unknown session"},
		{"no linker", sessionID, nil, "invalid push option parley.session: unknown session"},
		{"linker fails", sessionID, &sessionStub{err: errors.New("database is locked")}, "internal error; try again"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var linker gitserver.SessionLinker
			if tc.linker != nil {
				linker = tc.linker
			}
			got, refusal, err := gitserver.ValidatePushOptions(context.Background(), []string{"parley.session=" + tc.id}, caller, "demo", linker)
			if (err != nil) != (tc.name == "linker fails") {
				t.Errorf("err = %v; only a failing linker returns its error", err)
			}
			if refusal != tc.want {
				t.Errorf("refusal = %q, want %q", refusal, tc.want)
			}
			if got.SessionID != nil {
				t.Errorf("a refused session is set: %q", *got.SessionID)
			}
			if tc.linker != nil {
				want := []sessionCheck{{caller, tc.id, "demo"}}
				if !reflect.DeepEqual(tc.linker.calls, want) {
					t.Errorf("linker calls = %+v, want %+v", tc.linker.calls, want)
				}
			}
		})
	}
}
