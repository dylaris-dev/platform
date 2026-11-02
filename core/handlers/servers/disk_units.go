package servers

// diskMBToGBCeil converts a server's disk limit (stored in MEGABYTES) to the
// whole gigabytes the placement request carries, rounding UP.
//
// Rounding down is wrong in both directions here, and one of them silently skips
// the check entirely:
//
//   - A limit below 1024 MB truncates to 0, and the capacity check is guarded on
//     `DiskGB > 0`, so a small server was placed without its disk being
//     considered at all.
//   - A limit of 5000 MB truncated to 4 GB, so placement reserved ~900 MB less
//     than the server is actually promised.
//
// Rounding up can only ever over-reserve, which fails safe: the worst case is
// refusing a placement that would have just fit. The panel sends whole
// gigabytes, so it is unaffected either way; this matters for API clients and
// for plans whose limits are not multiples of 1024.
func diskMBToGBCeil(mb int64) int {
	if mb <= 0 {
		return 0
	}
	return int((mb + 1023) / 1024)
}
