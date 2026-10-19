import { describe, it, expect } from 'vitest';
import { buildMismatch, modFitKnown } from './modFit';

const build = (loaders: string[], game_versions: string[]) => ({ loaders, game_versions });

describe('buildMismatch', () => {
    const cases: { name: string; loader: string; mc: string; b: ReturnType<typeof build>; want: string | null }[] = [
        { name: 'fits', loader: 'neoforge', mc: '1.21.1', b: build(['neoforge'], ['1.21', '1.21.1']), want: null },
        { name: 'forge build on neoforge', loader: 'neoforge', mc: '1.21.1', b: build(['forge'], ['1.21.1']), want: 'Forge build on a NeoForge server' },
        { name: 'neoforge build on forge', loader: 'forge', mc: '1.20.1', b: build(['neoforge'], ['1.20.1']), want: 'NeoForge build on a Forge server' },
        { name: 'several wrong loaders', loader: 'neoforge', mc: '1.21.1', b: build(['forge', 'fabric'], ['1.21.1']), want: 'Forge/Fabric build on a NeoForge server' },
        { name: 'multi-loader build that includes ours', loader: 'neoforge', mc: '1.21.1', b: build(['forge', 'neoforge'], ['1.21.1']), want: null },
        { name: 'wrong MC, newest named', loader: 'neoforge', mc: '1.21.1', b: build(['neoforge'], ['26.2', '26.3']), want: 'Built for MC 26.3' },
        { name: 'a line is not the exact version', loader: 'forge', mc: '1.21.1', b: build(['forge'], ['1.21']), want: 'Built for MC 1.21' },
        { name: 'loader is checked before MC', loader: 'neoforge', mc: '1.21.1', b: build(['forge'], ['1.21']), want: 'Forge build on a NeoForge server' },
        { name: 'quilt loads fabric', loader: 'quilt', mc: '1.20.1', b: build(['fabric'], ['1.20.1']), want: null },
        { name: 'fabric does not load quilt', loader: 'fabric', mc: '1.20.1', b: build(['quilt'], ['1.20.1']), want: 'Quilt build on a Fabric server' },
        { name: 'case and spaces', loader: ' NeoForge ', mc: ' 1.21.1 ', b: build(['NeoForge'], ['1.21.1']), want: null },
        { name: 'plugin takes an older patch of its line', loader: 'paper', mc: '1.21.4', b: build(['paper'], ['1.21.3']), want: null },
        { name: 'plugin takes the bare line', loader: 'spigot', mc: '1.21.4', b: build(['spigot'], ['1.21']), want: null },
        { name: 'plugin on the 26 scheme', loader: 'purpur', mc: '26.1.2', b: build(['paper'], ['26.1']), want: null },
        { name: 'plugin refuses another line', loader: 'paper', mc: '1.21.4', b: build(['paper'], ['1.20.6']), want: 'Built for MC 1.20.6' },
        { name: 'plugin line is not a prefix', loader: 'paper', mc: '1.2.5', b: build(['bukkit'], ['1.21']), want: 'Built for MC 1.21' },
        { name: 'plugin refuses the next 26 line', loader: 'paper', mc: '26.1.2', b: build(['paper'], ['26.2']), want: 'Built for MC 26.2' },
        { name: 'mod stays exact on a patch', loader: 'fabric', mc: '1.21.4', b: build(['fabric'], ['1.21.3']), want: 'Built for MC 1.21.3' },
        { name: 'proxy ignores MC', loader: 'velocity', mc: '', b: build(['velocity'], ['1.8']), want: null },
        { name: 'unknown loader marks nothing', loader: 'upload', mc: '1.21.1', b: build(['forge'], ['1.0']), want: null },
        { name: 'unknown MC marks nothing', loader: 'forge', mc: '', b: build(['forge'], ['1.0']), want: null },
        { name: 'no game versions', loader: 'forge', mc: '1.20.1', b: build(['forge'], []), want: 'Built for no listed MC version' },
    ];
    for (const c of cases) {
        it(c.name, () => expect(buildMismatch(c.loader, c.mc, c.b)).toBe(c.want));
    }
});

describe('modFitKnown', () => {
    it('needs a known loader and, except for proxies, an MC version', () => {
        expect(modFitKnown('neoforge', '1.21.1')).toBe(true);
        expect(modFitKnown('neoforge', '')).toBe(false);
        expect(modFitKnown('upload', '1.21.1')).toBe(false);
        expect(modFitKnown('waterfall', '')).toBe(true);
    });
});
