import type { MemoryGuardAction, Server } from './api/types';

/**
 * What Core does when a server's container has been at 98% of its memory limit
 * for 30 seconds (the node also sends save-all from 95%). 'off' (warn only) is
 * the default: a small plan's server can sit there for good without being
 * killed, so stopping or restarting it is the owner's choice.
 */
export const MEMORY_GUARD_OPTIONS: { value: MemoryGuardAction; label: string }[] = [
    { value: 'off', label: 'Warn only (default)' },
    { value: 'stop', label: 'Stop the server (saves the world)' },
    { value: 'restart', label: 'Restart the server' },
];

export const MEMORY_GUARD_HELP =
    'When the server stays at 98% of its memory limit for 30 seconds, it may be killed without saving. ' +
    'By default you are only warned (the world is saved from 95%). Stopping it first keeps the world; ' +
    'a restart frees the memory but may hit the limit again. Some servers sit near the limit without ever being killed.';

/** The PATCH fields for a changed action, or undefined to leave it alone. */
export function memoryGuardPatch(
    current: MemoryGuardAction | undefined,
    edit: MemoryGuardAction,
): { memoryGuardAction: MemoryGuardAction } | undefined {
    if (current === undefined || current === edit) return undefined;
    return { memoryGuardAction: edit };
}

const OOM_BANNER_WINDOW_MS = 24 * 60 * 60 * 1000;

/**
 * Whether to show the "killed: out of memory" banner: the last crash was an
 * OOM kill, within the last 24 hours, and this crash was not dismissed.
 * dismissedAt is the lastCrashAt the viewer dismissed, so a new kill shows again.
 */
export function showOomBanner(
    server: Pick<Server, 'lastCrashReason' | 'lastCrashAt'>,
    now: number,
    dismissedAt: string | null,
): boolean {
    if (server.lastCrashReason !== 'oom_killed' || !server.lastCrashAt) return false;
    if (dismissedAt === server.lastCrashAt) return false;
    const at = Date.parse(server.lastCrashAt);
    if (Number.isNaN(at)) return false;
    return now - at >= -60_000 && now - at < OOM_BANNER_WINDOW_MS;
}

export const oomDismissKey = (serverId: number) => `dylaris:oom-dismissed:${serverId}`;
