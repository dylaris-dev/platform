import { describe, it, expect } from 'vitest';
import {
    advancePending, powerModel, startPending, PENDING_TIMEOUT_MS, POWER_COOLDOWN_MS, COOLDOWN_TITLE,
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
        const p = startPending('stop', 'online', T0);
        const m = powerModel('online', p, T0 + PENDING_TIMEOUT_MS, open);
        expect(m.primary).toMatchObject({ label: 'Still waiting for the node', icon: 'stale', disabled: true });
        expect(m.restartDisabled).toBe(true);
        expect(m.killDisabled).toBe(false);
        const before = powerModel('online', p, T0 + PENDING_TIMEOUT_MS - 1, open);
        expect(before.primary.label).toMatch(/^Stopping\.\.\./);
    });

    it('a start past the timeout on a server that reads starting is booting, not a silent node', () => {
        // The node says online only once the server answers a ping; a modpack takes minutes.
        for (const action of ['start', 'restart'] as PowerAction[]) {
            const m = powerModel('starting', startPending(action, 'x', T0), T0 + PENDING_TIMEOUT_MS + 60_000, open);
            expect(m.primary).toMatchObject({ label: action === 'start' ? 'Starting...' : 'Restarting...', icon: 'busy', disabled: true });
            expect(m.primary.title).toMatch(/still starting up/);
            expect(m.killDisabled).toBe(false);
        }
        // Still on the old status after the timeout: the node never acted.
        expect(powerModel('stopped', startPending('start', 'stopped', T0), T0 + PENDING_TIMEOUT_MS, open).primary.label)
            .toBe('Still waiting for the node');
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

    it('after an action ends, start/stop/restart pause for a moment; kill does not', () => {
        const until = T0 + POWER_COOLDOWN_MS;
        const on = powerModel('online', null, T0 + 1000, { ...open, cooldownUntil: until });
        expect(on.primary).toMatchObject({ action: 'stop', disabled: true, title: COOLDOWN_TITLE });
        expect(on).toMatchObject({ restartDisabled: true, killDisabled: false, menuDisabled: false });
        const off = powerModel('stopped', null, T0 + 1000, { ...open, cooldownUntil: until });
        expect(off.primary).toMatchObject({ action: 'start', disabled: true, title: COOLDOWN_TITLE, label: 'Start in 4s' });
        // Counts down to the deadline, then the plain label is back.
        expect(powerModel('stopped', null, until - 1, { ...open, cooldownUntil: until }).primary.label).toBe('Start in 1s');
        expect(powerModel('stopped', null, until, { ...open, cooldownUntil: until }).primary.label).toBe('Start');
        // Over at the deadline.
        const after = powerModel('online', null, until, { ...open, cooldownUntil: until });
        expect(after.primary).toMatchObject({ disabled: false, title: 'Stop server' });
        expect(after.restartDisabled).toBe(false);
        // A real gate keeps its own reason.
        expect(powerModel('online', null, T0, { ...open, cooldownUntil: until, canPower: false }).primary.title).toBe('No permission');
    });

    it('the menu holds Delete for admins, also on a stopped server', () => {
        const stopped = powerModel('stopped', null, T0, { ...open, canDelete: true });
        expect(stopped).toMatchObject({ showSecondary: false, showMenu: true, menuDisabled: false, killDisabled: true, deleteDisabled: false });
        // Without Delete a stopped server has no menu: it would only hold a disabled kill.
        expect(powerModel('stopped', null, T0, open)).toMatchObject({ showMenu: false, menuDisabled: true, deleteDisabled: true });
        // An upload locks Delete as the header button did.
        expect(powerModel('stopped', null, T0, { ...open, canDelete: true, uploadLocked: true })).toMatchObject({ menuDisabled: true, deleteDisabled: true });
        // During a kill nothing power-related is clickable, but Delete still is.
        const killing = powerModel('online', startPending('kill', 'online', T0), T0 + 1000, { ...open, canDelete: true });
        expect(killing).toMatchObject({ killDisabled: true, menuDisabled: false, deleteDisabled: false });
        expect(powerModel('online', null, T0, open)).toMatchObject({ showMenu: true, deleteDisabled: true });
    });

    it('kill cooldown locks only Force kill', () => {
        const m = powerModel('online', null, T0, { ...open, killCooldown: true });
        expect(m).toMatchObject({ restartDisabled: false, killDisabled: true });
        expect(m.primary.disabled).toBe(false);
    });
});
