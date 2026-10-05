package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// Bounds on a request body (LOOM-132): how big it may be, and how long it
// may take to arrive once the headers are in. The headers have their own
// timeout (cmd/loomuxd, LOOM-115). The read deadline covers only the
// body: a conversation stream or a blocking dispatch then stays open for
// as long as its turn runs.
const maxRequestBody = 1 << 20

var bodyReadTimeout = 30 * time.Second

// readJSON decodes r's JSON body into v within those bounds. On failure
// it writes the response (413 for too large, 400 otherwise) and reports
// false.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(bodyReadTimeout))
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody)).Decode(v)
	if err == nil {
		_ = rc.SetReadDeadline(time.Time{})
		return true
	}
	// The deadline stays: net/http drains what's left of a body before
	// answering, and a body that stalled would otherwise hold the
	// connection for ever. It isn't reused.
	w.Header().Set("Connection", "close")
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return false
	}
	writeError(w, http.StatusBadRequest, "malformed request body")
	return false
}
