import { describe, it, expect } from 'vitest';
import { envLabel, envParam, matchesEnv } from './modSide';

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
