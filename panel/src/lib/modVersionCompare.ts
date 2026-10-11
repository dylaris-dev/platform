// Pure helpers for the Content tab's per-row install/remove/update state:
// picking the newest Modrinth version matching the active loader + MC-version
// filter, and comparing an installed version against it. No I/O here - the
// caller fetches the candidate version list (see getModrinthVersions in
// lib/api/modrinth) and passes it in.

import type { ModrinthVersion } from './api/modrinth';

export type Channel = ModrinthVersion['version_type'];

export interface VersionFilter {
    loaders?: string[];
    mcVersions?: string[];
    // The least stable channel allowed: 'release' admits releases only,
    // 'beta' admits beta + release, 'alpha' (or absent) admits everything.
    leastStable?: Channel;
}

export type ModStatus = 'not-installed' | 'up-to-date' | 'update-available';

const CHANNEL_RANK: Record<Channel, number> = { release: 0, beta: 1, alpha: 2 };

// An unknown channel string ranks as alpha: a build we cannot classify is not
// one a release-only rule may let through.
function channelRank(c: string | undefined): number {
    return CHANNEL_RANK[c as Channel] ?? CHANNEL_RANK.alpha;
}

function matchesFilter(v: ModrinthVersion, filter: VersionFilter): boolean {
    if (filter.loaders?.length && !v.loaders.some(l => filter.loaders!.includes(l))) return false;
    if (filter.mcVersions?.length && !v.game_versions.some(g => filter.mcVersions!.includes(g))) return false;
    if (filter.leastStable && channelRank(v.version_type) > channelRank(filter.leastStable)) return false;
    return true;
}

// Newest (by date_published) candidate whose loaders + game_versions overlap
// the filter. Each filter field is OR-within-field (any listed loader, any
// listed MC version) and AND-across-fields, mirroring how Modrinth's own
// search/version-list facets behave. An empty/absent filter field matches
// everything for that field. Returns null when nothing matches (including an
// empty candidate list).
export function pickNewestMatchingVersion(
    candidates: ModrinthVersion[],
    filter: VersionFilter = {},
): ModrinthVersion | null {
    const matching = candidates.filter(v => matchesFilter(v, filter));
    if (matching.length === 0) return null;
    return matching.reduce((newest, v) =>
        v.date_published.localeCompare(newest.date_published) > 0 ? v : newest
    );
}

// Compares an installed version against the newest candidate matching the
// current filter:
// - no installedVersionId -> 'not-installed'
// - installed, and it already is the newest match -> 'up-to-date'
// - installed, but nothing in the candidate list matches the current filter
//   -> 'up-to-date' too (there is nothing better to offer under this filter,
//   so we don't nag with an update button that has nothing to install)
// - installed, and a different (newer-selected) candidate matches the filter
//   -> 'update-available'
export function compareInstalledVsLatest(
    installedVersionId: string | null | undefined,
    candidates: ModrinthVersion[],
    filter: VersionFilter = {},
): ModStatus {
    if (!installedVersionId) return 'not-installed';
    const latest = pickNewestMatchingVersion(candidates, filter);
    if (!latest || latest.id === installedVersionId) return 'up-to-date';
    return 'update-available';
}

export interface DependencyProblems {
    // Project ids. A dependency named only by a build whose project could not
    // be resolved is listed by that version id.
    missing: string[];
    incompatible: string[];
}

// Version ids a build names a dependency by, without saying the project. These
// must be resolved (see dependencyProblems) before the build can be judged.
export function unresolvedDependencyVersionIds(build: ModrinthVersion): string[] {
    return (build.dependencies ?? [])
        .filter(d => !d.project_id && d.version_id && (d.dependency_type === 'required' || d.dependency_type === 'incompatible'))
        .map(d => d.version_id!);
}

// Checks a build's declared dependencies against the projects the server has.
// Only managed mods can be matched by project id - an unmanaged jar has none -
// so a required dependency that is not managed counts as missing: guessing it
// is "probably one of the loose jars" is how a server ends up not booting.
//
// A dependency named only by a version id is compared by the PROJECT that
// version belongs to (versionProjects: version id -> project id), never by the
// version id itself, which can never equal a project id. One that cannot be
// resolved is missing when required (it cannot be shown to be there) and
// ignored when incompatible (it cannot be shown to be there either).
export function dependencyProblems(
    build: ModrinthVersion,
    installedProjectIds: Set<string>,
    versionProjects: Map<string, string> = new Map(),
): DependencyProblems {
    const out: DependencyProblems = { missing: [], incompatible: [] };
    for (const d of build.dependencies ?? []) {
        const project = d.project_id || (d.version_id ? versionProjects.get(d.version_id) : undefined);
        if (project === build.project_id) continue;
        if (!project) {
            if (d.dependency_type === 'required' && d.version_id) out.missing.push(d.version_id);
            continue;
        }
        if (d.dependency_type === 'required' && !installedProjectIds.has(project)) out.missing.push(project);
        if (d.dependency_type === 'incompatible' && installedProjectIds.has(project)) out.incompatible.push(project);
    }
    return out;
}

export type UpdatePlan =
    | { kind: 'current' }
    | { kind: 'unknown' }
    | { kind: 'update'; build: ModrinthVersion }
    | { kind: 'skip'; build: ModrinthVersion; problems: DependencyProblems };

export interface UpdateTarget {
    // The server's own loader and Minecraft version, never the UI filter: a
    // cleared filter must not widen a bulk run to builds for other versions.
    loader: string;
    mcVersion: string;
    // The installed build if it could be looked up. Unknown is treated as a
    // release, the strictest channel, so an unclassifiable install never
    // drifts onto a pre-release.
    installed: ModrinthVersion | null;
    // When the installed row was written. Used only when the build is unknown.
    installedAt?: string;
    installedProjectIds: Set<string>;
    // Version id -> project id, for dependencies named only by a build.
    versionProjects?: Map<string, string>;
}

// What a bulk update should do with one mod. Never moves to a less stable
// channel than the installed build, never to an older build, and skips a
// build whose dependencies the server cannot satisfy.
export function planModUpdate(
    installedVersionId: string,
    candidates: ModrinthVersion[],
    t: UpdateTarget,
): UpdatePlan {
    const next = pickNewestMatchingVersion(candidates, {
        loaders: [t.loader],
        mcVersions: [t.mcVersion],
        leastStable: t.installed?.version_type ?? 'release',
    });
    if (!next || next.id === installedVersionId) return { kind: 'current' };
    if (t.installed) {
        if (next.date_published.localeCompare(t.installed.date_published) <= 0) return { kind: 'current' };
    } else {
        // Without the installed build its age is unknown, and a newer beta read
        // as a release would "update" to an older release. The install time
        // bounds it: whatever was installed was published before it, so only a
        // build published after it is certainly newer. Anything else is unknown.
        const installedAt = Date.parse(t.installedAt ?? '');
        if (!(Date.parse(next.date_published) > installedAt)) return { kind: 'unknown' };
    }
    const problems = dependencyProblems(next, t.installedProjectIds, t.versionProjects);
    if (problems.missing.length || problems.incompatible.length) return { kind: 'skip', build: next, problems };
    return { kind: 'update', build: next };
}
