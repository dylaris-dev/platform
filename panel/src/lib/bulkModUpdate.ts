import type { InstalledMod, ModrinthVersion, ModrinthProjectName } from '@/lib/api/modrinth';
import { isOnServer } from '@/lib/installedState';
import { planModUpdate, unresolvedDependencyVersionIds, type UpdatePlan, type UpdateTarget } from '@/lib/modVersionCompare';

/** What one mod's slot in a bulk run ended as. */
export type BulkOutcome = 'updated' | 'current' | 'failed' | 'unknown';

export interface BulkProgress {
    /** How many slots have been decided so far. */
    done: number;
    total: number;
    /** The mod being worked on, for the label. */
    current: string;
    /** Set while deciding what to update, before anything is installed. */
    checking?: boolean;
}

export interface BulkTally {
    updated: number;
    current: number;
    failed: number;
    unknown: number;
}

export function emptyTally(): BulkTally {
    return { updated: 0, current: 0, failed: 0, unknown: 0 };
}

/**
 * Which mods a bulk update should even look at.
 *
 * isOnServer, not "a row exists": a queued install has not happened yet and a
 * failed one never will, so updating either would be updating from a build the
 * server is not running.
 */
export function updatableMods(installed: InstalledMod[]): InstalledMod[] {
    return installed.filter(m => isOnServer(m));
}

/**
 * One sentence for the end of a run.
 *
 * Every category is named, including the ones that are zero-worthy but not
 * zero: "12 updated" alone hides the three that failed, and a bulk action that
 * reports only its successes is how someone concludes a run was clean when a
 * third of it was not.
 */
export function summarise(t: BulkTally): string {
    const parts: string[] = [];
    if (t.updated) parts.push(`${t.updated} updated`);
    if (t.current) parts.push(`${t.current} already current`);
    if (t.failed) parts.push(`${t.failed} failed`);
    if (t.unknown) parts.push(`${t.unknown} could not be checked`);
    if (parts.length === 0) return 'Nothing to do.';
    return parts.join(', ') + '.';
}

/** A run that produced no failure and nothing unchecked is the good outcome. */
export function runWasClean(t: BulkTally): boolean {
    return t.failed === 0 && t.unknown === 0;
}

/** The Modrinth calls the update check makes; injected so tests can count them. */
export interface UpdateCheckApi {
    versionsByIds(ids: string[]): Promise<ModrinthVersion[]>;
    latestFromHashes(
        hashes: string[],
        filter: { loaders: string[]; gameVersions: string[]; versionTypes: ModrinthVersion['version_type'][] },
    ): Promise<Record<string, ModrinthVersion>>;
    /** Per-project list. [] means failed OR nothing, so it can only say "unknown". */
    projectVersions(slug: string, filter: { loaders: string[]; versions: string[] }): Promise<ModrinthVersion[]>;
    projectsByIds(ids: string[]): Promise<ModrinthProjectName[]>;
}

export interface UpdateCheck {
    planned: { mod: InstalledMod; build: ModrinthVersion }[];
    skipped: { mod: InstalledMod; reason: string }[];
    current: number;
    unknown: number;
}

// Channels a mod may move within: its own and every more stable one.
const ALLOWED: Record<ModrinthVersion['version_type'], ModrinthVersion['version_type'][]> = {
    release: ['release'],
    beta: ['release', 'beta'],
    alpha: ['release', 'beta', 'alpha'],
};

/**
 * Decide what Update all would do, in as few Modrinth calls as possible.
 *
 * One call per mod is what this used to cost, and Core reaches Modrinth from
 * ONE address for every customer: a 343-mod server spent more than the whole
 * platform's per-minute allowance and two thirds of it came back unchecked.
 * Now: the installed builds by id (100 per call), then the newest build per
 * installed FILE HASH, one call per channel group, filtered by Modrinth to the
 * server's loader, Minecraft version and allowed channels. Only a mod that
 * cannot be asked by hash (no recorded hash, or a build Modrinth no longer
 * lists) costs a call of its own.
 *
 * Returns null when cancelled.
 */
export async function checkModUpdates(
    mods: InstalledMod[],
    server: { loader: string; mcVersion: string },
    api: UpdateCheckApi,
    onProgress: (done: number, total: number) => void = () => {},
    cancelled: () => boolean = () => false,
): Promise<UpdateCheck | null> {
    const total = mods.length;
    let done = 0;
    const out: UpdateCheck = { planned: [], skipped: [], current: 0, unknown: 0 };
    const installedProjectIds = new Set(mods.map(m => m.modrinthProjectId));
    const base = { loader: server.loader, mcVersion: server.mcVersion, installedProjectIds };
    onProgress(0, total);

    let installedBuilds = new Map<string, ModrinthVersion>();
    try {
        installedBuilds = new Map((await api.versionsByIds(mods.map(m => m.modrinthVersionId).filter(Boolean))).map(v => [v.id, v]));
    } catch {
        // Without the installed builds nothing can be judged safely, and asking
        // per mod is exactly the load that just failed.
        return { ...out, unknown: total };
    }
    if (cancelled()) return null;

    // Each decided mod keeps its inputs, so dependencies can be re-judged once
    // the version-only ones are resolved below.
    const decided: { mod: InstalledMod; candidates: ModrinthVersion[]; target: UpdateTarget; plan: UpdatePlan }[] = [];
    const decide = (mod: InstalledMod, candidates: ModrinthVersion[], target: UpdateTarget) =>
        decided.push({ mod, candidates, target, plan: planModUpdate(mod.modrinthVersionId, candidates, target) });

    const byChannel = new Map<ModrinthVersion['version_type'], { mod: InstalledMod; hash: string; installed: ModrinthVersion }[]>();
    const oneByOne: InstalledMod[] = [];
    for (const mod of mods) {
        const installed = installedBuilds.get(mod.modrinthVersionId);
        const hash = mod.sha512?.toLowerCase();
        // The hash must be a file of the installed build: only then does
        // "absent from the answer" mean "nothing newer" rather than "unknown file".
        if (installed && hash && installed.files.some(f => f.hashes.sha512?.toLowerCase() === hash)) {
            const channel = ALLOWED[installed.version_type] ? installed.version_type : 'release';
            byChannel.set(channel, [...(byChannel.get(channel) ?? []), { mod, hash, installed }]);
        } else {
            oneByOne.push(mod);
        }
    }

    for (const [channel, group] of byChannel) {
        let latest: Record<string, ModrinthVersion> | null = null;
        try {
            latest = await api.latestFromHashes(group.map(g => g.hash), {
                loaders: [server.loader], gameVersions: [server.mcVersion], versionTypes: ALLOWED[channel],
            });
        } catch { latest = null; }
        if (cancelled()) return null;
        for (const g of group) {
            if (!latest) { out.unknown++; continue; }
            const next = latest[g.hash];
            decide(g.mod, next ? [next] : [], { ...base, installed: g.installed });
        }
        done += group.length;
        onProgress(done, total);
    }

    for (const mod of oneByOne) {
        const candidates = await api.projectVersions(mod.modrinthProjectSlug || mod.modrinthProjectId, {
            loaders: [server.loader], versions: [server.mcVersion],
        });
        if (cancelled()) return null;
        done++;
        onProgress(done, total);
        // [] is both "no build" and "the lookup failed"; it cannot say current.
        if (candidates.length === 0) { out.unknown++; continue; }
        const installed = installedBuilds.get(mod.modrinthVersionId) ?? candidates.find(v => v.id === mod.modrinthVersionId) ?? null;
        decide(mod, candidates, { ...base, installed, installedAt: mod.installedAt });
    }

    // Dependencies named only by a build: resolve them to projects, in bulk.
    const versionOnly = decided.flatMap(d => d.plan.kind === 'update' || d.plan.kind === 'skip' ? unresolvedDependencyVersionIds(d.plan.build) : []);
    if (versionOnly.length) {
        let versionProjects = new Map<string, string>();
        // A failed lookup leaves them unresolved, and an unresolved REQUIRED
        // dependency skips the mod: unknown is not "present".
        try { versionProjects = new Map((await api.versionsByIds(versionOnly)).map(v => [v.id, v.project_id])); } catch { /* unresolved */ }
        if (cancelled()) return null;
        for (const d of decided) d.plan = planModUpdate(d.mod.modrinthVersionId, d.candidates, { ...d.target, versionProjects });
    }

    const skips: { mod: InstalledMod; missing: string[]; incompatible: string[] }[] = [];
    for (const d of decided) {
        if (d.plan.kind === 'current') out.current++;
        else if (d.plan.kind === 'unknown') out.unknown++;
        else if (d.plan.kind === 'update') out.planned.push({ mod: d.mod, build: d.plan.build });
        else skips.push({ mod: d.mod, ...d.plan.problems });
    }

    // Name the projects a skip is about, so "needs X" means something.
    const names = new Map<string, string>();
    const ids = [...new Set(skips.flatMap(s => [...s.missing, ...s.incompatible]))];
    if (ids.length) {
        try { for (const p of await api.projectsByIds(ids)) names.set(p.id, p.title); } catch { /* ids stay as they are */ }
    }
    const name = (id: string) => names.get(id) ?? id;
    out.skipped = skips.map(s => {
        const parts: string[] = [];
        if (s.missing.length) parts.push(`needs ${s.missing.map(name).join(', ')}`);
        if (s.incompatible.length) parts.push(`conflicts with ${s.incompatible.map(name).join(', ')}`);
        return { mod: s.mod, reason: parts.join('; ') };
    });
    return out;
}
