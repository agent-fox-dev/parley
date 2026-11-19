package forgetest_test

import (
	"crypto/tls"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/forge/forgetest"
)

// A client that gives up on a request closes its TLS connection by sending
// close_notify and then closing the socket. Whatever the fake writes in
// between reaches the client as the answer to the request it gave up on,
// so an answer held back must never be written once the client stops
// sending.
func TestFake_WritesNoHeldBackAnswerOnceTheClientStopsSending(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name   string
		answer forgetest.Answer
	}{
		{"held", forgetest.Answer{Hold: true}},
		{"held until released", forgetest.Answer{Release: make(chan struct{}), Through: true}},
		{"delayed", forgetest.Answer{Delay: time.Hour}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			f := forgetest.New(t)
			f.Script(row.answer)
			config := f.Transport().(*http.Transport).TLSClientConfig.Clone()
			conn, err := tls.Dial("tcp", strings.TrimPrefix(f.URL, "https://"), config)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err := io.WriteString(conn, "GET "+forgetest.GitHubPrefix+"/repos/acme/widgets/labels/triage HTTP/1.1\r\nHost: forge\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(10 * time.Second)
			for f.Count() == 0 {
				if time.Now().After(deadline) {
					t.Fatal("the fake received no request")
				}
				time.Sleep(time.Millisecond)
			}
			if err := conn.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if got, _ := io.ReadAll(conn); len(got) > 0 {
				t.Errorf("the client that stopped sending was answered %.100q, want nothing", got)
			}
		})
	}
}
