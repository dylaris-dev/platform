package services

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/lib/pq"
)

// The whole point of the two-step test is that these two outcomes are told
// apart, so that is what the tests check: not the wording, but which stage a
// failure is attributed to. Getting that backwards sends an operator to fix the
// wrong thing, which is the bug this file exists to prevent.

func TestReachableSaysWhichKindOfFailure(t *testing.T) {
	// A listener that exists, on a port the OS picked: reachable.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	host, port, _ := net.SplitHostPort(ln.Addr().String())

	if got := Reachable(context.Background(), host, port); !got.OK {
		t.Fatalf("a live listener should be reachable, got %+v", got)
	}

	// The same host with nothing listening. Closing a listener frees the port,
	// so this is a refusal rather than a timeout on every platform that answers
	// RST - and a refusal is still "unreachable" as far as an operator's next
	// step is concerned.
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, deadPort, _ := net.SplitHostPort(ln2.Addr().String())
	ln2.Close()

	got := Reachable(context.Background(), host, deadPort)
	if got.OK || got.Stage != StageUnreachable {
		t.Fatalf("a closed port must be unreachable, got %+v", got)
	}
	if strings.Contains(strings.ToLower(got.Message), "password") {
		t.Errorf("an unreachable host must not talk about credentials: %q", got.Message)
	}

	// An empty host is the form's problem, not the network's, and it must not
	// spend the dial budget finding that out.
	if got := Reachable(context.Background(), "", "5432"); got.OK {
		t.Error("an empty host cannot be reachable")
	}
}

func TestDescribePostgresErrorNamesTheFix(t *testing.T) {
	cases := []struct {
		code, wants string
	}{
		{"28P01", "password"},
		{"3D000", "does not exist"},
		{"42501", "not allowed"},
		{"53300", "max_connections"},
	}
	for _, c := range cases {
		msg := DescribePostgresError(&pq.Error{Code: pq.ErrorCode(c.code), Message: "raw driver text"})
		if !strings.Contains(strings.ToLower(msg), strings.ToLower(c.wants)) {
			t.Errorf("SQLSTATE %s: expected the message to mention %q, got %q", c.code, c.wants, msg)
		}
	}

	// A wrong SSL mode is the one non-SQLSTATE failure worth translating: the
	// driver's own wording sends people to the credentials.
	if msg := DescribePostgresError(errors.New("pq: SSL is not enabled on the server")); !strings.Contains(msg, "SSL mode") {
		t.Errorf("an SSL mismatch should name the setting to change, got %q", msg)
	}

	if DescribePostgresError(nil) != "" {
		t.Error("no error means no message")
	}
}

func TestClassifyStorageFailureSplitsNetworkFromCredentials(t *testing.T) {
	cases := []struct {
		msg   string
		stage string
	}{
		{"Write failed: dial tcp: lookup s3.example: no such host", StageUnreachable},
		{"Write failed: dial tcp 10.0.0.9:443: connect: connection refused", StageUnreachable},
		{"Write failed: context deadline exceeded", StageUnreachable},
		{"Write failed: InvalidAccessKeyId: the key is not valid", StageRejected},
		{"Write failed: NoSuchBucket: the specified bucket does not exist", StageRejected},
		{"Write failed: AccessDenied", StageRejected},
	}
	for _, c := range cases {
		got := ClassifyStorageFailure(c.msg)
		if got.Stage != c.stage {
			t.Errorf("%q: expected stage %s, got %s (%q)", c.msg, c.stage, got.Stage, got.Message)
		}
		if got.OK {
			t.Errorf("%q: a failure is never OK", c.msg)
		}
	}
}

func TestHostPortExtraction(t *testing.T) {
	endpoints := []struct{ in, host, port string }{
		{"https://s3.eu-central-1.amazonaws.com", "s3.eu-central-1.amazonaws.com", "443"},
		{"http://minio:9000", "minio", "9000"},
		{"minio.example.com", "minio.example.com", "443"},
		{"http://minio.example.com/", "minio.example.com", "80"},
	}
	for _, e := range endpoints {
		h, p, ok := HostPortFromEndpoint(e.in)
		if !ok || h != e.host || p != e.port {
			t.Errorf("%q: got %s:%s (ok=%v), want %s:%s", e.in, h, p, ok, e.host, e.port)
		}
	}
	if _, _, ok := HostPortFromEndpoint("  "); ok {
		t.Error("an empty endpoint has no host")
	}

	dsns := []struct{ in, host, port string }{
		{"postgres://u:p@db.example:6432/things?sslmode=disable", "db.example", "6432"},
		{"postgres://u:p@db.example/things", "db.example", "5432"},
		{"host=metricsdb port=5432 user=metrics dbname=m sslmode=disable", "metricsdb", "5432"},
		{"host=metricsdb user=metrics", "metricsdb", "5432"},
	}
	for _, d := range dsns {
		h, p, ok := HostPortFromPostgresDSN(d.in)
		if !ok || h != d.host || p != d.port {
			t.Errorf("%q: got %s:%s (ok=%v), want %s:%s", d.in, h, p, ok, d.host, d.port)
		}
	}
	// A DSN with no host is not an error: the reachability step is skipped and
	// the driver gets to speak first.
	if _, _, ok := HostPortFromPostgresDSN("dbname=onlythis"); ok {
		t.Error("a DSN without a host should report that it has none")
	}
}
