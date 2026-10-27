import type { BuildContentEntry } from '@/lib/api/packs';

export type PackEnvironment = BuildContentEntry['side'];
export type PackEnvironmentFilter = PackEnvironment | 'all';
export type PackContentSort = 'name' | 'name-desc' | 'environment' | 'updates';

/** A content entry must remain installed on at least one environment. */
export function togglePackEnvironment(current: PackEnvironment, environment: 'client' | 'server'): PackEnvironment | null {
    if (current === 'both') return environment === 'client' ? 'server' : 'client';
    return current === environment ? null : 'both';
}

export function filterPackContent(entries: BuildContentEntry[], query: string, environment: PackEnvironmentFilter, sort: PackContentSort, name: (entry: BuildContentEntry) => string) {
    const search = query.trim().toLocaleLowerCase();
    const order: Record<PackEnvironment, number> = { server: 0, both: 1, client: 2 };
    const update = (entry: BuildContentEntry) => Number(!!(entry.linked && entry.modrinthLatestVersionId && entry.modrinthLatestVersionId !== entry.modrinthVersionId));
    return entries.filter(entry => (environment === 'all' || entry.side === environment)
        && `${name(entry)} ${entry.prettyName} ${entry.modSlug} ${entry.targetPath}`.toLocaleLowerCase().includes(search))
        .sort((a, b) => {
            if (sort === 'environment' && a.side !== b.side) return order[a.side] - order[b.side];
            if (sort === 'updates' && update(a) !== update(b)) return update(b) - update(a);
            return (sort === 'name-desc' ? -1 : 1) * name(a).localeCompare(name(b)) || a.id - b.id;
        });
}
