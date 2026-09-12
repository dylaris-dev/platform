package nodegrpc

import (
	"net"
	"testing"
	"time"
)

// startTestGRPCServer starts a server with the collaborators nilled out. None of
// them is touched until a node connects, and no test here connects one.
func startTestGRPCServer(t *testing.T, port int) (stop func(), err error) {
	t.Helper()
	srv, err := StartGRPCServer(port, NewRegistry(), nil, "core-test", nil, nil, nil, false, "", "")
	if err != nil {
		return nil, err
	}
	return srv.Stop, nil
}

// TestStartGRPCServerReturnsAStoppableServer covers the reason the signature
// returns the server at all: without a handle, main could not drain node
// streams on shutdown and every one of them was severed by process exit.
// Port 0 lets the kernel pick one and hand it over in the same syscall. This
// used to reserve a port, close it, and hope nothing took it in between - a
// window the helper's own comment dismissed as "not a practical source of
// flakes", which CI then disproved on a busy runner. No test here needs a
// chosen port.
func TestStartGRPCServerReturnsAStoppableServer(t *testing.T) {
	srv, err := StartGRPCServer(0, NewRegistry(), nil, "core-test", nil, nil, nil, false, "", "")
	if err != nil {
		t.Fatalf("StartGRPCServer: %v", err)
	}
	if srv == nil {
		t.Fatal("StartGRPCServer returned a nil server with a nil error")
	}

	// With no streams open this returns promptly. The timeout is here so a
	// regression that makes it block fails the test instead of hanging it.
	stopped := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		srv.Stop()
		t.Fatal("GracefulStop did not return within 10s on an idle server")
	}
}

// TestStartGRPCServerReportsABindFailure pins the other half of the signature
// change. The bind used to happen inside a goroutine, so a port clash killed the
// process via log.Fatalf after boot had already continued past it; now it is a
// returned error at a defined point in the boot sequence.
//
// The occupant is a listener this test HOLDS, not a first server started on a
// port that freePort released a moment earlier. That older shape went red in CI
// on a busy runner - "first StartGRPCServer: bind: address already in use" -
// because reserve-then-close leaves a window in which anything on the machine,
// including another package of this same test binary, can take the port. The
// test then failed on its own setup and said nothing about the behaviour it
// exists to pin. Holding the port has no window at all.
//
// It binds ":port" exactly as StartGRPCServer does, so the clash is the same
// one on every platform rather than relying on how an OS treats a wildcard bind
// against a loopback-only holder.
func TestStartGRPCServerReportsABindFailure(t *testing.T) {
	occupant, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("occupy a port: %v", err)
	}
	defer occupant.Close()
	port := occupant.Addr().(*net.TCPAddr).Port

	if _, err := startTestGRPCServer(t, port); err == nil {
		t.Fatal("StartGRPCServer on an occupied port = nil error, want a bind failure")
	}
}
