package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// bodyReadTimeout is how long a request body has to arrive. MaxBytesReader
// bounds how large a body may be, not how slowly it may be written, and a
// handful of connections dribbling a byte a minute would each hold a goroutine
// and a connection for as long as they liked.
//
// It is set here, per route, and deliberately not as the server's ReadTimeout:
// that puts a deadline on the underlying connection, and the terminal and the
// VS Code proxy hijack theirs and stream for as long as the browser stays
// attached.
const bodyReadTimeout = 30 * time.Second

// decodeJSON reads a JSON request body under both bounds: at most limit bytes,
// arriving within bodyReadTimeout.
//
// The deadline does not outlive the request. net/http sets the read deadline
// again before reading the next request on a kept-alive connection — to
// ReadHeaderTimeout, or to nothing when there is none — so a later WebSocket
// upgrade on the same connection does not inherit this one.
func decodeJSON(w http.ResponseWriter, r *http.Request, limit int64, into any) error {
	// ErrNotSupported means the response writer has no deadline to set — a
	// recorder in a test, say. The size bound below still applies, and there is
	// no connection to hold open in the first place.
	if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(bodyReadTimeout)); err != nil &&
		!errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(into)
}

// writeJSON sends v as JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	// The status line is already out, so a marshalling failure here can only be
	// logged by the caller's recovery path; truncating the body is the best we
	// can do.
	_ = json.NewEncoder(w).Encode(v)
}

// writeError sends a JSON error body, the shape the frontend expects for every
// non-2xx API response.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
