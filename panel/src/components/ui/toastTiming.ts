export type ToastClock = { elapsed: number; at: number; paused: boolean };

export function toastDuration(ok: boolean, requested = 0): number {
    return Math.round(Math.max(requested, ok ? 8000 : 14000) * 0.65);
}

export function advanceToastClock(clock: ToastClock, now: number, duration: number, paused = clock.paused): ToastClock {
    return {
        elapsed: Math.min(duration, clock.elapsed + (clock.paused ? 0 : Math.max(0, now - clock.at))),
        at: now,
        paused,
    };
}

export function toastOpacity(elapsed: number, duration: number): number {
    const progress = Math.min(1, Math.max(0, elapsed / duration));
    return progress <= 0.5 ? 1 : 1 - (progress - 0.5) * 1.5;
}
