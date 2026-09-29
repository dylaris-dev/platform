'use client';

import { useCallback, useEffect, useState } from 'react';
import { Loader2, ArrowUpRight, X, Copy } from 'lucide-react';
import CompatMatrix from '@/components/mods/CompatMatrix';
import { UnmanagedJars } from '@/components/mods/UnmanagedJars';
import UnlinkedContentWarning from '@/components/mods/UnlinkedContentWarning';
import { useCompat } from '@/components/mods/useCompat';
import {
    copySubServer,
    getUnmanagedMods,
    updateServerVersion,
    type CompatVersion,
} from '@/lib/api/modcompat';
import Checkbox from '@/components/ui/Checkbox';
import { isKnownLoader } from '@/lib/serverLoaderMetadata';
import type { Server } from '@/lib/api/types';
import ModalPanel from '@/components/ui/ModalPanel';

/**
 * "Move this server to another Minecraft version and take its mods along."
 *
 * The move itself is one node command, sequenced on the node: stop, drop the
 * old jars, fetch the new ones, reinstall the server software, restart. It is
 * one command because the node's queue runs commands concurrently, so a
 * separate reinstall could restart the container while a mod download was still
 * in flight.
 */
export default function ServerVersionPanel({
    server,
    onChanged,
    showToast,
}: {
    server: Server;
    onChanged: () => void;
    showToast: (msg: string, ok?: boolean) => void;
}) {
    const current = server.minecraftVersion || '';
    const loaderKnown = isKnownLoader(server.installerType || '');
    const compat = useCompat({ kind: 'server', serverId: server.id }, current);

    const [target, setTarget] = useState<CompatVersion | null>(null);
    // Kept only to colour the compat matrix's own warning. The list, the
    // identify action and the per-file reasons live in UnmanagedJars now, which
    // the Content tab shows too - somebody who uploaded a jar looks there, not
    // on a version-migration screen.
    const [unmanagedCount, setUnmanagedCount] = useState(0);
    const scan = useCallback(async () => {
        try {
            setUnmanagedCount((await getUnmanagedMods(server.id)).length);
        } catch {
            // The banner below is decoration for this panel; UnmanagedJars is
            // what reports a failed scan, and it is on the screen.
            setUnmanagedCount(0);
        }
    }, [server.id]);
    useEffect(() => { scan(); }, [scan]);

    if (!current || !loaderKnown) {
        return (
            <div className="card p-4">
                <p className="text-sm text-(--base-09) font-medium mb-1">
                    Declare this server&apos;s loader and Minecraft version first
                </p>
                <p className="text-xs text-(--base-07)">
                    A version check compares what is installed against a specific loader and Minecraft version. Without
                    both, there is nothing to compare against. The Browse tab offers the declare flow.
                </p>
            </div>
        );
    }

    return (
        // flex-1 min-h-0 so this fills the height its parent section has and
        // scrolls inside it. Without them it is as tall as its content, and the
        // page's overflow-hidden clips the end of it out of reach.
        <div className="flex-1 min-h-0 flex flex-col gap-4 overflow-y-auto">
            <UnmanagedJars
                serverId={server.id}
                serverUuid={server.uuid}
                activeSubServer={server.activeSubServer}
                onChanged={() => { scan(); onChanged(); }}
                showToast={showToast}
            />

            <CompatMatrix
                data={compat.data}
                loading={compat.loading}
                error={compat.error}
                mode={compat.mode}
                onModeChange={compat.setMode}
                specific={compat.specific}
                onSpecificChange={compat.setSpecific}
                versionOptions={compat.versionOptions}
                onRefresh={compat.refresh}
                renderAction={v => (
                    <button
                        type="button"
                        onClick={() => setTarget(v)}
                        title={`Move this server to Minecraft ${v.minecraft}`}
                        className="btn btn-secondary btn-sm inline-flex items-center gap-1.5"
                    >
                        <ArrowUpRight size={12} />
                        {v.missing.length > 0 ? `Move without ${v.missing.length}` : 'Move here'}
                    </button>
                )}
            />

            {target && (
                <MoveDialog
                    server={server}
                    target={target}
                    unmanaged={unmanagedCount}
                    onClose={() => setTarget(null)}
                    onChanged={onChanged}
                    showToast={showToast}
                />
            )}
        </div>
    );
}

function MoveDialog({
    server,
    target,
    unmanaged,
    onClose,
    onChanged,
    showToast,
}: {
    server: Server;
    target: CompatVersion;
    unmanaged: number;
    onClose: () => void;
    onChanged: () => void;
    showToast: (msg: string, ok?: boolean) => void;
}) {
    const [installerVersion, setInstallerVersion] = useState('');
    const [copyFirst, setCopyFirst] = useState(false);
    const [copyName, setCopyName] = useState(`${server.activeSubServer || 'server'}-${server.minecraftVersion || 'old'}`);
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState('');

    const running = server.status === 'online' || server.status === 'installing';
    const willDrop = target.missing.length;

    const submit = async () => {
        setBusy(true);
        setError('');
        if (copyFirst) {
            const copy = await copySubServer(server.id, copyName.trim());
            if (!copy.success) {
                setBusy(false);
                setError(copy.message || 'The copy failed, so the move was not started.');
                return;
            }
        }
        const res = await updateServerVersion(server.id, {
            minecraft: target.minecraft,
            installerVersion: installerVersion.trim() || undefined,
            dropUnavailable: willDrop > 0,
        });
        setBusy(false);
        if (!res.success) {
            setError(res.message || 'The version update failed.');
            return;
        }
        showToast(res.message || `Moving to Minecraft ${target.minecraft}.`);
        onClose();
        onChanged();
    };

    return (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4">
            <ModalPanel className="card w-full max-w-lg max-h-[90vh] flex flex-col">
                <div className="modal-header flex items-center justify-between">
                    <h3 className="modal-title">Move to Minecraft {target.minecraft}</h3>
                    <button type="button" onClick={onClose} className="text-(--base-07) hover:text-(--error-light) transition-colors">
                        <X size={18} />
                    </button>
                </div>

                <div className="p-6 flex flex-col gap-4 overflow-y-auto">
                    <p className="text-sm text-(--base-07)">
                        The server stops, its mods are replaced with their versions for{' '}
                        <span className="font-mono text-(--base-09)">{target.minecraft}</span>, the server software is
                        reinstalled at that version, and it starts again. The world and the configuration stay.
                    </p>

                    <div className="flex flex-col gap-[5px]">
                        <label className="input-label" htmlFor="move-loader-version">Loader build (optional)</label>
                        <input
                            id="move-loader-version"
                            className="input-field w-full"
                            placeholder="Leave empty for the newest build"
                            value={installerVersion}
                            onChange={e => setInstallerVersion(e.target.value)}
                        />
                    </div>

                    <div className="rounded-md border border-(--base-04) p-3 flex flex-col gap-2">
                        <Checkbox
                            checked={copyFirst}
                            onChange={setCopyFirst}
                            disabled={running}
                            label={
                                <span className="inline-flex items-center gap-1.5">
                                    <Copy size={12} className="text-(--accent-light)" /> Copy the current server first
                                </span>
                            }
                            hint={running
                                ? 'Stop the server to enable this: a running server’s world files would be copied mid-write.'
                                : 'Keeps the current state as a second sub-server, so the old version is one switch away if the move goes badly.'}
                        />
                        {copyFirst && (
                            <input
                                className="input-field w-full text-sm"
                                value={copyName}
                                onChange={e => setCopyName(e.target.value)}
                                aria-label="Name for the copy"
                            />
                        )}
                    </div>

                    {willDrop > 0 && (
                        <div className="rounded-md border border-(--error-light)/30 bg-(--error-ghost) px-3 py-2.5">
                            <p className="text-sm text-(--base-09) font-medium mb-1">
                                {willDrop} installed {willDrop === 1 ? 'mod is' : 'mods are'} removed
                            </p>
                            <p className="text-xs text-(--base-07) mb-2">
                                No version of these is published for {target.minecraft} on this loader, so they cannot come along.
                            </p>
                            <ul className="text-xs text-(--base-07) flex flex-col gap-0.5">
                                {target.missing.map(m => (
                                    <li key={m.key} className="truncate">
                                        {m.title || m.projectId}
                                        <span className="text-(--base-06)"> · {m.side === 'both' ? 'client + server' : m.side === 'client' ? 'client only' : 'server only'}</span>
                                    </li>
                                ))}
                            </ul>
                        </div>
                    )}

                    {unmanaged > 0 && <UnlinkedContentWarning count={unmanaged} context="server" />}

                    {error && <p className="text-xs text-(--error-light)">{error}</p>}
                </div>

                <div className="modal-footer flex justify-end gap-2">
                    <button type="button" onClick={onClose} className="btn btn-secondary">Cancel</button>
                    <button
                        type="button"
                        onClick={submit}
                        disabled={busy || (copyFirst && !copyName.trim())}
                        className="btn btn-primary inline-flex items-center gap-2 disabled:opacity-40 disabled:cursor-not-allowed"
                    >
                        {busy && <Loader2 size={14} className="animate-spin" />}
                        {copyFirst ? 'Copy and move' : 'Move'}
                    </button>
                </div>
            </ModalPanel>
        </div>
    );
}
