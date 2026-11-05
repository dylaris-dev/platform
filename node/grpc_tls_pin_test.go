package main

import (
	"strings"
	"testing"
)

// A hand-mangled GRPC_TLS_FINGERPRINT used to reach main's fp[:16] unchecked:
// a short value panicked with a stack trace instead of the FATAL that names the
// fix, and an uppercase or colon-separated one passed boot and then never
// matched the lowercase hex VerifyConnection compares against.
func TestResolveCorePinValidatesTheFingerprint(t *testing.T) {
	const good = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	colons := ""
	for i := 0; i < len(good); i += 2 {
		if i > 0 {
			colons += ":"
		}
		colons += strings.ToUpper(good[i : i+2])
	}
	cases := []struct {
		name, in, want string
		wantErr        bool
	}{
		{"lowercase hex", good, good, false},
		{"uppercase hex", strings.ToUpper(good), good, false},
		{"colon-separated", colons, good, false},
		{"truncated", "ab12cd34", "", true},
		{"one char short", good[:63], "", true},
		{"not hex", strings.Repeat("zz", 32), "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			oldSecret, oldFP := clusterSecret, grpcTLSFingerprint
			t.Cleanup(func() { clusterSecret, grpcTLSFingerprint = oldSecret, oldFP })
			clusterSecret, grpcTLSFingerprint = "", c.in

			_, got, err := resolveCorePin()
			if c.wantErr {
				if err == nil {
					t.Fatalf("accepted %q as a pin", c.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveCorePin: %v", err)
			}
			if got != c.want {
				t.Errorf("pin = %q, want %q", got, c.want)
			}
		})
	}
}
