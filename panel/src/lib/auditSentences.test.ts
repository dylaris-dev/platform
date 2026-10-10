import { describe, expect, it } from 'vitest';
import { auditActor, auditSentence, CAPABILITY_PREDICATES, type AuditSentenceEvent } from './auditSentences';
import { auditStep } from './auditPaging';

const by = (eventType: string, metadata?: Record<string, unknown>, extra: Partial<AuditSentenceEvent> = {}): string =>
    auditSentence({ eventType, actorUserId: 'u1', actorName: 'BartisD', metadata, ...extra });

describe('auditSentence: named server events', () => {
    const cases: [string, string, Record<string, unknown> | undefined, Partial<AuditSentenceEvent>, string][] = [
        ['power start', 'power_action', { action: 'start' }, {}, 'BartisD started the server'],
        ['power stop', 'power_action', { action: 'stop' }, {}, 'BartisD stopped the server'],
        ['power restart', 'power_action', { action: 'restart' }, {}, 'BartisD restarted the server'],
        ['power kill', 'power_action', { action: 'kill' }, {}, 'BartisD killed the server'],
        ['power override', 'power_action', { action: 'start', admin_suspend_override: 'x' }, {}, 'BartisD started the server (overriding a suspension)'],
        ['invite via target', 'member_invited', { username: 'ignored' }, { targetName: 'Dave' }, 'BartisD gave Dave access to the server'],
        ['invite via meta', 'member_invited', { username: 'Dave' }, {}, 'BartisD gave Dave access to the server'],
        ['removed', 'member_removed', undefined, { targetName: 'Dave' }, 'BartisD removed access for Dave'],
        ['perms', 'member_perms_changed', { grantCaps: ['files.read'] }, { targetName: 'Dave' }, 'BartisD changed the permissions of Dave'],
        ['resources', 'resources_changed', { ram: 16384, cpuLimit: 2, diskLimit: 20480 }, {}, 'BartisD set RAM to 16 GB, CPU to 2 cores and disk to 20 GB'],
        ['resources unlimited', 'resources_changed', { ram: 1536, cpuLimit: 0, diskLimit: 0 }, {}, 'BartisD set RAM to 1.5 GB, CPU to no limit and disk to no limit'],
        ['resources extras', 'resources_changed', { ram: 512, cpuPinningMode: 'manual', cpuset: '0-3', hostPort: 25600, ramPaddingMb: null, memoryGuardAction: 'stop' }, {},
            'BartisD set RAM to 512 MB, CPU pinning to manual (0-3), the port to 25600, RAM headroom to the default and the out-of-memory action to stop'],
        ['rename', 'name_changed', { from: 'a', to: 'b' }, {}, 'BartisD renamed the server from "a" to "b"'],
        ['setup', 'setup', { sub_server: 'main', installer: 'paper', version: '1.21.1' }, {}, 'BartisD set up the sub-server "main" with paper 1.21.1'],
        ['reinstall', 'reinstall', { installer: 'forge', version: '1.20.1' }, {}, 'BartisD reinstalled the server with forge 1.20.1'],
        ['sub deleted', 'subserver_deleted', { sub_server: 'old', was_active: true }, {}, 'BartisD deleted the active sub-server "old"'],
        ['sub switched', 'subserver_switched', { from: 'a', to: 'b' }, {}, 'BartisD switched the sub-server from "a" to "b"'],
        ['deleted', 'deleted', undefined, {}, 'BartisD deleted the server'],
        ['runtime', 'runtime_changed', { java_image: 'java21', jvm_flags_changed: true }, {}, 'BartisD changed the Java image to java21 and the JVM flags'],
        ['runtime image only', 'runtime_changed', { java_image: 'java21', jvm_flags_changed: false }, {}, 'BartisD changed the Java image to java21'],
        ['force on', 'audit_force_on_changed', { force_on: true }, {}, 'BartisD forced the audit log on'],
        ['force off', 'audit_force_on_changed', { force_on: false }, {}, 'BartisD stopped forcing the audit log on'],
        ['loader', 'loader_metadata_declared', { from_installer_type: 'forge', from_minecraft_version: '1.20', to_installer_type: 'neoforge', to_minecraft_version: '1.21' }, {},
            'BartisD changed the declared loader from forge 1.20 to neoforge 1.21'],
        ['whitelist add', 'player_action', { action: 'whitelist_add', player: 'Isthaltdave' }, { actorName: 'Dave' }, 'Dave added Isthaltdave to the whitelist'],
        ['whitelist remove', 'player_action', { action: 'whitelist_remove', player: 'X' }, {}, 'BartisD removed X from the whitelist'],
        ['kick', 'player_action', { action: 'kick', player: 'X' }, {}, 'BartisD kicked X'],
        ['refused kick', 'player_action', { action: 'kick', player: 'X', refused: true }, {}, 'BartisD tried "kick" on X, refused by the server'],
        ['ban', 'player_action', { action: 'ban', player: 'X' }, {}, 'BartisD banned X'],
        ['unban', 'player_action', { action: 'unban', player: 'X' }, {}, 'BartisD unbanned X'],
        ['op', 'player_action', { action: 'op', player: 'X' }, {}, 'BartisD made X an operator'],
        ['deop', 'player_action', { action: 'deop', player: 'X' }, {}, 'BartisD removed operator from X'],
        ['tell', 'player_action', { action: 'tell', player: 'X' }, {}, 'BartisD sent X a private message'],
        ['whitelist on', 'player_action', { action: 'whitelist_on' }, {}, 'BartisD turned the whitelist on'],
        ['whitelist off', 'player_action', { action: 'whitelist_off' }, {}, 'BartisD turned the whitelist off'],
        ['memory stop', 'memory_guard', { event: 'memory_critical', usedMB: 980, limitMB: 1000, action: 'stop', taken: 'stop' }, { actorName: undefined, actorUserId: undefined },
            'Memory guard stopped the server at 98%'],
        ['memory restart', 'memory_guard', { event: 'memory_critical', usedMB: 990, limitMB: 1000, taken: 'restart' }, { actorName: undefined, actorUserId: undefined },
            'Memory guard restarted the server at 99%'],
        ['memory failed', 'memory_guard', { event: 'memory_critical', usedMB: 980, limitMB: 1000, action: 'stop', taken: 'failed' }, { actorName: undefined, actorUserId: undefined },
            'Memory guard failed to stop the server at 98%'],
        ['memory off', 'memory_guard', { event: 'memory_critical', usedMB: 980, limitMB: 1000, action: 'off', taken: 'off' }, { actorName: undefined, actorUserId: undefined },
            'Memory guard warned that memory was critical at 98%'],
        ['memory none', 'memory_guard', { event: 'memory_critical', action: 'stop', taken: 'none' }, { actorName: undefined, actorUserId: undefined },
            'Memory guard left the server alone: it was not running'],
        ['oom', 'memory_guard', { event: 'oom_killed' }, { actorName: undefined, actorUserId: undefined },
            'Memory guard recorded that the server was killed: out of memory'],
        ['sftp', 'sftp.changes', { username: 'dave.abc', writes: 3, deletes: 1, renames: 0, mkdirs: 1, paths: ['/a'] }, { actorName: undefined, actorUserId: undefined },
            'dave.abc changed files over SFTP: 3 writes, 1 delete, 1 new folder'],
        ['beam', 'beam.changes', { username: 'dave', writes: 1 }, {}, 'BartisD changed files over Beam: 1 write'],
        ['cap', 'files.delete', { method: 'DELETE', path: '/api/x' }, {}, 'BartisD deleted files'],
        ['cap by key', 'console.send', { method: 'POST', path: '/x', apiKey: 'k1' }, {}, 'BartisD sent a console command with an API key'],
    ];
    it.each(cases)('%s', (_name, type, meta, extra, want) => {
        expect(by(type, meta, extra)).toBe(want);
    });

    it('every recorded server capability has its own sentence', () => {
        for (const cap of Object.keys(CAPABILITY_PREDICATES)) {
            expect(by(cap, {})).not.toMatch(/ by /);
        }
    });
});

describe('auditSentence: missing metadata never throws or leaks JSON', () => {
    const types = ['power_action', 'member_invited', 'member_removed', 'member_perms_changed', 'resources_changed',
        'name_changed', 'setup', 'reinstall', 'subserver_deleted', 'subserver_switched', 'deleted', 'runtime_changed',
        'audit_force_on_changed', 'loader_metadata_declared', 'player_action', 'memory_guard', 'sftp.changes', 'beam.changes'];
    it.each(types)('%s', type => {
        const s = by(type, undefined);
        expect(s.startsWith('BartisD ')).toBe(true);
        expect(s).not.toMatch(/[{}]|undefined|null/);
    });
    it('reads sensibly without metadata', () => {
        expect(by('power_action')).toBe('BartisD changed the power state');
        expect(by('resources_changed', {})).toBe('BartisD changed the resources');
        expect(by('member_removed')).toBe('BartisD removed access for a user');
    });
});

describe('auditSentence: fallback', () => {
    it('humanises an unknown type with a key=value summary', () => {
        expect(by('backup.weird_thing', { a: 1, b: 'x', c: [1, 2], d: { e: 1 }, f: 'dropped' }))
            .toBe('Backup weird thing by BartisD (a=1, b=x, c=2 items, d=1 fields)');
    });
    it('without metadata', () => {
        expect(by('new_event')).toBe('New event by BartisD');
    });
    it('truncates long values', () => {
        expect(by('x', { v: 'a'.repeat(60) })).toBe(`X by BartisD (v=${'a'.repeat(39)}...)`);
    });
});

describe('auditSentence: prototype keys are not lookups', () => {
    it.each(['constructor', '__proto__', 'toString'])('%s', key => {
        expect(by('power_action', { action: key })).toBe(`BartisD ran "${key}" on the server`);
        expect(by('player_action', { action: key, player: 'X' })).toBe(`BartisD ran the player action "${key}"`);
        expect(by(key)).toBe(`${key.replace(/_+/g, ' ').trim().replace(/^./, c => c.toUpperCase())} by BartisD`);
    });
});

describe('auditActor', () => {
    it('names system, removed users and SFTP logins', () => {
        expect(auditActor({ eventType: 'power_action' })).toBe('System');
        expect(auditActor({ eventType: 'power_action', actorUserId: 'gone' })).toBe('A removed user');
        expect(auditActor({ eventType: 'sftp.changes', metadata: { username: 'x.y' } })).toBe('x.y');
        expect(auditActor({ eventType: 'memory_guard' })).toBe('Memory guard');
    });
});

describe('auditStep', () => {
    const cases: [string, number, -1 | 1, number, number, ReturnType<typeof auditStep>][] = [
        ['prev at start', 0, -1, 10, 10, { kind: 'none' }],
        ['prev inside', 3, -1, 10, 10, { kind: 'index', index: 2 }],
        ['next inside', 3, 1, 10, 10, { kind: 'index', index: 4 }],
        ['next at end of everything', 9, 1, 10, 10, { kind: 'none' }],
        ['next at page boundary loads', 49, 1, 50, 120, { kind: 'load' }],
        ['next before page boundary', 48, 1, 50, 120, { kind: 'index', index: 49 }],
        ['empty list', 0, 1, 0, 0, { kind: 'none' }],
    ];
    it.each(cases)('%s', (_n, i, d, loaded, total, want) => {
        expect(auditStep(i, d, loaded, total)).toEqual(want);
    });
});
