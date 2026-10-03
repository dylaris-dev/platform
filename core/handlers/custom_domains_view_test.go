package handlers

import (
	"testing"

	"dylaris-core/store"
)

// The TXT record is the proof for every claim now, so the panel has to show it
// from the moment the claim exists - not only after two failed deadlines, which
// is when it used to appear.
func TestTheProofRecordIsShownUntilItIsProven(t *testing.T) {
	for _, state := range []string{store.ClaimPending, store.ClaimBlocked, store.ClaimPermablocked} {
		v := viewOf(store.CustomDomainClaim{Domain: "mc.example.com", State: state, TXTToken: "dylaris-verify=abc"})
		if v.TXTName != "_dylaris-verify.mc.example.com" || v.TXTValue != "dylaris-verify=abc" {
			t.Errorf("%s: record not shown: %+v", state, v)
		}
	}
	v := viewOf(store.CustomDomainClaim{Domain: "mc.example.com", State: store.ClaimVerified, TXTToken: "dylaris-verify=abc"})
	if v.TXTValue != "" {
		t.Errorf("a verified claim still asks for its record: %+v", v)
	}
}
