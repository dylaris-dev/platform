import { describe, it, expect } from 'vitest';
import {
    pickNewestMatchingVersion, compareInstalledVsLatest, dependencyProblems, planModUpdate, unresolvedDependencyVersionIds,
    pickPreferredVersion, rowUpdateTarget,
} from './modVersionCompare';
import type { ModrinthVersion } from './api/modrinth';

function version(overrides: Partial<ModrinthVersion> & { id: string }): ModrinthVersion {
    return {
        project_id: 'proj',
        name: overrides.id,
        version_number: overrides.id,
        game_versions: ['1.20.1'],
        loaders: ['fabric'],
        version_type: 'release',
        featured: false,
        date_published: '2026-01-01T00:00:00Z',
        files: [],
        ...overrides,
    };
}

describe('pickNewestMatchingVersion', () => {
    it('returns null for an empty candidate list', () => {
        expect(pickNewestMatchingVersion([], { loaders: ['fabric'], mcVersions: ['1.20.1'] })).toBeNull();
    });

    it('picks the newest by date among matching candidates, regardless of array order', () => {
        const older = version({ id: 'v1', date_published: '2026-01-01T00:00:00Z' });
        const newest = version({ id: 'v2', date_published: '2026-03-01T00:00:00Z' });
        const mid = version({ id: 'v3', date_published: '2026-02-01T00:00:00Z' });
        const result = pickNewestMatchingVersion([older, newest, mid], { loaders: ['fabric'], mcVersions: ['1.20.1'] });
        expect(result?.id).toBe('v2');
    });

    it('excludes a candidate matching MC version but not loader', () => {
        const wrongLoader = version({ id: 'v1', loaders: ['forge'], game_versions: ['1.20.1'] });
        const result = pickNewestMatchingVersion([wrongLoader], { loaders: ['fabric'], mcVersions: ['1.20.1'] });
        expect(result).toBeNull();
    });

    it('excludes a candidate matching loader but not MC version', () => {
        const wrongVersion = version({ id: 'v1', loaders: ['fabric'], game_versions: ['1.19.4'] });
        const result = pickNewestMatchingVersion([wrongVersion], { loaders: ['fabric'], mcVersions: ['1.20.1'] });
        expect(result).toBeNull();
    });

    it('matches when a candidate lists multiple loaders/versions and any one overlaps the filter', () => {
        const v = version({ id: 'v1', loaders: ['fabric', 'quilt'], game_versions: ['1.19.4', '1.20.1'] });
        const result = pickNewestMatchingVersion([v], { loaders: ['quilt'], mcVersions: ['1.20.1'] });
        expect(result?.id).toBe('v1');
    });

    it('treats an empty/absent filter as matching everything', () => {
        const v = version({ id: 'v1', loaders: ['forge'], game_versions: ['1.16.5'] });
        expect(pickNewestMatchingVersion([v], {})?.id).toBe('v1');
        expect(pickNewestMatchingVersion([v])?.id).toBe('v1');
    });
});

describe('compareInstalledVsLatest', () => {
    const filter = { loaders: ['fabric'], mcVersions: ['1.20.1'] };

    it('returns not-installed when there is no installed version id', () => {
        const candidates = [version({ id: 'v1' })];
        expect(compareInstalledVsLatest(null, candidates, filter)).toBe('not-installed');
        expect(compareInstalledVsLatest(undefined, candidates, filter)).toBe('not-installed');
        expect(compareInstalledVsLatest('', candidates, filter)).toBe('not-installed');
    });

    it('returns up-to-date when the installed version is the newest match', () => {
        const older = version({ id: 'v1', date_published: '2026-01-01T00:00:00Z' });
        const newest = version({ id: 'v2', date_published: '2026-02-01T00:00:00Z' });
        expect(compareInstalledVsLatest('v2', [older, newest], filter)).toBe('up-to-date');
    });

    it('returns update-available when a newer matching build exists', () => {
        const installed = version({ id: 'v1', date_published: '2026-01-01T00:00:00Z' });
        const newer = version({ id: 'v2', date_published: '2026-02-01T00:00:00Z' });
        expect(compareInstalledVsLatest('v1', [installed, newer], filter)).toBe('update-available');
    });

    it('returns up-to-date when installed but no candidate matches the current filter (nothing better to offer)', () => {
        const nonMatching = version({ id: 'v1', loaders: ['forge'], game_versions: ['1.16.5'] });
        expect(compareInstalledVsLatest('some-other-installed-id', [nonMatching], filter)).toBe('up-to-date');
    });

    it('returns up-to-date for an empty candidate list when installed', () => {
        expect(compareInstalledVsLatest('v1', [], filter)).toBe('up-to-date');
    });
});

// Prod audit: Update all moved releases onto betas (one of which required a mod
// the server did not have), and a cleared filter let it pick builds for every
// Minecraft version. Each row pins one of those rules.
describe('planModUpdate', () => {
    const installedRelease = version({ id: 'inst', loaders: ['forge'], date_published: '2026-01-01T00:00:00Z' });
    const installedBeta = { ...installedRelease, version_type: 'beta' as const };
    const forge = (over: Partial<ModrinthVersion> & { id: string }) =>
        version({ loaders: ['forge'], date_published: '2026-06-01T00:00:00Z', ...over });
    const target = { loader: 'forge', mcVersion: '1.20.1', installedProjectIds: new Set(['proj', 'dep-a']) };

    const cases: {
        name: string;
        installed: ModrinthVersion | null;
        installedAt?: string;
        candidates: ModrinthVersion[];
        want: string; // 'current' | 'unknown' | 'update:<id>' | 'skip:<id>'
    }[] = [
        {
            name: 'release does not move to a newer beta',
            installed: installedRelease,
            candidates: [installedRelease, forge({ id: 'b2', version_type: 'beta' })],
            want: 'current',
        },
        {
            name: 'release does not move to a newer alpha',
            installed: installedRelease,
            candidates: [installedRelease, forge({ id: 'a2', version_type: 'alpha' })],
            want: 'current',
        },
        {
            name: 'release moves to the newest release, past a newer beta',
            installed: installedRelease,
            candidates: [
                installedRelease,
                forge({ id: 'r2', date_published: '2026-03-01T00:00:00Z' }),
                forge({ id: 'b3', version_type: 'beta', date_published: '2026-07-01T00:00:00Z' }),
            ],
            want: 'update:r2',
        },
        {
            name: 'beta may move to a newer release',
            installed: installedBeta,
            candidates: [installedBeta, forge({ id: 'r2' })],
            want: 'update:r2',
        },
        {
            name: 'beta may move to a newer beta',
            installed: installedBeta,
            candidates: [installedBeta, forge({ id: 'b2', version_type: 'beta' })],
            want: 'update:b2',
        },
        {
            name: 'beta does not move to an alpha',
            installed: installedBeta,
            candidates: [installedBeta, forge({ id: 'a2', version_type: 'alpha' })],
            want: 'current',
        },
        {
            name: 'unknown installed build is treated as a release',
            installed: null,
            candidates: [forge({ id: 'b2', version_type: 'beta' })],
            want: 'current',
        },
        {
            // The installed build was a beta newer than this release; without
            // it, the release must not be read as an update.
            name: 'unknown installed build: a release older than the install is unknown, not an update',
            installed: null,
            installedAt: '2026-08-01T10:00:00.123456Z',
            candidates: [forge({ id: 'r-old', date_published: '2026-06-01T00:00:00Z' })],
            want: 'unknown',
        },
        {
            name: 'unknown installed build: a release published after the install is an update',
            installed: null,
            installedAt: '2026-05-01T10:00:00Z',
            candidates: [forge({ id: 'r-new', date_published: '2026-06-01T00:00:00Z' })],
            want: 'update:r-new',
        },
        {
            name: 'unknown installed build and no install time is unknown',
            installed: null,
            candidates: [forge({ id: 'r-new' })],
            want: 'unknown',
        },
        {
            name: 'MC version is pinned to the server, other versions are ignored',
            installed: installedRelease,
            candidates: [installedRelease, forge({ id: 'r-121', game_versions: ['1.21'] })],
            want: 'current',
        },
        {
            name: 'loader is pinned to the server',
            installed: installedRelease,
            candidates: [installedRelease, forge({ id: 'r-fabric', loaders: ['fabric'] })],
            want: 'current',
        },
        {
            name: 'never moves to an older build',
            installed: { ...installedRelease, date_published: '2026-09-01T00:00:00Z' },
            candidates: [forge({ id: 'older', date_published: '2026-02-01T00:00:00Z' })],
            want: 'current',
        },
        {
            // The run plans it with Core and installs the dependency first.
            name: 'missing required dependency is updated, not skipped',
            installed: installedRelease,
            candidates: [forge({ id: 'r2', dependencies: [{ project_id: 'dragons-plus', dependency_type: 'required' }] })],
            want: 'update:r2',
        },
        {
            name: 'installed required dependency is fine',
            installed: installedRelease,
            candidates: [forge({ id: 'r2', dependencies: [{ project_id: 'dep-a', dependency_type: 'required' }] })],
            want: 'update:r2',
        },
        {
            name: 'missing optional dependency is fine',
            installed: installedRelease,
            candidates: [forge({ id: 'r2', dependencies: [{ project_id: 'nope', dependency_type: 'optional' }] })],
            want: 'update:r2',
        },
        {
            name: 'installed incompatible dependency is skipped',
            installed: installedRelease,
            candidates: [forge({ id: 'r2', dependencies: [{ project_id: 'dep-a', dependency_type: 'incompatible' }] })],
            want: 'skip:r2',
        },
    ];

    for (const c of cases) {
        it(c.name, () => {
            const plan = planModUpdate('inst', c.candidates, { ...target, installed: c.installed, installedAt: c.installedAt });
            const got = plan.kind === 'current' || plan.kind === 'unknown' ? plan.kind : `${plan.kind}:${plan.build.id}`;
            expect(got).toBe(c.want);
        });
    }
});

describe('dependencyProblems', () => {
    it('names missing required and installed incompatible projects, ignores embedded and self', () => {
        const v = version({
            id: 'r2',
            dependencies: [
                { project_id: 'dragons-plus', dependency_type: 'required' },
                { version_id: 'ver-only', dependency_type: 'required' },
                { project_id: 'jei', dependency_type: 'incompatible' },
                { project_id: 'absent', dependency_type: 'incompatible' },
                { project_id: 'lib', dependency_type: 'embedded' },
                { project_id: 'proj', dependency_type: 'required' },
            ],
        });
        expect(dependencyProblems(v, new Set(['jei']))).toEqual({
            missing: ['dragons-plus', 'ver-only'],
            incompatible: ['jei'],
        });
    });
});

// A dependency Modrinth names only by a build used to be looked up in the set
// of PROJECT ids, where a version id never is: always "missing", by raw id.
describe('dependencyProblems with version-only dependencies', () => {
    const v = (deps: ModrinthVersion['dependencies']) => version({ id: 'r2', dependencies: deps });
    const resolved = new Map([['v-dep-a', 'dep-a'], ['v-self', 'proj'], ['v-gone', 'gone']]);
    const installed = new Set(['dep-a']);

    it.each([
        ['resolved to an installed project', [{ version_id: 'v-dep-a', dependency_type: 'required' }], { missing: [], incompatible: [] }],
        ['resolved to its own project', [{ version_id: 'v-self', dependency_type: 'required' }], { missing: [], incompatible: [] }],
        ['resolved to an absent project', [{ version_id: 'v-gone', dependency_type: 'required' }], { missing: ['gone'], incompatible: [] }],
        ['unresolved required', [{ version_id: 'v-unknown', dependency_type: 'required' }], { missing: ['v-unknown'], incompatible: [] }],
        ['unresolved incompatible', [{ version_id: 'v-unknown', dependency_type: 'incompatible' }], { missing: [], incompatible: [] }],
        ['incompatible resolved to an installed project', [{ version_id: 'v-dep-a', dependency_type: 'incompatible' }], { missing: [], incompatible: ['dep-a'] }],
    ])('%s', (_name, deps, want) => {
        expect(dependencyProblems(v(deps), installed, resolved)).toEqual(want);
    });

    it('a version-only dependency is resolved for planModUpdate', () => {
        const build = version({ id: 'r2', loaders: ['forge'], date_published: '2026-06-01T00:00:00Z', dependencies: [{ version_id: 'v-dep-a', dependency_type: 'required' }] });
        const plan = planModUpdate('inst', [build], {
            loader: 'forge', mcVersion: '1.20.1', installed: version({ id: 'inst', loaders: ['forge'] }),
            installedProjectIds: installed, versionProjects: resolved,
        });
        expect(plan.kind).toBe('update');
    });

    it('lists only the version ids that need resolving', () => {
        expect(unresolvedDependencyVersionIds(v([
            { version_id: 'a', dependency_type: 'required' },
            { version_id: 'b', project_id: 'pb', dependency_type: 'required' },
            { version_id: 'c', dependency_type: 'optional' },
            { version_id: 'd', dependency_type: 'incompatible' },
        ]))).toEqual(['a', 'd']);
    });
});

describe('pickPreferredVersion', () => {
    const rel = version({ id: 'rel', version_type: 'release', date_published: '2026-01-01T00:00:00Z' });
    const beta = version({ id: 'beta', version_type: 'beta', date_published: '2026-02-01T00:00:00Z' });
    const alpha = version({ id: 'alpha', version_type: 'alpha', date_published: '2026-03-01T00:00:00Z' });
    const f = { loaders: ['fabric'], mcVersions: ['1.20.1'] };

    it('takes a release over a newer beta and alpha', () => {
        expect(pickPreferredVersion([alpha, beta, rel], f)?.id).toBe('rel');
    });

    it('falls back to beta, then alpha', () => {
        expect(pickPreferredVersion([alpha, beta], f)?.id).toBe('beta');
        expect(pickPreferredVersion([alpha], f)?.id).toBe('alpha');
    });

    it('still honours the loader and MC filter', () => {
        const forgeRel = version({ id: 'forge', loaders: ['forge'], version_type: 'release' });
        expect(pickPreferredVersion([forgeRel, beta], f)?.id).toBe('beta');
        expect(pickPreferredVersion([], f)).toBeNull();
    });
});

describe('rowUpdateTarget', () => {
    const rel1 = version({ id: 'rel1', version_type: 'release', date_published: '2026-01-01T00:00:00Z' });
    const beta2 = version({ id: 'beta2', version_type: 'beta', date_published: '2026-02-01T00:00:00Z' });
    const rel3 = version({ id: 'rel3', version_type: 'release', date_published: '2026-03-01T00:00:00Z' });
    const beta4 = version({ id: 'beta4', version_type: 'beta', date_published: '2026-04-01T00:00:00Z' });

    it('offers the newest release, not a newer beta', () => {
        expect(rowUpdateTarget('rel1', [rel1, beta2, rel3, beta4])?.id).toBe('rel3');
    });

    it('past the newest release, offers a newer pre-release or nothing', () => {
        expect(rowUpdateTarget('rel3', [rel1, beta2, rel3, beta4])?.id).toBe('beta4');
        expect(rowUpdateTarget('rel1', [rel1, beta2])?.id).toBe('beta2');
        expect(rowUpdateTarget('rel3', [rel1, rel3])).toBeNull();
    });

    it('never moves a newer installed beta back to an older release', () => {
        expect(rowUpdateTarget('beta2', [rel1, beta2])).toBeNull();
    });

    it('uses the whole list when the installed build is not in it', () => {
        expect(rowUpdateTarget('gone', [rel1, beta2])?.id).toBe('rel1');
    });
});
