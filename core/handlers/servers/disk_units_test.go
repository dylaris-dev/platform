package servers

import "testing"

func TestDiskMBToGBCeil(t *testing.T) {
	tests := []struct {
		name string
		mb   int64
		want int
	}{
		{"no limit", 0, 0},
		{"negative is treated as no limit", -1, 0},
		{"exactly one gigabyte", 1024, 1},
		{"whole gigabytes are exact", 20480, 20},
		// The case that skipped the capacity check entirely: truncating to 0 hit
		// the `DiskGB > 0` guard and the disk was never considered.
		{"below a gigabyte still counts as one", 512, 1},
		{"one megabyte still counts as one", 1, 1},
		// Truncating 5000 MB to 4 GB reserved ~900 MB less than promised.
		{"partial gigabytes round up", 5000, 5},
		{"just over a boundary rounds up", 1025, 2},
		{"just under a boundary rounds up", 2047, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := diskMBToGBCeil(tt.mb); got != tt.want {
				t.Errorf("diskMBToGBCeil(%d) = %d, want %d", tt.mb, got, tt.want)
			}
		})
	}
}

// Rounding up may over-reserve but must never under-reserve: the placement check
// compares against DiskGB*1024, so a result below the real limit would admit a
// server the node cannot actually hold.
func TestDiskMBToGBCeil_NeverUnderReserves(t *testing.T) {
	for mb := int64(1); mb <= 10240; mb++ {
		if reserved := int64(diskMBToGBCeil(mb)) * 1024; reserved < mb {
			t.Fatalf("diskMBToGBCeil(%d) reserves %d MB, less than the limit", mb, reserved)
		}
	}
}
