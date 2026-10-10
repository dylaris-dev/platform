import type { InstalledMod } from '@/lib/api/modrinth';

/**
 * One mod install as the panel follows it, independent of which page is open.
 *
 * sending     the POST to Core is in flight
 * installing  Core queued it and the node will report; the node downloads and
 *             verifies the sha512 in the same pass, so there is no separate
 *             "verifying" step to show
 * sent        queued on a node too old to report: nothing will ever answer, so
 *             this is as far as the panel can honestly follow it
 * installed / failed   what the node reported
 * unknown     no answer within INSTALL_TIMEOUT_MS; never an endless spinner
 */
export type InstallJobState = 'sending' | 'installing' | 'sent' | 'installed' | 'failed' | 'unknown';

export interface InstallJob {
    id: string;
    serverId: number;
    serverName: string;
    projectId: string;
    versionId: string;
    title: string;
    state: InstallJobState;
    message?: string;
    startedAt: number;
    finishedAt?: number;
}

// A mod download is one file of at most 256 MB from Modrinth's CDN; three
// minutes without an answer means the report is not coming, not that it is slow.
export const INSTALL_TIMEOUT_MS = 3 * 60_000;

export function isPending(job: InstallJob): boolean {
    return job.state === 'sending' || job.state === 'installing';
}

/** Applies Core's reply to the POST. */
export function afterPost(
    job: InstallJob,
    res: { success: boolean; message?: string; status?: string },
    now: number,
): InstallJob {
    if (job.state !== 'sending') return job;
    if (!res.success) {
        return { ...job, state: 'failed', message: res.message || 'Core refused the install.', finishedAt: now };
    }
    // "installed" in the reply means Core recorded it as a fact at dispatch,
    // which it only does for a node that never reports. An absent status is an
    // older Core: follow it, the timeout bounds the wait.
    if (res.status === 'installed') return { ...job, state: 'sent', finishedAt: now };
    return { ...job, state: 'installing' };
}

/**
 * Moves an installing job on from the server's mod list, the thing the node's
 * report actually writes. Only the row for this project AND this build counts:
 * a row for another build is either the one being replaced or a later click.
 */
export function reconcile(job: InstallJob, rows: readonly InstalledMod[], now: number): InstallJob {
    if (job.state !== 'installing') return job;
    const row = rows.find(r => r.modrinthProjectId === job.projectId && r.modrinthVersionId === job.versionId);
    if (row && row.status === 'failed') {
        return { ...job, state: 'failed', message: describeInstallFailure(row.statusMessage), finishedAt: now };
    }
    if (row && row.status !== 'installing') {
        return { ...job, state: 'installed', finishedAt: now };
    }
    return expire(job, now);
}

/**
 * Follows installs this tab did not start: a reload, or another session. Core
 * writes the row as "installing" with installed_at at dispatch, so that time is
 * the job's start and the usual timeout still bounds it. A row already past the
 * timeout is left alone rather than shown as a ghost that can only expire.
 * Returns `jobs` itself when nothing was added, so a caller can skip a render.
 */
export function adopt(
    jobs: readonly InstallJob[],
    serverId: number,
    serverName: string,
    rows: readonly InstalledMod[],
    now: number,
): InstallJob[] {
    const added: InstallJob[] = [];
    for (const r of rows) {
        if (r.status !== 'installing') continue;
        // A browser clock behind Core's would otherwise stretch the timeout.
        const startedAt = Math.min(Date.parse(r.installedAt), now);
        if (!(now - startedAt < INSTALL_TIMEOUT_MS)) continue;
        const tracked = jobs.some(j => j.serverId === serverId && j.projectId === r.modrinthProjectId
            && (isPending(j) || j.versionId === r.modrinthVersionId));
        if (tracked) continue;
        added.push({
            id: `mod-${startedAt}-${serverId}-${r.modrinthProjectId}`,
            serverId,
            serverName,
            projectId: r.modrinthProjectId,
            versionId: r.modrinthVersionId,
            title: r.title || r.fileName,
            state: 'installing',
            startedAt,
        });
    }
    if (added.length === 0) return jobs as InstallJob[];
    // Same rule as a click: one entry per project and server.
    const replaced = new Set(added.map(j => j.projectId));
    return [...jobs.filter(j => !(j.serverId === serverId && replaced.has(j.projectId))), ...added];
}

/** forbidden: the last read of the mod list was refused (403), so the panel
 * could never have seen the answer and must not suggest looking for it. */
export function expire(job: InstallJob, now: number, forbidden = false): InstallJob {
    if (job.state !== 'installing' || now - job.startedAt < INSTALL_TIMEOUT_MS) return job;
    return {
        ...job,
        state: 'unknown',
        message: forbidden
            ? "Installed status unknown - you cannot view this server's content list."
            : 'No answer from the server yet. Check the content list.',
        finishedAt: now,
    };
}

// The node's reason is written for its own log. Lead with what it means for the
// reader and keep the raw detail, which is what support will ask for.
const FAILURE_HINTS: [RegExp, string][] = [
    [/sha512 mismatch/i, 'The downloaded file did not match Modrinth\'s checksum.'],
    [/no space left on device|disk.*full/i, 'The server\'s disk is full.'],
    [/exceeds the \d+ byte limit/i, 'The file is larger than the 256 MB limit.'],
    [/upstream status (\d+)/i, 'Modrinth did not serve the file (HTTP $1).'],
    [/download failed|http get/i, 'The server could not download the file.'],
];

export function describeInstallFailure(raw: string | undefined): string {
    const detail = (raw || '').trim();
    if (!detail) return 'The install failed. The server gave no reason.';
    for (const [re, hint] of FAILURE_HINTS) {
        const m = detail.match(re);
        if (m) return `${hint.replace('$1', m[1] ?? '')} ${detail}`;
    }
    return detail;
}

export function jobLabel(job: InstallJob): string {
    switch (job.state) {
        case 'sending': return 'Sending…';
        case 'installing': return 'Installing…';
        case 'sent': return 'Sent to the server';
        case 'installed': return 'Installed';
        case 'failed': return 'Failed';
        case 'unknown': return 'No answer yet';
    }
}
