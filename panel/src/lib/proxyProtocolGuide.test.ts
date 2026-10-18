import { describe, it, expect } from 'vitest';
import {
    proxyProtocolGuide, proxyProtocolEnableMessage,
    PROXY_PROTOCOL_LINK, PROXY_PROTOCOL_RESTART, PROXY_PROTOCOL_SECURITY,
} from './proxyProtocolGuide';

describe('proxyProtocolGuide', () => {
    const cases: { software: string; want: ReturnType<typeof proxyProtocolGuide> }[] = [
        { software: 'paper', want: { supported: true, file: 'config/paper-global.yml', line: 'proxies.proxy-protocol: true' } },
        { software: 'purpur', want: { supported: true, file: 'config/paper-global.yml', line: 'proxies.proxy-protocol: true' } },
        { software: 'velocity', want: { supported: true, file: 'velocity.toml', line: 'haproxy-protocol = true' } },
        { software: 'bungeecord', want: { supported: true, file: 'config.yml', line: 'proxy_protocol: true' } },
        { software: 'waterfall', want: { supported: true, file: 'config.yml', line: 'proxy_protocol: true' } },
        { software: 'fabric', want: { supported: true, mods: ['Proxy Protocol Support', 'HAProxyCompat'] } },
        { software: 'neoforge', want: { supported: true, mods: ['HAProxyCompat', 'ProperProxyProtocol'] } },
        { software: 'forge', want: { supported: true, mods: ['ProperProxyProtocol', 'NG Proxy Support (Minecraft 1.20.1)'] } },
        { software: 'vanilla', want: { supported: false } },
        { software: 'Paper', want: { supported: true, file: 'config/paper-global.yml', line: 'proxies.proxy-protocol: true' } },
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
        expect(m).toContain('proxies.proxy-protocol: true');
        expect(m).toContain(PROXY_PROTOCOL_RESTART);
        expect(m).toContain(PROXY_PROTOCOL_SECURITY);
        expect(m).not.toContain(PROXY_PROTOCOL_LINK);
    });
    it('lists the mods for a modded server', () => {
        expect(proxyProtocolEnableMessage('fabric', false)).toContain('Proxy Protocol Support, HAProxyCompat');
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
