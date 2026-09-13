package database

import (
	"testing"
	"time"

	"dylaris-core/models"
)

// A Core restart must not change what an account is allowed to do.
//
// Boot used to carry a one-time grandfathering backfill that ran on EVERY boot:
// it marked every account still waiting on its verification mail as verified,
// and gave all regions to every user without region rows - which is exactly
// what a self-registered user looks like under a "no regions until an admin
// grants them" policy. So a deploy opened the login gate and the region fence
// for everyone who was meant to be outside them.
//
// The admin half is asserted too, because those accounts had quietly relied on
// that backfill: no verification mail is ever sent for them, so without it they
// would be locked out for good once verification is required.
func TestIntegrationABootLeavesVerificationAndRegionsAlone(t *testing.T) {
	db, st := integrationDB(t)

	waiting := &models.User{Username: uniqueName("unverified_"), Password: "x", Email: uniqueName("u") + "@example.test"}
	if err := st.CreateUser(waiting); err != nil {
		t.Fatalf("CreateUser (self-registered): %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id = $1`, waiting.ID) })
	// The registration flow under a "no regions" default.
	if err := st.SetUserRegions(waiting.ID, false, []string{}); err != nil {
		t.Fatalf("SetUserRegions: %v", err)
	}

	verifiedAt := time.Now()
	vouched := &models.User{Username: uniqueName("vouched_"), Password: "x", EmailVerifiedAt: &verifiedAt}
	if err := st.CreateUser(vouched); err != nil {
		t.Fatalf("CreateUser (admin-created): %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE id = $1`, vouched.ID) })

	if err := EnsureSchema(db, false); err != nil {
		t.Fatalf("second boot: %v", err)
	}

	got, err := st.GetUserByID(waiting.ID)
	if err != nil || got == nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if got.EmailVerifiedAt != nil {
		t.Errorf("a restart verified an account that never confirmed its address (verified at %v)", got.EmailVerifiedAt)
	}
	all, err := st.GetUserAllRegionsAccess(waiting.ID)
	if err != nil {
		t.Fatalf("GetUserAllRegionsAccess: %v", err)
	}
	if all {
		t.Error("a restart gave all regions to a user the policy had given none")
	}

	got, err = st.GetUserByID(vouched.ID)
	if err != nil || got == nil {
		t.Fatalf("GetUserByID (admin-created): %v", err)
	}
	if got.EmailVerifiedAt == nil {
		t.Error("an admin-created account was stored unverified; it gets no verification mail and cannot log in once verification is required")
	}
}
