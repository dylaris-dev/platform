import { describe, expect, it } from 'vitest';
import {
    adopt, afterPost, reconcile, expire, describeInstallFailure, isPending, INSTALL_TIMEOUT_MS,
    type InstallJob,
} from '@/lib/modInstallJobs';
import type { InstalledMod } from '@/lib/api/modrinth';

const job = (over: Partial<InstallJob> = {}): InstallJob => ({
    id: 'j1', serverId: 7, serverName: 'Survival', projectId: 'P', versionId: 'V2',
    title: 'Spark', state: 'installing', startedAt: 1000, ...over,
});

const row = (over: Partial<InstalledMod> = {}): InstalledMod => ({
    id: 1, serverId: 7, subServerName: 'survival', modrinthProjectId: 'P', modrinthProjectSlug: 'spark',
    modrinthVersionId: 'V2', title: 'Spark', fileName: 'spark.jar', sha512: '', installedAt: '', ...over,
});

describe('afterPost', () => {
    it('follows a node that will report', () => {
        expect(afterPost(job({ state: 'sending' }), { success: true, status: 'installing' }, 2000).state).toBe('installing');
    });
    it('stops at "sent" for a node too old to report, so nothing spins forever', () => {
        const j = afterPost(job({ state: 'sending' }), { success: true, status: 'installed' }, 2000);
        expect(j.state).toBe('sent');
        expect(isPending(j)).toBe(false);
    });
    it('follows an older Core that sends no status, bounded by the timeout', () => {
        expect(afterPost(job({ state: 'sending' }), { success: true }, 2000).state).toBe('installing');
    });
    it('fails with Core\'s reason', () => {
        const j = afterPost(job({ state: 'sending' }), { success: false, message: 'Server is suspended' }, 2000);
        expect(j).toMatchObject({ state: 'failed', message: 'Server is suspended', finishedAt: 2000 });
    });
});

describe('reconcile', () => {
    it('marks installed when the row for this build is installed', () => {
        expect(reconcile(job(), [row({ status: 'installed' })], 2000).state).toBe('installed');
    });
    it('marks failed with the node\'s reason', () => {
        const j = reconcile(job(), [row({ status: 'failed', statusMessage: 'download failed for spark.jar: upstream status 404' })], 2000);
        expect(j.state).toBe('failed');
        expect(j.message).toContain('HTTP 404');
        expect(j.message).toContain('upstream status 404');
    });
    it('keeps waiting while the row is still installing', () => {
        expect(reconcile(job(), [row({ status: 'installing' })], 2000).state).toBe('installing');
    });
    it('ignores the row of a different build of the same project', () => {
        expect(reconcile(job(), [row({ modrinthVersionId: 'V1', status: 'installed' })], 2000).state).toBe('installing');
    });
    it('ignores another project', () => {
        expect(reconcile(job(), [row({ modrinthProjectId: 'Q', status: 'failed' })], 2000).state).toBe('installing');
    });
    it('leaves a settled job alone', () => {
        const done = job({ state: 'installed' });
        expect(reconcile(done, [row({ status: 'failed' })], 2000)).toBe(done);
    });
    it('gives up after the timeout instead of spinning', () => {
        const j = reconcile(job(), [row({ status: 'installing' })], 1000 + INSTALL_TIMEOUT_MS);
        expect(j.state).toBe('unknown');
        expect(j.message).toMatch(/Check the content list/);
    });
});

describe('expire', () => {
    it('does not expire before the timeout', () => {
        expect(expire(job(), 1000 + INSTALL_TIMEOUT_MS - 1).state).toBe('installing');
    });
    it('does not send someone without list access to the list', () => {
        const j = expire(job(), 1000 + INSTALL_TIMEOUT_MS, true);
        expect(j.state).toBe('unknown');
        expect(j.message).toBe("Installed status unknown - you cannot view this server's content list.");
    });
    it('does not touch a sending job: the POST itself resolves it', () => {
        expect(expire(job({ state: 'sending' }), 1000 + INSTALL_TIMEOUT_MS * 2).state).toBe('sending');
    });
});

describe('describeInstallFailure', () => {
    it.each([
        ['download failed for a.jar: sha512 mismatch: want x got y', 'did not match Modrinth\'s checksum'],
        ['download failed for a.jar: copy: write a.jar.part: no space left on device', 'disk is full'],
        ['download failed for a.jar: download exceeds the 268435456 byte limit', '256 MB limit'],
        ['download failed for a.jar: http get: dial tcp: i/o timeout', 'could not download'],
    ])('%s', (raw, hint) => {
        const msg = describeInstallFailure(raw);
        expect(msg).toContain(hint);
        expect(msg).toContain(raw);
    });
    it('passes an unrecognised reason through', () => {
        expect(describeInstallFailure('invalid target dir "x"')).toBe('invalid target dir "x"');
    });
    it('says so when the node gave no reason', () => {
        expect(describeInstallFailure('')).toMatch(/no reason/);
    });
});

describe('adopt', () => {
    const at = '2026-10-10T12:00:00Z';
    const t0 = Date.parse(at);
    const installing = (over: Partial<InstalledMod> = {}) => row({ status: 'installing', installedAt: at, ...over });

    it('follows an installing row nobody tracks, started when Core dispatched it', () => {
        const out = adopt([], 7, 'Survival', [installing(), row({ modrinthProjectId: 'Q', status: 'installed' })], t0 + 1000);
        expect(out).toHaveLength(1);
        expect(out[0]).toMatchObject({ serverId: 7, serverName: 'Survival', projectId: 'P', versionId: 'V2', state: 'installing', startedAt: t0 });
    });
    it('leaves a project alone that a pending job already follows, or that this build already settled', () => {
        const pending = [job({ state: 'sending', versionId: 'V1' })];
        expect(adopt(pending, 7, 'Survival', [installing()], t0)).toBe(pending);
        const settled = [job({ state: 'unknown', finishedAt: 5 })];
        expect(adopt(settled, 7, 'Survival', [installing()], t0)).toBe(settled);
    });
    it('replaces a settled job for another build of the same project', () => {
        const out = adopt([job({ state: 'installed', versionId: 'V1' })], 7, 'Survival', [installing()], t0);
        expect(out.map(j => [j.versionId, j.state])).toEqual([['V2', 'installing']]);
    });
    it("does not adopt the same project from another server's job", () => {
        expect(adopt([job({ serverId: 8 })], 7, 'Survival', [installing()], t0)).toHaveLength(2);
    });
    it('skips rows already past the timeout, and rows with no usable time', () => {
        expect(adopt([], 7, 'Survival', [installing()], t0 + INSTALL_TIMEOUT_MS)).toEqual([]);
        expect(adopt([], 7, 'Survival', [installing({ installedAt: '' })], t0)).toEqual([]);
    });
    it("keeps the timeout: an adopted job expires on Core's clock, not the reload's", () => {
        const [j] = adopt([], 7, 'Survival', [installing()], t0 + 60_000);
        expect(expire(j, t0 + INSTALL_TIMEOUT_MS - 1).state).toBe('installing');
        expect(expire(j, t0 + INSTALL_TIMEOUT_MS).state).toBe('unknown');
    });
    it('clamps a row from a Core clock ahead of the browser to now', () => {
        const [j] = adopt([], 7, 'Survival', [installing()], t0 - 60_000);
        expect(j.startedAt).toBe(t0 - 60_000);
    });
});
