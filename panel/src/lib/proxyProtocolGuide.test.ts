import { describe, it, expect } from 'vitest';
import {
    proxyModeStatus,
    proxyProtocolGuide, proxyProtocolEnableMessage,
    PROXY_PROTOCOL_LINK, PROXY_PROTOCOL_RESTART, PROXY_PROTOCOL_SECURITY,
    HAPROXYCOMPAT, PROXY_PROTOCOL_SUPPORT, NG_PROXY_SUPPORT, PROXY_SETUP_ROWS,
} from './proxyProtocolGuide';

describe('proxyProtocolGuide', () => {
    const cases: { software: string; want: ReturnType<typeof proxyProtocolGuide> }[] = [
        { software: 'paper', want: { supported: true, file: 'config/paper-global.yml', where: 'in the proxies: section', line: 'proxy-protocol: true' } },
        { software: 'purpur', want: { supported: true, file: 'config/paper-global.yml', where: 'in the proxies: section', line: 'proxy-protocol: true' } },
        { software: 'velocity', want: { supported: true, file: 'velocity.toml', where: 'in the [advanced] section', line: 'haproxy-protocol = true' } },
        { software: 'bungeecord', want: { supported: true, file: 'config.yml', where: 'on the listener', line: 'proxy_protocol: true' } },
        { software: 'waterfall', want: { supported: true, file: 'config.yml', where: 'on the listener', line: 'proxy_protocol: true' } },
        { software: 'folia', want: { supported: true, file: 'config/paper-global.yml', where: 'in the proxies: section', line: 'proxy-protocol: true' } },
        { software: 'fabric', want: { supported: true, mods: [HAPROXYCOMPAT, PROXY_PROTOCOL_SUPPORT] } },
        { software: 'neoforge', want: { supported: true, mods: [HAPROXYCOMPAT] } },
        { software: 'forge', want: { supported: true, mods: [NG_PROXY_SUPPORT] } },
        { software: 'vanilla', want: { supported: false } },
        { software: 'Paper', want: { supported: true, file: 'config/paper-global.yml', where: 'in the proxies: section', line: 'proxy-protocol: true' } },
        { software: 'upload', want: { supported: true } },
        { software: '', want: { supported: true } },
    ];
    for (const c of cases) {
        it(`${c.software || '(empty)'}`, () => {
            expect(proxyProtocolGuide(c.software)).toEqual(c.want);
        });
    }
});

describe('proxyProtocolEnableMessage', () => {
    it('names the file and line, the restart and the security rule', () => {
        const m = proxyProtocolEnableMessage('paper', false);
        expect(m).toContain('config/paper-global.yml');
        expect(m).toContain('proxy-protocol: true');
        expect(m).toContain(PROXY_PROTOCOL_RESTART);
        expect(m).toContain(PROXY_PROTOCOL_SECURITY);
        expect(m).not.toContain(PROXY_PROTOCOL_LINK);
    });
    // Copy-safe: the value is a paragraph of its own with nothing after it.
    for (const [software, first, line] of [
        ['paper', 'In config/paper-global.yml, in the proxies: section, set:', 'proxy-protocol: true'],
        ['velocity', 'In velocity.toml, in the [advanced] section, set:', 'haproxy-protocol = true'],
        ['waterfall', 'In config.yml, on the listener, set:', 'proxy_protocol: true'],
    ]) {
        it(`puts the ${software} value on its own paragraph`, () => {
            const paras = proxyProtocolEnableMessage(software, false).split('\n\n');
            expect(paras[0]).toBe(first);
            expect(paras[1]).toBe(line);
        });
    }
    it('lists the mods for a modded server', () => {
        expect(proxyProtocolEnableMessage('fabric', false)).toContain('HAProxyCompat, Proxy Protocol Support');
        expect(proxyProtocolEnableMessage('forge', false)).toContain('NG Proxy Support (Minecraft 1.20.1)');
        expect(proxyProtocolEnableMessage('fabric', false)).toContain('trusted proxies');
    });
    it('says the link needs an update when traffic goes through one', () => {
        expect(proxyProtocolEnableMessage('paper', true)).toContain(PROXY_PROTOCOL_LINK);
    });
    it('falls back to the generic text for unknown software', () => {
        const m = proxyProtocolEnableMessage('', true);
        expect(m).toContain('PROXY protocol v2');
        expect(m).toContain(PROXY_PROTOCOL_SECURITY);
        expect(m).toContain(PROXY_PROTOCOL_LINK);
    });
});

describe('proxyModeStatus', () => {
    it('says what forwarding does now, and what to do in auto mode', () => {
        expect(proxyModeStatus('off', false, 'yes')).toMatch(/^Off/);
        expect(proxyModeStatus('on', true, 'unknown')).toMatch(/^Always on: every connection/);
        expect(proxyModeStatus('on', false, 'unknown')).toMatch(/not applied/);
        expect(proxyModeStatus('auto', true, 'yes')).toMatch(/^Forwarding/);
        expect(proxyModeStatus('auto', false, 'no')).toMatch(/Install a PROXY protocol plugin/);
        expect(proxyModeStatus('auto', false, 'unknown')).toMatch(/has not been checked/);
    });
});


describe('PROXY_SETUP_ROWS', () => {
    // Every software with a guide has a setup row, so the panel can mark it,
    // and every row with mods links to Modrinth only.
    it('covers every installer the guide knows', () => {
        for (const sw of ['paper', 'purpur', 'folia', 'velocity', 'bungeecord', 'waterfall', 'fabric', 'neoforge', 'forge', 'vanilla']) {
            expect(PROXY_SETUP_ROWS.some(r => r.software.includes(sw))).toBe(true);
        }
        for (const m of PROXY_SETUP_ROWS.flatMap(r => r.mods ?? [])) {
            expect(m.url).toMatch(/^https:\/\/modrinth\.com\/mod\/[a-z-]+$/);
        }
    });
});
