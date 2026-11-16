package carrypatch_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

func TestRunGit_GivesEachOperationItsDeadline(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		limit time.Duration
		want  time.Duration
	}{
		{"no limit is the default", 0, carrypatch.DefaultGitDeadline},
		{"a shorter limit is taken", 2 * time.Minute, 2 * time.Minute},
		{"a longer limit is cut to the default", time.Hour, carrypatch.DefaultGitDeadline},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			start := time.Now()
			err := carrypatch.RunGit(context.Background(), c.limit, gitrepo.Credential{}, func(ctx context.Context, deadline time.Duration) error {
				if deadline != c.want {
					t.Errorf("the operation's deadline = %v, want %v", deadline, c.want)
				}
				end, ok := ctx.Deadline()
				if !ok || end.Sub(start) < c.want-time.Second || end.Sub(start) > c.want+time.Second {
					t.Errorf("the operation's context ends at %v after the start, want %v", end.Sub(start), c.want)
				}
				return nil
			})
			if err != nil {
				t.Errorf("RunGit = %v, want nil", err)
			}
		})
	}
}

func TestRunGit_ReportsTheTimeout(t *testing.T) {
	t.Parallel()
	err := carrypatch.RunGit(context.Background(), 50*time.Millisecond, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		<-ctx.Done()
		return fmt.Errorf("git fetch: %w", ctx.Err())
	})
	if !errors.Is(err, carrypatch.ErrGitTimeout) || !errors.Is(err, gitrepo.ErrDeadline) {
		t.Errorf("err = %v, want one that matches ErrGitTimeout and gitrepo.ErrDeadline", err)
	}

	// A caller that gives up is no timeout.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = carrypatch.RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		return fmt.Errorf("git fetch: %w", ctx.Err())
	})
	if !errors.Is(err, context.Canceled) || errors.Is(err, carrypatch.ErrGitTimeout) {
		t.Errorf("err of a canceled call = %v, want context.Canceled and no timeout", err)
	}
}

func TestRunGit_CleansTheErrorText(t *testing.T) {
	t.Parallel()
	moved := gitrepo.ErrRefMoved
	cred := gitrepo.Credential{Username: "deploy-bot", Password: "pw-77aa"}
	err := carrypatch.RunGit(context.Background(), 0, cred, func(context.Context, time.Duration) error {
		return fmt.Errorf("git fetch: fatal: unable to access 'https://deploy-bot:pw-77aa@up.example/x.git/': sent pw-77aa: %w", moved)
	})
	if err == nil {
		t.Fatal("RunGit = nil, want the operation's error")
	}
	text := err.Error()
	for _, secret := range []string{"pw-77aa", "deploy-bot"} {
		if strings.Contains(text, secret) {
			t.Errorf("the error %q shows %q", text, secret)
		}
	}
	if !strings.Contains(text, "https://***@up.example/x.git/") {
		t.Errorf("the error %q does not show the URL with ***@", text)
	}
	if !errors.Is(err, moved) {
		t.Errorf("the cleaned error %v no longer matches the operation's", err)
	}
}
