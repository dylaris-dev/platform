package settings

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/store"
)

type dataBudgetFakeStore struct {
	store.Store
	saved []string
}

func (f *dataBudgetFakeStore) SetUserDataBudget(_, raw string) error {
	f.saved = append(f.saved, raw)
	return nil
}

func TestDataBudgetSetMine(t *testing.T) {
	cases := []struct {
		body      string
		wantCode  int
		wantSaved string // "" = nothing stored
	}{
		{`{"budget_gb":null}`, 200, "unlimited"},
		{`{"budget_gb":0}`, 200, "0"},
		{`{"budget_gb":1000}`, 200, "1000"},
		{`{"budget_gb":1000000}`, 200, "1000000"},
		{`{"budget_gb":1000001}`, 400, ""},
		{`{"budget_gb":-1}`, 400, ""},
		{`{"budget_gb":9223372036854775807}`, 400, ""},
	}
	for _, c := range cases {
		t.Run(c.body, func(t *testing.T) {
			fs := &dataBudgetFakeStore{}
			h := NewDataBudgetHandler(Deps{Store: fs})
			r := httptest.NewRequest("PUT", "/api/me/data-budget", strings.NewReader(c.body))
			r = r.WithContext(context.WithValue(r.Context(), "userID", "u1"))
			rec := httptest.NewRecorder()
			h.SetMine(rec, r)
			if rec.Code != c.wantCode {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, c.wantCode, rec.Body.String())
			}
			got := ""
			if len(fs.saved) > 0 {
				got = fs.saved[0]
			}
			if got != c.wantSaved {
				t.Fatalf("stored %q, want %q", got, c.wantSaved)
			}
		})
	}
}
