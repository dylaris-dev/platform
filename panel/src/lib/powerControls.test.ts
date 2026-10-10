import { describe, it, expect } from 'vitest';
import {
    advancePending, powerModel, startPending, PENDING_TIMEOUT_MS,
    type PendingPower, type PowerAction, type PowerGates,
} from './powerControls';

const open: PowerGates = { canPower: true, uploadLocked: false, cooldownSeconds: 0, killCooldown: false };
const T0 = 1_000_000;

// Feeds a status sequence through advancePending the way the shell does.
function run(action: PowerAction, from: string, statuses: string[]): PendingPower | null {
    let p: PendingPower | null = startPending(action, from, T0);
    for (const s of statuses) {
        if (!p) break;
        p = advancePending(p, s);
    }
    return p;
}

describe('advancePending', () => {
    it.each([
        // [action, from, sequence, cleared]
        ['restart', 'online', [], false],
        // The old wait cleared here: the target "online" was already true.
        ['restart', 'online', ['online'], false],
        ['restart', 'online', ['starting', 'stopping', 'restarting', 'starting'], false],
        ['restart', 'online', ['starting', 'stopping', 'restarting', 'starting', 'online'], true],
        ['start', 'stopped', ['starting'], false],
        ['start', 'stopped', ['starting', 'online'], true],
        ['start', 'stopped', ['starting', 'offline'], true],
        ['stop', 'online', ['stopping'], false],
        ['stop', 'online', ['online'], false],
        ['stop', 'online', ['stopping', 'stopped'], true],
        ['stop', 'online', ['offline'], true],
        ['kill', 'stopping', ['stopping'], false],
        ['kill', 'stopping', ['stopped'], true],
        ['kill', 'online', ['stopping', 'online'], false],
    ] as [PowerAction, string, string[], boolean][])('%s from %s through %j cleared=%s', (action, from, seq, cleared) => {
        expect(run(action, from, seq) === null).toBe(cleared);
    });
});

describe('powerModel', () => {
    it('stopped: only Start, nothing else', () => {
        const m = powerModel('stopped', null, T0, open);
        expect(m.primary).toMatchObject({ action: 'start', label: 'Start', disabled: false });
        expect(m.showSecondary).toBe(false);
        expect(m.killDisabled).toBe(true);
        expect(m.busy).toBe(false);
    });

    it('online: Stop primary, Restart and the menu with Force kill', () => {
        const m = powerModel('online', null, T0, open);
        expect(m.primary).toMatchObject({ action: 'stop', label: 'Stop', variant: 'stop', disabled: false });
        expect(m).toMatchObject({ showSecondary: true, restartDisabled: false, menuDisabled: false, killDisabled: false });
    });

    it('stop in progress: elapsed seconds, Restart locked, Force kill still allowed', () => {
        const p = { ...startPending('stop', 'online', T0), left: true };
        const m = powerModel('stopping', p, T0 + 12_400, open);
        expect(m.primary).toMatchObject({ label: 'Stopping... 12s', icon: 'busy', disabled: true });
        expect(m.primary.title).toMatch(/Saving the world/);
        expect(m).toMatchObject({ restartDisabled: true, menuDisabled: false, killDisabled: false, busy: true });
    });

    it('kill in progress: nothing clickable', () => {
        for (const status of ['online', 'stopping']) {
            const m = powerModel(status, startPending('kill', status, T0), T0 + 1000, open);
            expect(m.primary).toMatchObject({ label: 'Killing...', disabled: true });
            expect(m).toMatchObject({ restartDisabled: true, menuDisabled: true, killDisabled: true });
        }
    });

    it.each([
        ['start', 'stopped', 'Starting...'],
        ['start', 'starting', 'Starting...'],
        ['restart', 'online', 'Restarting...'],
        ['restart', 'stopping', 'Restarting...'],
    ] as [PowerAction, string, string][])('local %s at %s: %s, everything else locked', (action, status, label) => {
        const m = powerModel(status, startPending(action, 'x', T0), T0 + 5000, open);
        expect(m.primary).toMatchObject({ label, disabled: true });
        expect(m).toMatchObject({ showSecondary: true, restartDisabled: true, menuDisabled: true, killDisabled: true });
    });

    it('external stopping without a local click shows the stop state, kill open', () => {
        const m = powerModel('stopping', null, T0, open);
        expect(m.primary).toMatchObject({ label: 'Stopping...', disabled: true, variant: 'busy' });
        expect(m.killDisabled).toBe(false);
    });

    it('external starting/restarting keeps Force kill as the way out', () => {
        expect(powerModel('starting', null, T0, open)).toMatchObject({ primary: { label: 'Starting...' }, killDisabled: false });
        expect(powerModel('restarting', null, T0, open)).toMatchObject({ primary: { label: 'Restarting...' }, killDisabled: false });
    });

    it('terminal status after the wait clears back to the idle layout', () => {
        const p = advancePending({ ...startPending('stop', 'online', T0), left: true }, 'stopped');
        expect(p).toBeNull();
        expect(powerModel('stopped', p, T0 + 20_000, open).primary.label).toBe('Start');
    });

    it('timeout: says it is still waiting instead of re-enabling, kill becomes the escape', () => {
        const p = startPending('start', 'stopped', T0);
        const m = powerModel('starting', p, T0 + PENDING_TIMEOUT_MS, open);
        expect(m.primary).toMatchObject({ label: 'Still waiting for the node', icon: 'stale', disabled: true });
        expect(m.restartDisabled).toBe(true);
        expect(m.killDisabled).toBe(false);
        const before = powerModel('starting', p, T0 + PENDING_TIMEOUT_MS - 1, open);
        expect(before.primary.label).toBe('Starting...');
    });

    it('gates disable everything with a reason', () => {
        const cases: [string, Partial<PowerGates>, RegExp][] = [
            ['online', { canPower: false }, /No permission/],
            ['online', { uploadLocked: true }, /Upload in progress/],
            ['online', { cooldownSeconds: 12 }, /12s remaining/],
            ['migrating', {}, /migrating/],
            ['installing', {}, /installing/],
            ['pending_setup', {}, /setup/],
        ];
        for (const [status, gate, reason] of cases) {
            const m = powerModel(status, null, T0, { ...open, ...gate });
            expect(m.primary.disabled).toBe(true);
            expect(m.primary.title).toMatch(reason);
            expect(m.killDisabled).toBe(true);
        }
        expect(powerModel('disk_full', null, T0, open).primary).toMatchObject({ action: 'start', disabled: true });
    });

    it('kill cooldown locks only Force kill', () => {
        const m = powerModel('online', null, T0, { ...open, killCooldown: true });
        expect(m).toMatchObject({ restartDisabled: false, killDisabled: true });
        expect(m.primary.disabled).toBe(false);
    });
});
