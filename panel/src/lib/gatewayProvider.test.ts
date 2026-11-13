import { describe, it, expect } from 'vitest';
import {
    visibilityFor, providerErrorText, providerModeOf, outboxWarning, barPercent, isDockerNetworkName,
    switchConfirm, purchaseNote, usageFinalLabel, providerGbToBytes, productLabel, testedAccount,
    type RemoteVisibility,
} from './gatewayProvider';

// Every surface the panel gates. Listed by hand, not derived from the type,
// so a new field without a decision here fails the "all keys" check below.
const SURFACES: (keyof RemoteVisibility)[] = [
    'hubDnsCard', 'hosterDomainEditor', 'routeLimits', 'warpTab', 'voice', 'bedrock', 'edgeMotd',
    'customDomains', 'infrastructureGateway', 'playerCounts', 'tenancy', 'managedLinkKit', 'providerUsage',
];

describe('visibilityFor', () => {
    it('covers every surface', () => {
        expect(Object.keys(visibilityFor('local')).sort()).toEqual([...SURFACES].sort());
    });

    // local is today's panel: every local gateway surface, nothing of the provider.
    it.each(SURFACES)('local: %s', (k) => {
        const expected = !(k === 'managedLinkKit' || k === 'providerUsage');
        expect(visibilityFor('local')[k]).toBe(expected);
    });

    // remote hides each surface whose route answers 409 gateway_remote. Route
    // limits stay: this Core still enforces them per user.
    it.each(SURFACES)('remote: %s', (k) => {
        const expected = k === 'routeLimits' || k === 'managedLinkKit' || k === 'providerUsage';
        expect(visibilityFor('remote')[k]).toBe(expected);
    });
});

describe('providerModeOf', () => {
    it.each([
        [{ gatewayProvider: 'remote' }, 'remote'],
        [{ gatewayProvider: 'local' }, 'local'],
        [{}, 'local'],
        [null, 'local'],
        [{ gatewayProvider: 'REMOTE' }, 'local'],
    ])('%j -> %s', (flags, want) => {
        expect(providerModeOf(flags as { gatewayProvider?: string } | null)).toBe(want);
    });
});

describe('providerErrorText', () => {
    const codes = [
        'unauthorized', 'forbidden', 'no_entitlement', 'billing_suspended', 'rate_limited', 'unreachable',
        'gateway_remote', 'no_key', 'key_from_env', 'not_managed', 'account_suspended', 'missing_scope',
        'tenancy_enabled', 'invalid_url',
    ];
    const generic = providerErrorText('something_new');

    it.each(codes)('%s has its own sentence', (code) => {
        const t = providerErrorText(code);
        expect(t.length).toBeGreaterThan(10);
        expect(t).toMatch(/\.$/);
        expect(t).not.toBe(generic);
    });

    it('an unknown code gets a generic sentence', () => {
        expect(generic).toMatch(/^[A-Z].*\.$/);
    });
});

describe('outboxWarning', () => {
    const now = Date.parse('2026-11-13T10:10:00Z');
    it.each([
        [null, null],
        [{ open: 0 }, null],
        [{ open: 3 }, null],
        [{ open: 3, oldestAt: '2026-11-13T10:09:00Z' }, null],
        [{ open: 3, oldestAt: '2026-11-13T10:08:00Z' }, null],
        [{ open: 3, oldestAt: '2026-11-13T10:07:59Z' }, '3 changes are waiting to reach the gateway provider.'],
        [{ open: 1, oldestAt: '2026-11-13T09:00:00Z' }, '1 change is waiting to reach the gateway provider.'],
        [{ open: 2, oldestAt: 'garbage' }, null],
    ])('%j -> %j', (outbox, want) => {
        expect(outboxWarning(outbox, now)).toBe(want);
    });
});

describe('barPercent', () => {
    it.each([[undefined, 0], [null, 0], [NaN, 0], [-5, 0], [42.4, 42], [150, 100]])('%s -> %s', (p, want) => {
        expect(barPercent(p as number)).toBe(want);
    });
});

describe('isDockerNetworkName', () => {
    it.each([
        ['dylaris_net', true],
        ['byon_dylaris_net', true],
        ['my.net-1', true],
        ['', false],
        ['-lead', false],
        ['has space', false],
        ['a"b', false],
        ['x\ny', false],
        ['a'.repeat(64), false],
        // YAML would read these bare words as something other than a string.
        ['true', false],
        ['False', false],
        ['null', false],
        ['yes', false],
        ['off', false],
        ['123', false],
        ['1.5', false],
        ['net1', true],
    ])('%j -> %s', (name, want) => {
        expect(isDockerNetworkName(name)).toBe(want);
    });
});

describe('switchConfirm', () => {
    it('asks on a mode change, naming the target mode', () => {
        expect(switchConfirm({ mode: 'remote' }, { mode: 'local' })).toMatchObject({
            title: 'Switch to the DYLARIS Managed Gateway?', confirmLabel: 'Switch', destructive: false, focusCancel: true,
        });
        expect(switchConfirm({ mode: 'local' }, { mode: 'remote' })?.title).toMatch(/own gateway/);
    });

    it('asks nothing when the mode stays', () => {
        expect(switchConfirm({ mode: 'remote' }, { mode: 'remote' })).toBeNull();
        expect(switchConfirm({ mode: 'local' }, { mode: 'local' })).toBeNull();
    });
});

describe('usage labels', () => {
    const fmt = (b: number) => `${b}B`;

    it.each([
        [0, 'No extra traffic can be bought.'],
        [null, 'More can be bought.'],
        [500, 'Up to 500000000000B more can be bought.'],
    ])('purchaseNote(%j)', (gb, want) => {
        expect(purchaseNote(gb, fmt)).toBe(want);
    });

    it.each([
        [true, 'Final'],
        [false, 'Estimate, the month is still running'],
        [undefined, 'Estimate, the month is still running'],
    ])('usageFinalLabel(%j)', (final, want) => {
        expect(usageFinalLabel(final)).toBe(want);
    });

    // The provider's GB is decimal: 1 TB = 1000 GB.
    it('converts provider GB to bytes in base 1000', () => {
        expect(providerGbToBytes(1000)).toBe(1e12);
    });
});

describe('productLabel', () => {
    it.each([['managed', 'Managed gateway'], ['route_only', 'Route-only'], ['other', 'other'], [undefined, 'Unknown']])('%j -> %s', (id, want) => {
        expect(productLabel(id as string | undefined)).toBe(want);
    });
});

// A failed test must never sit beside the saved account's summary.
describe('testedAccount', () => {
    const saved = { product: 'managed' };
    it.each([
        ['no test: the saved account', null, saved],
        ['ok test: the tested account', { ok: true, account: { product: 'x' } }, { product: 'x' }],
        ['failed test with an account: that account', { ok: false, account: { product: 'route_only' } }, { product: 'route_only' }],
        ['failed test without one: none', { ok: false }, undefined],
    ])('%s', (_name, test, want) => {
        expect(testedAccount(test as { account?: { product: string } } | null, saved)).toEqual(want);
    });
});
