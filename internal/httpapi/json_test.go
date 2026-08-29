package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// deadlineWriter is a ResponseWriter that has a read deadline to set, which
// httptest's recorder has not: http.ResponseController finds this method by
// looking for it on the writer.
type deadlineWriter struct {
	http.ResponseWriter
	deadline time.Time
	err      error
}

func (d *deadlineWriter) SetReadDeadline(t time.Time) error {
	if d.err != nil {
		return d.err
	}
	d.deadline = t
	return nil
}

// MaxBytesReader bounds how large a body may be, not how slowly it arrives.
func TestDecodeJSONBoundsBothTheSizeAndTheWait(t *testing.T) {
	w := &deadlineWriter{ResponseWriter: httptest.NewRecorder()}
	r := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(`{"imageId":"img-1"}`))

	var body struct {
		ImageID string `json:"imageId"`
	}
	if err := decodeJSON(w, r, 1024, &body); err != nil {
		t.Fatalf("decodeJSON: %v", err)
	}
	if body.ImageID != "img-1" {
		t.Errorf("imageId = %q, want img-1", body.ImageID)
	}
	if w.deadline.IsZero() {
		t.Fatal("no read deadline was set: a body may arrive as slowly as it likes")
	}
	if got := time.Until(w.deadline).Round(time.Second); got != bodyReadTimeout {
		t.Errorf("deadline is %v away, want %v", got, bodyReadTimeout)
	}

	// The size bound is still the one the caller asked for.
	big := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(`{"imageId":"`+strings.Repeat("x", 64)+`"}`))
	if err := decodeJSON(&deadlineWriter{ResponseWriter: httptest.NewRecorder()}, big, 8, &body); err == nil {
		t.Error("decodeJSON accepted a body over the limit")
	}
}

// A response writer with no deadline to set is not a reason to refuse the
// request: the size bound still applies, and there is no connection to hold.
func TestDecodeJSONWithoutADeadlineToSet(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(`{"imageId":"img-1"}`))
	var body struct {
		ImageID string `json:"imageId"`
	}
	if err := decodeJSON(httptest.NewRecorder(), r, 1024, &body); err != nil {
		t.Fatalf("decodeJSON: %v", err)
	}
	if body.ImageID != "img-1" {
		t.Errorf("imageId = %q, want img-1", body.ImageID)
	}
}
