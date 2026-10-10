import { describe, expect, it } from 'vitest';
import { setupPreviewModel } from './setupPreview';
import type { SubServerInstall } from '@/lib/api/subServerInstalls';

// The servers row describes the ACTIVE sub-server only.
const server = { activeSubServer: 'main', installerType: 'forge', minecraftVersion: '1.20.1', buildNumber: '47.2.0' };
const subs = ['main', 'lobby', 'old'];
const lobby: SubServerInstall = {
    subServerName: 'lobby', installerType: 'paper', mcVersion: '1.21.4', buildVersion: '200',
    installedAt: '2026-10-01T10:00:00Z',
};
const rows = (m: ReturnType<typeof setupPreviewModel>) => Object.fromEntries(m.installed.map(r => [r.label, r.value]));

describe('setupPreviewModel', () => {
    it('describes the active sub-server from the servers row when nothing was recorded', () => {
        const m = setupPreviewModel(server, [lobby], subs, null);
        expect(m).toMatchObject({ subServer: 'main', isActive: true, mcVersion: '1.20.1' });
        expect(rows(m)).toMatchObject({ Software: 'Forge', Minecraft: '1.20.1', Build: '47.2.0' });
    });

    it('previews another sub-server from its own record, not the active one', () => {
        const m = setupPreviewModel(server, [lobby], subs, 'lobby');
        expect(m).toMatchObject({ subServer: 'lobby', isActive: false, mcVersion: '1.21.4' });
        expect(rows(m)).toMatchObject({ Software: 'Paper', Minecraft: '1.21.4', Build: '200' });
    });

    // The row would claim the active sub-server's Forge for it.
    it('knows nothing about a previewed sub-server without a record', () => {
        const m = setupPreviewModel(server, [lobby], subs, 'old');
        expect(m).toMatchObject({ subServer: 'old', isActive: false, mcVersion: '' });
        expect(m.installed).toEqual([]);
    });

    it('falls back to the active one when the preview is gone or is the active one', () => {
        expect(setupPreviewModel(server, [], subs, 'deleted')).toMatchObject({ subServer: 'main', isActive: true });
        expect(setupPreviewModel(server, [], subs, 'main')).toMatchObject({ subServer: 'main', isActive: true });
    });

    it('names a modpack without repeating its version as a build', () => {
        const mp: SubServerInstall = {
            subServerName: 'main', installerType: 'modpack', mcVersion: '1.20.1', buildVersion: 'abc123',
            modrinthProjectId: 'p1', modrinthProjectSlug: 'beyond-depth', modrinthVersionId: 'abc123', loader: 'forge',
            installedAt: '',
        };
        const r = rows(setupPreviewModel(server, [mp], subs, null));
        expect(r).toMatchObject({ Software: 'Modpack', Modpack: 'beyond-depth', 'Pack version': 'abc123', Loader: 'forge' });
        expect(r.Build).toBeUndefined();
        expect(r.Installed).toBeUndefined();
    });
});

// A switch restarts the server and drops its players, so it stays behind the
// explicit button and its confirm. A sidebar click only previews.
describe('sidebar click', () => {
    it('previews and never switches', async () => {
        const { readFileSync } = await import('node:fs');
        const src = readFileSync(new URL('../SetupView.tsx', import.meta.url), 'utf8').replace(/\r\n/g, '\n');
        const onPreview = src.match(/onPreview=\{\(name\) => \{([\s\S]*?)\n {16}\}\}/);
        expect(onPreview?.[1]).toContain('setPreviewSub(');
        expect(onPreview?.[1]).not.toMatch(/setSwitchTarget|switchSubServer/);
        // The preview's own switch goes through the confirm dialog.
        expect(src).toContain('onSwitch: () => setSwitchTarget(preview.subServer),');
    });
});
