'use client';

import { useEffect, useRef, useState } from 'react';
import { GitFork, Loader2 } from 'lucide-react';
import { forkPackBuild, type Pack, type PackBuild } from '@/lib/api/packs';
import ModalPanel from '@/components/ui/ModalPanel';
import Select from '@/components/ui/Select';
import { toast } from '@/components/ui/Toast';
import { API_URL, getAuthHeader } from '@/lib/api/core';

export default function ForkBuildDialog({
    pack,
    build,
    onClose,
    onCreated,
}: {
    pack: Pack;
    build: PackBuild;
    onClose: () => void;
    onCreated: (pack: Pack, build: PackBuild) => void;
}) {
    const [mode, setMode] = useState<'new' | 'existing'>('new');
    const [name, setName] = useState(pack.internalName + ' fork');
    const [slug, setSlug] = useState('');
    const [version, setVersion] = useState(build.versionString);
    const [target, setTarget] = useState('');
    const [targets, setTargets] = useState<Pack[]>([]);
    const [loading, setLoading] = useState(true);
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState('');
    const controller = useRef<AbortController | null>(null);
    const mounted = useRef(true);
    useEffect(() => {
        mounted.current = true;
        return () => {
            mounted.current = false;
            controller.current?.abort();
        };
    }, []);
    useEffect(() => {
        let live = true;
        const listRequest = new AbortController();
        const read = async (path: string) => {
            const response = await fetch(`${API_URL}${path}`, { headers: getAuthHeader(), signal: listRequest.signal });
            const data = await response.json();
            if (!response.ok || data.success === false) throw new Error('Could not inspect target packs.');
            return data;
        };
        const load = async () => {
            try {
                const overview = (await read('/me/packs')) as {
                    packs: Pack[];
                    shared: (Pack & { canEdit: boolean })[];
                };
                const candidates = [...overview.packs, ...overview.shared.filter((p) => p.canEdit)].filter(
                    (p) => p.id !== pack.id,
                );
                const empty: Pack[] = [];
                let next = 0;
                const worker = async () => {
                    while (live && next < candidates.length) {
                        const p = candidates[next++];
                        const builds = (await read(`/packs/${p.id}/builds`)).builds as PackBuild[];
                        if (!builds.length) empty.push(p);
                    }
                };
                await Promise.all([worker(), worker(), worker()]);
                if (live) setTargets(empty.sort((a, b) => a.internalName.localeCompare(b.internalName)));
            } catch {
                if (live) setError('Could not load empty target packs.');
            } finally {
                if (live) setLoading(false);
            }
        };
        void load();
        return () => {
            live = false;
            listRequest.abort();
        };
    }, [pack.id]);
    const close = () => {
        controller.current?.abort();
        onClose();
    };
    const submit = async () => {
        setBusy(true);
        setError('');
        const request = new AbortController();
        controller.current = request;
        const result = await forkPackBuild(
            pack.id,
            build.id,
            {
                ...(mode === 'existing'
                    ? { targetPackId: Number(target) }
                    : { name: name.trim(), slug: slug.trim() || undefined }),
                versionString: version.trim(),
            },
            request.signal,
        );
        if (!mounted.current || request.signal.aborted) return;
        setBusy(false);
        if (result.success && result.pack && result.build) {
            toast('Build forked into an independent draft.');
            onCreated(result.pack, result.build);
        } else setError(result.message || 'Could not fork this build.');
    };
    return (
        <div className="modal-overlay" onClick={close}>
            <ModalPanel className="modal-panel max-w-xl" onClose={close} onClick={(event) => event.stopPropagation()}>
                <div className="modal-header">
                    <h2 className="modal-title">Fork build</h2>
                    <p className="text-xs text-(--base-06) mt-2">
                        {pack.internalName} / {build.versionString} · MC {build.minecraft} · {build.loader}
                    </p>
                </div>
                <div className="modal-body space-y-4">
                    <p className="text-sm">
                        Copy every pinned mod, resource and uploaded config/script into an independent draft, including
                        folders and client/server settings. The source stays unchanged.
                    </p>
                    <div role="group" aria-label="Fork destination" className="flex gap-2">
                        {(['new', 'existing'] as const).map((value) => (
                            <button
                                key={value}
                                className={`btn btn-sm ${mode === value ? 'btn-primary' : 'btn-secondary'}`}
                                aria-pressed={mode === value}
                                disabled={busy}
                                onClick={() => setMode(value)}
                            >
                                {value === 'new' ? 'New modpack' : 'Existing empty modpack'}
                            </button>
                        ))}
                    </div>
                    {mode === 'new' ? (
                        <>
                            <label className="input-label block">
                                New pack name
                                <input
                                    aria-label="New pack name"
                                    className="input-field w-full mt-2"
                                    maxLength={128}
                                    value={name}
                                    disabled={busy}
                                    onChange={(event) => setName(event.target.value)}
                                />
                            </label>
                            <label className="input-label block">
                                Slug (optional)
                                <input
                                    aria-label="Fork pack slug"
                                    className="input-field w-full mt-2"
                                    maxLength={64}
                                    placeholder="Generated from the name"
                                    value={slug}
                                    disabled={busy}
                                    onChange={(event) => setSlug(event.target.value)}
                                />
                            </label>
                        </>
                    ) : (
                        <>
                            <Select
                                ariaLabel="Empty target modpack"
                                searchable
                                value={target}
                                disabled={busy || loading}
                                onChange={setTarget}
                                placeholder={loading ? 'Finding empty packs...' : 'Choose an empty pack'}
                                options={targets.map((p) => ({ value: String(p.id), label: p.internalName }))}
                            />
                            {!loading && !targets.length && (
                                <p className="text-xs text-(--base-06)">
                                    No empty writable packs found. Create a new pack here instead. An empty target has
                                    no builds.
                                </p>
                            )}
                        </>
                    )}
                    <label className="input-label block">
                        Draft build version
                        <input
                            aria-label="Fork build version"
                            className="input-field w-full mt-2"
                            maxLength={64}
                            value={version}
                            disabled={busy}
                            onChange={(event) => setVersion(event.target.value)}
                        />
                    </label>
                    <p className="text-xs text-(--base-06)">
                        A new pack starts private, without the source&apos;s Modrinth binding, share links or
                        collaborators. Existing target settings stay unchanged. Uploaded files are copied to the target
                        owner; large copies can take a while.
                    </p>
                    {busy && (
                        <p role="status" className="flex gap-2 items-center text-sm">
                            <Loader2 className="animate-spin" size={14} />
                            Copying and verifying content...
                        </p>
                    )}
                    {error && (
                        <p role="alert" className="text-sm text-(--error-light)">
                            {error}
                        </p>
                    )}
                </div>
                <div className="modal-footer">
                    <button className="btn btn-secondary" onClick={close}>
                        Cancel
                    </button>
                    <button
                        className="btn btn-primary"
                        onClick={submit}
                        disabled={busy || !version.trim() || (mode === 'new' ? !name.trim() : !target)}
                    >
                        <GitFork size={14} />
                        {busy ? 'Copying...' : 'Fork into draft'}
                    </button>
                </div>
            </ModalPanel>
        </div>
    );
}
