package agent

import (
	"math"
	"testing"
)

func TestSubClamp(t *testing.T) {
	tests := []struct {
		name string
		a, b uint64
		want uint64
	}{
		{"normal difference", 10, 3, 7},
		{"equal operands", 5, 5, 0},
		{"underflow clamps to zero", 3, 10, 0},
		{"zero minus max clamps", 0, math.MaxUint64, 0},
		{"max minus zero", math.MaxUint64, 0, math.MaxUint64},
		{"max minus one", math.MaxUint64, 1, math.MaxUint64 - 1},
		{"one minus max clamps", 1, math.MaxUint64, 0},
		{"both zero", 0, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := subClamp(tt.a, tt.b); got != tt.want {
				t.Fatalf("subClamp(%d, %d) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
