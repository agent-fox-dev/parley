package scm_test

import (
	"context"
	"slices"
	"testing"
)

func TestRead_ReportsEachRequestOnceToTheRecorder(t *testing.T) {
	w := newWorld(t)
	_, seq, err := w.events.ReadAfter(context.Background(), 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, path, credential string, want ...string) {
		t.Helper()
		w.observed.reset()
		w.get(t, path, credential)
		if got := w.observed.reset(); !slices.Equal(got, want) {
			t.Errorf("GET %s: the recorder saw %q, want %q", path, got, want)
		}
	}
	for _, rt := range scmRoutes {
		t.Run(rt.ok, func(t *testing.T) {
			check(t, scmPath("demo", rt.ok), w.ownerKey, rt.op+" ok")
			check(t, scmPath("demo", rt.bad), w.ownerKey, rt.op+" error")
		})
	}
	t.Run("refused by the state check", func(t *testing.T) {
		w.set(t, "demo", "maintenance", "reclone")
		defer w.set(t, "demo", "maintenance", "")
		check(t, scmPath("demo", "/refs"), w.ownerKey, "refs error")
	})
	t.Run("refused before the route", func(t *testing.T) {
		check(t, scmPath("demo", "/refs"), "")
		check(t, scmPath("demo", "/refs"), w.outsiderKey)
	})
	if _, after, err := w.events.ReadAfter(context.Background(), 0, 10000); err != nil || after != seq {
		t.Errorf("the event log's last seq went from %d to %d (%v), want no event", seq, after, err)
	}
}
