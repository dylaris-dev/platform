import { afterEach, describe, expect, it, vi } from 'vitest';

import {
    fetchRetryingRateLimit, getModrinthVersions, searchModrinth, getModrinthVersionsByIds, getModrinthProjectsByIds,
    getLatestVersionsFromHashes, planModInstall, type ModrinthVersion, type InstalledMod,
} from './modrinth';
import { checkModUpdates, type UpdateCheckApi } from '@/lib/bulkModUpdate';

afterEach(() => {
    vi.unstubAllGlobals();
});

const json = (body: unknown, status = 200, headers: Record<string, string> = {}) =>
    new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json', ...headers } });

// Update all used to read a rate-limited answer as "no versions", which it then
// counted as unchecked: on a 343-mod server two thirds came back that way.
describe('fetchRetryingRateLimit', () => {
    it.each([
        ['429 from Core', 429],
        ['503 cooldown from Modrinth', 503],
    ])('waits Retry-After and retries a %s', async (_name, status) => {
        const fetchMock = vi.fn()
            .mockResolvedValueOnce(json({}, status, { 'Retry-After': '2' }))
            .mockResolvedValueOnce(json(['ok']));
        vi.stubGlobal('fetch', fetchMock);
        const wait = vi.fn(async () => {});
        const res = await fetchRetryingRateLimit('/x', {}, { wait });
        expect(res.status).toBe(200);
        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(wait).toHaveBeenCalledWith(2000);
    });

    it('a 503 without Retry-After is an answer, not a rate limit', async () => {
        const fetchMock = vi.fn().mockResolvedValue(json({}, 503));
        vi.stubGlobal('fetch', fetchMock);
        const res = await fetchRetryingRateLimit('/x', {}, { wait: async () => {} });
        expect(res.status).toBe(503);
        expect(fetchMock).toHaveBeenCalledTimes(1);
    });

    it('gives up after the retries, and caps the wait', async () => {
        const fetchMock = vi.fn().mockResolvedValue(json({}, 429, { 'Retry-After': '600' }));
        vi.stubGlobal('fetch', fetchMock);
        const wait = vi.fn(async () => {});
        const res = await fetchRetryingRateLimit('/x', {}, { retries: 2, maxWaitMs: 5000, wait });
        expect(res.status).toBe(429);
        expect(fetchMock).toHaveBeenCalledTimes(3);
        expect(wait).toHaveBeenCalledWith(5000);
    });

    it('getModrinthVersions survives a 429 instead of answering []', async () => {
        vi.useFakeTimers();
        try {
            vi.stubGlobal('fetch', vi.fn()
                .mockResolvedValueOnce(json({}, 429, { 'Retry-After': '1' }))
                .mockResolvedValueOnce(json([{ id: 'v1' }])));
            const p = getModrinthVersions('sodium');
            await vi.advanceTimersByTimeAsync(1000);
            await expect(p).resolves.toEqual([{ id: 'v1' }]);
        } finally {
            vi.useRealTimers();
        }
    });
});

// Search used to answer null on a 429 with no retry, and the tab read that as
// "No projects match."
describe('searchModrinth', () => {
    it('waits out a rate limit', async () => {
        vi.useFakeTimers();
        try {
            vi.stubGlobal('fetch', vi.fn()
                .mockResolvedValueOnce(json({}, 429, { 'Retry-After': '1' }))
                .mockResolvedValueOnce(json({ hits: [], offset: 0, limit: 20, total_hits: 0 })));
            const p = searchModrinth({ query: 'x' });
            await vi.advanceTimersByTimeAsync(1000);
            await expect(p).resolves.toMatchObject({ hits: [] });
        } finally {
            vi.useRealTimers();
        }
    });

    it('a failure is null, an empty result is not', async () => {
        vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json({}, 502)));
        await expect(searchModrinth({ query: 'x' })).resolves.toBeNull();
        vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('offline')));
        await expect(searchModrinth({ query: 'x' })).resolves.toBeNull();
        vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json({ hits: [], offset: 0, limit: 20, total_hits: 0 })));
        await expect(searchModrinth({ query: 'x' })).resolves.not.toBeNull();
    });
});

// --- A fake Core + Modrinth, so the check below runs through the real client
// and its request count is the number of HTTP calls it would really make.

const hashOf = (id: string) => id.padEnd(128, '0').replace(/[^0-9a-f]/g, 'a');

interface FakeVersion extends ModrinthVersion { }

function fakeModrinth(versions: FakeVersion[], titles: Record<string, string>) {
    const byId = new Map(versions.map(v => [v.id, v]));
    const byHash = new Map(versions.flatMap(v => v.files.map(f => [f.hashes.sha512!, v] as const)));
    const calls: string[] = [];
    const fetchMock = vi.fn(async (input: string, init?: RequestInit) => {
        const u = new URL(input, 'http://core');
        const path = u.pathname.replace(/^.*\/modrinth\//, '/');
        calls.push(path);
        const ids = (u.searchParams.get('ids') ?? '').split(',').filter(Boolean);
        if (ids.length > 100) return json({ error: 'too many' }, 400);
        if (path === '/versions') return json(ids.flatMap(id => byId.get(id) ?? []));
        if (path === '/projects') return json(ids.filter(id => titles[id]).map(id => ({ id, slug: id, title: titles[id] })));
        if (path === '/version-files/update') {
            const body = JSON.parse(String(init?.body));
            const out: Record<string, ModrinthVersion> = {};
            for (const h of body.hashes as string[]) {
                const inst = byHash.get(h);
                if (!inst) continue;
                const latest = versions
                    .filter(v => v.project_id === inst.project_id
                        && v.loaders.some(l => body.loaders.includes(l))
                        && v.game_versions.some(g => body.game_versions.includes(g))
                        && (!body.version_types?.length || body.version_types.includes(v.version_type)))
                    .sort((a, b) => b.date_published.localeCompare(a.date_published))[0];
                if (latest) out[h] = latest;
            }
            return json(out);
        }
        const m = path.match(/^\/project\/([^/]+)\/versions$/);
        if (m) {
            return json(versions.filter(v => v.project_id === m[1]
                && v.loaders.includes(u.searchParams.get('loaders') ?? '')
                && v.game_versions.includes(u.searchParams.get('versions') ?? '')));
        }
        return json({}, 404);
    });
    vi.stubGlobal('fetch', fetchMock);
    return calls;
}

const ver = (project: string, id: string, over: Partial<ModrinthVersion> = {}): FakeVersion => ({
    id, project_id: project, name: id, version_number: id,
    game_versions: ['1.20.1'], loaders: ['forge'], version_type: 'release', featured: false,
    date_published: '2026-01-01T00:00:00Z',
    files: [{ hashes: { sha512: hashOf(id) }, url: `https://cdn.modrinth.com/${id}.jar`, filename: `${id}.jar`, primary: true, size: 1 }],
    ...over,
});

const realApi: UpdateCheckApi = {
    versionsByIds: getModrinthVersionsByIds,
    latestFromHashes: getLatestVersionsFromHashes,
    projectVersions: getModrinthVersions,
    projectsByIds: getModrinthProjectsByIds,
};

describe('checkModUpdates on a 343-mod server', () => {
    it('decides every mod in a handful of requests, without crossing channels', async () => {
        const N = 343;
        const versions: FakeVersion[] = [];
        const mods: InstalledMod[] = [];
        for (let i = 0; i < N; i++) {
            const p = `p${i}`;
            // Every tenth mod runs a beta; the rest run releases.
            const installedType = i % 10 === 0 ? 'beta' : 'release';
            versions.push(ver(p, `${p}a`, { version_type: installedType }));
            // Even mods have a newer release, every third a newer beta (newest of all).
            if (i % 2 === 0) versions.push(ver(p, `${p}b`, { date_published: '2026-03-01T00:00:00Z' }));
            if (i % 3 === 0) versions.push(ver(p, `${p}c`, { version_type: 'beta', date_published: '2026-05-01T00:00:00Z' }));
            mods.push({
                id: i, serverId: 32, subServerName: 'main',
                modrinthProjectId: p, modrinthProjectSlug: p, modrinthVersionId: `${p}a`,
                title: `Mod ${i}`, fileName: `${p}a.jar`,
                // Five old rows carry no hash and must be asked one by one.
                sha512: i < 5 ? '' : hashOf(`${p}a`),
                installedAt: '2026-01-02T00:00:00Z',
            });
        }
        // p2's newer release requires a build of p4 (installed) by version id only,
        // p8's requires a build of an absent project, also by version id only.
        versions.push(ver('p4', 'p4old', { date_published: '2025-01-01T00:00:00Z' }));
        versions.push(ver('lib', 'libv', { date_published: '2025-01-01T00:00:00Z' }));
        versions.find(v => v.id === 'p2b')!.dependencies = [{ version_id: 'p4old', dependency_type: 'required' }];
        versions.find(v => v.id === 'p8b')!.dependencies = [{ version_id: 'libv', dependency_type: 'required' }];

        const calls = fakeModrinth(versions, { lib: 'Shared Lib' });
        const progress: number[] = [];
        const got = await checkModUpdates(mods, { loader: 'forge', mcVersion: '1.20.1' }, realApi, d => progress.push(d));
        expect(got).not.toBeNull();

        // 4 x versions?ids, 2 hash groups (release, beta), 5 one-by-one, 1 dependency
        // resolution. No naming call: nothing is skipped. The old loop made at
        // least one per mod: 343.
        expect(calls.length).toBe(12);
        expect(calls.filter(c => c.startsWith('/project/'))).toHaveLength(5);
        expect(progress.at(-1)).toBe(N);

        const plannedById = new Map(got!.planned.map(p => [p.mod.modrinthProjectId, p.build.id]));
        // A release never moves onto a beta, even when the beta is newest.
        expect(plannedById.get('p6')).toBe('p6b');
        expect(plannedById.has('p3')).toBe(false); // release, only a newer beta
        // A beta may take the newest beta.
        expect(plannedById.get('p30')).toBe('p30c');
        // The version-only dependency resolves to an installed project: no skip.
        expect(plannedById.get('p2')).toBe('p2b');
        // The one that needs an absent project is planned too: the run plans it
        // with Core and installs Shared Lib first.
        expect(plannedById.get('p8')).toBe('p8b');
        expect(got!.skipped).toEqual([]);
        expect(got!.unknown).toBe(0);
        expect(got!.planned.length + got!.skipped.length + got!.current).toBe(N);
    });

    it('a failed bulk lookup is unknown for every mod, not a per-mod storm', async () => {
        const fetchMock = vi.fn(async () => json({}, 500));
        vi.stubGlobal('fetch', fetchMock);
        const mods = [1, 2, 3].map(i => ({
            id: i, serverId: 1, subServerName: 'main', modrinthProjectId: `p${i}`, modrinthProjectSlug: `p${i}`,
            modrinthVersionId: `v${i}`, title: '', fileName: '', sha512: '', installedAt: '',
        }));
        const got = await checkModUpdates(mods, { loader: 'forge', mcVersion: '1.20.1' }, realApi);
        expect(got).toEqual({ planned: [], skipped: [], current: 0, unknown: 3 });
        expect(fetchMock).toHaveBeenCalledTimes(1);
    });
});

describe('planModInstall', () => {
    it('keeps the status of a 401', async () => {
        vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('unauthorized', { status: 401 })));
        const res = await planModInstall(7, 'v1');
        expect(res).toMatchObject({ ok: false, status: 401 });
    });
});
