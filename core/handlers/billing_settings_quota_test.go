package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// billingSettingsFakeStore serves the settings table from a map and answers the
// two calls the quota check makes. Everything else is the embedded nil
// interface, which panics - that is what keeps this test about the round trip.
type billingSettingsFakeStore struct {
	store.Store
	kv       map[string]string
	tenants  []store.UserBilling
	notified []models.Notification
}

func (f *billingSettingsFakeStore) ListUserBilling() ([]store.UserBilling, error) {
	return f.tenants, nil
}

func (f *billingSettingsFakeStore) InsertNotification(n *models.Notification) (int64, error) {
	f.notified = append(f.notified, *n)
	return int64(len(f.notified)), nil
}

// No tenant has their own traffic row in these tests; the backup path does not
// consult it at all, and this keeps the embedded nil interface out of the way.
func (f *billingSettingsFakeStore) GetTrafficLimit(string, string, string) (*models.TrafficLimit, error) {
	return nil, nil
}

func (f *billingSettingsFakeStore) GetSetting(key string) (string, error) { return f.kv[key], nil }

func (f *billingSettingsFakeStore) SetSetting(key, value string) error {
	if f.kv == nil {
		f.kv = map[string]string{}
	}
	f.kv[key] = value
	return nil
}

// A tenant who bought nothing, which is every user on a self-hosted install.
func (f *billingSettingsFakeStore) GetUserBilling(string) (*store.UserBilling, error) {
	return &store.UserBilling{}, nil
}

func (f *billingSettingsFakeStore) BackupBytesByOwner(string) (int64, error) { return 0, nil }

// Loading the billing screen and pressing Save must not change what the platform
// enforces.
//
// It used to: the GET answered "0" for an unset R2 quota and the PUT turned an
// empty field back into "0", so an operator who came to edit the payment URL
// stored a quota of NONE for every tenant - and the panel's own help text called
// that "no cap". The screen said one thing, the guard did the opposite, and
// nothing failed anywhere.
//
// Driven as the panel drives it: read the settings, send them back unchanged.
func TestBillingSettingsRoundTripDoesNotCapBackups(t *testing.T) {
	st := &billingSettingsFakeStore{kv: map[string]string{}}
	h := &BillingHandler{state: &AppState{Store: st}}

	// Before: nothing configured, so nothing is capped.
	if exceeded, _, _ := services.R2QuotaExceeded(st, "u1"); exceeded {
		t.Fatal("a fresh install already reports the backup quota as exceeded")
	}

	rec := httptest.NewRecorder()
	h.GetBillingSettings(rec, httptest.NewRequest(http.MethodGet, "/api/admin/settings/billing", nil))
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	if q, _ := got["r2QuotaGb"].(string); q != "" {
		t.Errorf("unset quota reads back as %q, want \"\" - an unset limit is not a limit of zero", q)
	}

	body, _ := json.Marshal(got)
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/admin/settings/billing", bytes.NewReader(body))
	h.SetBillingSettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("save returned %d: %s", rec.Code, rec.Body.String())
	}

	// After: still nothing capped. This is the assertion that was false.
	if exceeded, _, quota := services.R2QuotaExceeded(st, "u1"); exceeded {
		t.Errorf("saving the screen unchanged capped backups at %d bytes for a tenant storing none", quota)
	}
}

// The three states an operator can express have to survive a write and a read.
func TestBillingSettingsQuotaStates(t *testing.T) {
	tests := []struct {
		name     string
		typed    string
		want     string // what lands in the settings table
		exceeded bool   // for a tenant storing zero bytes
	}{
		{name: "empty is unset", typed: "", want: "", exceeded: false},
		{name: "unlimited is a decided no-cap", typed: services.LimitUnlimited, want: services.LimitUnlimited, exceeded: false},
		// Zero is a real cap of none, and at zero bytes stored the tenant is
		// already at it: the create gate answers at-or-over.
		{name: "zero is a cap of none", typed: "0", want: "0", exceeded: true},
		{name: "a number is that cap", typed: "250", want: "250", exceeded: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &billingSettingsFakeStore{kv: map[string]string{}}
			h := &BillingHandler{state: &AppState{Store: st}}
			body := billingSettingsBody(map[string]string{"r2QuotaGb": tt.typed})
			rec := httptest.NewRecorder()
			h.SetBillingSettings(rec, httptest.NewRequest(http.MethodPut, "/x", bytes.NewReader(body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("save returned %d: %s", rec.Code, rec.Body.String())
			}
			if got := st.kv[services.BillingR2QuotaKey]; got != tt.want {
				t.Errorf("stored %q, want %q", got, tt.want)
			}
			if exceeded, _, _ := services.R2QuotaExceeded(st, "u1"); exceeded != tt.exceeded {
				t.Errorf("exceeded = %v, want %v", exceeded, tt.exceeded)
			}
		})
	}
}

// The included and bookable allowances are editable at all. They are read on
// every backup and on the customer's consent screen, and until this existed the
// only way to change either was an UPDATE against the settings table.
func TestBillingSettingsCarryTheBackupAllowances(t *testing.T) {
	units := int64(2)
	st := &billingSettingsFakeStore{
		kv: map[string]string{},
		// One tenant who agreed to be charged for backup storage, so lowering
		// the bookable amount has somebody to tell.
		tenants: []store.UserBilling{{UserID: "u1", MaxNodes: &units, BackupBillingEnabled: true}},
	}
	h := &BillingHandler{state: &AppState{Store: st}}

	rec := httptest.NewRecorder()
	h.GetBillingSettings(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	// Unset reads back as the built-in default rather than empty: these are not
	// tri-state limits, and a blank field would look like "none included".
	if got["r2IncludedGb"] != "50" || got["r2BookableGb"] != "500" {
		t.Errorf("defaults = %v / %v, want \"50\" / \"500\"", got["r2IncludedGb"], got["r2BookableGb"])
	}

	body := billingSettingsBody(map[string]string{"r2IncludedGb": "100", "r2BookableGb": "0"})
	rec = httptest.NewRecorder()
	h.SetBillingSettings(rec, httptest.NewRequest(http.MethodPut, "/x", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("save returned %d: %s", rec.Code, rec.Body.String())
	}
	b := &store.UserBilling{MaxNodes: services.LimitPtr(2)}
	if inc := services.R2IncludedGB(st, b); inc != 200 {
		t.Errorf("included = %d, want 200 (100 per unit, two units)", inc)
	}
	// Zero bookable is a real answer - nothing is for sale on top - and must not
	// fall back to the built-in 500.
	if bk := services.R2BookableGB(st, b); bk != 0 {
		t.Errorf("bookable = %d, want 0", bk)
	}

	// Lowering it took away room a customer had already agreed to pay for, so
	// they hear about it on the same save.
	if len(st.notified) != 1 {
		t.Fatalf("wrote %d notifications, want 1 for the tenant with metered storage on", len(st.notified))
	}
	if st.notified[0].UserID != "u1" || st.notified[0].Type != services.NotifyTypeBookableChanged {
		t.Errorf("notification = %+v", st.notified[0])
	}
}

// billingSettingsBody is a complete, valid payload with the named fields
// overridden. Complete because the handler validates every field, so a partial
// body would fail for reasons that have nothing to do with the test.
func billingSettingsBody(over map[string]string) []byte {
	body := map[string]string{
		"gracePeriod":       "3d",
		"r2Retention":       "3m",
		"nodeRetention":     "2w",
		"r2QuotaGb":         "",
		"r2IncludedGb":      "50",
		"r2BookableGb":      "500",
		"presignTtlNodeMin": "60",
		"presignTtlByonMin": "360",
		"paymentUrl":        "",
	}
	for k, v := range over {
		body[k] = v
	}
	b, _ := json.Marshal(body)
	return b
}

// The override modal renders "uses default" from this endpoint, so an unset
// platform quota must reach it as unset.
//
// It used to default to "0" here - the same defect the settings GET above had
// and had fixed - and the panel then labelled that "default (unlimited)". The
// two errors cancelled on screen and agreed on nothing: what the guard
// enforces for a tenant with no entitlement is a cap of NONE.
func TestUserBillingDefaultsDoNotInventAQuotaOfZero(t *testing.T) {
	for _, tt := range []struct {
		name, stored, want string
	}{
		{name: "unset stays unset", stored: "", want: ""},
		{name: "a real zero is reported as zero", stored: "0", want: "0"},
		{name: "a number is passed through", stored: "250", want: "250"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			kv := map[string]string{}
			if tt.stored != "" {
				kv[services.BillingR2QuotaKey] = tt.stored
			}
			st := &billingSettingsFakeStore{kv: kv}
			h := &BillingHandler{state: &AppState{Store: st}}

			rec := httptest.NewRecorder()
			h.GetUserBilling(rec, httptest.NewRequest(http.MethodGet, "/api/admin/users/u1/billing", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET returned %d: %s", rec.Code, rec.Body.String())
			}
			var got struct {
				Defaults struct {
					R2QuotaGb string `json:"r2QuotaGb"`
				} `json:"defaults"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Defaults.R2QuotaGb != tt.want {
				t.Errorf("defaults.r2QuotaGb = %q, want %q", got.Defaults.R2QuotaGb, tt.want)
			}
		})
	}
}
