// Wails Beam adapter: routes file operations through the Wails-injected
// native bindings (window.go.main.App.*) which in turn talk to the
// BeamRelay over gRPC. Used only when the panel runs inside the Beam
// Desktop App; the regular browser uses panelFileBrowserAdapter.
//
// Detection lives in createBeamAdapter() — components keep their existing
// `const adapter = useMemo(() => createBeamAdapter(...))` call and never
// have to know which transport is active.

import type { FileBrowserAdapter, FileEntry } from '@dylaris/ui-filebrowser';
import { uploadFiles as apiUploadFiles, getUserLimits as apiGetUserLimits } from '@/lib/api';
import { isFatalBeamUploadError, cleanBeamGrpcMessage } from './beamUploadErrors';
import { devLog } from '@/lib/devLog';
import { cacheSuccess } from '@/lib/cacheSuccess';
import { API_URL } from '@/lib/api/core';
import {
    reportUploadStart,
    reportUploadProgress,
    reportUploadFinish,
    reportUploadUpdate,
    isServerUploadLocked,
} from '@/lib/uploadManager';

// uploadLockedError is the error every mutating Beam op throws when the
// target server is currently being written to by an active upload. The
// FileBrowserAdapter contract turns this into a {success:false, message:…}
// pair that the FileBrowser surfaces inline.
const UPLOAD_LOCK_MSG = 'Upload in progress on this server — file actions are paused until it finishes or is cancelled.';

function guardWrite(serverUuid: string | undefined): { success: false; message: string } | null {
    if (serverUuid && isServerUploadLocked(serverUuid)) {
        return { success: false, message: UPLOAD_LOCK_MSG };
    }
    return null;
}

// Chunk size for the JS → Go upload bridge. Smaller = more IPC calls
// (overhead-bound on the JS side); larger = bigger base64 strings in
// each IPC payload (memory-bound on both ends). 512 KB is the sweet
// spot in practice: ~2k calls per GB, ~700 KB JSON per call.
const UPLOAD_CHUNK_SIZE = 512 * 1024;

// btoa() chokes on String.fromCharCode.apply() with very large arrays
// (call-stack overflow above ~64K args). Chunk the conversion.
function uint8ToBase64(bytes: Uint8Array): string {
    let binary = '';
    const STEP = 0x8000; // 32 KB at a time
    for (let i = 0; i < bytes.length; i += STEP) {
        const sub = bytes.subarray(i, i + STEP);
        binary += String.fromCharCode.apply(null, sub as unknown as number[]);
    }
    return btoa(binary);
}

// Shape of the bindings exposed by gateway/beam/app/app.go. We type only
// the methods the FileBrowser actually calls; everything else stays
// loosely typed since the rest of the panel doesn't reach into it.
export interface WailsAppBindings {
    Login(apiURL: string, username: string, password: string): Promise<{ token: string; username: string; isAdmin: boolean }>;
    SetSession?(apiURL: string, token: string): Promise<void>;
    // The panel URL the app was pointed at. Declared optional because an older
    // app build may not expose it; every method on the Go App is bound, so a
    // current one does.
    GetPanelURL?(): Promise<string>;
    Logout(): Promise<void>;
    GetBeamConfig(): Promise<{ relay_address: string }>;
    ConnectToServer(serverUUID: string): Promise<void>;
    // Read-only: the active beam transport ("lan-fastpath" | "relay" | "direct") for
    // the live tunnel, or "" when not connected. Optional - older app builds omit it.
    GetConnectionMode?(): Promise<string>;
    ListFiles(path: string, serverUUID: string): Promise<{ success: boolean; files?: FileEntry[]; message?: string }>;
    GetFileContent(path: string, serverUUID: string): Promise<{ success: boolean; content?: string; message?: string }>;
    SaveFile(path: string, content: string, serverUUID: string): Promise<void>;
    CreateFile(path: string, isDir: boolean, serverUUID: string): Promise<void>;
    DeleteFile(path: string, serverUUID: string): Promise<void>;
    RenameFile(oldPath: string, newPath: string, serverUUID: string): Promise<void>;
    CopyFile(srcPath: string, dstPath: string, serverUUID: string): Promise<void>;
    DownloadFile(path: string, serverUUID: string, isDir: boolean): Promise<void>;
    SelectiveDownload(basePath: string, selected: string[], selectAll: boolean, serverUUID: string): Promise<void>;
    RevealInExplorer?(localPath: string): Promise<void>;
    // Chunked upload: open a stream, send N chunks (base64-encoded
    // bytes), close. Each call references an opaque uploadID minted
    // by the JS side. Optional on the typing because older app builds
    // don't expose them — the adapter feature-detects at call time.
    BeamUploadStart?(uploadID: string, path: string, filename: string, strategy: string, totalSize: number): Promise<void>;
    BeamUploadChunk?(uploadID: string, dataB64: string, offset: number): Promise<void>;
    BeamUploadFinish?(uploadID: string): Promise<void>;
    BeamUploadCancel?(uploadID: string): Promise<void>;
    // Resume after a chunk Send failed (relay restart / transient drop).
    // The Wails side tears down the dead session, reconnects to whichever
    // relay Core hands out, and reopens a stream with the SAME upload id
    // so the Node reattaches to the existing temp file.
    BeamUploadResume?(uploadID: string, serverUUID: string, path: string, filename: string, strategy: string, totalSize: number): Promise<void>;
}

declare global {
    interface Window {
        go?: { main?: { App?: WailsAppBindings } };
    }
}

export function getWailsApp(): WailsAppBindings | null {
    if (typeof window === 'undefined') return null;
    return window.go?.main?.App ?? null;
}

export function isWails(): boolean {
    return getWailsApp() !== null;
}

// getWailsConnectionMode reads the active beam transport ("lan-fastpath" | "relay" |
// "direct") from the Wails side after a successful ConnectToServer. Returns "" when
// the binding is missing (older app build) or the call fails, so the caller renders no
// badge rather than surfacing an error.
export async function getWailsConnectionMode(): Promise<string> {
    const app = getWailsApp();
    if (!app || typeof app.GetConnectionMode !== 'function') return '';
    try {
        return await app.GetConnectionMode();
    } catch {
        return '';
    }
}

// The outcome of the handshake below, carried rather than logged.
//
// A failure used to be invisible: doSyncSession returned void on every path,
// so a caller could not tell "the session is on the native side" from "we tried
// and it did not go". The native side then answered for it, with the only thing
// it knows in that state - "not logged in" - which is both unactionable and, to
// somebody who is plainly signed into the panel it is being shown in, untrue.
export type SessionSync = { ok: true } | { ok: false; reason: string };

// syncSessionWithWails pushes the panel's session token + API base to the Wails
// side so its Core client (used for the relay-address lookup) is authenticated.
//
// Cached on SUCCESS, not on having tried. SetSession tears down any live relay
// tunnel, so a handshake that WORKED must not run twice; one that failed tore
// nothing down - it is refused before that point on every path - so the next
// caller is free to try again, and something has to, or the app stays
// unauthenticated until it is restarted.
const syncSession = cacheSuccess(doSyncSession, result => result.ok);
export function syncSessionWithWails(): Promise<SessionSync> {
    return syncSession();
}

// wailsAPIBase is the API base SetSession will actually accept.
//
// The Go side refuses any apiURL whose origin is not the panel the app was
// pointed at (isPanelOrigin), so a compromised page cannot aim the
// credential-bearing native client somewhere else. window.location.origin is
// NOT that panel: the app proxies the panel onto wails.localhost on purpose, so
// the webview never leaves that origin - which means the obvious expression
// names a host the check can never match, and the handoff is refused every
// time.
//
// Asking the app which panel it is showing gives the one value that matches, and
// it is correct by construction now that Core serves the panel and the API
// together. The old expression stays as the fallback for an app build that does
// not expose GetPanelURL.
export async function wailsAPIBase(app: WailsAppBindings): Promise<string> {
    if (typeof app.GetPanelURL === 'function') {
        try {
            const url = (await app.GetPanelURL())?.trim();
            if (url) return url.replace(/\/+$/, '') + '/api';
        } catch {
            // fall through to the build-time value
        }
    }
    return window.location.origin + '/api';
}

async function doSyncSession(): Promise<SessionSync> {
    devLog('beam.session', 'info', 'syncSessionWithWails: start');
    const app = getWailsApp();
    if (!app || typeof app.SetSession !== 'function') {
        devLog('beam.session', 'warn', 'syncSessionWithWails: Wails app or SetSession binding unavailable');
        // Not a transient failure - this build of Beam has no way to be handed
        // a session - so the reason says what to do about it rather than
        // inviting the user to try the same thing again.
        return { ok: false, reason: 'This version of the Beam app cannot accept a sign-in from the panel. Update Beam and open it again.' };
    }
    // The one place the panel still needs a real bearer, and the reason the
    // endpoint below exists: Wails' NATIVE side calls Core directly, with no
    // cookie jar and no browser, so a cookie is of no use to it.
    //
    // Core refuses that endpoint with 404 anywhere but this webview's own
    // origin, so a page in an ordinary browser cannot use it to turn the
    // HttpOnly cookie back into a token it could carry away. That restriction is
    // the whole security argument; see SessionToken in core/handlers/auth.go.
    let token = '';
    try {
        const res = await fetch(`${API_URL}/auth/session-token`, { method: 'POST' });
        const data = await res.json().catch(() => null);
        token = (res.ok && data?.token) || '';
    } catch {
        token = '';
    }
    if (!token) {
        devLog('beam.session', 'warn', 'syncSessionWithWails: Core did not issue a session token');
        return { ok: false, reason: 'The panel could not hand your sign-in to Beam. Reload the page; if it keeps happening, sign in again.' };
    }
    const apiUrl = await wailsAPIBase(app);
    devLog('beam.session', 'info', `SetSession → ${apiUrl}`);
    try {
        await app.SetSession(apiUrl, token);
        devLog('beam.session', 'info', 'SetSession OK');
        return { ok: true };
    } catch (err) {
        const message = err instanceof Error ? err.message : String(err);
        devLog('beam.session', 'error', `SetSession failed: ${message}`);
        console.warn('Wails SetSession failed:', err);
        // The Go-side message carried through rather than replaced: the two
        // rejections it can produce - an apiURL that is not the configured
        // panel origin, and a Core client that would not build - say different
        // things, and neither is guessable from here.
        return { ok: false, reason: `Beam would not accept the sign-in: ${message}` };
    }
}

// ensureWailsConnection points the native relay tunnel at `serverUuid`,
// throwing the real Go-side error on failure so callers can surface it
// (the Wails side lazily resolves the relay address, so this also
// covers first-call bootstrap). Deduped: a server that's already the
// live tunnel is a no-op — unless { force: true } is passed.
//
// Why force exists: the cheap "did we already connect?" check is just a
// string compare, it can't tell whether the underlying gRPC tunnel
// still works. Anything that disturbs the relay (restart, network
// blip, panel-side relay-address change) leaves us thinking we're
// connected when we aren't. Operations that are about to push real
// bytes (uploads especially) opt into a fresh dial so they fail fast
// with the actual reason instead of an Unavailable on a dead tunnel.
let lastConnectedServer = '';
export async function ensureWailsConnection(
    serverUuid: string,
    opts: { force?: boolean } = {},
): Promise<void> {
    devLog('beam.connect', 'info', `ensureWailsConnection: serverUuid=${serverUuid}, force=${opts.force ?? false}`);
    const app = getWailsApp();
    if (!app || !serverUuid) {
        devLog('beam.connect', 'warn', 'ensureWailsConnection: skipped (no Wails app or no serverUuid)');
        return;
    }
    // A failed handshake is reported here rather than let through to
    // ConnectToServer, which can only answer "not logged in" - the least
    // informative sentence available and the one that used to be shown.
    const sync = await syncSessionWithWails();
    if (!sync.ok) {
        devLog('beam.connect', 'error', `ensureWailsConnection: session handshake failed: ${sync.reason}`);
        throw new Error(sync.reason);
    }
    if (!opts.force && serverUuid === lastConnectedServer) {
        devLog('beam.connect', 'info', 'ensureWailsConnection: cache hit, skipping reconnect');
        return;
    }
    devLog('beam.connect', 'info', `ConnectToServer(${serverUuid}) → calling Wails binding`);
    // Drop the cache before the call: if ConnectToServer throws partway
    // through, the next attempt must re-dial — otherwise we'd treat a
    // failed half-connect as "still good".
    lastConnectedServer = '';
    try {
        await app.ConnectToServer(serverUuid);
        lastConnectedServer = serverUuid;
        devLog('beam.connect', 'info', 'ConnectToServer OK — relay tunnel established');
    } catch (err) {
        const message = err instanceof Error ? err.message : String(err);
        devLog('beam.connect', 'error', `ConnectToServer FAILED: ${message}`);
        throw err;
    }
}

// connectWailsToServer is the best-effort pre-warm used on navigation:
// it opens the tunnel ahead of time but only logs failures — the actual
// file op (e.g. upload) re-checks via ensureWailsConnection and surfaces
// the real error there.
export async function connectWailsToServer(serverUuid: string): Promise<void> {
    try {
        await ensureWailsConnection(serverUuid);
    } catch (err) {
        console.warn('Wails ConnectToServer failed:', err);
    }
}

export function createWailsBeamAdapter(): FileBrowserAdapter {
    const app = getWailsApp();
    if (!app) {
        throw new Error('Wails bindings are not available — createWailsBeamAdapter called outside Beam Desktop');
    }

    // wrap turns native Wails-binding calls into the {success, message?}
    // shape the FileBrowserAdapter contract expects. Return type is
    // intentionally loose — TS can't validate the gRPC payload shape
    // any tighter, and the FileBrowser code accesses fields structurally.
    type WrapResult = { success: boolean; message?: string } & Record<string, unknown>;
    const wrap = async (label: string, fn: () => Promise<unknown>): Promise<WrapResult> => {
        try {
            // Wait for the session to reach the Wails side first — the
            // native ops reject with "not logged in" until SetSession
            // has run, which would otherwise flash in the file list.
            //
            // And if it did not reach it, say so instead of calling anyway.
            // The binding's answer in that state is "not logged in", which the
            // file browser prints verbatim - to somebody looking at it through
            // a panel they are signed into.
            const sync = await syncSessionWithWails();
            if (!sync.ok) {
                devLog('beam.op', 'error', `${label} not attempted: ${sync.reason}`);
                return { success: false, message: sync.reason };
            }
            const result = await fn();
            if (result === undefined || result === null) return { success: true };
            if (typeof result === 'object' && 'success' in (result as Record<string, unknown>)) {
                const r = result as WrapResult;
                if (!r.success) {
                    devLog('beam.op', 'error', `${label} returned success=false: ${r.message ?? '(no message)'}`);
                }
                return r;
            }
            return { success: true, ...(result as Record<string, unknown>) };
        } catch (err) {
            const message = err instanceof Error ? err.message : String(err);
            devLog('beam.op', 'error', `${label} threw: ${message}`);
            return { success: false, message };
        }
    };

    return {
        // Reads stay open even while an upload is mid-stream so the user
        // can navigate the FileBrowser to watch progress.
        getFiles: (path, serverUuid) => wrap(`ListFiles ${path}`, () => app.ListFiles(path, serverUuid ?? '')) as ReturnType<FileBrowserAdapter['getFiles']>,
        getFileContent: (path, serverUuid) => wrap(`GetFileContent ${path}`, () => app.GetFileContent(path, serverUuid ?? '')) as ReturnType<FileBrowserAdapter['getFileContent']>,
        // Writes are blocked at the adapter so the FileBrowser surfaces a
        // clean inline error instead of letting an op race a live upload.
        saveFile: async (path, content, serverUuid) => guardWrite(serverUuid) ?? wrap(`SaveFile ${path}`, () => app.SaveFile(path, content, serverUuid ?? '')),
        createFile: async (path, isDir, serverUuid) => guardWrite(serverUuid) ?? wrap(`CreateFile ${path}`, () => app.CreateFile(path, isDir, serverUuid ?? '')),
        deleteFile: async (path, serverUuid) => guardWrite(serverUuid) ?? wrap(`DeleteFile ${path}`, () => app.DeleteFile(path, serverUuid ?? '')),
        renameFile: async (oldPath, newPath, serverUuid) => guardWrite(serverUuid) ?? wrap(`RenameFile ${oldPath}→${newPath}`, () => app.RenameFile(oldPath, newPath, serverUuid ?? '')),
        copyFile: async (srcPath, dstPath, serverUuid) => guardWrite(serverUuid) ?? wrap(`CopyFile ${srcPath}→${dstPath}`, () => app.CopyFile(srcPath, dstPath, serverUuid ?? '')),
        // Native chunked upload: bytes flow JS → Wails IPC → gRPC stream
        // → Relay tunnel → Node's temp file → atomic rename. Core never
        // sees the payload, so its body-size limit and the user's admin
        // upload cap don't apply here. If the build is missing the
        // bindings (older app), we fall back to HTTP through Core.
        uploadFiles: async (path, files, onProgress, strategy, mergeConflict, serverUuid) => {
            const hasNative =
                typeof app.BeamUploadStart === 'function' &&
                typeof app.BeamUploadChunk === 'function' &&
                typeof app.BeamUploadFinish === 'function';
            if (!hasNative) {
                devLog('beam.upload', 'warn', 'native bindings missing — falling back to HTTP upload via Core');
                return apiUploadFiles(path, files, onProgress, strategy, mergeConflict, serverUuid);
            }
            if (!files || files.length === 0) {
                devLog('beam.upload', 'info', 'uploadFiles: empty file list, no-op');
                return { success: true };
            }
            // FileBrowser zips folders into a single .zip before calling
            // the adapter, so we always upload exactly one File here.
            const file = files[0];
            const uploadID =
                typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function'
                    ? crypto.randomUUID()
                    : `${Date.now()}-${Math.random().toString(36).slice(2)}`;
            devLog('beam.upload', 'info', `uploadFiles: file="${file.name}" size=${file.size} path="${path}" serverUuid=${serverUuid ?? '?'} uploadID=${uploadID}`);

            // cancelRequested flips true when the user clicks the cancel
            // button in the UploadManager widget. The chunk loop checks it
            // between writes so a click takes effect on the next chunk
            // boundary instead of having to abort mid-WriteAt. We also
            // call app.BeamUploadCancel to tear the gRPC stream down
            // server-side immediately — that's what actually deletes the
            // temp file. The local flag just suppresses further chunks.
            let cancelRequested = false;
            const cancel = () => {
                cancelRequested = true;
                try { app.BeamUploadCancel?.(uploadID); } catch { /* best-effort */ }
            };

            // Register the job with the global UploadManager so the navbar
            // widget + modal can show it, and so the cancel button on the
            // row can invoke the cancel callback above.
            reportUploadStart({
                id: uploadID,
                filename: file.name,
                size: file.size,
                serverUuid: serverUuid ?? '',
                path,
                cancel,
            });

            try {
                // The native upload has no HTTP fallback — it needs the
                // relay tunnel. Force a fresh ConnectToServer here even
                // if we think we're already connected: a stale tunnel
                // (relay restart, network drop, address change) would
                // otherwise surface as a misleading "could not reach"
                // gRPC error on the upload stream instead of failing
                // upfront with the real reason.
                if (serverUuid) {
                    devLog('beam.upload', 'info', 'forcing fresh ConnectToServer before upload');
                    await ensureWailsConnection(serverUuid, { force: true });
                }
                devLog('beam.upload', 'info', `BeamUploadStart: opening gRPC stream (strategy="${strategy ?? ''}")`);
                await app.BeamUploadStart!(uploadID, path, file.name, strategy ?? '', file.size);
                devLog('beam.upload', 'info', 'BeamUploadStart OK — streaming chunks');

                // sendChunk wraps a single BeamUploadChunk call with a
                // time-budgeted retry loop. The loop tries the chunk,
                // tears the dead session down via BeamUploadResume on
                // failure (forces a fresh ConnectToServer — picks up
                // whichever relay Core hands out), and re-attempts.
                //
                // Two thresholds tuned for UX:
                //   * RETRY_VISIBLE_AFTER_MS — until this elapses,
                //     transient blips stay invisible (running status,
                //     no flicker). The user only sees "retrying" when
                //     the failure persists past this point.
                //   * RETRY_GIVE_UP_AFTER_MS — past this, we throw a
                //     user-facing "Connection unstable" message so
                //     the upload row clearly says try again rather
                //     than hanging in retry forever.
                //
                // WriteAt on the Node side is idempotent on identical
                // offsets, so re-sending the failed chunk after a
                // resume is safe.
                const RETRY_VISIBLE_AFTER_MS = 5000;
                const RETRY_GIVE_UP_AFTER_MS = 20000;
                const RETRY_BACKOFF_MS = 2000;
                const sendChunk = async (dataB64: string, off: number, chunkIdx: number) => {
                    let firstFailureAt = 0;
                    let attempt = 0;
                    let visibleFlipped = false;
                    while (true) {
                        if (cancelRequested) throw new Error('cancelled');
                        try {
                            await app.BeamUploadChunk!(uploadID, dataB64, off);
                            if (visibleFlipped) {
                                // Recovered from a visible retry — back to running.
                                reportUploadUpdate(uploadID, { status: 'running' });
                                devLog('beam.upload', 'info', `chunk #${chunkIdx} recovered after ${attempt} retry/retries`);
                            } else if (attempt > 0) {
                                devLog('beam.upload', 'info', `chunk #${chunkIdx} succeeded after ${attempt} silent retry/retries`);
                            }
                            return;
                        } catch (err) {
                            const m = err instanceof Error ? err.message : String(err);
                            // A deliberate Node refusal (limit, bad path, auth) is
                            // permanent for this upload — retrying wastes the whole
                            // retry budget and buries the real reason under a
                            // "Connection unstable" message. Fail fast with the
                            // Node's own reason instead.
                            if (isFatalBeamUploadError(m)) {
                                devLog('beam.upload', 'error', `chunk #${chunkIdx} refused by node (fatal, no retry): ${m}`);
                                throw new Error(cleanBeamGrpcMessage(m));
                            }
                            attempt++;
                            if (firstFailureAt === 0) firstFailureAt = Date.now();
                            const elapsed = Date.now() - firstFailureAt;

                            if (elapsed > RETRY_GIVE_UP_AFTER_MS || !app.BeamUploadResume) {
                                devLog('beam.upload', 'error', `chunk #${chunkIdx} gave up after ${attempt} attempt(s) / ${elapsed}ms: ${m}`);
                                throw new Error('Connection unstable — please try again. Underlying: ' + m);
                            }

                            if (!visibleFlipped && elapsed >= RETRY_VISIBLE_AFTER_MS) {
                                // Cross the visibility threshold — surface "retrying"
                                // so the user knows we're working on it.
                                reportUploadUpdate(uploadID, { status: 'retrying' });
                                visibleFlipped = true;
                            }

                            devLog('beam.upload', 'warn', `chunk #${chunkIdx} at offset ${off} failed (attempt ${attempt}, elapsed ${elapsed}ms): ${m} — resuming in ${RETRY_BACKOFF_MS}ms`);
                            await new Promise(r => setTimeout(r, RETRY_BACKOFF_MS));
                            if (cancelRequested) throw new Error('cancelled');

                            try {
                                await app.BeamUploadResume(uploadID, serverUuid ?? '', path, file.name, strategy ?? '', file.size);
                                devLog('beam.upload', 'info', `resumed upload "${file.name}" — retrying chunk #${chunkIdx}`);
                            } catch (resumeErr) {
                                // Resume failed too — the loop continues; the time
                                // budget above caps how long we keep trying.
                                const rm = resumeErr instanceof Error ? resumeErr.message : String(resumeErr);
                                devLog('beam.upload', 'warn', `resume attempt ${attempt} failed (elapsed ${elapsed}ms): ${rm}`);
                            }
                        }
                    }
                };

                let offset = 0;
                let chunks = 0;
                while (offset < file.size) {
                    if (cancelRequested) {
                        devLog('beam.upload', 'warn', `upload "${file.name}" cancelled by user at offset ${offset}`);
                        throw new Error('cancelled');
                    }
                    const end = Math.min(offset + UPLOAD_CHUNK_SIZE, file.size);
                    const buf = new Uint8Array(await file.slice(offset, end).arrayBuffer());
                    await sendChunk(uint8ToBase64(buf), offset, chunks);
                    offset = end;
                    chunks++;
                    onProgress(Math.round((offset / Math.max(file.size, 1)) * 100));
                    reportUploadProgress(uploadID, offset);
                }
                devLog('beam.upload', 'info', `streamed ${chunks} chunks (${file.size} bytes total) — calling BeamUploadFinish`);
                await app.BeamUploadFinish!(uploadID);
                devLog('beam.upload', 'info', `upload "${file.name}" finished successfully`);
                reportUploadFinish(uploadID, 'done');
                return { success: true };
            } catch (err) {
                try { await app.BeamUploadCancel?.(uploadID); } catch { /* best-effort */ }
                const message = err instanceof Error ? err.message : String(err);
                devLog('beam.upload', 'error', `upload "${file.name}" FAILED: ${message}`);
                reportUploadFinish(
                    uploadID,
                    cancelRequested || message === 'cancelled' ? 'cancelled' : 'error',
                    message,
                );
                return { success: false, message };
            }
        },
        downloadFile: async (path, serverUuid, isDir) => {
            // Wails opens its own native save dialog, so we don't need
            // browser progress tracking here.
            await app.DownloadFile(path, serverUuid ?? '', !!isDir);
        },
        selectiveDownload: async (basePath, selected, selectAll, serverUuid) => {
            await app.SelectiveDownload(basePath, selected, selectAll, serverUuid ?? '');
        },
        // Limits in Wails mode follow the active upload transport:
        //   * native gRPC available → unlimited (Node throttles itself)
        //   * HTTP fallback         → admin cap from Core (don't blow
        //     the body limit and overload shared infra)
        getUserLimits: async () => {
            if (typeof app.BeamUploadStart === 'function') {
                return { success: true, uploadLimit: 0, downloadLimit: 0 };
            }
            return apiGetUserLimits();
        },
    };
}
