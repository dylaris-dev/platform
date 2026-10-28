import { describe, it, expect } from 'vitest';
import {
    envLabel, envParam, matchesEnv,
    matchesServerBucket, serverBucket, serverBucketLabel, serverBucketTitle, serverSideParam, packSideFor,
} from './modSide';

const lithium = { client_side: 'optional', server_side: 'optional' };
const chunky = { client_side: 'unsupported', server_side: 'required' };
const spark = { client_side: 'optional', server_side: 'required' };
const create = { client_side: 'required', server_side: 'required' };
const sodium = { client_side: 'required', server_side: 'unsupported' };
const jeiLike = { client_side: 'required', server_side: 'optional' };
const unknown = { client_side: 'unknown', server_side: 'unknown' };

const none = { client: false, server: false };
const client = { client: true, server: false };
const server = { client: false, server: true };
const both = { client: true, server: true };

describe('envParam', () => {
    it('maps the two toggles', () => {
        expect(envParam(none)).toBeUndefined();
        expect(envParam(client)).toBe('client');
        expect(envParam(server)).toBe('server');
        expect(envParam(both)).toBe('both');
    });
});

describe('matchesEnv', () => {
    it('no toggle keeps everything', () => {
        for (const p of [lithium, chunky, create, sodium, unknown, {}]) expect(matchesEnv(none, p)).toBe(true);
    });

    it('server: runs on the server, client does not need it', () => {
        expect(matchesEnv(server, chunky)).toBe(true);
        expect(matchesEnv(server, spark)).toBe(true);
        expect(matchesEnv(server, lithium)).toBe(true);
        expect(matchesEnv(server, create)).toBe(false);
        expect(matchesEnv(server, sodium)).toBe(false);
        expect(matchesEnv(server, jeiLike)).toBe(false);
        expect(matchesEnv(server, unknown)).toBe(false);
    });

    it('client: runs on the client, server does not need it', () => {
        expect(matchesEnv(client, sodium)).toBe(true);
        expect(matchesEnv(client, jeiLike)).toBe(true);
        expect(matchesEnv(client, lithium)).toBe(true);
        expect(matchesEnv(client, create)).toBe(false);
        expect(matchesEnv(client, chunky)).toBe(false);
        expect(matchesEnv(client, spark)).toBe(false);
    });

    it('both: runs on either side', () => {
        expect(matchesEnv(both, create)).toBe(true);
        expect(matchesEnv(both, lithium)).toBe(true);
        expect(matchesEnv(both, spark)).toBe(true);
        expect(matchesEnv(both, jeiLike)).toBe(true);
        expect(matchesEnv(both, sodium)).toBe(false);
        expect(matchesEnv(both, chunky)).toBe(false);
    });
});

describe('envLabel', () => {
    it('uses modrinth.com card labels', () => {
        expect(envLabel(lithium)).toBe('Client or server');
        expect(envLabel(create)).toBe('Client and server');
        expect(envLabel(sodium)).toBe('Client');
        expect(envLabel(jeiLike)).toBe('Client');
        expect(envLabel(chunky)).toBe('Server');
        expect(envLabel(spark)).toBe('Server');
        expect(envLabel(unknown)).toBeNull();
        expect(envLabel({})).toBeNull();
    });
});

describe('serverBucket', () => {
    // Every client_side x server_side pair Modrinth can send, plus missing (-),
    // spelled out rather than derived so the table is the spec.
    const S = 'server_only', CS = 'client_server', C = 'client_only';
    const table: [string | undefined, string | undefined, string | null][] = [
        // client        server         bucket
        ['required', 'required', CS], ['required', 'optional', CS], ['required', 'unsupported', C], ['required', 'unknown', null], ['required', undefined, null],
        ['optional', 'required', S], ['optional', 'optional', S], ['optional', 'unsupported', C], ['optional', 'unknown', null], ['optional', undefined, null],
        ['unsupported', 'required', S], ['unsupported', 'optional', S], ['unsupported', 'unsupported', C], ['unsupported', 'unknown', null], ['unsupported', undefined, null],
        ['unknown', 'required', null], ['unknown', 'optional', null], ['unknown', 'unsupported', C], ['unknown', 'unknown', null], ['unknown', undefined, null],
        [undefined, 'required', null], [undefined, 'optional', null], [undefined, 'unsupported', C], [undefined, 'unknown', null], [undefined, undefined, null],
    ];

    it('places every combination', () => {
        expect(table).toHaveLength(25);
        for (const [c, s, want] of table) {
            expect(serverBucket({ client_side: c, server_side: s }), `client=${c} server=${s}`).toBe(want);
        }
    });
});

describe('serverBucketLabel / serverBucketTitle', () => {
    it('labels', () => {
        expect(serverBucketLabel('server_only')).toBe('Server only');
        expect(serverBucketLabel('client_server')).toBe('Client + Server');
        expect(serverBucketLabel('client_only')).toBe('Client only');
    });

    it('says a player MAY add an optional-client server mod', () => {
        expect(serverBucketTitle(spark)).toContain('may add it');
        expect(serverBucketTitle(chunky)).toBe("Players don't need it.");
        expect(serverBucketTitle(create)).toBe('Players must install it too.');
        expect(serverBucketTitle(sodium)).toBe('Does nothing on a server and can stop it from starting; players install it themselves');
        expect(serverBucketTitle(unknown)).toBeNull();
    });
});

describe('serverSideParam', () => {
    it('no button asks for everything a server can run', () => {
        expect(serverSideParam(null)).toBe('server_runs');
        expect(serverSideParam('server_only')).toBe('server_only');
        expect(serverSideParam('client_server')).toBe('client_server');
    });
});

describe('matchesServerBucket', () => {
    const browse = { browse: true };
    const installed = { browse: false };

    it('browse never shows a client-only mod', () => {
        for (const f of [null, 'server_only', 'client_server'] as const) {
            expect(matchesServerBucket(f, sodium, browse)).toBe(false);
        }
    });

    it('installed keeps a client-only mod whatever is selected', () => {
        for (const f of [null, 'server_only', 'client_server'] as const) {
            expect(matchesServerBucket(f, sodium, installed)).toBe(true);
        }
    });

    it('no button keeps every server-runnable and unknown mod', () => {
        for (const p of [chunky, spark, lithium, create, jeiLike, unknown, {}]) {
            expect(matchesServerBucket(null, p, browse)).toBe(true);
            expect(matchesServerBucket(null, p, installed)).toBe(true);
        }
    });

    it('a button keeps only its bucket, and drops unknown', () => {
        for (const opts of [browse, installed]) {
            expect(matchesServerBucket('server_only', chunky, opts)).toBe(true);
            expect(matchesServerBucket('server_only', lithium, opts)).toBe(true);
            expect(matchesServerBucket('server_only', create, opts)).toBe(false);
            expect(matchesServerBucket('server_only', unknown, opts)).toBe(false);
            expect(matchesServerBucket('client_server', create, opts)).toBe(true);
            expect(matchesServerBucket('client_server', jeiLike, opts)).toBe(true);
            expect(matchesServerBucket('client_server', spark, opts)).toBe(false);
            expect(matchesServerBucket('client_server', {}, opts)).toBe(false);
        }
    });
});

describe('packSideFor', () => {
    it('keeps a client-only mod out of the server side', () => {
        expect(packSideFor({ client_side: 'required', server_side: 'unsupported' })).toBe('client');
        expect(packSideFor({ client_side: 'optional', server_side: 'unsupported' })).toBe('client');
    });
    it('keeps a server-only mod off the client', () => {
        expect(packSideFor({ client_side: 'unsupported', server_side: 'required' })).toBe('server');
    });
    it('installs everything else on both sides', () => {
        expect(packSideFor({ client_side: 'required', server_side: 'optional' })).toBe('both');
        expect(packSideFor({ client_side: 'required', server_side: 'required' })).toBe('both');
        expect(packSideFor({ client_side: 'unknown', server_side: 'unsupported' })).toBe('both');
        expect(packSideFor({})).toBe('both');
    });
});
