import { describe, it, expect } from 'vitest';
import {
    visibilityFor, providerErrorText, providerModeOf, outboxWarning, barPercent, isDockerNetworkName, initialNetwork,
    switchConfirm, purchaseNote, usageFinalLabel, providerGbToBytes, productLabel, testedAccount,
    routingModeWarning, refusalLine, playersTitle, refusalText, routeOptionsFailure,
    RETIRE_CONFIRM, canReplaceLink, retiringLine, ORPHAN_INTRO, showOrphanSection, orphanRevokeConfirm,
    type RemoteVisibility,
} from './gatewayProvider';

// Every surface the panel gates. Listed by hand, not derived from the type,
// so a new field without a decision here fails the "all keys" check below.
const SURFACES: (keyof RemoteVisibility)[] = [
    'hubDnsCard', 'hosterDomainEditor', 'routeLimits', 'warpTab', 'voice', 'bedrock', 'edgeMotd',
    'infrastructureGateway', 'playerCounts', 'tenancy', 'managedLinkKit', 'providerUsage',
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
    // limits stay: this Core still enforces them per user. Own domains are no
    // surface here: they show in both modes, as route-options says. Voice,
    // Bedrock and player counts run through the provider since stage 7b-2.
    it.each(SURFACES)('remote: %s', (k) => {
        const shown: (keyof RemoteVisibility)[] = ['routeLimits', 'managedLinkKit', 'providerUsage', 'voice', 'bedrock', 'playerCounts'];
        expect(visibilityFor('remote')[k]).toBe(shown.includes(k));
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

    it('an unknown code gets the fallback when one is given', () => {
        expect(providerErrorText('something_new', 'Could not add the domain.')).toBe('Could not add the domain.');
        expect(providerErrorText('domain_in_use', 'x')).not.toBe('x');
    });

    it.each([
        'domain_unverified', 'domain_blocked', 'custom_domain_cap', 'custom_domain_limit', 'too_many_domains',
        'custom_domains_off', 'domain_in_use', 'domain_taken', 'change_pending', 'verify_cooldown', 'pending_domains_limit',
        'gateway_provider_unknown',
    ])('own-domain code %s has its own sentence', (code) => {
        const t = providerErrorText(code);
        expect(t).toMatch(/^[A-Z].*\.$/);
        expect(t).not.toBe(generic);
        expect(t).not.toMatch(/belong|another user/i);
    });

    it.each([
        'voice_not_allowed', 'link_without_voice', 'no_relay', 'voice_pool_full', 'no_routes',
        'bedrock_disabled', 'bedrock_unavailable', 'bedrock_one_address', 'voice_one_address', 'own_domain_unsupported',
    ])('voice/Bedrock code %s has its own sentence', (code) => {
        const t = providerErrorText(code);
        expect(t).toMatch(/^[A-Z].*\.$/);
        expect(t).not.toBe(generic);
    });

    // The provider plan's cap is the whole platform's; the per-user one is this
    // platform's. A user must be able to tell which one stopped them.
    it('keeps the platform-wide limit apart from the per-user one', () => {
        const platformWide = "This platform's own-domain limit at the gateway provider is reached. Ask the platform operator.";
        expect(providerErrorText('custom_domain_limit')).toBe(platformWide);
        expect(providerErrorText('too_many_domains')).toBe(platformWide);
        expect(providerErrorText('custom_domain_cap')).not.toBe(platformWide);
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
        ['a'.repeat(64), true],
        // A long compose project prefix is legitimate; Docker has no cap.
        ['p'.repeat(88) + '_dylaris_net', true],
        ['a'.repeat(127), true],
        ['a'.repeat(128), false],
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

describe('initialNetwork', () => {
    it.each([
        [{ network: 'platform_dylaris_net', networkKnown: true, networkHint: 'dylaris_net' }, { value: 'platform_dylaris_net', known: true }],
        // An older node or an older Core: today's editable hint.
        [{ network: '', networkKnown: false, networkHint: 'byon_dylaris_net' }, { value: 'byon_dylaris_net', known: false }],
        [{ networkHint: 'dylaris_net' }, { value: 'dylaris_net', known: false }],
        [{}, { value: '', known: false }],
        // Known without a name, or a name the field would refuse: never read-only.
        [{ network: '', networkKnown: true, networkHint: 'dylaris_net' }, { value: 'dylaris_net', known: false }],
        [{ network: 'x\nservices:', networkKnown: true, networkHint: 'dylaris_net' }, { value: 'dylaris_net', known: false }],
    ])('%j -> %j', (kit, want) => {
        expect(initialNetwork(kit)).toEqual(want);
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

describe('routingModeWarning', () => {
    const text = 'Switch the routing mode to Gateway or Both first. Servers get their addresses from the gateway only in those modes.';
    it.each([
        [false, true, false, text],
        [false, false, true, text],
        [false, false, false, null],
        [true, true, true, null],
        [undefined, true, true, null],
    ])('ok=%j chosen=%j saved=%j', (ok, chosen, saved, want) => {
        expect(routingModeWarning(ok, chosen, saved)).toBe(want);
    });
    it('has its own error text', () => {
        expect(providerErrorText('routing_mode')).toBe(text);
    });
});

describe('refusalLine', () => {
    it('names the change, the address and the answer', () => {
        expect(refusalLine({ op: 'proxy', domain: 'a.example.com', error: 'gateway provider answered 409 proxy_protocol_disabled: off' }))
            .toBe('Real player IP for a.example.com: gateway provider answered 409 proxy_protocol_disabled: off');
        expect(refusalLine({ op: 'other', domain: 'b.example.com', error: 'x' })).toBe('other for b.example.com: x');
        expect(refusalLine({ op: 'delete_custom_domain', domain: 'play.own.net', error: 'x' }))
            .toBe('Release an own-domain proof at the gateway provider for play.own.net: x');
        expect(refusalLine({ op: 'revoke_kit', domain: 'k', error: 'x' })).toBe('Revoke a link for k: x');
    });
});

describe('replacing a node link', () => {
    it('the confirm states the 8 hours, the host limit and that it is final', () => {
        expect(RETIRE_CONFIRM.message).toBe(
            'The link keeps working for 8 hours so players can finish, then it is removed. '
            + "During that time another node can get a link even if your plan's host limit is reached. "
            + "Move this node's servers to the new node before the time is up. This cannot be undone.",
        );
        expect(RETIRE_CONFIRM).toMatchObject({ destructive: true, focusCancel: true });
    });

    it('offers the replacement only for a link that is not being replaced', () => {
        expect(canReplaceLink({})).toBe(true);
        expect(canReplaceLink({ retireAt: '2026-11-13T20:00:00Z' })).toBe(false);
        expect(canReplaceLink(null)).toBe(false);
    });

    it('names the removal time', () => {
        expect(retiringLine('2026-11-13T20:00:00Z', v => `<${v}>`)).toBe('Being replaced. Removed at <2026-11-13T20:00:00Z>.');
    });

    it('has texts for the provider refusals', () => {
        expect(providerErrorText('retire_limit')).toBe('All replacement slots of this plan are in use. Wait until a link being replaced has run out.');
        expect(providerErrorText('retire_budget')).toBe("This plan's monthly number of link replacements is used up.");
        expect(providerErrorText('link_limit')).toBe("This plan's host limit is reached. Remove a node's link, or use Replace this node's link on a node you are retiring.");
    });
});

describe('unknown provider links', () => {
    it('is hidden while empty or not loaded, shown with links or a failed load', () => {
        expect(showOrphanSection(null, '')).toBe(false);
        expect(showOrphanSection([], '')).toBe(false);
        expect(showOrphanSection([{ linkId: 'link-a' }], '')).toBe(true);
        expect(showOrphanSection(null, 'The provider could not be reached.')).toBe(true);
    });

    it('warns about a second installation on the same account', () => {
        expect(ORPHAN_INTRO).toContain('Only revoke a link here if no other DYLARIS installation uses this provider account.');
        expect(orphanRevokeConfirm('old node').message).toContain('another DYLARIS installation');
        expect(orphanRevokeConfirm('').message.startsWith('This link stops connecting')).toBe(true);
    });
});

describe('playersTitle', () => {
    it.each([
        // Without editions (this platform's own edges): today's text.
        [3, undefined, '3 connected through the gateway'],
        [4, { java: 3, bedrock: 1 }, '4 players: 3 Java, 1 Bedrock. Live, can be a few seconds old.'],
        [1, { java: 1, bedrock: 0 }, '1 player: 1 Java, 0 Bedrock. Live, can be a few seconds old.'],
        [2, { java: 0, bedrock: 2 }, '2 players: 0 Java, 2 Bedrock. Live, can be a few seconds old.'],
    ])('%i %j', (total, e, want) => {
        expect(playersTitle(total, e)).toBe(want);
    });
});

describe('refusalText', () => {
    it('prefers the sentence Core wrote for this mode', () => {
        expect(refusalText({ code: 'bedrock_one_address', message: 'Take Bedrock off a.eu.example.com first.' }, 'x'))
            .toBe('Take Bedrock off a.eu.example.com first.');
        expect(refusalText({ code: 'no_relay', error: 'No relay in eu yet.' }, 'x')).toBe('No relay in eu yet.');
    });
    it('reads a bare code in error as a code, not a sentence', () => {
        expect(refusalText({ error: 'gateway_disabled', message: 'Gateway routing is disabled.' }, 'x')).toBe('Gateway routing is disabled.');
        expect(refusalText({ error: 'voice_pool_full' }, 'x')).toBe(providerErrorText('voice_pool_full'));
    });
    it('uses its own sentence where Core has only a generic one', () => {
        expect(refusalText({ code: 'change_pending', error: 'a previous change to this address is still being applied' }, 'x'))
            .toBe(providerErrorText('change_pending'));
    });
    it('falls back without a code or a sentence', () => {
        expect(refusalText({}, 'Voice chat could not be changed.')).toBe('Voice chat could not be changed.');
        expect(refusalText(null, 'Voice chat could not be changed.')).toBe('Voice chat could not be changed.');
    });
});

describe('routeOptionsFailure', () => {
    it('says the provider was not reached on the managed gateway only', () => {
        expect(routeOptionsFailure('remote', true)).toMatch(/gateway provider could not be reached/);
        expect(routeOptionsFailure('remote', true)).not.toMatch(/No domain configured/);
        expect(routeOptionsFailure('remote', false)).toBeNull();
        // Local mode keeps its block as it was.
        expect(routeOptionsFailure('local', true)).toBeNull();
    });
});
