package api

import (
	"encoding/json"
	"errors"
	"io"
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

// readJSON decodes r's JSON body into v within those bounds. The body is
// one JSON value: anything but whitespace after it is malformed
// (LOOM-175). On failure it writes the response (413 for a body over
// maxRequestBody, however malformed; 400 otherwise) and reports false.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.ContentLength > maxRequestBody {
		// Too big by its own account: refused before reading any of it.
		w.Header().Set("Connection", "close")
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return false
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(bodyReadTimeout))
	body := http.MaxBytesReader(w, r.Body, maxRequestBody)
	dec := json.NewDecoder(body)
	err := dec.Decode(v)
	if err == nil {
		// What follows the JSON value is read here, under the deadline,
		// rather than drained by net/http after the handler with none of
		// its own (LOOM-133).
		err = onlyWhitespace(io.MultiReader(dec.Buffered(), body))
	}
	if err == nil {
		_ = rc.SetReadDeadline(time.Time{})
		return true
	}
	// The deadline stays: net/http drains what's left of a body before
	// answering, and a body that stalled would otherwise hold the
	// connection for ever. It isn't reused.
	w.Header().Set("Connection", "close")
	var tooBig *http.MaxBytesError
	if !errors.As(err, &tooBig) {
		// A body malformed early still counts as too big if it is: read
		// the rest, bounded and under the deadline, to tell (LOOM-175).
		if _, rest := io.Copy(io.Discard, body); errors.As(rest, &tooBig) {
			err = rest
		}
	}
	if errors.As(err, &tooBig) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return false
	}
	writeError(w, http.StatusBadRequest, "malformed request body")
	return false
}

// errTrailingData is onlyWhitespace's error for a body with more than
// one JSON value, or a value followed by anything else.
var errTrailingData = errors.New("api: data after the JSON value")

// onlyWhitespace reads r to its end and fails if it holds anything but
// JSON whitespace.
func onlyWhitespace(r io.Reader) error {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		for _, c := range buf[:n] {
			if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
				return errTrailingData
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
