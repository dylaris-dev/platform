package tickets

import "testing"

// The upload handler parses the multipart body BEFORE the per-file, per-ticket
// and per-user quota checks can run, and ParseMultipartForm reads the whole
// request - spilling everything past its memory budget to a temp file. So
// without a body cap, a caller holding attach rights writes an arbitrarily large
// file to Core's disk and is told "too large" only afterwards.
//
// The server file upload already caps its body (file.go), and LimitBody's own
// comment assumed every upload handler did: "the upload handlers set their own,
// much larger MaxBytesReader". This one did not.
func TestTicketUploadBodyLimit(t *testing.T) {
	const mb = 1024 * 1024
	mbp := func(v int64) *int64 { return &v }
	cases := []struct {
		name      string
		maxFileMB *int64
		want      int64
	}{
		{"the default per-file limit", mbp(10), 10*mb + ticketUploadEnvelopeSlack},
		{"a raised limit", mbp(200), 200*mb + ticketUploadEnvelopeSlack},
		{
			// No cap is about what an operator wants to ALLOW, not licence to
			// write unbounded data to disk before anything checks it.
			name:      "no cap still gets the hard cap",
			maxFileMB: nil, want: ticketUploadHardCapMB*mb + ticketUploadEnvelopeSlack,
		},
		{
			// The case that changed meaning. 0 is now "attachments are not
			// allowed", so there is nothing to read - it must NOT be promoted to
			// the hard cap, which is what treating it as "unset" used to do.
			name:      "a cap of none reads nothing but the envelope",
			maxFileMB: mbp(0), want: ticketUploadEnvelopeSlack,
		},
		{
			// LoadTicketSettings refuses to read a setting above 1024, but a value
			// that arrived another way must not raise the cap either.
			name:      "a value above the hard cap is clamped",
			maxFileMB: mbp(99999), want: ticketUploadHardCapMB*mb + ticketUploadEnvelopeSlack,
		},
		{"a negative value is treated as unset", mbp(-5), ticketUploadHardCapMB*mb + ticketUploadEnvelopeSlack},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ticketUploadBodyLimit(c.maxFileMB); got != c.want {
				t.Errorf("ticketUploadBodyLimit(%v) = %d, want %d", c.maxFileMB, got, c.want)
			}
		})
	}
}

// A file of EXACTLY the per-file limit has to reach the quota check that is meant
// to judge it, so the cap must leave room for the multipart envelope. Without the
// slack the body cap would reject a legitimate at-the-limit upload with a 413
// before the handler ever compared it against the limit.
func TestTicketUploadBodyLimitLeavesRoomForTheEnvelope(t *testing.T) {
	const mb = 1024 * 1024
	for _, maxFileMB := range []int64{1, 10, 200, 1024} {
		limit := ticketUploadBodyLimit(&maxFileMB)
		fileBytes := maxFileMB * mb
		if limit <= fileBytes {
			t.Errorf("limit %d for a %d MB file leaves no room for the multipart envelope", limit, maxFileMB)
		}
	}
}
