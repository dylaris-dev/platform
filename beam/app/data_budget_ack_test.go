package main

import "testing"

// The ack rides on the ticket request only when the user confirmed, and the
// app's own reconnects repeat it only for the server that was confirmed.
func TestBeamTicketPathAck(t *testing.T) {
	cases := []struct {
		ack  bool
		want string
	}{
		{false, "/beam/ticket?server_uuid=a%2Fb"},
		{true, "/beam/ticket?server_uuid=a%2Fb&ack_over_budget=true"},
	}
	for _, c := range cases {
		if got := beamTicketPath("a/b", c.ack); got != c.want {
			t.Fatalf("ack=%v: %q, want %q", c.ack, got, c.want)
		}
	}
}

func TestBudgetAckIsPerServer(t *testing.T) {
	a := &App{}
	a.budgetAcked = "srv-1"
	if !a.budgetAckFor("srv-1") || a.budgetAckFor("srv-2") || a.budgetAckFor("") {
		t.Fatal("ack must hold for the confirmed server only")
	}
	// A plain connect to another server drops the ack (ConnectToServer clears
	// it before dialing; not logged in fails the dial afterwards).
	_ = a.ConnectToServer("srv-2")
	if a.budgetAckFor("srv-1") {
		t.Fatal("connecting to another server must clear the ack")
	}
}
