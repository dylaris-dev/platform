package settings

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"dylaris-core/handlers/httpx"
	"dylaris-core/services"
)

// DataBudgetHandler serves the caller's own self-set data budget: how much of
// their data allowance beam may use before every transfer asks first.
type DataBudgetHandler struct{ d Deps }

func NewDataBudgetHandler(d Deps) *DataBudgetHandler { return &DataBudgetHandler{d: d} }

// GetMine GET /api/me/data-budget - {budget_gb, used_gb, reached, allowance_gb}.
// allowance_gb is the plan's data allowance, null when the account has none
// (a self-hosted install, or an account that is not metered).
func (h *DataBudgetHandler) GetMine(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	now := time.Now()
	b, err := services.DataBudgetFor(h.d.Store, userID, now)
	if err != nil {
		httpx.SendJSONError(w, "Could not load your data budget", http.StatusInternalServerError)
		return
	}
	if b.BudgetGB == nil {
		// No budget skips the usage read; the page still shows the usage.
		if used, uerr := services.DataBudgetUsageBytes(h.d.Store, userID, now); uerr == nil {
			b.UsedGB = used / 1_000_000_000
		}
	}
	var allowance *int64
	if v, verr := services.DataTrafficVerdictFor(h.d.Store, userID, now); verr == nil && v.Limited {
		allowance = &v.IncludedGB
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":      true,
		"budget_gb":    b.BudgetGB,
		"used_gb":      b.UsedGB,
		"reached":      b.Reached,
		"allowance_gb": allowance,
	})
}

// SetMine PUT /api/me/data-budget - body {budget_gb: n | null}; null removes
// the budget, 0 makes every transfer ask.
func (h *DataBudgetHandler) SetMine(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	var req struct {
		BudgetGB *int64 `json:"budget_gb"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if req.BudgetGB != nil && *req.BudgetGB < 0 {
		httpx.SendJSONError(w, "budget_gb must be 0 or more, or null for no budget", http.StatusBadRequest)
		return
	}
	if err := h.d.Store.SetUserDataBudget(userID, services.FormatLimitSetting(req.BudgetGB)); err != nil {
		httpx.SendJSONError(w, "Could not save your data budget", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "budget_gb": req.BudgetGB})
}
