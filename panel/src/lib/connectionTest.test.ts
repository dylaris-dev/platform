import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { readConnTest } from './connectionTest';

// The point of the shared lifecycle is that a failed test says WHICH step
// failed. These tests are about that attribution, not about wording: telling an
// operator "not reachable" when the host answered and refused a password sends
// them to the firewall for a credential problem, and back again.

describe('reading a test endpoint answer', () => {
    it('keeps the stage the server reported', () => {
        const unreachable = readConnTest(
            { success: true, ok: false, stage: 'unreachable', message: 'nothing answered' }, 'ok');
        expect(unreachable.stage).toBe('unreachable');
        expect(unreachable.heading).toBe('Not reachable');
        expect(unreachable.severity).toBe('error');

        const rejected = readConnTest(
            { success: true, ok: false, stage: 'rejected', message: 'wrong password' }, 'ok');
        expect(rejected.heading).toBe('Reached, but refused');
    });

    // An older Core, or an endpoint that has not been given a stage yet. The
    // fallback must never invent "unreachable": claiming a host is down when
    // nobody dialled it is a worse answer than a vaguer one.
    it('falls back to rejected, never to unreachable', () => {
        expect(readConnTest({ success: false, message: 'boom' }, 'ok').stage).toBe('rejected');
        expect(readConnTest({ success: true, ok: true }, 'Connected.').stage).toBe('ok');
    });

    it('carries the success message through when the server sends none', () => {
        expect(readConnTest({ success: true, ok: true }, 'Connected.').message).toBe('Connected.');
    });

    // Three severities, because "connected but no TimescaleDB" is real: it
    // works, and it will hurt later. A two-state banner has to call that either
    // a pass or a failure, and both are wrong.
    it('a warning is its own severity, not a pass and not a failure', () => {
        const warned = readConnTest(
            { success: true, ok: true, severity: 'warning', message: 'no timescaledb here' }, 'ok');
        expect(warned.severity).toBe('warning');
        expect(warned.stage).toBe('ok');
        expect(warned.heading).toContain('caveat');
    });

    // A warning that arrives WITHOUT an explicit severity - which is how the
    // storage endpoints send the ephemeral-path warning - must still not be
    // rendered green.
    it('an unlabelled warning still reads as a warning', () => {
        const res = readConnTest({ success: true, ok: true, warning: 'this path is not durable' }, 'ok');
        expect(res.severity).toBe('warning');
        expect(res.message).toContain('durable');
    });
});

describe('the buttons that use it', () => {
    const read = (rel: string) => readFileSync(join(__dirname, '..', rel), 'utf8');

    // Every one of these ran a request that could not be cancelled and left the
    // button clickable while it was in flight. The check is deliberately on the
    // source: the failure mode is a screen that forgets to opt in, and that is
    // invisible to a unit test of the hook itself.
    const wired = [
        'components/settings/MetricsDatabaseCard.tsx',
        'components/settings/DatabaseTab.tsx',
        'components/settings/TicketDBTab.tsx',
        'components/settings/CoreStorageTab.tsx',
        'components/settings/StorageConnectionsTab.tsx',
    ];

    it.each(wired)('%s runs its test through the shared lifecycle', rel => {
        expect(read(rel)).toContain('useConnectionTest');
    });

    // These two test one row of a list rather than one form, so they drive the
    // request themselves - but they must still take the deadline from here
    // rather than inventing one, and they must render the shared verdict.
    const rowTesters = [
        'components/settings/BackupsTab.tsx',
        'components/settings/StorageConnectionsTab.tsx',
    ];

    it.each(rowTesters)('%s uses the shared deadline and banner', rel => {
        const src = read(rel);
        expect(src).toContain('CONN_TEST_TIMEOUT_MS');
        expect(src).toContain('ConnectionTestNote');
        // A row test disables every row's button, not only its own: two
        // verdicts on screen with one slot to show them is how the wrong row
        // gets believed.
        expect(src).toContain('testingId !== null');
    });

    it('no screen keeps a hand-rolled testing flag beside the shared one', () => {
        for (const rel of wired) {
            expect(read(rel)).not.toContain('setTesting(true)');
        }
    });
});
