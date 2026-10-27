"use client";

import { useEffect, useState } from 'react';
import { Copy, Loader2, RefreshCw } from 'lucide-react';
import Select from '@/components/ui/Select';
import { createBuild, listContent, type PackBuild } from '@/lib/api/packs';
import { getPackMinecraftVersions, getPackLoaderVersions } from '@/lib/api/packMetadata';
import { PACK_LOADERS, preferredPackLoaderVersion, packBuildMetadataError, nextPackBuildLabel, type PackLoaderVersion } from '@/lib/serverLoaderMetadata';
import { compareVersionsDesc } from '@/views/setup/VersionPicker';
import { WorkspaceDetails } from '@/components/modpacks/PackWorkspace';
import styles from './workspace.module.css';

export default function CreateBuildForm({ packId, previous, builds, existingVersions, onClose, onCreated }: {
    packId: number;
    previous?: PackBuild;
    builds: PackBuild[];
    existingVersions: string[];
    onClose: () => void;
    onCreated: (build: PackBuild) => void;
}) {
    const [versionString, setVersionString] = useState(() => nextPackBuildLabel(existingVersions));
    const [minecraft, setMinecraft] = useState(previous?.minecraft || '');
    const [loader, setLoader] = useState(previous && PACK_LOADERS.some(v => v === previous.loader) ? previous.loader : 'fabric');
    const [loaderVersion, setLoaderVersion] = useState(previous?.loaderVersion || '');
    const [minecraftVersions, setMinecraftVersions] = useState<string[]>([]);
    const [loaderVersions, setLoaderVersions] = useState<PackLoaderVersion[]>([]);
    const [loadingMinecraft, setLoadingMinecraft] = useState(true);
    const [loadingLoader, setLoadingLoader] = useState(true);
    const [minecraftError, setMinecraftError] = useState('');
    const [loaderError, setLoaderError] = useState('');
    const [submitError, setSubmitError] = useState('');
    const [retry, setRetry] = useState(0);
    const [saving, setSaving] = useState(false);
    const [sourceId, setSourceId] = useState(previous ? String(previous.id) : '');
    const [sourceCount, setSourceCount] = useState<number | null>(null);
    const source = builds.find(build => String(build.id) === sourceId);
    const copyCompatible = !source || (source.minecraft === minecraft && source.loader === loader);
    useEffect(() => { let live = true; setSourceCount(null); if (sourceId) listContent(packId, Number(sourceId)).then(entries => { if (live) setSourceCount(entries.length); }); return () => { live = false; }; }, [packId, sourceId]);

    useEffect(() => {
        const controller = new AbortController();
        setLoadingMinecraft(true);
        setMinecraftError('');
        getPackMinecraftVersions(controller.signal).then(versions => {
            if (controller.signal.aborted) return;
            const sorted = versions.sort(compareVersionsDesc);
            setMinecraftVersions(sorted);
            setMinecraft(current => sorted.includes(current) ? current : sorted[0] || '');
            if (!sorted.length) setMinecraftError('No Minecraft releases are available.');
        }).catch(error => {
            if (!controller.signal.aborted) setMinecraftError(error instanceof Error ? error.message : 'Could not load Minecraft versions.');
        }).finally(() => { if (!controller.signal.aborted) setLoadingMinecraft(false); });
        return () => controller.abort();
    }, [retry]);

    useEffect(() => {
        const controller = new AbortController();
        setLoaderVersions([]);
        setLoaderError('');
        if (!minecraft || loader === 'vanilla') {
            setLoadingLoader(false);
            setLoaderVersion('');
            return () => controller.abort();
        }
        setLoadingLoader(true);
        getPackLoaderVersions(loader, minecraft, controller.signal).then(versions => {
            if (controller.signal.aborted) return;
            setLoaderVersions(versions);
            setLoaderVersion(current => preferredPackLoaderVersion(versions, current));
        }).catch(error => {
            if (!controller.signal.aborted) setLoaderError(error instanceof Error ? error.message : 'Could not load loader versions.');
        }).finally(() => { if (!controller.signal.aborted) setLoadingLoader(false); });
        return () => controller.abort();
    }, [minecraft, loader, retry]);

    const metadataError = packBuildMetadataError({ minecraft, loader, loaderVersion }, minecraftVersions, loaderVersions);
    const busy = loadingMinecraft || loadingLoader;
    const submit = async () => {
        if (!copyCompatible || saving || busy || minecraftError || loaderError || metadataError || !versionString.trim()) return;
        setSaving(true);
        setSubmitError('');
        const result = await createBuild(packId, {
            versionString: versionString.trim(), minecraft, loader,
            loaderVersion: loader === 'vanilla' ? undefined : loaderVersion,
            sourceBuildId: sourceId ? Number(sourceId) : undefined,
        });
        setSaving(false);
        if (result.success && result.build) onCreated(result.build);
        else setSubmitError(result.message || 'Could not create this build.');
    };

    return (
        <div className="max-w-3xl w-full mx-auto">
            <h2 className="text-lg font-display font-semibold text-(--base-09)">Create a build</h2>
            <div className="space-y-5 mt-5">
                <div>
                    <label className="input-label" htmlFor="pack-build-source">Start with</label>
                    <Select id="pack-build-source" value={sourceId} disabled={saving} onChange={value => {
                        setSourceId(value);
                        const selected = builds.find(build => String(build.id) === value);
                        if (selected) { setMinecraft(selected.minecraft); setLoader(selected.loader); setLoaderVersion(selected.loaderVersion); }
                    }} options={[{ value: '', label: 'Empty build' }, ...builds.map(build => ({ value: String(build.id), label: `Copy ${build.versionString}` }))]} />
                    <p className="text-xs text-(--base-06) mt-2">Copy a build to keep its exact mod versions and independent config files, or start empty.</p>
                </div>
                <div className="grid grid-cols-1 md:grid-cols-2 gap-5">
                    <p className="text-xs text-(--base-06)">Choose a Minecraft release and loader. Compatible loader builds are fetched automatically; the recommended stable build is selected when available.</p>
                    <div>
                        <label htmlFor="pack-build-version" className="input-label">Pack version (editable)</label>
                        <input id="pack-build-version" value={versionString} onChange={e => setVersionString(e.target.value)} disabled={saving} placeholder="0.1.0" className="input-field input-mono w-full" />
                    </div>
                    <div>
                        <label htmlFor="pack-build-minecraft" className="input-label">Minecraft</label>
                        <Select id="pack-build-minecraft" value={minecraft} onChange={value => { if (value === minecraft) return; setMinecraft(value); setLoaderVersion(''); }} options={minecraftVersions.map(value => ({ value, label: value }))} searchable disabled={saving || loadingMinecraft} placeholder={loadingMinecraft ? 'Loading Minecraft versions...' : 'Select Minecraft version'} />
                    </div>
                    <div>
                        <label htmlFor="pack-build-loader" className="input-label">Loader</label>
                        <Select id="pack-build-loader" value={loader} onChange={value => { if (value === loader) return; setLoader(value); setLoaderVersion(''); }} options={PACK_LOADERS.map(value => ({ value, label: value === 'vanilla' ? 'Vanilla (no mod loader)' : value === 'neoforge' ? 'NeoForge' : value[0].toUpperCase() + value.slice(1) }))} disabled={saving} />
                    </div>
                    {loader !== 'vanilla' && (
                        <div>
                            <label htmlFor="pack-build-loader-version" className="input-label">Loader version</label>
                            <Select id="pack-build-loader-version" value={loaderVersion} onChange={setLoaderVersion} options={loaderVersions.map(v => ({ value: v.version, label: `${v.version}${v.recommended ? ' (recommended)' : v.stable ? '' : ' (pre-release)'}` }))} searchable disabled={saving || busy || !minecraft} placeholder={loadingLoader ? 'Loading compatible loader versions...' : 'Select loader version'} />
                            {!busy && !loaderError && minecraft && loaderVersions.length === 0 && <p className="text-xs text-(--warning-light) mt-2">No compatible {loader} builds for Minecraft {minecraft}. Choose another Minecraft version or loader.</p>}
                        </div>
                    )}
                    {!copyCompatible && <p role="alert" className="text-xs text-(--warning-light) md:col-span-2">Copying requires the same Minecraft version and loader. Select an empty build, or use the source build’s Minecraft migration tool.</p>}
                    {busy && <p role="status" className="text-xs text-(--base-06) flex items-center gap-2"><Loader2 size={13} className="animate-spin" /> Loading version metadata...</p>}
                    {(minecraftError || loaderError) && <div role="alert" className="text-xs text-(--error-light) space-y-2"><p>{minecraftError || loaderError}</p><button onClick={() => setRetry(v => v + 1)} className="btn btn-secondary btn-sm"><RefreshCw size={12} /> Retry</button></div>}
                    {submitError && <p role="alert" className="text-xs text-(--error-light)">{submitError}</p>}
                </div>
                {source && <p className="text-xs text-(--base-06) bg-(--base-02) p-3 rounded-md flex gap-2"><Copy size={14} className="shrink-0" /> {sourceCount === null ? 'Loading source content...' : `Copying ${sourceCount} content entries from ${source.versionString}. Changes to this draft do not modify the source.`}</p>}
                <div className="flex justify-end gap-2 pt-5 border-t border-(--base-03)">
                    <button onClick={onClose} disabled={saving} className="btn btn-secondary">Cancel</button>
                    <button onClick={submit} disabled={!copyCompatible || saving || busy || !!minecraftError || !!loaderError || !!metadataError || !versionString.trim()} className="btn btn-primary disabled:opacity-40">{saving ? 'Creating...' : 'Create build'}</button>
                </div>
            </div>
            <WorkspaceDetails>
                <p className="mono-label text-(--base-06) mb-4">Build preview</p>
                <h2 className="text-xl font-mono text-(--base-09) mb-3 break-all">{versionString || 'New draft'}</h2>
                <dl className={styles.metadata}><div><dt>Minecraft</dt><dd>{minecraft || 'Select version'}</dd></div><div><dt>Loader</dt><dd>{loader}</dd></div><div><dt>Loader build</dt><dd>{loaderVersion || 'None'}</dd></div><div><dt>Source</dt><dd>{source?.versionString || 'Empty build'}</dd></div><div><dt>Content entries</dt><dd>{sourceId ? sourceCount ?? 'Loading...' : 0}</dd></div></dl>
            </WorkspaceDetails>
        </div>
    );
}
