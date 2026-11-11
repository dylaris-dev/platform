package services

import (
	"time"

	"dylaris-core/store"
)

// The owner's self-set data budget (managed-gateway beam plan, Task 7): a
// server owner keeps part of their data allowance for players by capping their
// own beam transfers, e.g. 1 TB of 5 TB. Past it every transfer needs an
// explicit confirmation from the owner or a panel admin; nobody else may
// transfer. It only ever ADDS a gate: the store allowance, the data throttle
// and the managed pool stay in force whatever the budget says.

// Codes the beam ticket refusal carries, so the app branches on a word and not
// on the prose.
const (
	DataBudgetReachedCode     = "data_budget_reached"
	DataBudgetAckRequiredCode = "data_budget_ack_required"
)

// DataBudgetMaxGB is the largest budget the panel may store (1 PB); it keeps
// the GB-to-bytes product far from int64 overflow.
const DataBudgetMaxGB int64 = 1_000_000

// DataBudget is one owner's budget state this month. BudgetGB nil = no budget.
type DataBudget struct {
	BudgetGB  *int64 `json:"budget_gb"`
	UsedGB    int64  `json:"used_gb"`
	UsedBytes int64  `json:"-"`
	Reached   bool   `json:"reached"`
}

// JudgeDataBudget is the pure decision. A budget of 0 is reached at 0 bytes, so
// every transfer needs the confirmation.
func JudgeDataBudget(usedBytes int64, budgetGB *int64) DataBudget {
	b := DataBudget{BudgetGB: budgetGB, UsedGB: usedBytes / 1_000_000_000, UsedBytes: usedBytes}
	b.Reached = budgetGB != nil && usedBytes >= *budgetGB*1_000_000_000
	return b
}

// DataBudgetUsageBytes is where the budget's usage comes from: this Core's own
// monthly data traffic for the owner. It is the one seam for remote mode (a
// self-hosted Core asking the provider usage endpoint), which is not built yet.
func DataBudgetUsageBytes(st store.Store, ownerID string, now time.Time) (int64, error) {
	return DataTrafficUsedBytes(st, ownerID, now)
}

// DataBudgetFor reads an owner's budget and usage. No budget skips the usage
// read entirely.
func DataBudgetFor(st store.Store, ownerID string, now time.Time) (DataBudget, error) {
	raw, err := st.GetUserDataBudget(ownerID)
	if err != nil {
		return DataBudget{}, err
	}
	budget := ParseLimitSetting(raw, nil)
	if budget == nil {
		return DataBudget{}, nil
	}
	if *budget > DataBudgetMaxGB {
		budget = LimitPtr(DataBudgetMaxGB) // a hand-edited row past the cap
	}
	used, err := DataBudgetUsageBytes(st, ownerID, now)
	if err != nil {
		return DataBudget{}, err
	}
	return JudgeDataBudget(used, budget), nil
}

// DataBudgetGate decides a beam ticket against the budget: "" lets it through,
// otherwise the refusal code. Only the owner or a panel admin (privileged) may
// go past a reached budget, and only with the explicit ack.
func DataBudgetGate(b DataBudget, privileged, ack bool) string {
	switch {
	case !b.Reached:
		return ""
	case !privileged:
		return DataBudgetReachedCode
	case !ack:
		return DataBudgetAckRequiredCode
	}
	return ""
}
