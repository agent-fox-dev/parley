package access_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/store"
)

// openStore opens the transactional store in dir, on disk, with the
// workspace record step, and closes it when the test ends. Opening the same
// dir again reopens the same store.
func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), store.Options{
		DatabasePath:  filepath.Join(dir, "parley.db"),
		WorkspaceRoot: filepath.Join(dir, "workspaces"),
	}, []store.Step{access.SchemaStep})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// repo is a repository record.
func repo(slug, owner, org, status string) access.Record {
	return access.Record{Slug: slug, OwnerID: owner, OrgID: org, Kind: access.KindRepository, Status: status}
}

// fork is a ready fork record of parent.
func fork(slug, owner, org, parent string) access.Record {
	return access.Record{Slug: slug, OwnerID: owner, OrgID: org, Kind: access.KindFork, ParentSlug: parent, Status: access.StatusReady}
}

// insert inserts rec in a transaction of its own.
func insert(st *store.Store, rec access.Record) error {
	return st.Tx(context.Background(), func(tx *sql.Tx) error {
		return access.InsertRecord(context.Background(), tx, rec)
	})
}

// seed inserts recs in order and fails the test at the first refusal.
func seed(t *testing.T, st *store.Store, recs ...access.Record) {
	t.Helper()
	for _, rec := range recs {
		if err := insert(st, rec); err != nil {
			t.Fatalf("insert %+v: %v", rec, err)
		}
	}
}

// memberships is a membership source stub: the orgs each user is a member
// of, of which it answers only those in active, as the real source answers
// only active orgs.
type memberships struct {
	of     map[string][]string
	active []string
}

func (m memberships) ActiveOrgIDs(_ context.Context, userID string) ([]string, error) {
	var ids []string
	for _, id := range m.of[userID] {
		if slices.Contains(m.active, id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// errRead is the failure of the failing sources. It names a file of the
// hub, which no answer may show.
var errRead = errors.New("disk I/O error at /data/parley.db")

// failingRecords reads through RecordReader, except the record of a slug in
// fail, and the list of owned-or-in records or of forks when failOwned or
// failForks is set, whose reads fail with errRead.
type failingRecords struct {
	access.RecordReader
	fail                 []string
	failOwned, failForks bool
}

func (f failingRecords) Record(ctx context.Context, slug string) (access.Record, bool, error) {
	if slices.Contains(f.fail, slug) {
		return access.Record{}, false, errRead
	}
	return f.RecordReader.Record(ctx, slug)
}

func (f failingRecords) OwnedOrIn(ctx context.Context, ownerID string, orgIDs []string) ([]access.Record, error) {
	if f.failOwned {
		return nil, errRead
	}
	return f.RecordReader.OwnedOrIn(ctx, ownerID, orgIDs)
}

func (f failingRecords) ForksOf(ctx context.Context, parents []string) ([]access.Record, error) {
	if f.failForks {
		return nil, errRead
	}
	return f.RecordReader.ForksOf(ctx, parents)
}

// failingMemberships is a membership source whose every read fails with
// errRead.
var failingMemberships = access.MembershipsFunc(func(context.Context, string) ([]string, error) {
	return nil, errRead
})

// newService is a service bound to records and m.
func newService(records access.RecordReader, m access.Memberships) *access.Service {
	svc := &access.Service{}
	svc.Bind(access.Deps{Records: records, Memberships: m})
	return svc
}

// probe is a server with svc's access check in the access position and the
// probe routes GET /workspaces/:slug/r, /w and /o, which need read, write
// and owner. A probe route answers 200 with the decision it reads, and
// counts its calls. The authenticator takes the caller from the header
// X-Caller: "admin" is the admin token, another value a user id, and no
// header a caller with neither.
type probe struct {
	srv   *server.Server
	calls atomic.Int64
}

func newProbe(svc *access.Service) *probe {
	p := &probe{srv: server.New(server.Options{Logger: slog.New(slog.DiscardHandler), RateLimitDisabled: true})}
	p.srv.SetAuthenticator(func(r *http.Request) (server.Caller, error) {
		switch id := r.Header.Get("X-Caller"); id {
		case "":
			return server.Caller{CredentialID: "anonymous"}, nil
		case "admin":
			return server.Caller{CredentialID: id, Admin: true}, nil
		default:
			return server.Caller{CredentialID: id, UserID: id}, nil
		}
	})
	p.srv.AddAccessCheck(svc.Check)
	p.route("/workspaces/:slug/r", "read")
	p.route("/workspaces/:slug/w", "write")
	p.route("/workspaces/:slug/o", "owner")
	return p
}

// route registers a probe route GET pattern that declares need.
func (p *probe) route(pattern, need string) {
	p.routeFor(http.MethodGet, pattern, need)
}

// routeFor registers a probe route method pattern that declares need.
func (p *probe) routeFor(method, pattern, need string) {
	p.srv.Register(server.Route{
		Method: method, Pattern: pattern, Marks: map[string]string{access.MarkNeed: need},
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			p.calls.Add(1)
			d, ok := access.DecisionOf(r)
			if !ok {
				return errors.New("the handler found no decision")
			}
			server.WriteJSON(w, http.StatusOK, map[string]any{"level": d.Level, "record": d.Record})
			return nil
		},
	})
}

// answer is what the probe answered.
type answer struct {
	status      int
	contentType string
	body        []byte
}

// get calls GET path, under /api/v1, as caller ("" for none).
func (p *probe) get(caller, path string) answer {
	return p.call(http.MethodGet, caller, path)
}

// call calls method path, under /api/v1, as caller ("" for none), without
// a body.
func (p *probe) call(method, caller, path string) answer {
	r := httptest.NewRequest(method, "/api/v1"+path, nil)
	if caller != "" {
		r.Header.Set("X-Caller", caller)
	}
	rec := httptest.NewRecorder()
	p.srv.Handler().ServeHTTP(rec, r)
	return answer{rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes()}
}

// envelope is the error envelope's error object, or the probe's answer.
type envelope struct {
	Error struct {
		Code      int    `json:"code"`
		Message   string `json:"message"`
		ErrorType string `json:"error_type"`
	} `json:"error"`
	Level  access.Level  `json:"level"`
	Record access.Record `json:"record"`
}

func decode(t *testing.T, a answer) envelope {
	t.Helper()
	var e envelope
	if err := json.Unmarshal(a.body, &e); err != nil {
		t.Fatalf("body %s is not JSON: %v", a.body, err)
	}
	return e
}
