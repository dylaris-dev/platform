package httpx

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCapBody_RefusesFromContentLengthWithoutReading is the property the ticket
// attachment upload was missing, found by uploading a 13MB file against a 10MB
// limit with curl: 60 seconds and no reply, versus 50ms for the identical
// request with Expect disabled.
//
// The client had sent "Expect: 100-continue" (curl does for any body over 1KB).
// Go emits that 100 Continue the first time the handler reads the body, so a
// bare MaxBytesReader means: read, send 100, client streams megabytes, reader
// trips, handler writes 413 - into a socket the client is not reading from
// because it is still blocked writing. Refusing before the first read makes Go
// send the final status instead of the go-ahead.
func TestCapBody_RefusesFromContentLengthWithoutReading(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("body"))
	req.ContentLength = 5000

	if CapBody(rec, req, 1000) {
		t.Fatal("CapBody accepted a request whose declared Content-Length is over the limit")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	// Nothing may have been consumed: reading is what triggers the 100 Continue.
	if n := req.ContentLength; n != 5000 {
		t.Errorf("ContentLength = %d, want it untouched", n)
	}
}

// TestCapBody_StillCapsWhenContentLengthIsAbsentOrLies: Content-Length is a
// claim, not a fact. A chunked body has none (-1), and a header can understate
// the payload, so the reader has to stay.
func TestCapBody_StillCapsWhenContentLengthIsAbsentOrLies(t *testing.T) {
	for _, tc := range []struct {
		name     string
		declared int64
	}{
		{"chunked, no Content-Length", -1},
		{"header understates the body", 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			body := strings.Repeat("x", 5000)
			req := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader(body))
			req.ContentLength = tc.declared

			if !CapBody(rec, req, 1000) {
				t.Fatal("CapBody refused up front on a Content-Length that does not exceed the limit")
			}
			buf := make([]byte, 5000)
			var total int
			var readErr error
			for {
				n, err := req.Body.Read(buf)
				total += n
				if err != nil {
					readErr = err
					break
				}
			}
			// The read must stop at the cap, and stop because of the cap: io.EOF
			// here would mean the whole 5000 bytes were handed over.
			var maxErr *http.MaxBytesError
			if !errors.As(readErr, &maxErr) {
				t.Fatalf("read %d bytes and ended with %v; want a MaxBytesError at the cap", total, readErr)
			}
			if total > 1000 {
				t.Errorf("read %d bytes past a 1000-byte cap", total)
			}
		})
	}
}

// TestCapBody_AllowsWhatFits: the guard must not refuse a legitimate upload that
// declares a size within the limit.
func TestCapBody_AllowsWhatFits(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("small"))
	req.ContentLength = 5

	if !CapBody(rec, req, 1000) {
		t.Fatalf("CapBody refused a 5-byte body against a 1000-byte limit (status %d)", rec.Code)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("wrote status %d before the handler ran", rec.Code)
	}
}
