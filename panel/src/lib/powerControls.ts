// The server power controls as a pure state machine: which button is primary,
// what it says, and what may be clicked, from the server status and the power
// action this browser sent and is still waiting on.
//
// The old controls cleared their wait when the status matched a target or
// after 60 s, whichever came first. A restart's target was "online", which the
// server already was, so a restart cleared at once; a graceful stop that took
// longer than the guess re-enabled every button while the JVM was still
// saving. Here a wait ends only on an outcome, and a wait that outlives the
// timeout says so instead of quietly handing the buttons back.

export type PowerAction = 'start' | 'stop' | 'restart' | 'kill';

export interface PendingPower {
    action: PowerAction;
    // The status when the action was sent. Until the status moves off it the
    // outcome cannot have arrived: a restart starts and ends "online".
    from: string;
    since: number;
    left: boolean;
}

export const PENDING_TIMEOUT_MS = 90_000;

// Statuses in which nothing is running, so the only action is Start.
export const OFFLINE_STATUSES = ['stopped', 'offline', 'pending_setup', 'disk_full'];
const DOWN = ['stopped', 'offline', 'disk_full'];

export function startPending(action: PowerAction, status: string, now: number): PendingPower {
    return { action, from: status, since: now, left: false };
}

// Returns the pending action after a status update, or null once it ended. A
// start or restart that ends with the server down has ended too: showing
// "Starting..." over a server that crashed on boot is the lie this replaces.
export function advancePending(p: PendingPower, status: string): PendingPower | null {
    const left = p.left || status !== p.from;
    if (!left) return p;
    if (DOWN.includes(status)) return null;
    if (status === 'online' && (p.action === 'start' || p.action === 'restart')) return null;
    return p.left ? p : { ...p, left };
}

export interface PowerGates {
    canPower: boolean;
    uploadLocked: boolean;
    // Install settling window, non-admins only (admins get a prompt instead).
    cooldownSeconds: number;
    // Non-admin 60 s lock after a kill.
    killCooldown: boolean;
}

export type PrimaryIcon = 'start' | 'stop' | 'busy' | 'stale';

export interface PowerModel {
    primary: {
        action: 'start' | 'stop' | null;
        label: string;
        icon: PrimaryIcon;
        variant: 'start' | 'stop' | 'busy';
        disabled: boolean;
        title: string;
    };
    // Restart and the overflow menu exist only while something is running.
    showSecondary: boolean;
    restartDisabled: boolean;
    menuDisabled: boolean;
    killDisabled: boolean;
    busy: boolean;
    // What the live region announces as the state changes.
    announcement: string;
}

const STOP_TITLE = 'Saving the world, then waiting for Java to exit (up to about 35 s)';
const STALE_TITLE = 'The node has not reported the outcome yet. Reload the page to check, or force kill the server.';

const BUSY: Record<PowerAction, string> = {
    start: 'Starting...',
    stop: 'Stopping...',
    restart: 'Restarting...',
    kill: 'Killing...',
};

// A transitional status written by Core, the node or another user's click.
function statusAction(status: string): PowerAction | null {
    if (status === 'stopping') return 'stop';
    if (status === 'starting') return 'start';
    if (status === 'restarting') return 'restart';
    return null;
}

function blockedReason(status: string, g: PowerGates): string {
    if (!g.canPower) return 'No permission';
    if (status === 'migrating') return 'Server is migrating to another node. Power actions are locked until it finishes.';
    if (status === 'installing') return 'Server is installing. Power actions unlock when it finishes.';
    if (status === 'pending_setup') return 'Finish the setup first';
    if (g.uploadLocked) return 'Upload in progress. Wait or cancel it first.';
    if (g.cooldownSeconds > 0) return `Server is settling. ${g.cooldownSeconds}s remaining`;
    return '';
}

export function powerModel(status: string, pending: PendingPower | null, now: number, g: PowerGates): PowerModel {
    const blocked = blockedReason(status, g);
    const offline = OFFLINE_STATUSES.includes(status);
    const local = pending?.action ?? null;
    const action = local ?? statusAction(status);
    const stale = !!pending && now - pending.since >= PENDING_TIMEOUT_MS;
    // Kill reaches the node through the same gates as everything else, plus the
    // non-admin lock after a kill. Not on a server that is already down.
    const killGate = !blocked && !g.killCooldown && !offline;

    if (!action) {
        if (offline) {
            const diskFull = status === 'disk_full';
            return {
                primary: {
                    action: 'start', label: 'Start', icon: 'start', variant: 'start',
                    disabled: !!blocked || diskFull,
                    title: blocked || (diskFull ? 'Storage full. Delete files or raise the limit.' : 'Start server'),
                },
                showSecondary: false, restartDisabled: true, menuDisabled: true, killDisabled: true,
                busy: false, announcement: 'Server stopped',
            };
        }
        return {
            primary: { action: 'stop', label: 'Stop', icon: 'stop', variant: 'stop', disabled: !!blocked, title: blocked || 'Stop server' },
            showSecondary: true,
            restartDisabled: !!blocked,
            menuDisabled: !!blocked,
            killDisabled: !killGate,
            busy: false, announcement: status === 'online' ? 'Server running' : '',
        };
    }

    const elapsed = pending ? Math.max(0, Math.floor((now - pending.since) / 1000)) : 0;
    let label = BUSY[action];
    if (action === 'stop' && pending) label = `${label} ${elapsed}s`;
    let title = action === 'stop' ? STOP_TITLE : label;
    // Force kill is the one way out of a stop that hangs, and of a start or
    // restart this browser did not send: there is no click time to time out
    // from, so a server stuck "starting" since before the page loaded would
    // otherwise have no escape at all. Nothing is clickable during a kill.
    let killOpen = action === 'stop' || (action !== 'kill' && !local);
    if (stale) {
        label = 'Still waiting for the node';
        title = STALE_TITLE;
        killOpen = true;
    }
    const killDisabled = !(killOpen && killGate);
    return {
        primary: { action: null, label, icon: stale ? 'stale' : 'busy', variant: 'busy', disabled: true, title },
        showSecondary: true,
        restartDisabled: true,
        menuDisabled: killDisabled,
        killDisabled,
        busy: true,
        announcement: stale ? label : BUSY[action],
    };
}
