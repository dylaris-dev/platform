import { describe, expect, it } from 'vitest';
import {
    afterPost, reconcile, expire, describeInstallFailure, isPending, INSTALL_TIMEOUT_MS,
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
