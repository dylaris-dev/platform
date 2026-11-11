package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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

// A refused ticket must not leave the previous server's tunnel installed:
// file ops for the new server would otherwise run against the old one.
func TestRefusedTicketDropsThePreviousTunnel(t *testing.T) {
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"success":false,"code":"data_budget_ack_required","message":"budget"}`))
	}))
	defer core.Close()
	c, err := NewCoreClient(core.URL)
	if err != nil {
		t.Fatal(err)
	}
	c.token = "t"
	a := &App{}
	a.setClient(c)
	a.setRelay(&BeamNodeClient{ServerUUID: "srv-a"})
	a.setConnMode("relay")

	if err := a.ConnectToServer("srv-b"); err == nil || !strings.Contains(err.Error(), "data_budget_ack_required") {
		t.Fatalf("err = %v, want the ticket refusal", err)
	}
	if a.getRelay() != nil || a.GetConnectionMode() != "" {
		t.Fatal("the tunnel to srv-a survived a refused switch to srv-b")
	}
}

func TestRelayForOnlyServesItsOwnServer(t *testing.T) {
	a := &App{}
	rc := &BeamNodeClient{ServerUUID: "srv-a"}
	a.setRelay(rc)
	if a.relayFor("srv-a") != rc || a.relayFor("") != rc {
		t.Fatal("the tunnel must serve its own server and an unnamed request")
	}
	if a.relayFor("srv-b") != nil {
		t.Fatal("the tunnel to srv-a served a request for srv-b")
	}
}

// One confirmation covers one transfer and its retries, not the next one.
func TestBudgetAckEndsWithTheTransfer(t *testing.T) {
	for name, end := range map[string]func(a *App){
		"upload finish":      func(a *App) { _ = a.BeamUploadFinish("u1") },
		"upload cancel":      func(a *App) { a.BeamUploadCancel("u1") },
		"download":           func(a *App) { _ = a.DownloadFile("p", "srv-1", false) },
		"selective download": func(a *App) { _ = a.SelectiveDownload("p", nil, true, "srv-1") },
	} {
		t.Run(name, func(t *testing.T) {
			a := &App{uploads: map[string]*UploadSession{}}
			a.budgetAcked = "srv-1"
			end(a)
			if a.budgetAckFor("srv-1") {
				t.Fatal("the ack outlived its transfer")
			}
		})
	}
}
