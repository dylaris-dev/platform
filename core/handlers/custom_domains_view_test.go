package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"dylaris-core/services"
	"dylaris-core/store"
)

// The TXT record is the proof for every claim, so the panel has to show it from
// the moment the claim exists - not only after two failed deadlines, which is
// when it used to appear. And after verification too: the claim is re-checked,
// so a customer who cannot see the record cannot keep it published.
func TestTheProofRecordIsAlwaysShown(t *testing.T) {
	for _, state := range []string{store.ClaimPending, store.ClaimBlocked, store.ClaimPermablocked, store.ClaimVerified} {
		v := viewOf(store.CustomDomainClaim{Domain: "mc.example.com", State: state, TXTToken: "dylaris-verify=abc"})
		if v.TXTName != "_dylaris-verify.mc.example.com" || v.TXTValue != "dylaris-verify=abc" {
			t.Errorf("%s: record not shown: %+v", state, v)
		}
	}
}

// A verified claim whose record went missing says when its routes go.
func TestAFailingVerifiedClaimShowsWhenItLapses(t *testing.T) {
	since := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	v := viewOf(store.CustomDomainClaim{Domain: "mc.example.com", State: store.ClaimVerified,
		TXTToken: "dylaris-verify=abc", FailingSince: &since})
	if v.LapsesAt == nil || !v.LapsesAt.Equal(since.Add(services.CustomDomainLapseGrace)) {
		t.Fatalf("lapsesAt = %v, want %v", v.LapsesAt, since.Add(services.CustomDomainLapseGrace))
	}
	if v := viewOf(store.CustomDomainClaim{Domain: "mc.example.com", State: store.ClaimVerified}); v.LapsesAt != nil {
		t.Fatalf("a healthy claim shows a lapse date: %v", v.LapsesAt)
	}
}

type verifyFakeStore struct {
	claimFakeStore
	verified []int
}

func (f *verifyFakeStore) MarkCustomDomainVerified(id int) error {
	f.verified = append(f.verified, id)
	return nil
}

type txtResolver struct{ services.DomainResolver }

func (txtResolver) LookupTXT(context.Context, string) ([]string, error) {
	return []string{"dylaris-verify=abc"}, nil
}

// "Check now" ends a run of missed re-checks at once instead of at the next
// pass; it used to answer "already verified" to any verified claim.
func TestCheckNowEndsARunOfMisses(t *testing.T) {
	since := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name    string
		failing *time.Time
		want    int
	}{
		{"failing", &since, http.StatusOK},
		{"healthy", nil, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &verifyFakeStore{claimFakeStore: claimFakeStore{claim: &store.CustomDomainClaim{ID: 7,
				Domain: "mc.example.com", State: store.ClaimVerified, TXTToken: "dylaris-verify=abc", FailingSince: tc.failing}}}
			h := NewCustomDomainHandler(&AppState{Store: fs}, txtResolver{})
			r := mux.SetURLVars(httptest.NewRequest(http.MethodPost, "/", nil), map[string]string{"domain": "mc.example.com"})
			w := httptest.NewRecorder()
			h.VerifyTXT(w, r)
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.want, w.Body)
			}
			if (len(fs.verified) == 1) != (tc.want == http.StatusOK) {
				t.Fatalf("verified = %v", fs.verified)
			}
		})
	}
}
