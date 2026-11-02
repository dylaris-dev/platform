package httpx

import (
	"context"
	"io"
	"net/http"
	"time"
)

// sseMaxStreamAge ends every stream after this long. The browser reconnects on
// its own, and the reconnect goes through authentication and the capability
// check again. A stream was checked once, when it opened, and then ran until
// the client left - through a revoked permission, a removal from the server,
// a password change, for days.
//
// Shorter than sseTicketTTL on purpose. The ticket slides forward when a
// stream opens, not while it runs, so a stream that lived exactly as long as
// the ticket reconnected with a ticket that had just expired: the reconnect
// was refused, and the console sat dead until the page minted a new one.
var sseMaxStreamAge = 4 * time.Minute

// SSERetryLine tells the browser to reconnect after one second instead of its
// default of about three, so the rotation above is a blink, not a gap.
const SSERetryLine = "retry: 1000\n\n"

// StreamContext bounds one stream's lifetime and opens it with the retry
// line. Every stream handler calls it once, after its last refusal (a refusal
// is a JSON error, which a line written before it would turn into a 200) and
// at the start of what it streams. Not flushed here: it rides out with the
// stream's first flush, or with the end of the response, which is the only
// moment the browser needs it - and a flush of its own would add a write per
// stream.
func StreamContext(w http.ResponseWriter, r *http.Request) (context.Context, context.CancelFunc) {
	io.WriteString(w, SSERetryLine)
	return context.WithTimeout(r.Context(), sseMaxStreamAge)
}

// SSEMaxStreamAge is the current stream lifetime, for the tests that compare
// it with the SSE ticket's TTL.
func SSEMaxStreamAge() time.Duration { return sseMaxStreamAge }

// SetSSEMaxStreamAge changes how long an SSE stream lives before it ends and
// the client reconnects, and returns the function that restores the previous
// value. For tests: production never changes it.
func SetSSEMaxStreamAge(d time.Duration) (restore func()) {
	prev := sseMaxStreamAge
	sseMaxStreamAge = d
	return func() { sseMaxStreamAge = prev }
}
