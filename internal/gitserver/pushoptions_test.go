package gitserver_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/gitserver"
	"github.com/agent-fox-dev/parley/internal/workspace"
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

// The options stage answers a push with the refusal of its options, or with
// the session it is written for and whether it opens a change; a session
// linker that cannot tell refuses the push, logged, rather than failing it.
func TestPushOptionsStage_AnswersWithTheRefusalOrTheSessionAndOpening(t *testing.T) {
	repo := workspace.Workspace{Slug: "demo", Kind: access.KindRepository}
	fork := func(auto bool) workspace.Workspace {
		return workspace.Workspace{Slug: "demo--t1", Kind: access.KindFork, ParentSlug: ptr("demo"), AutoChange: &auto}
	}
	for _, c := range []struct {
		name    string
		ws      workspace.Workspace
		options []string
		linker  error
		want    gitserver.OptionsAnswer
	}{
		{"no option", repo, nil, nil, gitserver.OptionsAnswer{}},
		{"a change asked for", repo, []string{"parley.change"}, nil, gitserver.OptionsAnswer{OpensChange: true}},
		{"a target", repo, []string{"parley.target=release"}, nil, gitserver.OptionsAnswer{OpensChange: true}},
		{"a fork that opens changes", fork(true), nil, nil, gitserver.OptionsAnswer{OpensChange: true}},
		{"a fork that does not", fork(false), nil, nil, gitserver.OptionsAnswer{}},
		{"a session", repo, []string{"parley.session=ABC"}, nil, gitserver.OptionsAnswer{SessionID: ptr("abc")}},
		{"an invalid option", fork(true), []string{"parley.bogus"}, nil,
			gitserver.OptionsAnswer{Refusal: "invalid push option parley.bogus: unknown option"}},
		{"a linker that cannot tell", repo, []string{"parley.session=abc", "parley.change"}, errors.New("store down"),
			gitserver.OptionsAnswer{Refusal: "internal error; try again"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var logged bytes.Buffer
			s := &gitserver.PushOptionsStage{}
			s.Bind(slog.New(slog.NewTextHandler(&logged, nil)))
			s.RegisterSessionLinker(&sessionStub{err: c.linker})
			got, err := s.CheckOptions(context.Background(), gitserver.OptionsInput{Workspace: c.ws, Options: c.options})
			if err != nil || !reflect.DeepEqual(got, c.want) {
				t.Errorf("answer = %+v, %v; want %+v, nil", got, err, c.want)
			}
			if failed := c.linker != nil; strings.Contains(logged.String(), "store down") != failed {
				t.Errorf("log = %q, want the linker's failure logged: %t", logged.String(), failed)
			}
		})
	}
}
