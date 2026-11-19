package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"

	"github.com/agent-fox-dev/parley/internal/server"
)

// maxBatch is the most items a batch may hold.
const maxBatch = 1000

// batchAnswer is the answer to a batch: how many of its items were stored,
// how many were already stored, and an entry per item that was not stored
// for a reason, in the order of the items.
type batchAnswer struct {
	Accepted   int         `json:"accepted"`
	Duplicates int         `json:"duplicates"`
	Errors     []itemError `json:"errors"`
}

// itemError is the reason the item at Index of a batch was not stored. ID
// is the item's id in lower case, nil when the item carries no valid one.
type itemError struct {
	Index   int     `json:"index"`
	ID      *string `json:"id"`
	Message string  `json:"message"`
}

// valid is an item of a batch that read accepted, with its index.
type valid[R record] struct {
	index int
	rec   R
}

// batch is the validator of a batch route of the kind k: its body is a JSON
// array of 1 to maxBatch items, each read with read like the body of a
// single record, a run_id member included. An invalid item is reported and
// skipped, not a reason to refuse the batch, so that one bad item does not
// cost an agent the rest. The valid items are classified and stored in one
// transaction: an id already stored in this workspace and run, or sent
// earlier in the batch, is a duplicate and changes nothing; an id of
// another workspace or run is reported; the rest are stored, all of them or,
// when the store fails, none. A batch takes no Idempotency-Key: sent again,
// its items are duplicates.
func batch[R record](c *Component, k kind[R], read func(fields, target) (R, error)) validator {
	return func(r *http.Request, t target) (act, error) {
		items, err := readItems(r)
		if err != nil {
			return nil, err
		}
		ans := batchAnswer{Errors: []itemError{}}
		var recs []valid[R]
		for i, raw := range items {
			rec, err := readItem(raw, t, read)
			if err != nil {
				var refusal *server.Error
				if !errors.As(err, &refusal) {
					return nil, err
				}
				ans.Errors = append(ans.Errors, itemError{Index: i, ID: itemID(raw), Message: refusal.Message()})
				continue
			}
			recs = append(recs, valid[R]{index: i, rec: rec})
		}
		return func(ctx context.Context, w http.ResponseWriter) error {
			err := c.write(ctx, func(ctx context.Context, tx Tx) error {
				for _, v := range recs {
					stored, created, err := storeOnce(ctx, tx, k, v.rec)
					if err != nil {
						return err
					}
					h, s := v.rec.common(), stored.common()
					switch {
					case created:
						ans.Accepted++
					case s.Workspace != h.Workspace || s.RunID != h.RunID:
						ans.Errors = append(ans.Errors, itemError{
							Index: v.index, ID: &h.ID,
							Message: fmt.Sprintf("id %s names a %s of another workspace or run", h.ID, k.name),
						})
					default:
						ans.Duplicates++
					}
				}
				return nil
			})
			if err != nil {
				return fmt.Errorf("store a batch of %d %ss of run %s in workspace %s: %w", len(recs), k.name, t.runID, t.workspace, err)
			}
			slices.SortFunc(ans.Errors, func(a, b itemError) int { return a.Index - b.Index })
			server.WriteJSON(w, http.StatusOK, ans)
			return nil
		}, nil
	}
}

// readItems reads the body of r as a JSON array of 1 to maxBatch items.
func readItems(r *http.Request) ([]json.RawMessage, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, server.InvalidRequest("request body could not be read")
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] != '[' {
		return nil, server.InvalidRequest("request body must be a JSON array of items")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, server.InvalidRequest("request body is not a valid JSON array")
	}
	switch {
	case len(items) == 0:
		return nil, server.InvalidRequest("request body must hold at least one item")
	case len(items) > maxBatch:
		return nil, server.PayloadTooLarge(fmt.Sprintf("a batch holds at most %d items, not %d", maxBatch, len(items)))
	}
	return items, nil
}

// readItem reads the item raw of a batch to t like the body of a single
// record: a JSON object whose run_id member, when present, equals the
// path's.
func readItem[R record](raw json.RawMessage, t target, read func(fields, target) (R, error)) (rec R, err error) {
	var f fields
	if raw[0] != '{' || json.Unmarshal(raw, &f) != nil {
		return rec, server.InvalidRequest("item must be a JSON object")
	}
	if err := f.runID(t.runID); err != nil {
		return rec, err
	}
	return read(f, t)
}

// itemID is the id of the item raw in lower case, nil when raw is not an
// object or its id member is not a UUID.
func itemID(raw json.RawMessage) *string {
	var f fields
	if raw[0] != '{' || json.Unmarshal(raw, &f) != nil {
		return nil
	}
	id, err := f.uuid("id")
	if err != nil {
		return nil
	}
	return id
}
