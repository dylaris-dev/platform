import { afterEach, describe, expect, it, vi } from 'vitest';
import { getPackLoaderVersions, getPackMinecraftVersions } from './packMetadata';
import { getModrinthVersionsOrThrow } from './modrinth';
import { getPackDetails, uploadContent } from './packs';

afterEach(() => { vi.unstubAllGlobals(); });
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });

describe('pack metadata requests', () => {
    it('distinguishes an unavailable provider from an empty compatibility list', async () => {
        vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json({ success: false, message: 'Provider unavailable' }, 502)).mockResolvedValueOnce(json({ success: true, versions: [] })));
        await expect(getPackLoaderVersions('forge', '1.20.1')).rejects.toThrow('Provider unavailable');
        await expect(getPackLoaderVersions('forge', '1.20.1')).resolves.toEqual([]);
    });

    it('keeps the Minecraft filter paired with the selected loader', async () => {
        const fetchMock = vi.fn().mockResolvedValue(json({ success: true, versions: [{ version: '47.4.28', stable: true }] }));
        vi.stubGlobal('fetch', fetchMock);
        await getPackLoaderVersions('forge', '1.20.1');
        expect(fetchMock.mock.calls[0][0]).toContain('/versions/loaders?software=forge&minecraft=1.20.1');
    });

    it('uses individual Minecraft releases, not the server picker major grouping', async () => {
        vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json({ versions: [{ major: '1.20', build: '1.20.1' }, { major: '1.20', build: '1.20.4' }] })));
        await expect(getPackMinecraftVersions()).resolves.toEqual(['1.20.1', '1.20.4']);
    });

    it('does not silently label a failed Modrinth version lookup as no compatible versions', async () => {
        vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json({}, 502)).mockResolvedValueOnce(json([])));
        await expect(getModrinthVersionsOrThrow('kubejs', { loaders: ['forge'], versions: ['1.20.1'] })).rejects.toThrow('502');
        await expect(getModrinthVersionsOrThrow('kubejs')).resolves.toEqual([]);
    });
});

describe('pack access and upload contracts', () => {
    it('fails closed when effective edit/delete rights are absent', async () => {
        vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json({ pack: { id: 1 } })));
        const result = await getPackDetails(1);
        expect(result.permissions).toEqual({ canEdit: false, canDelete: false, isOwner: false });
    });

    it('sends a raw config target without an archive flag or an explicit multipart Content-Type', async () => {
        const fetchMock = vi.fn().mockResolvedValue(json({ success: true }));
        vi.stubGlobal('fetch', fetchMock);
        await uploadContent(1, 2, new File(['ServerEvents.recipes(() => {})'], 'recipes.js'), 'both', 'config', { targetPath: 'kubejs/server_scripts/recipes.js' });
        const request = fetchMock.mock.calls[0][1] as RequestInit;
        const body = request.body as FormData;
        expect(body.get('targetPath')).toBe('kubejs/server_scripts/recipes.js');
        expect(body.has('archive')).toBe(false);
        expect(new Headers(request.headers).has('Content-Type')).toBe(false);
    });

    it('opts config bundles into archive extraction explicitly', async () => {
        const fetchMock = vi.fn().mockResolvedValue(json({ success: true }));
        vi.stubGlobal('fetch', fetchMock);
        await uploadContent(1, 2, new File(['zip'], 'configs.zip'), 'server', 'config', { archive: true });
        const body = (fetchMock.mock.calls[0][1] as RequestInit).body as FormData;
        expect(body.get('archive')).toBe('true');
        expect(body.has('targetPath')).toBe(false);
    });
});
