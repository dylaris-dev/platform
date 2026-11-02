package billing

import (
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// Copies of the handlers package's test helpers and fakes that the billing
// tests used while they lived there. Test files cannot be imported across
// packages; each says where the original is.

// termsStore: handlers/staff_bounds_test.go.
type termsStore struct {
	store.Store
	target *models.User
}

func (f *termsStore) GetUserByID(string) (*models.User, error) { return f.target, nil }
func (f *termsStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{Grant: []string{"plans.write"}}, nil
}
func (f *termsStore) GetPanelRole(int) (*store.PanelRole, error) { return nil, nil }

// voicePoolStore moved here from handlers/voice_stats_test.go; billing is its
// only user now.
type voicePoolStore struct {
	store.Store
	cells []store.RegionUsage
}

func (f *voicePoolStore) GetTrafficUsageRegions(string, time.Time) ([]store.RegionUsage, error) {
	return f.cells, nil
}
func (f *voicePoolStore) GetTrafficLimit(scope, region, kind string) (*models.TrafficLimit, error) {
	if scope != services.TrafficScopeDefault {
		return nil, nil
	}
	gb := int64(100)
	return &models.TrafficLimit{IncludedGB: &gb}, nil
}
func (f *voicePoolStore) GetUserDataBillingEnabled(string) (bool, error) { return false, nil }
func (f *voicePoolStore) GetSetting(string) (string, error)              { return "", nil }
