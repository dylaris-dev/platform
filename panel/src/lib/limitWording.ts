// "0 = unlimited" is the one sentence the platform limit convention forbids,
// and it keeps coming back.
//
// The convention (services.Limits, and the LimitField control):
//
//   null / empty  no limit at all
//   0             none - they may hold zero of this
//   n             the cap
//
// So 0 and "no limit" are OPPOSITES. Text that promises the reverse is worse
// than no text: an admin who wants a tenant to have unlimited nodes types 0 and
// gives them zero. Measured when this was added - the per-user limits section
// and the per-user R2 quota both still said "0 = unlimited" long after the Go
// side had been converted, and the Go side carries a comment warning that the
// stale wording is how someone writes the old semantics back in.
//
// A few places genuinely DO mean it, and they are listed here rather than
// pattern-matched, because "which convention does this number use" is a fact
// about the enforcement site and cannot be read off the sentence:
//
//   - The Beam bandwidth throttle (bytes/sec). A cap of 0 B/s is a hung
//     transfer, not a policy; "off" is what its Enabled flag says.
//   - Docker's own limits - the container pids cap, the CPU quota, the disk
//     limit. Docker's API defines 0 as unlimited and a container that may run
//     zero processes cannot start. Translating between two opposite conventions
//     at that boundary could only add a place to get it wrong.
//
// Both carve-outs are the ones named in the project's own rules. Adding to this
// list is a decision: check what the enforcement site does with a stored 0
// before you do.

/** Files allowed to say a zero means unlimited, and why. */
export const ZERO_IS_UNLIMITED_ALLOWED: Record<string, string> = {
    'src/components/settings/BeamTab.tsx': 'the bandwidth throttle, where 0 B/s is not a policy',
    'src/components/server/ServerSettingsDrawer.tsx': "Docker's cpu and disk limits",
    'src/components/CreateServerWizard.tsx': "Docker's cpu and disk limits",
    'src/lib/api/types.ts': "Docker's container pids cap",
    'src/components/settings/LimitField.tsx': 'describes the wording it replaced',
    'src/lib/limitWording.ts': 'this file',
};

// The claim in its usual spellings. Deliberately narrow: it looks for the
// inversion itself, not for the digit, so ordinary copy about a value of zero
// is left alone.
const CLAIM = /0\s*(?:=|means|for|is)\s*(?:unlimited|no limit)/i;

/**
 * linesClaimingZeroIsUnlimited returns the offending lines of one file.
 *
 * Line-level rather than file-level on purpose: a file that legitimately
 * documents one Docker limit would otherwise be a free pass for every other
 * limit in it. The allowlist above still works per file - that is a deliberate
 * trade, since the alternative is pinning line numbers that move on every edit -
 * but the report names the line so a reviewer sees which claim they are waving
 * through.
 */
export function linesClaimingZeroIsUnlimited(source: string): string[] {
    return source
        .split('\n')
        .map((line, i) => ({ line: line.trim(), n: i + 1 }))
        .filter(({ line }) => CLAIM.test(line))
        .map(({ line, n }) => `${n}: ${line}`);
}
