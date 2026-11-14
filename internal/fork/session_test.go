package fork_test

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"uuid"
)

func TestCreate_LinksSession(t *testing.T) {
	t.Parallel()
	l := &linker{}
	w := newWorld(t, collaborate(l, nil))
	sid := uuid.New().String()
	// The id is stored as the linker answers it, in lower case.
	_, ws := created(t, w.post(t, w.devKey, forksPath, map[string]any{"name": "l1", "session_id": strings.ToUpper(sid)}))
	wantMembers(t, ws, map[string]any{"session_id": sid})
	if evs := w.forkEvents(t); len(evs) != 1 || evs[0].SessionID == nil || *evs[0].SessionID != sid {
		t.Errorf("fork events = %+v, want one with session_id %s", evs, sid)
	}
	checks, records := l.asked()
	if want := [][3]string{{strings.ToUpper(sid), w.dev, parentSlug}}; !reflect.DeepEqual(checks, want) {
		t.Errorf("checks = %v, want %v", checks, want)
	}
	if want := [][3]string{{sid, parentSlug, "demo--l1"}}; !reflect.DeepEqual(records, want) {
		t.Errorf("records = %v, want %v", records, want)
	}
	var marks int
	if err := w.Store.Main().QueryRow(`SELECT count(*) FROM session_marks WHERE fork = 'demo--l1'`).Scan(&marks); err != nil || marks != 1 {
		t.Errorf("rows the record wrote = %d (%v), want 1, committed with the fork", marks, err)
	}

	t.Run("without session_id", func(t *testing.T) {
		created(t, w.post(t, w.devKey, forksPath, map[string]any{"name": "l2"}))
		if checks, records := l.asked(); len(checks) != 1 || len(records) != 1 {
			t.Errorf("checks %v and records %v, want neither asked again", checks, records)
		}
	})
	for name, fn := range map[string]func(*linker){
		"record fails": func(l *linker) { l.recordErr = errors.New("link store down") },
		"check fails":  func(l *linker) { l.fail = errors.New("session store down") },
	} {
		t.Run(name, func(t *testing.T) {
			l.set(fn)
			defer l.set(func(l *linker) { l.recordErr, l.fail = nil, nil })
			before := w.footprint(t)
			wantError(t, w.post(t, w.devKey, forksPath, map[string]any{"name": "l3", "session_id": uuid.New().String()}),
				http.StatusInternalServerError, "internal")
			w.unchanged(t, before)
		})
	}
}

func TestCreate_RefusesUnknownSession(t *testing.T) {
	t.Parallel()
	for name, l := range map[string]*linker{"linker refusing every id": {refuse: true}, "no linker": nil} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t, collaborate(l, nil))
			before := w.footprint(t)
			wantError(t, w.post(t, w.devKey, forksPath, map[string]any{"session_id": uuid.New().String()}),
				http.StatusBadRequest, "unknown_session")
			w.unchanged(t, before)
		})
	}
}
