package httpapi

import (
	"encoding/json"
	"net/http"
)

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
