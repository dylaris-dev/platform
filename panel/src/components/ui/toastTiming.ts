export function toastDuration(ok: boolean, requested = 0): number {
    return Math.round(Math.max(requested, ok ? 8000 : 14000) * 0.65);
}
