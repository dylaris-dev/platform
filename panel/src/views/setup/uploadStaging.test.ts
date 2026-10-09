import { describe, expect, it } from 'vitest';
import { createUploadStager, uploadStartStep } from './uploadStaging';

// An upload that finishes when the test says so, like a real one that cannot
// be aborted. dirs holds what list() answers per sub-server.
function fakeIO(dirs: Record<string, string[] | null> = {}) {
    const log: string[] = [];
    const pending: { done: (ok: boolean, message?: string) => void }[] = [];
    const io = {
        upload: (sub: string, file: File, onProgress: (p: number) => void) => {
            log.push(`start ${sub}/${file.name}`);
            onProgress(50);
            return new Promise<{ success: boolean; message?: string }>(resolve => {
                pending.push({ done: (ok, message) => { log.push(`end ${sub}/${file.name}`); resolve({ success: ok, message }); } });
            });
        },
        removeZip: async (sub: string) => { log.push(`rm ${sub}/.upload.zip`); },
        list: async (sub: string) => { log.push(`ls ${sub}`); return sub in dirs ? dirs[sub] : []; },
        removeDir: async (sub: string) => { log.push(`rmdir ${sub}`); },
    };
    return { io, log, pending };
}
const file = (name: string) => new File(['x'], name);
const tick = () => new Promise(r => setTimeout(r, 0));
const settle = async () => { for (let i = 0; i < 5; i++) await tick(); };

describe('uploadStartStep', () => {
    const base = { onUploadTab: true, filePicked: true, started: false, nameValid: true };
    it('starts with a file and a valid name', () => expect(uploadStartStep(base)).toBe('start'));
    it('asks for a name first', () => expect(uploadStartStep({ ...base, nameValid: false })).toBe('need-name'));
    it('does nothing without a file', () => expect(uploadStartStep({ ...base, filePicked: false })).toBe('none'));
    it('does not start twice', () => expect(uploadStartStep({ ...base, started: true })).toBe('none'));
    it('waits while another tab is open', () => expect(uploadStartStep({ ...base, onUploadTab: false })).toBe('none'));
});

describe('createUploadStager', () => {
    it('uploads into the sub-server and reports progress', async () => {
        const { io, log, pending } = fakeIO();
        const s = createUploadStager(io);
        const progress: number[] = [];
        const r = s.stage('main', true, file('a'), p => progress.push(p));
        await tick();
        pending[0].done(true);
        expect(await r).toEqual({ status: 'staged' });
        expect(progress).toEqual([50]);
        expect(log).toEqual(['start main/a', 'end main/a']);
    });

    it('reports the reason an upload failed', async () => {
        const { io, pending } = fakeIO();
        const s = createUploadStager(io);
        const r = s.stage('main', false, file('a'), () => {});
        await tick();
        pending[0].done(false, 'Storage limit reached');
        expect(await r).toEqual({ status: 'failed', message: 'Storage limit reached' });
    });

    it('starts a replacement only after the replaced upload has landed', async () => {
        const { io, log, pending } = fakeIO();
        const s = createUploadStager(io);
        const a = s.stage('main', false, file('a'), () => {});
        await tick();
        const b = s.stage('main', false, file('b'), () => {});
        await tick();
        expect(log).toEqual(['start main/a']);
        pending[0].done(true);
        expect(await a).toEqual({ status: 'superseded' });
        await settle();
        expect(log).toEqual(['start main/a', 'end main/a', 'start main/b']);
        pending[1].done(true);
        expect(await b).toEqual({ status: 'staged' });
    });

    it('discards a new sub-server: archive, then the directory it made once empty', async () => {
        const { io, log, pending } = fakeIO();
        const s = createUploadStager(io);
        void s.stage('fresh', true, file('a'), () => {});
        await tick();
        s.discard();
        await tick();
        expect(log).toEqual(['start fresh/a']); // nothing deleted under a running upload
        pending[0].done(true);
        await settle();
        expect(log).toEqual(['start fresh/a', 'end fresh/a', 'rm fresh/.upload.zip', 'ls fresh', 'rmdir fresh']);
    });

    it('never deletes a directory with anything else in it', async () => {
        for (const listing of [['world'], null]) {
            const { io, log, pending } = fakeIO({ fresh: listing });
            const s = createUploadStager(io);
            void s.stage('fresh', true, file('a'), () => {});
            await tick();
            pending[0].done(true);
            s.discard();
            await settle();
            expect(log).not.toContain('rmdir fresh');
            expect(log).toContain('rm fresh/.upload.zip');
        }
    });

    it('deletes only the archive of an existing sub-server', async () => {
        const { io, log, pending } = fakeIO();
        const s = createUploadStager(io);
        void s.stage('main', false, file('a'), () => {});
        await tick();
        pending[0].done(true);
        s.discard();
        await settle();
        expect(log).toEqual(['start main/a', 'end main/a', 'rm main/.upload.zip']);
    });

    it('does not delete what a newer pick is about to overwrite', async () => {
        const { io, log, pending } = fakeIO();
        const s = createUploadStager(io);
        void s.stage('main', false, file('a'), () => {});
        await tick();
        s.discard();
        void s.stage('main', false, file('b'), () => {});
        pending[0].done(true);
        await settle();
        expect(log).toEqual(['start main/a', 'end main/a', 'start main/b']);
    });

    it('cleans up the old sub-server when a pick goes to another', async () => {
        const { io, log, pending } = fakeIO();
        const s = createUploadStager(io);
        void s.stage('one', true, file('a'), () => {});
        await tick();
        pending[0].done(true);
        void s.stage('two', true, file('a'), () => {});
        await settle();
        expect(log).toEqual(['start one/a', 'end one/a', 'rm one/.upload.zip', 'ls one', 'rmdir one', 'start two/a']);
    });

    it('leaves an installed archive to the node', async () => {
        const { io, log, pending } = fakeIO();
        const s = createUploadStager(io);
        const r = s.stage('fresh', true, file('a'), () => {});
        await tick();
        pending[0].done(true);
        await r;
        s.beginInstall();
        s.endInstall(true);
        s.discard();
        await settle();
        expect(log).toEqual(['start fresh/a', 'end fresh/a']);
    });

    // Leaving the tab while the install request is out: Core may have queued it.
    it('ignores a discard while the install request is out', async () => {
        const { io, log, pending } = fakeIO();
        const s = createUploadStager(io);
        const r = s.stage('fresh', true, file('a'), () => {});
        await tick();
        pending[0].done(true);
        await r;
        s.beginInstall();
        s.discard();
        await settle();
        expect(log).toEqual(['start fresh/a', 'end fresh/a']);
    });

    it('can discard again after a refused install', async () => {
        const { io, log, pending } = fakeIO();
        const s = createUploadStager(io);
        const r = s.stage('fresh', true, file('a'), () => {});
        await tick();
        pending[0].done(true);
        await r;
        s.beginInstall();
        s.discard();
        s.endInstall(false);
        s.discard();
        await settle();
        expect(log).toEqual(['start fresh/a', 'end fresh/a', 'rm fresh/.upload.zip', 'ls fresh', 'rmdir fresh']);
    });

    it('deletes nothing when nothing was staged', async () => {
        const { io, log } = fakeIO();
        const s = createUploadStager(io);
        s.discard();
        await settle();
        expect(log).toEqual([]);
    });
});
