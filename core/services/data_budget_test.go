package services

import "testing"

func TestJudgeDataBudget(t *testing.T) {
	gb := func(n int64) *int64 { return &n }
	cases := []struct {
		name    string
		used    int64
		budget  *int64
		reached bool
	}{
		{"no budget never reaches", 50_000_000_000_000, nil, false},
		{"zero budget is reached at zero", 0, gb(0), true},
		{"below", 999_999_999_999, gb(1000), false},
		{"exactly at", 1_000_000_000_000, gb(1000), true},
		{"past", 1_200_000_000_000, gb(1000), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := JudgeDataBudget(c.used, c.budget); got.Reached != c.reached {
				t.Fatalf("reached = %v, want %v", got.Reached, c.reached)
			}
		})
	}
}

func TestDataBudgetGate(t *testing.T) {
	reached := DataBudget{Reached: true}
	cases := []struct {
		name            string
		b               DataBudget
		privileged, ack bool
		want            string
	}{
		{"budget unset: no gate", DataBudget{}, false, false, ""},
		{"not reached: no gate for anyone", DataBudget{BudgetGB: LimitPtr(10)}, false, false, ""},
		{"reached, member: refused", reached, false, false, DataBudgetReachedCode},
		{"reached, member with ack: still refused", reached, false, true, DataBudgetReachedCode},
		{"reached, owner without ack", reached, true, false, DataBudgetAckRequiredCode},
		{"reached, owner with ack", reached, true, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DataBudgetGate(c.b, c.privileged, c.ack); got != c.want {
				t.Fatalf("gate = %q, want %q", got, c.want)
			}
		})
	}
}
