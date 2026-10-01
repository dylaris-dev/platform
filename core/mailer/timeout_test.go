package mailer

import (
	"net"
	"testing"
	"time"
)

// A relay that accepts the connection and then says nothing used to hold the
// caller forever: no dial timeout, no deadline. The callers include a password
// reset and the billing pass that suspends accounts.
func TestSendGivesUpOnASilentRelay(t *testing.T) {
	prev := smtpSessionTimeout
	smtpSessionTimeout = 300 * time.Millisecond
	t.Cleanup(func() { smtpSessionTimeout = prev })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // accept, then never send the greeting
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	done := make(chan error, 1)
	go func() {
		done <- Send(&SMTPConfig{Host: "127.0.0.1", Port: port, Encryption: "none", FromEmail: "a@example.test"},
			Message{To: "b@example.test", Subject: "s", Body: "b"})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a send to a silent relay reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a send to a silent relay did not give up")
	}
}
