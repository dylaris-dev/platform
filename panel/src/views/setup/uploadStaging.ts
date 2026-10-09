/**
 * The name an upload install's archive has in the sub-server directory. The
 * node looks for exactly this (node/installer.go installFromUploadZip), on
 * every node release, so the panel uploads straight to it as soon as a file
 * and a sub-server name are both there.
 */
export const UPLOAD_ZIP_NAME = '.upload.zip';

export type StageResult =
    | { status: 'staged' }
    | { status: 'failed'; message: string }
    /** A newer pick, a clear or an install took over; ignore this one. */
    | { status: 'superseded' };

export interface StagingIO {
    /** Uploads the file as <sub>/UPLOAD_ZIP_NAME. */
    upload(sub: string, file: File, onProgress: (p: number) => void): Promise<{ success: boolean; message?: string }>;
    /** Deletes <sub>/UPLOAD_ZIP_NAME. */
    removeZip(sub: string): Promise<unknown>;
    /** The names in <sub>, or null when they could not be read. */
    list(sub: string): Promise<string[] | null>;
    /** Deletes the directory <sub>. */
    removeDir(sub: string): Promise<unknown>;
}

/**
 * When the upload may start: a picked file and a valid name. On another tab it
 * waits; a new sub-server is named after the file may have been picked.
 */
export function uploadStartStep(i: { onUploadTab: boolean; filePicked: boolean; started: boolean; nameValid: boolean }): 'none' | 'need-name' | 'start' {
    if (!i.onUploadTab || !i.filePicked || i.started) return 'none';
    return i.nameValid ? 'start' : 'need-name';
}

interface Staged { sub: string; ownsDir: boolean }

/**
 * Serialises every write and delete of the staged archive. An upload cannot be
 * aborted mid-flight in a browser (lib/api/files uploadFiles has no
 * AbortSignal), so a replaced or cleared one keeps running; a delete sent
 * while it runs would be undone when it lands, and a second upload started
 * beside it could land first and be overwritten by the archive no longer on
 * screen. So each step waits for the one before, and a step that is no longer
 * the latest does nothing.
 */
export function createUploadStager(io: StagingIO) {
    let chain: Promise<unknown> = Promise.resolve();
    let gen = 0;
    // What a clear has to remove: the last archive that may be on the server.
    let staged: Staged | null = null;
    // An install request is on its way: Core may already have queued it, so
    // the archive and its directory are no longer the panel's to remove.
    let installing = false;
    const enqueue = <T>(step: () => Promise<T>): Promise<T> => {
        const p = chain.then(step);
        chain = p.catch(() => undefined);
        return p;
    };
    // The directory only when this upload made it (a new sub-server) and only
    // when nothing else is in it: the list could be stale, and a directory with
    // anything in it is a server.
    const cleanup = async (t: Staged) => {
        try { await io.removeZip(t.sub); } catch { /* best effort */ }
        if (!t.ownsDir) return;
        try {
            const names = await io.list(t.sub);
            if (names && names.length === 0) await io.removeDir(t.sub);
        } catch { /* best effort */ }
    };
    return {
        /** ownsDir: the sub-server does not exist yet, so the upload creates its directory. */
        stage(sub: string, ownsDir: boolean, file: File, onProgress: (p: number) => void): Promise<StageResult> {
            const mine = ++gen;
            return enqueue(async (): Promise<StageResult> => {
                if (mine !== gen) return { status: 'superseded' };
                if (staged && staged.sub !== sub) await cleanup(staged);
                staged = { sub, ownsDir: ownsDir || (staged?.sub === sub && staged.ownsDir) };
                let res: { success: boolean; message?: string };
                try {
                    res = await io.upload(sub, file, p => { if (mine === gen) onProgress(p); });
                } catch (e) {
                    res = { success: false, message: e instanceof Error ? e.message : '' };
                }
                if (mine !== gen) return { status: 'superseded' };
                return res.success ? { status: 'staged' } : { status: 'failed', message: res.message || 'Upload failed' };
            });
        },
        /** Removes the staged archive once nothing is still writing it. */
        discard(): void {
            if (installing) return;
            const mine = ++gen;
            void enqueue(async () => {
                // A newer pick overwrites it, or cleans it up if it goes elsewhere.
                if (mine !== gen || !staged || installing) return;
                const t = staged;
                staged = null;
                await cleanup(t);
            });
        },
        /** Before the install request is sent: discards do nothing until it is answered. */
        beginInstall(): void {
            installing = true;
        },
        /**
         * The install was answered. Queued, the archive is the node's and is
         * forgotten; refused, it is the panel's again to retry or discard.
         */
        endInstall(queued: boolean): void {
            installing = false;
            if (!queued) return;
            ++gen;
            staged = null;
        },
    };
}

export type UploadStager = ReturnType<typeof createUploadStager>;
