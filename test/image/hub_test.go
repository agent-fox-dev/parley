package image_test

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// readyLimit bounds the wait for a started hub container to answer
// GET /readyz with 200.
const readyLimit = 60 * time.Second

func TestHubImage_RunsTheHubOnOneVolume(t *testing.T) {
	engine := engineFor(t)
	ref := buildImage(t, engine, "containers/hub/Dockerfile", "parley-hub")
	vol := volume(t, engine)

	// The container lives across the subtests: the arguments are passed
	// beside it running, and the stop ends it.
	id, base := startHub(t, engine, ref, vol)
	if !t.Run("serves as user 10001 on port 8080 with its config and data on an empty volume", func(t *testing.T) {
		var v struct {
			Version   string `json:"version"`
			Commit    string `json:"commit"`
			BuildTime string `json:"build_time"`
		}
		if err := json.Unmarshal([]byte(get(t, base+"/version")), &v); err != nil {
			t.Fatalf("GET /version: %v", err)
		}
		if v.Version != buildVersion || v.Commit != buildCommit || v.BuildTime != buildTime {
			t.Errorf("GET /version = %+v; want version %q, commit %q, build_time %q", v, buildVersion, buildCommit, buildTime)
		}
		logs := run(t, engine, "logs", id)
		if !strings.Contains(logs.stdout+logs.stderr, "git version checked") {
			t.Errorf("the container's log holds no line 'git version checked':\n%s%s", logs.stdout, logs.stderr)
		}
		if got := must(t, engine, "exec", id, "id", "-u"); got != "10001" {
			t.Errorf("id -u in the container = %q, want 10001", got)
		}
		if got := must(t, engine, "exec", id, "stat", "-c", "%u", "/var/lib/parley/data/parley.db"); got != "10001" {
			t.Errorf("the owner of data/parley.db on the volume = %q, want 10001", got)
		}
	}) {
		return
	}

	t.Run("passes its arguments to parleyd and ships parley with the same build", func(t *testing.T) {
		if r := run(t, engine, "run", "--rm", "-v", vol+":/var/lib/parley", ref, "backup", "/var/lib/parley/snapshot"); r.code != 0 {
			t.Fatalf("backup /var/lib/parley/snapshot: exit code %d\n%s%s", r.code, r.stdout, r.stderr)
		}
		got := strings.Fields(must(t, engine, "exec", id, "ls", "/var/lib/parley/snapshot"))
		slices.Sort(got)
		if want := []string{"analytics.db", "parley.db", "workspaces"}; !slices.Equal(got, want) {
			t.Errorf("the snapshot holds %q, want %q", got, want)
		}

		help := run(t, engine, "run", "--rm", ref, "--help")
		if help.code != 0 || !strings.Contains(help.stdout, "backup") {
			t.Errorf("--help: exit code %d, output %q; want 0 and parleyd's usage", help.code, help.stdout)
		}

		version := run(t, engine, "run", "--rm", "--entrypoint", "parley", ref, "version")
		if version.code != 0 {
			t.Fatalf("parley version: exit code %d\n%s%s", version.code, version.stdout, version.stderr)
		}
		var doc struct {
			CLI struct {
				Version string `json:"version"`
			} `json:"cli"`
		}
		if err := json.Unmarshal([]byte(version.stdout), &doc); err != nil || doc.CLI.Version != buildVersion {
			t.Errorf("parley version printed %q (%v); want cli.version %q", version.stdout, err, buildVersion)
		}
	})

	t.Run("stops on the engine's stop signal and starts again on the same volume", func(t *testing.T) {
		start := time.Now()
		must(t, engine, "stop", "-t", "40", id)
		if took := time.Since(start); took >= 30*time.Second {
			t.Errorf("the container ended %s after the stop request, want less than 30s", took.Round(time.Millisecond))
		}
		if got := must(t, engine, "inspect", "-f", "{{.State.ExitCode}}", id); got != "0" {
			t.Errorf("the container's exit code = %s, want 0", got)
		}
		startHub(t, engine, ref, vol)
	})
}

// startHub starts a hub container from ref with no arguments and vol at
// /var/lib/parley, port 8080 published on a free loopback port, waits until
// it answers GET /readyz with 200, and returns its id and base URL. The
// container is removed when t ends.
func startHub(t *testing.T, engine, ref, vol string) (id, base string) {
	t.Helper()
	id = must(t, engine, "run", "-d", "-p", "127.0.0.1::8080", "-v", vol+":/var/lib/parley", ref)
	t.Cleanup(func() { run(t, engine, "rm", "-f", id) })

	// The engine prints one line per published address.
	published, _, _ := strings.Cut(must(t, engine, "port", id, "8080"), "\n")
	host, port, err := net.SplitHostPort(strings.TrimSpace(published))
	if err != nil {
		t.Fatalf("%s port %s 8080 = %q: %v", engine, id, published, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	base = "http://" + net.JoinHostPort(host, port)

	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(readyLimit)
	for {
		resp, err := client.Get(base + "/readyz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return id, base
			}
		}
		if time.Now().After(deadline) {
			logs := run(t, engine, "logs", id)
			t.Fatalf("GET /readyz did not answer 200 within %s (last: %v)\n%s%s", readyLimit, err, logs.stdout, logs.stderr)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// get answers the body of a GET of url that answers 200.
func get(t *testing.T, url string) string {
	t.Helper()
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d, %v\n%s", url, resp.StatusCode, err, body)
	}
	return string(body)
}
