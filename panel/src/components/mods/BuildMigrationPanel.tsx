'use client';

import { useState } from 'react';
import { useRouter } from 'next/navigation';
import { ChevronRight, Loader2, ArrowUpRight, X } from 'lucide-react';
import CompatMatrix from '@/components/mods/CompatMatrix';
import UnlinkedContentWarning from '@/components/mods/UnlinkedContentWarning';
import { useCompat } from '@/components/mods/useCompat';
import { migrateBuild, type CompatVersion } from '@/lib/api/modcompat';
import type { BuildContentEntry } from '@/lib/api/packs';
import ModalPanel from '@/components/ui/ModalPanel';

/**
 * What to tell someone after a migration returned 200.
 *
 * `failed` is NOT the list the matrix showed. Those mods were CHOSEN: the button
 * named them and the dialog confirmed dropping them. `failed` is content that
 * was supposed to travel and did not - a download that would not complete, a
 * storage write that was refused - and the build now exists without it.
 *
 * Reporting that as "Build created" and navigating away is how someone finds out
 * weeks later, from a mod that is simply not there. Exported for its own test:
 * the whole risk is a success message that is not one.
 */
export function migrationOutcome(
    versionString: string,
    minecraft: string,
    failed: { title: string }[] | undefined,
): { message: string; ok: boolean } {
    const lost = failed ?? [];
    if (lost.length === 0) {
        return { message: `Build ${versionString} created for Minecraft ${minecraft}.`, ok: true };
    }
    const names = lost.slice(0, 3).map(f => f.title).join(', ');
    const rest = lost.length > 3 ? ` and ${lost.length - 3} more` : '';
    return {
        message:
            `Build ${versionString} was created, but ${lost.length} ` +
            `${lost.length === 1 ? 'mod' : 'mods'} could not be added to it: ${names}${rest}. ` +
            `Add them by hand on the new build.`,
        ok: false,
    };
}

/**
 * "Could this build move to a newer Minecraft version, and what would it cost."
 *
 * A migration always produces a NEW build. The source is never rewritten: a
 * published build is frozen and could not be edited anyway, and rewriting a
 * draft would destroy the stand the new version is being compared against.
 */
export default function BuildMigrationPanel({
    packId,
    build,
    content,
    disabled,
    disabledReason,
    showToast,
}: {
    packId: number;
    build: { id: number; minecraft: string; loader: string; versionString: string };
    content: BuildContentEntry[];
    disabled: boolean;
    disabledReason?: string;
    showToast: (msg: string, ok?: boolean) => void;
}) {
    const [open, setOpen] = useState(false);
    const [target, setTarget] = useState<CompatVersion | null>(null);
    const compat = useCompat({ kind: 'build', packId, buildId: build.id }, build.minecraft);

    const unlinked = content.filter(e => !e.linked).length;

    return (
        <section className="card mx-6 mb-4 max-w-6xl overflow-hidden">
            <button
                type="button"
                onClick={() => setOpen(o => !o)}
                aria-expanded={open}
                className="w-full flex items-center gap-3 px-4 py-3 text-left hover:bg-(--base-03) focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] transition-colors"
            >
                <ChevronRight size={14} className={`text-(--base-06) transition-transform ${open ? 'rotate-90' : ''}`} />
                <div className="min-w-0">
                    <h2 className="text-sm font-medium text-(--base-09)">Minecraft version</h2>
                    <p className="text-xs text-(--base-06)">
                        Check which newer versions this build&apos;s mods are available for, then create a build for one.
                    </p>
                </div>
            </button>

            {open && (
                <div className="border-t border-(--base-03) p-4 flex flex-col gap-4">
                    {unlinked > 0 && <UnlinkedContentWarning count={unlinked} context="pack" />}

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
                                disabled={disabled}
                                onClick={() => setTarget(v)}
                                title={disabled ? disabledReason : `Create a build for Minecraft ${v.minecraft}`}
                                className="btn btn-secondary btn-sm inline-flex items-center gap-1.5 disabled:opacity-40 disabled:cursor-not-allowed"
                            >
                                <ArrowUpRight size={12} />
                                {v.missing.length > 0 ? `Build without ${v.missing.length}` : 'Create build'}
                            </button>
                        )}
                    />
                </div>
            )}

            {target && (
                <MigrateDialog
                    packId={packId}
                    build={build}
                    target={target}
                    unlinked={unlinked}
                    onClose={() => setTarget(null)}
                    showToast={showToast}
                />
            )}
        </section>
    );
}

function MigrateDialog({
    packId,
    build,
    target,
    unlinked,
    onClose,
    showToast,
}: {
    packId: number;
    build: { id: number; versionString: string };
    target: CompatVersion;
    unlinked: number;
    onClose: () => void;
    showToast: (msg: string, ok?: boolean) => void;
}) {
    const router = useRouter();
    // A sensible default the author will usually keep: the same version string
    // with the target Minecraft version appended, which stays unique against
    // the source build.
    const [versionString, setVersionString] = useState(`${build.versionString}-mc${target.minecraft}`);
    const [changelog, setChangelog] = useState('');
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState('');

    const willDrop = target.missing.length;

    const submit = async () => {
        setBusy(true);
        setError('');
        const res = await migrateBuild(packId, build.id, {
            minecraft: target.minecraft,
            versionString: versionString.trim(),
            changelog,
            // The matrix already showed exactly what is missing and the button
            // said so, so the confirmation is this dialog rather than a second
            // round trip that comes back 409.
            dropUnavailable: willDrop > 0,
        });
        setBusy(false);
        if (!res.success || !res.build) {
            setError(res.message || 'Creating the build failed.');
            return;
        }

        // `failed` is NOT the same list as the mods the matrix said were
        // missing. Those were chosen: the button named them and this dialog
        // confirmed dropping them. `failed` is content that was supposed to
        // travel and did not - a download that would not complete, a storage
        // write that was refused - and the build exists without it.
        //
        // Reporting that as "Build created" and navigating away is how someone
        // finds out weeks later, by a mod that is simply not there.
        const outcome = migrationOutcome(res.build.versionString, target.minecraft, res.failed);
        showToast(outcome.message, outcome.ok);
        onClose();
        router.push(`/modpacks/${packId}/builds/${res.build.id}`);
    };

    return (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4">
            <ModalPanel className="card w-full max-w-lg max-h-[90vh] flex flex-col">
                <div className="modal-header flex items-center justify-between">
                    <h3 className="modal-title">Create a build for Minecraft {target.minecraft}</h3>
                    <button type="button" onClick={onClose} className="text-(--base-07) hover:text-(--error-light) transition-colors">
                        <X size={18} />
                    </button>
                </div>

                <div className="p-6 flex flex-col gap-4 overflow-y-auto">
                    <p className="text-sm text-(--base-07)">
                        A new build is created from <span className="font-mono text-(--base-09)">{build.versionString}</span>.
                        That build stays exactly as it is.
                    </p>

                    <div className="flex flex-col gap-[5px]">
                        <label className="input-label" htmlFor="migrate-version">New build version</label>
                        <input
                            id="migrate-version"
                            className="input-field w-full"
                            value={versionString}
                            onChange={e => setVersionString(e.target.value)}
                            autoFocus
                        />
                        <p className="text-xs text-(--base-06)">
                            Has to be unique within this pack, and is used in download file names.
                        </p>
                    </div>

                    <div className="flex flex-col gap-[5px]">
                        <label className="input-label" htmlFor="migrate-changelog">Changelog (optional)</label>
                        <textarea
                            id="migrate-changelog"
                            className="input-field w-full text-sm"
                            rows={3}
                            value={changelog}
                            onChange={e => setChangelog(e.target.value)}
                        />
                    </div>

                    {willDrop > 0 && (
                        <div className="rounded-md border border-(--error-light)/30 bg-(--error-ghost) px-3 py-2.5">
                            <p className="text-sm text-(--base-09) font-medium mb-1">
                                {willDrop} {willDrop === 1 ? 'mod is' : 'mods are'} left out
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

                    {unlinked > 0 && (
                        <UnlinkedContentWarning count={unlinked} context="pack" />
                    )}

                    {error && <p className="text-xs text-(--error-light)">{error}</p>}
                </div>

                <div className="modal-footer flex justify-end gap-2">
                    <button type="button" onClick={onClose} className="btn btn-secondary">Cancel</button>
                    <button
                        type="button"
                        onClick={submit}
                        disabled={busy || !versionString.trim()}
                        className="btn btn-primary inline-flex items-center gap-2 disabled:opacity-40 disabled:cursor-not-allowed"
                    >
                        {busy && <Loader2 size={14} className="animate-spin" />}
                        Create build
                    </button>
                </div>
            </ModalPanel>
        </div>
    );
}
