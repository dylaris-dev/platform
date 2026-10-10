// Plain-English sentences for the server audit trail.
//
// Every event type Core writes to server_audit_events is mapped here: the
// named ones (core/handlers/server_audit.go, players.go, services/memory_guard.go,
// services/sftp_audit.go) and the capability ids RequireCap and API-key
// requests record verbatim (authz/catalog.go, server scope, non-read verbs).
// An unknown type still reads as words, never as JSON, so a server-side
// addition shows up before the panel learns its wording.

export interface AuditSentenceEvent {
    eventType: string;
    actorUserId?: string;
    actorName?: string;
    targetName?: string;
    metadata?: Record<string, unknown>;
}

type Meta = Record<string, unknown>;

function str(m: Meta, k: string): string {
    const v = m[k];
    return typeof v === 'string' ? v.trim() : typeof v === 'number' ? String(v) : '';
}

function num(m: Meta, k: string): number | null {
    const v = m[k];
    return typeof v === 'number' && Number.isFinite(v) ? v : null;
}

// Event types and actions come from stored rows; a plain index would hand
// "constructor" or "toString" the Object prototype's functions.
function own<T>(m: Record<string, T>, k: string): T | undefined {
    return Object.hasOwn(m, k) ? m[k] : undefined;
}

function quoted(s: string): string {
    return `"${s}"`;
}

// MB as the panel shows them: whole GB when it divides, else MB.
export function formatMB(mb: number): string {
    if (mb >= 1024) {
        const gb = mb / 1024;
        return `${Number.isInteger(gb) ? gb : gb.toFixed(1)} GB`;
    }
    return `${mb} MB`;
}

// Who did it. Memory guard rows carry no actor because Core acted on its own;
// SFTP/Beam rows name the login even when it matches no panel user.
export function auditActor(ev: AuditSentenceEvent): string {
    if (ev.actorName) return ev.actorName;
    const m = ev.metadata ?? {};
    if (ev.eventType === 'memory_guard') return 'Memory guard';
    if ((ev.eventType === 'sftp.changes' || ev.eventType === 'beam.changes') && str(m, 'username')) {
        return str(m, 'username');
    }
    return ev.actorUserId ? 'A removed user' : 'System';
}

function target(ev: AuditSentenceEvent, m: Meta): string {
    return ev.targetName || str(m, 'username') || 'a user';
}

function loaderLabel(installer: string, version: string): string {
    return [installer, version].filter(Boolean).join(' ');
}

const POWER: Record<string, string> = {
    start: 'started the server',
    stop: 'stopped the server',
    restart: 'restarted the server',
    kill: 'killed the server',
};

const PLAYER: Record<string, (p: string) => string> = {
    kick: p => `kicked ${p}`,
    ban: p => `banned ${p}`,
    unban: p => `unbanned ${p}`,
    op: p => `made ${p} an operator`,
    deop: p => `removed operator from ${p}`,
    whitelist_add: p => `added ${p} to the whitelist`,
    whitelist_remove: p => `removed ${p} from the whitelist`,
    tell: p => `sent ${p} a private message`,
    whitelist_on: () => 'turned the whitelist on',
    whitelist_off: () => 'turned the whitelist off',
};

// Capability ids written by the generic recorder (RequireCap) and by API-key
// requests. Only server-scoped, non-read capabilities are ever recorded.
export const CAPABILITY_PREDICATES: Record<string, string> = {
    'console.send': 'sent a console command',
    'power.start': 'started the server',
    'power.stop': 'stopped the server',
    'power.restart': 'restarted the server',
    'power.kill': 'killed the server',
    'rcon.exec': 'ran an RCON command',
    'players.manage': 'managed players',
    'files.write': 'changed files',
    'files.delete': 'deleted files',
    'sftp.access': 'used SFTP access',
    'config.write': 'changed the configuration',
    'mods.write': 'installed or updated mods',
    'mods.delete': 'removed mods',
    'backups.create': 'created a backup',
    'backups.delete': 'deleted a backup',
    'backups.restore': 'restored a backup',
    'network.write': 'changed the network settings',
    'tabs.write': 'changed the custom tabs',
    'schedule.write': 'changed the scheduled tasks',
    'schedule.delete': 'deleted a scheduled task',
    'spark.use': 'used the Spark profiler',
    'members.write': 'changed the members',
    'members.delete': 'removed a member',
    'server.settings.write': 'changed the server settings',
    'server.delete': 'deleted the server',
};

function resourcesPredicate(m: Meta): string {
    const parts: string[] = [];
    const ram = num(m, 'ram');
    if (ram !== null) parts.push(`RAM to ${formatMB(ram)}`);
    const cpu = num(m, 'cpuLimit');
    // 0 is how the resource editor spells "no CPU limit".
    if (cpu !== null) parts.push(cpu > 0 ? `CPU to ${cpu} core${cpu === 1 ? '' : 's'}` : 'CPU to no limit');
    const disk = num(m, 'diskLimit');
    if (disk !== null) parts.push(disk > 0 ? `disk to ${formatMB(disk)}` : 'disk to no limit');
    const pin = str(m, 'cpuPinningMode');
    if (pin) parts.push(`CPU pinning to ${pin}${str(m, 'cpuset') ? ` (${str(m, 'cpuset')})` : ''}`);
    const host = num(m, 'hostPort');
    if (host !== null) parts.push(`the port to ${host}`);
    if ('ramPaddingMb' in m) {
        const pad = num(m, 'ramPaddingMb');
        parts.push(pad === null ? 'RAM headroom to the default' : `RAM headroom to ${formatMB(pad)}`);
    }
    const guard = str(m, 'memoryGuardAction');
    if (guard) parts.push(`the out-of-memory action to ${guard}`);
    if (parts.length === 0) return 'changed the resources';
    return `set ${joinList(parts)}`;
}

function joinList(parts: string[]): string {
    if (parts.length <= 1) return parts.join('');
    return `${parts.slice(0, -1).join(', ')} and ${parts[parts.length - 1]}`;
}

function memoryPredicate(m: Meta): string {
    const used = num(m, 'usedMB');
    const limit = num(m, 'limitMB');
    const at = used !== null && limit !== null && limit > 0 ? ` at ${Math.round((used / limit) * 100)}%` : '';
    switch (str(m, 'event')) {
        case 'oom_killed':
            return 'recorded that the server was killed: out of memory';
        case 'memory_warning':
            return `saved the world as memory ran high${at}`;
        case 'memory_critical':
            switch (str(m, 'taken')) {
                case 'stop': return `stopped the server${at}`;
                case 'restart': return `restarted the server${at}`;
                case 'failed': return `failed to ${str(m, 'action') || 'act on'} the server${at}`;
                case 'none': return `left the server alone${at}: it was not running`;
                default: return `warned that memory was critical${at}`;
            }
        default:
            return 'acted on the server memory';
    }
}

function filePredicate(m: Meta, via: string): string {
    const counts: string[] = [];
    const add = (k: string, one: string, many: string) => {
        const n = num(m, k);
        if (n && n > 0) counts.push(`${n} ${n === 1 ? one : many}`);
    };
    add('writes', 'write', 'writes');
    add('deletes', 'delete', 'deletes');
    add('renames', 'rename', 'renames');
    add('mkdirs', 'new folder', 'new folders');
    return `changed files over ${via}${counts.length ? `: ${counts.join(', ')}` : ''}`;
}

// The predicate for a known event type, or null when the type is unknown.
function predicate(ev: AuditSentenceEvent): string | null {
    const m = ev.metadata ?? {};
    switch (ev.eventType) {
        case 'power_action': {
            const action = str(m, 'action');
            const base = own(POWER, action) ?? (action ? `ran "${action}" on the server` : 'changed the power state');
            return str(m, 'admin_suspend_override') ? `${base} (overriding a suspension)` : base;
        }
        case 'member_invited':
            return `gave ${target(ev, m)} access to the server`;
        case 'member_removed':
            return `removed access for ${target(ev, m)}`;
        case 'member_perms_changed':
            return `changed the permissions of ${target(ev, m)}`;
        case 'resources_changed':
            return resourcesPredicate(m);
        case 'name_changed': {
            const from = str(m, 'from');
            const to = str(m, 'to');
            if (from && to) return `renamed the server from ${quoted(from)} to ${quoted(to)}`;
            return to ? `renamed the server to ${quoted(to)}` : 'renamed the server';
        }
        case 'setup':
        case 'reinstall': {
            const verb = ev.eventType === 'setup' ? 'set up' : 'reinstalled';
            const what = loaderLabel(str(m, 'installer'), str(m, 'version'));
            const sub = str(m, 'sub_server');
            return `${verb} ${sub ? `the sub-server ${quoted(sub)}` : 'the server'}${what ? ` with ${what}` : ''}`;
        }
        case 'subserver_deleted': {
            const sub = str(m, 'sub_server');
            return `deleted the ${m.was_active === true ? 'active ' : ''}sub-server${sub ? ` ${quoted(sub)}` : ''}`;
        }
        case 'subserver_switched': {
            const from = str(m, 'from');
            const to = str(m, 'to');
            if (from && to) return `switched the sub-server from ${quoted(from)} to ${quoted(to)}`;
            return to ? `switched to the sub-server ${quoted(to)}` : 'switched the sub-server';
        }
        case 'deleted':
            return 'deleted the server';
        case 'runtime_changed': {
            const image = str(m, 'java_image');
            const base = image ? `changed the Java image to ${image}` : 'changed the Java runtime';
            return m.jvm_flags_changed === true ? `${base} and the JVM flags` : base;
        }
        case 'audit_force_on_changed':
            return m.force_on === false ? 'stopped forcing the audit log on' : 'forced the audit log on';
        case 'loader_metadata_declared': {
            const from = loaderLabel(str(m, 'from_installer_type'), str(m, 'from_minecraft_version'));
            const to = loaderLabel(str(m, 'to_installer_type'), str(m, 'to_minecraft_version'));
            if (from && to) return `changed the declared loader from ${from} to ${to}`;
            return to ? `declared the loader as ${to}` : 'changed the declared loader';
        }
        case 'player_action': {
            const action = str(m, 'action');
            const player = str(m, 'player') || 'a player';
            const fn = own(PLAYER, action);
            if (m.refused === true) return `tried ${quoted(action || 'unknown')} on ${player}, refused by the server`;
            return fn ? fn(player) : `ran the player action ${quoted(action || 'unknown')}`;
        }
        case 'memory_guard':
            return memoryPredicate(m);
        case 'sftp.changes':
            return filePredicate(m, 'SFTP');
        case 'beam.changes':
            return filePredicate(m, 'Beam');
    }
    const cap = own(CAPABILITY_PREDICATES, ev.eventType);
    if (cap) return str(m, 'apiKey') ? `${cap} with an API key` : cap;
    return null;
}

// "power_action" -> "Power action", "files.write" -> "Files write".
export function humaniseEventType(t: string): string {
    const words = t.replace(/[._-]+/g, ' ').trim();
    return words ? words[0].toUpperCase() + words.slice(1) : 'Unknown event';
}

// A short scalar rendering for the list fallback: never JSON.
export function summariseValue(v: unknown): string {
    if (v === null || v === undefined) return 'none';
    if (Array.isArray(v)) return v.length === 0 ? 'none' : `${v.length} item${v.length === 1 ? '' : 's'}`;
    if (typeof v === 'object') return `${Object.keys(v as object).length} fields`;
    const s = String(v);
    return s.length > 40 ? `${s.slice(0, 39)}...` : s;
}

const FALLBACK_PAIRS = 4;

export function auditSentence(ev: AuditSentenceEvent): string {
    const actor = auditActor(ev);
    const p = predicate(ev);
    if (p) return `${actor} ${p}`;
    const pairs = Object.entries(ev.metadata ?? {})
        .slice(0, FALLBACK_PAIRS)
        .map(([k, v]) => `${k}=${summariseValue(v)}`);
    const summary = pairs.length ? ` (${pairs.join(', ')})` : '';
    return `${humaniseEventType(ev.eventType)} by ${actor}${summary}`;
}
