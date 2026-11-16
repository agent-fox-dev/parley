package carrypatch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/server"
)

// batchRequestsOf validates the body raw of a batch, an array of patch
// objects, element by element in order, each with the validation of a
// single add. A batch registers new patches only, so if_not_exists true is
// refused (false is accepted), and so is a branch an earlier element named
// already. A refusal names the zero-based index of the element it refuses,
// as "patch[<i>]: " at the start of its message; an empty batch is refused
// as such.
func batchRequestsOf(raw json.RawMessage, integration func() (string, error)) ([]addRequest, error) {
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, server.InvalidRequest("request body must be a patch object or an array of them")
	}
	if len(elems) == 0 {
		return nil, server.InvalidRequest("a batch must hold at least one patch object")
	}
	reqs := make([]addRequest, 0, len(elems))
	named := make(map[string]int, len(elems))
	for i, elem := range elems {
		prefix := fmt.Sprintf("patch[%d]: ", i)
		if !bytes.HasPrefix(bytes.TrimSpace(elem), []byte("{")) {
			return nil, server.InvalidRequest(prefix + "a patch must be a JSON object")
		}
		req, err := addRequestOf(elem, integration)
		if se := (*server.Error)(nil); errors.As(err, &se) && se.Status() == http.StatusBadRequest {
			return nil, server.InvalidRequest(prefix + se.Message())
		}
		if err != nil {
			return nil, err
		}
		if req.ifNotExists {
			return nil, server.InvalidRequest(prefix + "if_not_exists cannot be true in a batch")
		}
		if first, dup := named[req.branch]; dup {
			return nil, server.InvalidRequest(fmt.Sprintf("%sbranch_name %q is named by patch[%d] already", prefix, req.branch, first))
		}
		named[req.branch] = i
		reqs = append(reqs, req)
	}
	return reqs, nil
}

// writeBatch answers a batch with 201 and the one member items: the
// patches it created, in request order.
func writeBatch(w http.ResponseWriter, created []Patch) {
	items := make([]Object, len(created))
	for i, p := range created {
		items[i] = ObjectOf(p, "")
	}
	server.WriteJSON(w, http.StatusCreated, struct {
		Items []Object `json:"items"`
	}{items})
}
