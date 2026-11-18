package forge_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/forge"
	"github.com/agent-fox-dev/parley/internal/forge/forgetest"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/scrub"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// closedURL is an https URL on a loopback port nothing listens on.
func closedURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return "https://" + addr
}

// notTLSURL is an https URL on a listener that answers every connection
// with bytes that are no TLS handshake, until the test ends.
func notTLSURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"))
			conn.Close()
		}
	}()
	return "https://" + l.Addr().String()
}

func TestMapError_BuildsTheMessageFromTheForgesAnswer(t *testing.T) {
	t.Parallel()
	answered := func(status int, body string) *forge.Failure {
		return &forge.Failure{Kind: forge.FailureStatus, Status: status, Body: []byte(body)}
	}
	limited := func(wait time.Duration, stated bool) *forge.Failure {
		return &forge.Failure{Kind: forge.FailureRateLimited, Status: 429, Wait: wait, WaitStated: stated}
	}
	cases := []struct {
		name           string
		err            error
		wantMessage    string // the whole message, or with a trailing * its prefix
		wantUnanswered bool   // the message describes a missing answer
		wantRetryAfter string
	}{
		{"GitHub's message", answered(404, `{"message":"Not Found"}`), "the forge answered 404: Not Found", false, ""},
		{"GitLab's message", answered(404, `{"message":"404 Project Not Found"}`),
			"the forge answered 404: 404 Project Not Found", false, ""},
		{"GitLab's error", answered(400, `{"error":"state does not have a valid value"}`),
			"the forge answered 400: state does not have a valid value", false, ""},
		{"a message that is no string", answered(400, `{"message": {"title": ["is too long"]}}`),
			`the forge answered 400: {"title":["is too long"]}`, false, ""},
		{"a body that is no JSON", answered(500, "oops"), "the forge answered 500*", false, ""},
		{"a wrapped failure", fmt.Errorf("read the issue: %w", answered(410, `{"message":"Gone"}`)),
			"the forge answered 410: Gone", false, ""},
		{"no answer before the deadline", &forge.Failure{Kind: forge.FailureTimeout, Err: context.DeadlineExceeded}, "", true, ""},
		{"a connection refused", &forge.Failure{Kind: forge.FailureUnreachable,
			Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}}, "", true, ""},
		{"a stated wait in whole seconds", limited(31*time.Second, true), "the forge answered 429*", false, "31"},
		{"a stated wait rounded up", limited(44500*time.Millisecond, true), "the forge answered 429*", false, "45"},
		{"no stated wait", limited(0, false), "the forge answered 429*", false, ""},
	}
	for _, c := range cases {
		got, ok := forge.MapError(c.err, scrub.New(), "")
		if !ok {
			t.Errorf("%s: MapError found no forge failure", c.name)
			continue
		}
		switch prefix, isPrefix := strings.CutSuffix(c.wantMessage, "*"); {
		case c.wantUnanswered:
			if got.Message == "" || strings.Contains(got.Message, "the forge answered") {
				t.Errorf("%s: message %q, want a description of the missing answer", c.name, got.Message)
			}
		case isPrefix && !strings.HasPrefix(got.Message, prefix), !isPrefix && got.Message != c.wantMessage:
			t.Errorf("%s: message %q, want %q", c.name, got.Message, c.wantMessage)
		}
		if got.RetryAfter != c.wantRetryAfter {
			t.Errorf("%s: Retry-After %q, want %q", c.name, got.RetryAfter, c.wantRetryAfter)
		}
	}
	if _, ok := forge.MapError(errors.New("the hub broke"), scrub.New(), ""); ok {
		t.Error("MapError mapped an error that is no forge failure")
	}
}

func TestForgeRoute_AnswersAForgeFailureAsItsHubError(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	other := forgetest.New(t)
	apiURLs := map[string]string{
		"gh": w.fake.GitHubURL(), "gl": w.fake.GitLabURL(),
		"closed": closedURL(t) + "/github", "nxdomain": "https://forge.invalid/github", "nottls": notTLSURL(t) + "/github",
	}
	for slug, apiURL := range apiURLs {
		w.addWorkspace(t, workspace.Workspace{Slug: slug, GitURL: new("https://github.com/acme/widgets.git"),
			Settings: settings(forge.SettingAPIURL, apiURL)})
	}
	padded := func(object string, size int) string { return object + strings.Repeat(" ", size-len(object)) }
	type row struct {
		name, slug, path string
		answers          []forgetest.Answer
		status           int
		errorType        string // "" for a success
		sent             int    // the requests the fake receives; 0 for any
		unanswered       bool   // no forge answered
	}
	var rows []row
	for _, slug := range []string{"gh", "gl"} {
		for _, s := range []struct {
			forge, status int
			errorType     string
		}{
			{404, 404, forge.ErrorTypeNotFound},
			{401, 502, forge.ErrorTypeUnauthenticated},
			{403, 502, forge.ErrorTypeForbidden},
			{400, 422, forge.ErrorTypeRejected},
			{410, 422, forge.ErrorTypeRejected},
			{422, 422, forge.ErrorTypeRejected},
			{500, 502, forge.ErrorTypeError},
			{503, 502, forge.ErrorTypeError},
		} {
			rows = append(rows, row{fmt.Sprintf("%s answers %d", slug, s.forge), slug, commentsPath,
				[]forgetest.Answer{{Status: s.forge, Body: `{"message":"no"}`}}, s.status, s.errorType, 1, false})
		}
	}
	rows = append(rows,
		row{"nothing listens on the port", "closed", repositoryPath, nil, 502, forge.ErrorTypeUnreachable, 0, true},
		row{"the host name does not resolve", "nxdomain", repositoryPath, nil, 502, forge.ErrorTypeUnreachable, 0, true},
		row{"the TLS handshake fails", "nottls", repositoryPath, nil, 502, forge.ErrorTypeUnreachable, 0, true},
		row{"a body past 8 MiB", "gh", repositoryPath, []forgetest.Answer{{Body: padded("{}", forge.DefaultMaxBody+1)}},
			502, forge.ErrorTypeError, 1, false},
		row{"a body that is no JSON", "gh", repositoryPath, []forgetest.Answer{{Body: "<html>"}}, 502, forge.ErrorTypeError, 1, false},
		row{"an array for an object", "gh", repositoryPath, []forgetest.Answer{{Body: "[]"}}, 502, forge.ErrorTypeError, 1, false},
		row{"an object of exactly 8 MiB", "gh", repositoryPath,
			[]forgetest.Answer{{Body: padded(`{"full_name":"acme/widgets"}`, forge.DefaultMaxBody)}}, 200, "", 1, false},
		row{"a redirect to another port", "gh", issuePath, []forgetest.Answer{{Redirect: other.URL + "/x"}},
			502, forge.ErrorTypeError, 1, false},
		row{"a redirect to plain http on the same host", "gh", issuePath,
			[]forgetest.Answer{{Redirect: "http://" + strings.TrimPrefix(w.fake.URL, "https://") + "/x"}},
			502, forge.ErrorTypeError, 1, false},
	)
	eleven := make([]forgetest.Answer, 11)
	for i := range eleven {
		eleven[i] = forgetest.Answer{Redirect: "/again"}
	}
	rows = append(rows, row{"an eleventh redirect", "gh", issuePath, append(eleven, forgetest.Answer{}),
		502, forge.ErrorTypeError, 11, false})

	for _, c := range rows {
		w.fake.Script(c.answers...)
		status, body, sent := w.get(t, c.slug, c.path, w.Token)
		if status != c.status || c.sent != 0 && sent != c.sent {
			t.Errorf("%s: %d %.200s after %d forge requests, want %d after %d", c.name, status, body, sent, c.status, c.sent)
			continue
		}
		if c.errorType == "" {
			continue
		}
		e := errorOf(t, body)
		if e.Type != c.errorType || e.Code != c.status {
			t.Errorf("%s: error %s with code %d, want %s with %d", c.name, e.Type, e.Code, c.errorType, c.status)
		}
		if answered := strings.HasPrefix(e.Message, "the forge answered"); c.unanswered && answered {
			t.Errorf("%s: message %q, want a description of the missing answer", c.name, e.Message)
		}
	}
	if n := other.Count(); n != 0 {
		t.Errorf("the redirect's other host received %d requests", n)
	}
}

func TestForgeRoute_LetsNoForgeCredentialOut(t *testing.T) {
	t.Parallel()
	const token = "s3cr3t-token-value"
	w := newWorldWith(t, hubtest.Config{TOML: "[logging]\nlevel = \"debug\"\n"}, nil)
	for slug, apiURL := range map[string]string{"w": w.fake.GitHubURL(), "closed": closedURL(t) + "/github"} {
		w.addWorkspace(t, workspace.Workspace{Slug: slug, GitURL: new("https://github.com/acme/widgets.git"),
			Settings: settings(forge.SettingAPIURL, apiURL)})
		w.putSecret(t, slug, "GIT_PAT", token)
	}
	messages := []struct {
		name, message string
		check         func(message string) bool
	}{
		{"the token", "bad credentials for " + token,
			func(m string) bool { return strings.Contains(m, scrub.Marker) }},
		{"the token as a URL's userinfo", "see https://user:" + token + "@forge.example.com/x",
			func(m string) bool {
				return strings.Contains(m, "https://forge.example.com/x") && !strings.Contains(m, "user:")
			}},
		{"5,000 letters", strings.Repeat("a", 5000),
			func(m string) bool { return len(m) <= scrub.MaxLen }},
	}
	for _, c := range messages {
		refusal := forgetest.Answer{Status: 422, Body: fmt.Sprintf(`{"message":%q}`, c.message)}
		w.fake.Script(refusal)
		status, body, _ := w.get(t, "w", issuePath, w.Token)
		if status != http.StatusUnprocessableEntity || strings.Contains(string(body), token) {
			t.Errorf("%s: %d %.300s, want 422 without the token", c.name, status, body)
		} else if m := errorOf(t, body).Message; !c.check(m) {
			t.Errorf("%s: message %.300q", c.name, m)
		}

		// The same message for the comments of an issue thread.
		w.fake.Script(forgetest.Answer{Body: `{"number":1}`}, refusal)
		status, body, _ = w.get(t, "w", issuePath, w.Token)
		var thread struct {
			CommentsError *hubError `json:"comments_error"`
		}
		if status != http.StatusOK || strings.Contains(string(body), token) || json.Unmarshal(body, &thread) != nil || thread.CommentsError == nil {
			t.Errorf("%s for the comments: %d %.300s, want 200 with comments_error and without the token", c.name, status, body)
		} else if m := thread.CommentsError.Message; !c.check(m) {
			t.Errorf("%s for the comments: comments_error.message %.300q", c.name, m)
		}
	}

	w.fake.Script(
		forgetest.Answer{Body: `{"number":1}`}, forgetest.Answer{Body: "[]"},
		forgetest.Answer{Status: 401, Body: `{"message":"Bad credentials: Authorization: Bearer ` + token + `"}`},
	)
	reads := []struct {
		slug   string
		status int
	}{{"w", http.StatusOK}, {"w", http.StatusBadGateway}, {"closed", http.StatusBadGateway}}
	for _, r := range reads {
		if status, body, _ := w.get(t, r.slug, issuePath, w.Token); status != r.status || strings.Contains(string(body), token) {
			t.Errorf("a read of %s: %d %.300s, want %d without the token", r.slug, status, body, r.status)
		}
	}
	log := w.Log.String()
	if strings.Contains(log, token) {
		t.Errorf("the hub's log holds the forge token:\n%s", log)
	}
	for _, failure := range []string{forge.ErrorTypeUnauthenticated, forge.ErrorTypeUnreachable} {
		if !strings.Contains(log, failure) {
			t.Errorf("the hub's log has no line of the %s failure:\n%s", failure, log)
		}
	}
}
