'use client';

import { useCallback, useEffect, useState } from 'react';
import { AlertTriangle, Link2, Loader2, RefreshCw, Trash2 } from 'lucide-react';

import { getUnmanagedMods, identifyMods, type UnmanagedFile, type IdentifyResult } from '@/lib/api/modcompat';
import { deleteFile } from '@/lib/api/files';
import { confirmDialog } from '@/components/ui/ConfirmDialog';

/**
 * The jars in mods/ and plugins/ that nothing in the database claims, and what
 * Modrinth says they are.
 *
 * Two quite different things end up here and both matter:
 *
 *   - A jar somebody uploaded by SFTP, beam or the file manager. Nothing knows
 *     what it is, so it gets no update badge, no version comparison and is not
 *     carried by a Minecraft-version migration. Identifying it by hash turns it
 *     into an ordinary managed mod.
 *
 *   - A leftover from the update bug, where installing a newer build wrote the
 *     new jar beside the old one instead of replacing it. Core recognises those
 *     precisely - the hash resolves to a project that is ALREADY installed
 *     under a different file name - and says so. They are the reason there is a
 *     Remove button here: every server updated before the fix is carrying one
 *     per updated mod, and the row that could have named the old file was
 *     overwritten by the update itself.
 *
 * This used to live only inside the Minecraft-version panel, which is the wrong
 * place to find it: somebody who uploaded a jar goes to Content, sees it
 * nowhere, and has no reason to open a version-migration screen. It is a
 * component now so both screens show the same thing.
 */
export function UnmanagedJars({
    serverId,
    serverUuid,
    activeSubServer,
    onChanged,
    showToast,
}: {
    serverId: number;
    serverUuid?: string;
    activeSubServer?: string;
    onChanged: () => void;
    showToast: (msg: string, ok?: boolean) => void;
}) {
    const [files, setFiles] = useState<UnmanagedFile[] | null>(null);
    const [scanError, setScanError] = useState<string | null>(null);
    const [scanning, setScanning] = useState(false);
    const [identifying, setIdentifying] = useState(false);
    const [results, setResults] = useState<Record<string, IdentifyResult>>({});
    const [removing, setRemoving] = useState<string | null>(null);

    const scan = useCallback(async () => {
        setScanning(true);
        try {
            setFiles(await getUnmanagedMods(serverId));
            setScanError(null);
        } catch (e) {
            // Core distinguishes "could not read this server's files from its
            // node" from "nothing unmanaged" on purpose, and says why in its own
            // comment: reporting the first as the second hides exactly what this
            // endpoint exists to reveal. The panel used to flatten both to an
            // empty list, so the guard only existed on one side of the boundary.
            setScanError(e instanceof Error ? e.message : 'Could not check this server for unknown jars.');
        } finally {
            setScanning(false);
        }
    }, [serverId]);

    useEffect(() => { scan(); }, [scan]);

    const handleIdentify = async () => {
        if (!files || files.length === 0) return;
        setIdentifying(true);
        const res = await identifyMods(serverId, files.map(f => ({ directory: f.directory, name: f.name })));
        setIdentifying(false);
        if (!res.success) {
            showToast(res.message || 'Identifying failed', false);
            return;
        }
        // Kept per file, not summed into a toast. Core writes a REASON for every
        // file it could not link - "no Modrinth version has this file's hash",
        // "Modrinth could not be reached", "already installed as x.jar, so this
        // is a second copy" - and the old summary counted all three as "still
        // unidentified" and discarded the sentence that tells them apart. The
        // last one is the leftover this screen exists to clear.
        const byName: Record<string, IdentifyResult> = {};
        for (const r of res.results || []) byName[`${r.directory}/${r.name}`] = r;
        setResults(byName);

        const linked = res.linked || 0;
        showToast(
            linked === 0
                ? 'None of these matched a Modrinth version. See the reason on each file.'
                : `Linked ${linked} ${linked === 1 ? 'file' : 'files'}.`,
            linked > 0,
        );
        await scan();
        onChanged();
    };

    const handleRemove = async (f: UnmanagedFile) => {
        if (!serverUuid) { showToast('This server has no storage to delete from', false); return; }
        const key = `${f.directory}/${f.name}`;
        const ok = await confirmDialog({
            title: 'Delete this file?',
            message: `${key} will be removed from the server. It takes effect when the server next starts - a running server keeps the copy it already loaded.`,
            confirmLabel: 'Delete',
            destructive: true,
        });
        if (!ok) return;
        setRemoving(key);
        const path = activeSubServer ? `${activeSubServer}/${key}` : key;
        const res = await deleteFile(path, serverUuid);
        setRemoving(null);
        if (res.success) {
            showToast(`Deleted ${f.name}`, true);
            await scan();
            onChanged();
        } else {
            showToast(res.message || 'Delete failed', false);
        }
    };

    if (scanError) {
        return (
            <div className="flex items-start gap-2 rounded-md border border-(--warning)/30 bg-(--warning-ghost) p-3 text-xs text-(--warning-light)">
                <AlertTriangle size={14} className="mt-0.5 shrink-0" />
                <div className="flex-1">
                    <p className="text-(--base-09)">Could not check for unknown jars.</p>
                    <p className="mt-0.5">{scanError}</p>
                </div>
                <button type="button" onClick={scan} disabled={scanning} className="btn btn-secondary btn-sm shrink-0">
                    Try again
                </button>
            </div>
        );
    }
    if (!files || files.length === 0) return null;

    return (
        <div className="rounded-md border border-(--warning)/30 bg-(--warning-ghost) p-3 space-y-2">
            <div className="flex items-start justify-between gap-3 flex-wrap">
                <div className="flex items-start gap-2 min-w-0">
                    <AlertTriangle size={14} className="mt-0.5 shrink-0 text-(--warning-light)" />
                    <div className="min-w-0">
                        <p className="text-xs text-(--base-09)">
                            {files.length} {files.length === 1 ? 'file' : 'files'} in this server that nothing here knows about
                        </p>
                        <p className="text-[11px] text-(--base-06) mt-0.5">
                            They get no update check and are not carried by a Minecraft-version move. Identifying
                            them by their hash turns them into ordinary entries.
                        </p>
                    </div>
                </div>
                <div className="flex items-center gap-1.5 shrink-0">
                    <button
                        type="button"
                        onClick={handleIdentify}
                        disabled={identifying}
                        className="btn btn-secondary btn-sm"
                    >
                        {identifying ? <Loader2 size={12} className="animate-spin" /> : <Link2 size={12} />}
                        {identifying ? 'Identifying' : 'Identify against Modrinth'}
                    </button>
                    <button type="button" onClick={scan} disabled={scanning} className="btn btn-secondary btn-sm">
                        <RefreshCw size={12} className={scanning ? 'animate-spin' : ''} />
                        Rescan
                    </button>
                </div>
            </div>

            <ul className="space-y-1">
                {files.map(f => {
                    const key = `${f.directory}/${f.name}`;
                    const r = results[key];
                    return (
                        <li key={key} className="flex items-start gap-2 rounded-md bg-(--base-01)/60 px-2.5 py-1.5">
                            <div className="min-w-0 flex-1">
                                <div className="font-mono text-[11px] text-(--base-08) truncate">{key}</div>
                                {r?.reason && (
                                    <div className="text-[11px] text-(--base-06) mt-0.5">{r.reason}</div>
                                )}
                            </div>
                            {/* Offered for anything identify could not link. A file it
                                DID link is a managed mod now and is removed the normal
                                way, with its row, rather than as a stray file. */}
                            {r && !r.matched && (
                                <button
                                    type="button"
                                    onClick={() => handleRemove(f)}
                                    disabled={removing === key}
                                    className="btn btn-secondary btn-sm shrink-0"
                                >
                                    <Trash2 size={11} className="text-(--error-light)" />
                                    {removing === key ? 'Deleting…' : 'Delete'}
                                </button>
                            )}
                        </li>
                    );
                })}
            </ul>
        </div>
    );
}
