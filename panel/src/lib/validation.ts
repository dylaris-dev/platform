// Canonical input-validation patterns for the panel, kept in lockstep with the
// Go source of truth in platform/pkg/validate (validate.go) and the core
// handlers. Client-side validation here is UX only - it lets the form show a
// clear error before submit; the backend enforces the same rules.

// Username: 3-32 chars, first char alphanumeric, then letters / digits / . _ - .
// Forbids ':' and whitespace. Mirrors validate.Username.
export const USERNAME = /^[a-zA-Z0-9][a-zA-Z0-9_.\-]{2,31}$/;

// Email - light structural check. Mirrors validate.Email.
export const EMAIL = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

// Server name: 1-50 chars, first char alphanumeric, then alnum / space / - / + / _ .
// Mirrors validate.ServerName.
export const SERVER_NAME = /^[a-zA-Z0-9][a-zA-Z0-9 \-+_]{0,49}$/;

// Sub-server directory name: 1-50, alnum / - / _ / + , no space.
// Mirrors validate.SubServerName.
export const SUB_SERVER_NAME = /^[a-zA-Z0-9\-_+]{1,50}$/;

// Pack slug: 2-64 lowercase, inner - / _ . Mirrors validate.Slug / packs.go.
export const PACK_SLUG = /^[a-z0-9]([a-z0-9_-]{1,62}[a-z0-9])?$/;

// Node label / tag item. Mirrors validate.Label.
export const NODE_LABEL = /^[a-z0-9][a-z0-9_-]{0,31}$/;

// The name a customer gives one of their own machines. Mirrors
// validate.LocationName: 4-20 characters, letters / digits / hyphen, and no
// hyphen at either end - a LEADING one reads as a flag wherever the name is
// pasted into a command or a compose file.
export const LOCATION_NAME = /^[a-zA-Z0-9]([a-zA-Z0-9-]{2,18})[a-zA-Z0-9]$/;

// Semver-ish version (e.g. the beam minVersion): 1.2.3 with optional pre/build.
export const SEMVER = /^\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?$/;

// Minecraft version, e.g. 1.21 or 1.21.4. Mirrors validate.McVersion.
export const MC_VERSION = /^[0-9]+\.[0-9]+(\.[0-9]+)?$/;

// Length limits [min, max] (or a single max) used across free-text forms.
export const LIMITS = {
    ticketTitle: [3, 200],
    ticketBody: [1, 10000],
    cannedName: 128,
    cannedBody: 10000,
    taskName: 128,
    taskPayload: 512,
} as const;

export const isUsername = (s: string) => USERNAME.test(s);
export const isEmail = (s: string) => EMAIL.test(s);
export const isServerName = (s: string) => SERVER_NAME.test(s);
export const isSubServerName = (s: string) => SUB_SERVER_NAME.test(s);
export const isPackSlug = (s: string) => PACK_SLUG.test(s);
export const isSemver = (s: string) => SEMVER.test(s);
export const isMcVersion = (s: string) => MC_VERSION.test(s);
export const isLocationName = (s: string) => LOCATION_NAME.test(s);

// sanitizeServerName coerces a raw name into the SERVER_NAME alphabet (the
// create-time behaviour): strip invalid chars (spaces are kept), trim to 50,
// and drop a non-alphanumeric first character. Mirrors validate.SanitizeServerName.
// (Was duplicated byte-for-byte in SetupView + SetupNewWizard.)
export function sanitizeServerName(raw: string): string {
    let s = raw.replace(/[^a-zA-Z0-9 \-+_]/g, '');
    if (s.length > 50) s = s.slice(0, 50);
    return s.replace(/^[ \-+_]+/, '');
}

// clampInt coerces v to an integer within [min, max] (max optional). Non-numeric
// input falls back to min. Use for numeric form fields (RAM, CPU-derived, ...).
export function clampInt(v: unknown, min: number, max?: number): number {
    let n = Math.floor(Number(v));
    if (!Number.isFinite(n)) n = min;
    if (n < min) n = min;
    if (max !== undefined && n > max) n = max;
    return n;
}
