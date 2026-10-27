'use client';

import { createContext, useContext, useEffect, useState, useCallback, useRef, type ReactNode } from 'react';
import { Star, Bell, Trash2, Loader2 } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import { libraryRequest, type Favorite, type AvailabilityWatch } from '@/lib/api/modrinthLibrary';
import { getPackMinecraftVersions } from '@/lib/api/packMetadata';
import Select from '@/components/ui/Select';
import ModalPanel from '@/components/ui/ModalPanel';
import { toast } from '@/components/ui/Toast';
import ProjectIcon, { useProjectMetadata } from '@/components/modpacks/ProjectIcon';

type Library = {
    favorites: Favorite[];
    ready: boolean;
    refresh: () => Promise<void>;
    toggle: (id: string) => Promise<void>;
};
const Context = createContext<Library | null>(null);
export function ModrinthLibraryProvider({ children }: { children: ReactNode }) {
    const { user } = useAppData();
    const [favorites, setFavorites] = useState<Favorite[]>([]);
    const [ready, setReady] = useState(false);
    const generation = useRef(0);
    const refresh = useCallback(async () => {
        const current = generation.current;
        try {
            const data = await libraryRequest('/me/modrinth-favorites');
            if (current !== generation.current) return;
            setFavorites(data.favorites || []);
            setReady(true);
        } catch {
            if (current === generation.current) setReady(false);
        }
    }, []);
    useEffect(() => {
        generation.current++;
        setReady(false);
        setFavorites([]);
        if (user) void refresh();
        return () => {
            generation.current++;
        };
    }, [user?.id, refresh]);
    const toggle = async (id: string) => {
        try {
            await libraryRequest('/me/modrinth-favorites', 'PUT', {
                projectId: id,
                remove: favorites.some((f) => f.projectId === id),
            });
            await refresh();
        } catch (error) {
            toast(error instanceof Error ? error.message : 'Could not save favorite.', false);
        }
    };
    return <Context.Provider value={{ favorites, ready, refresh, toggle }}>{children}</Context.Provider>;
}

export function ModrinthProjectActions({
    projectId,
    title,
    loader,
}: {
    projectId: string;
    title: string;
    loader?: string;
}) {
    const library = useContext(Context);
    const [watch, setWatch] = useState(false);
    const [saving, setSaving] = useState(false);
    if (!projectId || !library) return null;
    const favorite = library.favorites.some((f) => f.projectId === projectId);
    return (
        <>
            <button
                aria-label={`${favorite ? 'Unfavorite' : 'Favorite'} ${title}`}
                aria-pressed={favorite}
                disabled={saving}
                title={library.ready ? 'Save this project as a favorite' : 'Reload favorites'}
                onClick={async () => {
                    setSaving(true);
                    try {
                        if (!library.ready) await library.refresh();
                        else await library.toggle(projectId);
                    } finally {
                        setSaving(false);
                    }
                }}
                className={`btn btn-ghost btn-sm ${favorite ? 'text-(--warning-light)' : ''}`}
            >
                <Star size={14} fill={favorite ? 'currentColor' : 'none'} />
            </button>
            <button
                aria-label={`Version alerts for ${title}`}
                title="Notify when a Minecraft version is supported"
                onClick={() => setWatch(true)}
                className="btn btn-ghost btn-sm"
            >
                <Bell size={14} />
            </button>
            {watch && (
                <ModrinthWatchDialog project={{ projectId, title }} loader={loader} onClose={() => setWatch(false)} />
            )}
        </>
    );
}

export function ModrinthWatchDialog({
    project,
    packId,
    buildId,
    title,
    loader: initialLoader,
    onClose,
    onSaved,
}: {
    project?: Favorite;
    packId?: number;
    buildId?: number;
    title?: string;
    loader?: string;
    onClose: () => void;
    onSaved?: () => void;
}) {
    const [versions, setVersions] = useState<string[]>([]);
    const [selected, setSelected] = useState<string[]>([]);
    const [future, setFuture] = useState(false);
    const [loader, setLoader] = useState(initialLoader && initialLoader !== 'vanilla' ? initialLoader : 'fabric');
    const [query, setQuery] = useState('');
    const [error, setError] = useState('');
    const [loading, setLoading] = useState(true);
    const [saving, setSaving] = useState(false);
    const [attempt, setAttempt] = useState(0);
    useEffect(() => {
        const controller = new AbortController();
        setLoading(true);
        setError('');
        getPackMinecraftVersions(controller.signal)
            .then(setVersions)
            .catch((e) => {
                if (!controller.signal.aborted) setError(e.message);
            })
            .finally(() => {
                if (!controller.signal.aborted) setLoading(false);
            });
        return () => controller.abort();
    }, [attempt]);
    const save = async () => {
        setSaving(true);
        setError('');
        try {
            await libraryRequest('/me/modrinth-watches', 'POST', {
                projectId: project?.projectId,
                packId,
                buildId,
                loader,
                targets: selected,
                future,
            });
            toast('Daily version alert created.');
            onSaved?.();
            onClose();
        } catch (e) {
            setError(e instanceof Error ? e.message : 'Could not create alert.');
        } finally {
            setSaving(false);
        }
    };
    return (
        <div
            className="modal-overlay"
            onClick={() => {
                if (!saving) onClose();
            }}
        >
            <ModalPanel
                onClose={onClose}
                dismissible={!saving}
                className="modal-panel max-w-xl"
                onClick={(e) => e.stopPropagation()}
            >
                <div className="modal-header">
                    <h2 className="modal-title">Minecraft version alerts</h2>
                    <p className="text-xs text-(--base-06) mt-2">{project?.title || title}</p>
                </div>
                <div className="modal-body space-y-4">
                    <p className="text-sm text-(--base-07)">
                        Check daily and notify once per Minecraft release. Specific targets finish after notification.
                        Future-release alerts stay active. Pack alerts use a snapshot of this build&apos;s linked mods.
                    </p>
                    <label className="input-label">
                        Target loader
                        <Select
                            value={loader}
                            onChange={setLoader}
                            options={['fabric', 'forge', 'neoforge', 'quilt'].map((value) => ({ value, label: value }))}
                        />
                    </label>
                    <input
                        className="input-field w-full"
                        value={query}
                        onChange={(e) => setQuery(e.target.value)}
                        placeholder="Find a Minecraft version..."
                        aria-label="Find alert versions"
                    />
                    <div className="max-h-48 overflow-auto grid grid-cols-3 gap-2">
                        {versions
                            .filter((v) => v.includes(query))
                            .slice(0, 90)
                            .map((v) => (
                                <label key={v} className="flex items-center gap-2 text-sm">
                                    <input
                                        type="checkbox"
                                        checked={selected.includes(v)}
                                        disabled={!selected.includes(v) && selected.length >= 32}
                                        onChange={() =>
                                            setSelected((prev) =>
                                                prev.includes(v) ? prev.filter((x) => x !== v) : [...prev, v],
                                            )
                                        }
                                    />
                                    {v}
                                </label>
                            ))}
                    </div>
                    {loading && <p role="status">Loading releases...</p>}
                    <label className="flex items-start gap-2 text-sm">
                        <input type="checkbox" checked={future} onChange={(e) => setFuture(e.target.checked)} />
                        Also notify for every new stable Minecraft release published from now on.
                    </label>
                    <p className="text-xs text-(--base-06)">
                        Notifications stay in the panel inbox until you delete them. Local mods, configs and gameplay
                        require manual review. This does not notify for individual mod build updates.
                    </p>
                    {error && (
                        <p role="alert" className="text-sm text-(--error-light)">
                            {error}
                            <button onClick={() => setAttempt((n) => n + 1)} className="btn btn-ghost btn-sm">
                                Retry metadata
                            </button>
                        </p>
                    )}
                </div>
                <div className="modal-footer">
                    <button className="btn btn-secondary" disabled={saving} onClick={onClose}>
                        Cancel
                    </button>
                    <button
                        className="btn btn-primary"
                        disabled={saving || loading || (!future && !selected.length)}
                        onClick={save}
                    >
                        {saving ? <Loader2 size={14} className="animate-spin" /> : <Bell size={14} />}Create alert
                    </button>
                </div>
            </ModalPanel>
        </div>
    );
}

export function ModrinthLibraryPanel({ packId, buildId }: { packId?: number; buildId?: number }) {
    const library = useContext(Context);
    const metadata = useProjectMetadata(library?.favorites.map((f) => f.projectId) || []);
    const [watches, setWatches] = useState<AvailabilityWatch[]>([]);
    const [error, setError] = useState('');
    const [watch, setWatch] = useState<Favorite | null>(null);
    const refresh = useCallback(async () => {
        try {
            const data = await libraryRequest('/me/modrinth-watches');
            setWatches(data.watches || []);
            setError('');
        } catch (e) {
            setError(e instanceof Error ? e.message : 'Could not load alerts.');
        }
    }, []);
    useEffect(() => {
        void refresh();
        const timer = setInterval(() => {
            if (!document.hidden) void refresh();
        }, 30000);
        return () => clearInterval(timer);
    }, [refresh]);
    return (
        <div className="space-y-5">
            <div className="flex justify-between items-center">
                <h2 className="text-lg font-medium">
                    {packId ? 'Build version alerts' : 'Favorites & version alerts'}
                </h2>
                <button
                    className="btn btn-secondary btn-sm"
                    onClick={() => {
                        void refresh();
                        void library?.refresh();
                    }}
                >
                    Refresh
                </button>
            </div>
            {!packId && (
                <section className="card p-4 space-y-3">
                    <h3 className="font-medium">Favorite mods</h3>
                    {library?.favorites.map((f) => (
                        <div key={f.projectId} className="flex items-center gap-2">
                            <ProjectIcon name={f.title} url={metadata.projects[f.projectId]?.icon_url} />
                            <a
                                className="text-sm flex-1 text-(--accent-light)"
                                href={`https://modrinth.com/project/${encodeURIComponent(f.projectId)}`}
                                target="_blank"
                                rel="noopener noreferrer"
                            >
                                {f.title}
                            </a>
                            <button className="btn btn-secondary btn-sm" onClick={() => setWatch(f)}>
                                <Bell size={13} />
                                Version alert
                            </button>
                            <ModrinthProjectActions projectId={f.projectId} title={f.title} />
                        </div>
                    ))}
                    {library?.favorites.length === 0 && (
                        <p className="text-sm text-(--base-06)">
                            Star projects in Modpacks or Server Content to save them here.
                        </p>
                    )}
                </section>
            )}
            <section className="space-y-3">
                <h3 className="font-medium">Active daily checks</h3>
                {watches
                    .filter((w) => !packId || (w.packId === packId && w.buildId === buildId))
                    .map((w) => (
                        <article className="card p-4 space-y-2" key={w.id}>
                            <div className="flex gap-3 items-start">
                                <div className="flex-1">
                                    <h4 className="text-sm font-medium">{w.title}</h4>
                                    <p className="text-xs text-(--base-06)">
                                        {w.loader} · {w.projects.length} linked mods · {w.unlinked} local mods to review
                                    </p>
                                </div>
                                <button
                                    className="btn btn-secondary btn-sm"
                                    aria-label={`Delete alert ${w.title}`}
                                    onClick={async () => {
                                        try {
                                            await libraryRequest(`/me/modrinth-watches/${w.id}`, 'DELETE');
                                            await refresh();
                                            toast('Version alert removed.');
                                        } catch (e) {
                                            toast(e instanceof Error ? e.message : 'Delete failed.', false);
                                        }
                                    }}
                                >
                                    <Trash2 size={13} />
                                </button>
                            </div>
                            <p className="text-xs">
                                {w.targets.join(', ')}
                                {w.future ? ` · Future releases after ${w.current}` : ''}
                            </p>
                            <p className="text-xs text-(--base-06)">
                                {w.lastChecked
                                    ? `Last checked ${new Date(w.lastChecked).toLocaleString()}`
                                    : 'Waiting for the first scheduled check.'}
                                {w.lastChecked &&
                                    w.nextCheck &&
                                    ` · Next check ${new Date(w.nextCheck).toLocaleString()}`}
                            </p>
                            {w.lastError && (
                                <p role="alert" className="text-xs text-(--error-light)">
                                    {w.lastError}
                                </p>
                            )}
                            {w.versions?.map((v) => (
                                <details key={v.minecraft} className="text-xs">
                                    <summary>
                                        MC {v.minecraft}: {v.available}/{w.projects.length} available ·{' '}
                                        {v.missing.length} missing
                                    </summary>
                                    <ul className="list-disc pl-5 mt-2">
                                        {v.missing.map((m, i) => (
                                            <li key={i}>{m}</li>
                                        ))}
                                    </ul>
                                </details>
                            ))}
                        </article>
                    ))}
                {!watches.some((w) => !packId || (w.packId === packId && w.buildId === buildId)) && (
                    <p className="text-sm text-(--base-06)">
                        No active alerts. Completed single-version checks are removed automatically.
                    </p>
                )}
                {error && (
                    <p role="alert" className="text-(--error-light)">
                        {error}
                    </p>
                )}
            </section>
            {watch && (
                <ModrinthWatchDialog project={watch} onClose={() => setWatch(null)} onSaved={() => void refresh()} />
            )}
        </div>
    );
}
