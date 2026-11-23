"use client";

import React, { useState, useEffect, useMemo, useRef, useCallback, lazy, Suspense } from 'react';

import { AlertTriangle, RotateCcw, Search, FileQuestion, Power } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import { SkeletonFormRow } from '@/components/Skeleton';
import { toast } from '@/components/ui/Toast';
import Select from '@/components/ui/Select';
import Switch from '@/components/ui/Switch';
import Segmented from '@/components/ui/Segmented';
import Callout from '@/components/server/Callout';
import { Input, InputGroup } from '@ui-sdk/react/input';
import { Textarea } from '@ui-sdk/react/textarea';
import { NumberInput, NumberInputControl, NumberInputGroup } from '@ui-sdk/react/number-input';
import { Slider, SliderControl, SliderIndicator, SliderThumb, SliderTrack } from '@ui-sdk/react/slider';
import { useUnsavedChanges } from '@/components/settings/UnsavedChanges';
import { SettingsCardSave } from '@/components/settings/SettingsCard';
import { getFileContent, saveFile } from '@/lib/api';
import {
    VANILLA_SCHEMA, GROUP_LABELS, groupedSchema,
    type PropertyDef, type PropertyGroup,
} from '@/lib/server-properties-schema';
import { parseProperties, serializeProperties, type PropertiesDoc } from '@/lib/properties-codec';
import { useRouteId } from '@/lib/routeParams';

const CodeMirrorEditor = lazy(() => import('@dylaris/ui-filebrowser').then(m => ({ default: m.CodeMirrorEditor })));

type Mode = 'simple' | 'advanced';

function inferValue(def: PropertyDef, raw: string | undefined): string | boolean | number {
    if (raw === undefined) return def.default;
    switch (def.type) {
        case 'toggle':
            return raw === 'true';
        case 'number':
        case 'slider':
            return Number.isFinite(Number(raw)) ? Number(raw) : def.default;
        default:
            return raw;
    }
}

function stringifyValue(def: PropertyDef, v: string | boolean | number): string {
    if (def.type === 'toggle') return v ? 'true' : 'false';
    if (def.type === 'number' || def.type === 'slider') return String(v);
    return String(v);
}

interface PropertyRowProps {
    def: PropertyDef;
    /** The value in the form: a pending edit if there is one, else the file. */
    raw: string | undefined;
    onChange: (key: string, newRaw: string) => void;
    /** Edited since the last save. Marks the row so it can be found again. */
    edited?: boolean;
}

function PropertyRow({ def, raw, onChange, edited }: PropertyRowProps) {
    const value = inferValue(def, raw);
    const commit = (next: string | boolean | number) => onChange(def.key, stringifyValue(def, next));

    return (
        <div className="srv-config-row" data-edited={edited ? 'true' : undefined}>
            <div className="min-w-0">
                <div className="flex flex-wrap items-center gap-2">
                    <label htmlFor={`prop-${def.key}`} className="text-sm font-medium text-(--base-09)">{def.label}</label>
                    <code className="srv-config-key">{def.key}</code>
                    {edited && <span className="srv-config-pill">Edited</span>}
                    {def.requiresRestart && <span className="srv-config-pill" data-tone="warning">Restart</span>}
                </div>
                {def.description && (
                    <p className="mt-1 text-xs leading-relaxed text-(--base-06)">{def.description}</p>
                )}
            </div>
            <div className="srv-config-control">
                {def.type === 'toggle' && (
                    <Switch checked={value as boolean} onChange={commit} ariaLabel={def.label} />
                )}
                {def.type === 'text' && (
                    <Input
                        id={`prop-${def.key}`}
                        type="text"
                        value={value as string}
                        onChange={e => commit(e.target.value)}
                        placeholder={def.placeholder}
                        className="input-field w-full"
                    />
                )}
                {def.type === 'textarea' && (
                    <Textarea
                        id={`prop-${def.key}`}
                        value={value as string}
                        maxLength={def.maxLength}
                        onChange={e => commit(e.target.value)}
                        className="input-field min-h-20 w-full font-mono text-xs leading-relaxed"
                    />
                )}
                {def.type === 'number' && (
                    <NumberInput value={value as number} onValueChange={v => commit(v ?? 0)} min={def.min} max={def.max} format={{ useGrouping: false }}>
                        <NumberInputGroup>
                            <NumberInputControl id={`prop-${def.key}`} aria-label={def.label} className="input-field srv-num w-32 text-right" />
                        </NumberInputGroup>
                    </NumberInput>
                )}
                {def.type === 'slider' && (
                    <>
                        <Slider
                            className="flex-1"
                            value={value as number}
                            min={def.min}
                            max={def.max}
                            step={def.step ?? 1}
                            onValueChange={v => commit(v as number)}
                        >
                            <SliderControl className="flex h-5 cursor-pointer items-center touch-none">
                                <SliderTrack className="relative h-1.5 w-full rounded-full bg-(--base-04)">
                                    <SliderIndicator className="rounded-full bg-(--accent)" />
                                    <SliderThumb
                                        id={`prop-${def.key}`}
                                        aria-label={def.label}
                                        className="size-4 rounded-full border-2 border-(--base-00) bg-(--accent-light) has-focus-visible:[box-shadow:var(--focus-ring)] data-dragging:scale-110"
                                    />
                                </SliderTrack>
                            </SliderControl>
                        </Slider>
                        <span className="srv-num w-10 text-right text-xs text-(--base-08)">{value as number}</span>
                    </>
                )}
                {def.type === 'select' && (
                    <Select
                        id={`prop-${def.key}`}
                        className="w-full"
                        value={value as string}
                        onChange={commit}
                        options={def.options}
                    />
                )}
            </div>
        </div>
    );
}

export default function ServerPropertiesView() {
    const paramId = useRouteId('servers');
    const { servers } = useAppData();
    const serverId = Number(paramId);
    const server = servers.find(s => s.id === serverId);

    const [mode, setMode] = useState<Mode>('simple');
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState<string | null>(null);
    // Separate state for "the file just doesn't exist yet" so the UI
    // can show a calm empty state with actionable guidance, instead of
    // a red error + Retry that just re-fires the same 404.
    const [notFound, setNotFound] = useState(false);
    const [doc, setDoc] = useState<PropertiesDoc | null>(null);
    // Edits made since the last save, keyed by property.
    //
    // Simple mode used to issue one PUT and one toast per KEYSTROKE - every
    // character typed into a MOTD was a write of the whole file and a toast
    // saying so, while the Advanced tab next to it had Discard and Save. The
    // file is rewritten from doc.rawLines with these applied, so comments,
    // ordering and unknown keys survive exactly as they did.
    const [pending, setPending] = useState<Record<string, string>>({});
    const [savingSimple, setSavingSimple] = useState(false);
    const [advancedText, setAdvancedText] = useState('');
    const [advancedDirty, setAdvancedDirty] = useState(false);
    const [savingAdvanced, setSavingAdvanced] = useState(false);
    const [restartPending, setRestartPending] = useState(false);
    const [search, setSearch] = useState('');

    // External-change banner state: the text we last loaded/saved so
    // the background poll can tell apart "nothing changed" from "the
    // server (or another panel session) just rewrote the file". When
    // non-null, the banner offers the user a chance to reload the
    // remote version or stick with their in-progress edits.
    const lastLoadedTextRef = useRef<string>('');
    const [externalChange, setExternalChange] = useState<string | null>(null);

    // Stable primitives derived from the server object. `server` itself
    // is replaced on every 5s useAppData poll (different reference, same
    // content), and depending on it directly made reload's identity
    // churn on every tick -- which fired the whole load pipeline every
    // 5 seconds and made the tab visibly flicker. The reload/save
    // callbacks now key off these primitives instead.
    const serverUuid = server?.uuid ?? '';
    const activeSubServer = server?.activeSubServer ?? '';

    // server.properties lives inside the active sub-server dir, not
    // at the server root. The Node's validatePath resolves request
    // paths relative to <storage>/<uuid>/, so without the sub-server
    // prefix we get "file not found" no matter how many times the
    // user clicks retry. If no active sub-server is selected we
    // surface that as a friendly empty state instead of a 404 storm.
    const filePath = activeSubServer ? `${activeSubServer}/server.properties` : '';
    const showToast = useCallback((msg: string, ok = true) => toast(msg, ok), []);

    const reload = useCallback(async () => {
        if (!serverUuid) return;
        setNotFound(false);
        setExternalChange(null);
        // No active sub-server -> no file to load. Treat it as a soft
        // empty state, not an error -- retrying won't help until the
        // user installs / switches to a sub-server.
        if (!filePath) {
            setLoading(false);
            setError(null);
            setNotFound(true);
            setDoc(null);
            return;
        }
        setLoading(true);
        setError(null);
        try {
            const res = await getFileContent(filePath, serverUuid);
            if (res?.success === false) {
                // File-not-found is the expected state for a brand-new
                // sub-server that hasn't run yet (MC generates
                // server.properties on first boot). Tell that apart
                // from real errors so the UI doesn't yell at the user.
                const msg = (res.message || '').toLowerCase();
                if (msg.includes('not found') || msg.includes('no such file') || msg.includes('does not exist') || msg.includes('enoent')) {
                    setNotFound(true);
                    setDoc(null);
                } else {
                    setError(res.message || 'Could not load server.properties.');
                    setDoc(null);
                }
            } else {
                const text: string = res.content ?? res.text ?? '';
                const parsed = parseProperties(text);
                setDoc(parsed);
                setAdvancedText(text);
                setAdvancedDirty(false);
                lastLoadedTextRef.current = text;
            }
        } catch {
            setError('Network error while loading server.properties.');
        }
        setLoading(false);
    }, [serverUuid, filePath]);

    useEffect(() => { reload(); }, [reload]);

    // Background change-detection. Polls the file every 30s and
    // surfaces a banner if the content on disk has drifted from what
    // we last loaded/saved. Doesn't auto-overwrite the user's view --
    // they choose between Reload (take theirs) and Keep mine (next
    // save will overwrite). Skipped when there's no file yet, when
    // we're in an error state, or when the user is mid-edit in
    // Advanced mode (a fresh remote write at the wrong moment would
    // be noise -- we still flag it, the user just sees an offer to
    // resolve).
    useEffect(() => {
        if (!serverUuid || !filePath || notFound || error) return;
        let cancelled = false;
        const check = async () => {
            try {
                const res = await getFileContent(filePath, serverUuid);
                if (cancelled) return;
                if (res?.success === false) return;
                const text: string = res.content ?? res.text ?? '';
                if (text !== lastLoadedTextRef.current) {
                    setExternalChange(text);
                }
            } catch { /* network blip; try again next tick */ }
        };
        const interval = setInterval(check, 30_000);
        return () => { cancelled = true; clearInterval(interval); };
    }, [serverUuid, filePath, notFound, error]);

    const acceptExternalChange = useCallback(() => {
        if (externalChange === null) return;
        const text = externalChange;
        setDoc(parseProperties(text));
        setAdvancedText(text);
        setAdvancedDirty(false);
        lastLoadedTextRef.current = text;
        setExternalChange(null);
        // "Reload = take theirs" has to include the edits that have not been
        // written yet. Without this the rows kept showing them, still marked as
        // edited, and the next save re-applied them on top of the file that had
        // just been reloaded to get rid of them.
        setPending({});
        showToast('Reloaded from disk');
    }, [externalChange, showToast]);

    const dismissExternalChange = useCallback(() => {
        // Stamp the baseline to whatever's currently on disk so the
        // banner doesn't immediately reappear -- the user said "keep
        // mine", they'll overwrite on next save.
        if (externalChange !== null) lastLoadedTextRef.current = externalChange;
        setExternalChange(null);
    }, [externalChange]);

    // Mirror the latest doc into a ref so two inline edits fired before a
    // re-render both merge onto the newest state instead of a stale closure
    // (which dropped the earlier edit).
    const docRef = useRef(doc);
    useEffect(() => { docRef.current = doc; }, [doc]);
    const pendingRef = useRef(pending);
    useEffect(() => { pendingRef.current = pending; }, [pending]);

    // An edit. Nothing leaves the browser: it lands in `pending` and the Save
    // in this page's header commits the lot in one write.
    const editProperty = useCallback((key: string, newRaw: string) => {
        setPending(prev => ({ ...prev, [key]: newRaw }));
    }, []);

    const simpleDirty = Object.keys(pending).length > 0;

    const saveSimple = useCallback(async (): Promise<boolean> => {
        const cur = docRef.current;
        if (!cur || !serverUuid) return false;
        const edits = pendingRef.current;
        if (Object.keys(edits).length === 0) return true;

        setSavingSimple(true);
        const fileText = serializeProperties(cur, edits);
        try {
            const res = await saveFile(filePath, fileText, serverUuid);
            if (res?.success === false) {
                showToast(res.message || 'Save failed.', false);
                setSavingSimple(false);
                return false;
            }
            // Re-parse so rawLines / lineIndex stay accurate for the next save.
            setDoc(parseProperties(fileText));
            setAdvancedText(fileText);
            // Bump the change-detection baseline so the background poll does
            // not see our own save as an external change.
            lastLoadedTextRef.current = fileText;
            if (Object.keys(edits).some(k => VANILLA_SCHEMA.find(d => d.key === k)?.requiresRestart)) {
                setRestartPending(true);
            }
            setPending({});
            showToast('Saved server.properties');
            setSavingSimple(false);
            return true;
        } catch {
            showToast('Network error', false);
        }
        setSavingSimple(false);
        return false;
    }, [serverUuid, filePath, showToast]);

    const discardSimple = useCallback(() => setPending({}), []);

    const saveAdvanced = useCallback(async (): Promise<boolean> => {
        if (!serverUuid) return false;
        setSavingAdvanced(true);
        try {
            const res = await saveFile(filePath, advancedText, serverUuid);
            if (res?.success === false) {
                showToast(res.message || 'Save failed.', false);
            } else {
                const parsed = parseProperties(advancedText);
                setDoc(parsed);
                setAdvancedDirty(false);
                lastLoadedTextRef.current = advancedText;
                setRestartPending(true);
                showToast('Saved server.properties');
                setSavingAdvanced(false);
                return true;
            }
        } catch {
            showToast('Network error', false);
        }
        setSavingAdvanced(false);
        return false;
    }, [serverUuid, filePath, advancedText, showToast]);

    const discardAdvanced = useCallback(() => {
        if (docRef.current) setAdvancedText(serializeProperties(docRef.current, {}));
        setAdvancedDirty(false);
    }, []);

    // One registration, for whichever mode is showing. They edit the same file
    // by two routes, so letting both be dirty at once would mean one save
    // overwriting the other with a stale copy.
    useUnsavedChanges({
        dirty: mode === 'advanced' ? advancedDirty : simpleDirty,
        saving: mode === 'advanced' ? savingAdvanced : savingSimple,
        save: mode === 'advanced' ? saveAdvanced : saveSimple,
        discard: mode === 'advanced' ? discardAdvanced : discardSimple,
    });

    const switchMode = (next: Mode) => {
        if (next === mode) return;
        if (next === 'simple' && advancedDirty) {
            showToast('Save or discard advanced changes first', false);
            return;
        }
        if (next === 'advanced' && simpleDirty) {
            showToast('Save or discard your changes first', false);
            return;
        }
        setMode(next);
    };

    const groups = useMemo(() => groupedSchema(VANILLA_SCHEMA), []);
    const groupKeys = useMemo(() => Object.keys(groups) as PropertyGroup[], [groups]);
    const filteredGroups = useMemo(() => {
        if (!search.trim()) return groups;
        const needle = search.toLowerCase();
        const out: Partial<Record<PropertyGroup, PropertyDef[]>> = {};
        for (const key of groupKeys) {
            const matches = groups[key].filter(d =>
                d.key.toLowerCase().includes(needle) ||
                d.label.toLowerCase().includes(needle) ||
                (d.description?.toLowerCase().includes(needle) ?? false),
            );
            if (matches.length) out[key] = matches;
        }
        return out as Record<PropertyGroup, PropertyDef[]>;
    }, [groups, groupKeys, search]);

    // Keys present in the file but not in the platform's schema.
    // Surfaces those as raw key + text input at the bottom of Simple
    // mode so the panel stays useful when MC adds new properties (or
    // a plugin/mod injects custom ones) before our schema catches up.
    // Without this they'd silently disappear from the UI and only be
    // visible in Advanced mode.
    const unknownEntries = useMemo(() => {
        if (!doc) return [] as Array<{ key: string; value: string }>;
        const known = new Set(VANILLA_SCHEMA.map(d => d.key));
        const needle = search.trim().toLowerCase();
        const entries: Array<{ key: string; value: string }> = [];
        for (const key of doc.order) {
            if (known.has(key)) continue;
            if (needle && !key.toLowerCase().includes(needle) && !(doc.values[key] ?? '').toLowerCase().includes(needle)) {
                continue;
            }
            entries.push({ key, value: pending[key] ?? doc.values[key] ?? '' });
        }
        return entries;
    }, [doc, search, pending]);

    // Sections carry scroll-mt so the sticky search bar does not cover the heading.
    const jumpTo = (id: string) => document.getElementById(id)?.scrollIntoView({ block: 'start' });

    const cmRef = useRef<HTMLDivElement>(null); // lazy-load placeholder

    if (!server) return null;

    const dirty = mode === 'advanced' ? advancedDirty : simpleDirty;
    const form = {
        dirty,
        saving: mode === 'advanced' ? savingAdvanced : savingSimple,
        save: mode === 'advanced' ? saveAdvanced : saveSimple,
        discard: mode === 'advanced' ? discardAdvanced : discardSimple,
    };
    const visibleGroups = Object.keys(filteredGroups) as PropertyGroup[];
    const pendingCount = Object.keys(pending).length;

    return (
        <div className="flex min-h-full flex-col gap-6">
            <div className="srv-config-head">
                <div className="min-w-0">
                    <h1 className="srv-config-title">Server properties</h1>
                    <p className="srv-config-desc">
                        Edits <code className="srv-num text-(--base-08)">{server.activeSubServer ? `${server.activeSubServer}/server.properties` : 'server.properties'}</code>
                    </p>
                </div>
                <Segmented<Mode>
                    ariaLabel="Editor mode"
                    value={mode}
                    onChange={switchMode}
                    options={[{ id: 'simple', label: 'Simple' }, { id: 'advanced', label: 'Advanced' }]}
                />
            </div>

            {/* The poll found the file on disk different from what we loaded:
                the server rewrote it, an SFTP edit, another panel tab.
                Reload takes theirs; Keep mine re-baselines so the banner stops
                nagging and the next save overwrites. */}
            {externalChange !== null && (
                <Callout
                    tone="info"
                    role="status"
                    icon={<AlertTriangle size={16} />}
                    title="File changed externally"
                    actions={
                        <>
                            <button type="button" onClick={acceptExternalChange} className="btn btn-primary btn-sm">Reload</button>
                            <button type="button" onClick={dismissExternalChange} className="btn btn-secondary btn-sm">Keep mine</button>
                        </>
                    }
                >
                    server.properties was modified on disk since you opened it.
                </Callout>
            )}
            {restartPending && (
                <Callout tone="warning" role="status" icon={<Power size={16} />} title="Restart pending">
                    Restart the server for these changes to take effect.
                </Callout>
            )}

            {loading && (
                <div className="srv-surface divide-y divide-(--base-03)" aria-busy="true" aria-label="Loading server.properties">
                    {Array.from({ length: 6 }, (_, i) => <SkeletonFormRow key={i} className="px-5 py-4" />)}
                </div>
            )}

            {error && (
                <Callout
                    tone="error"
                    role="alert"
                    icon={<AlertTriangle size={16} />}
                    title="Could not load server.properties"
                    actions={<button type="button" onClick={reload} className="btn btn-secondary btn-sm"><RotateCcw size={14} /> Retry</button>}
                >
                    {error}
                </Callout>
            )}

            {/* The file does not exist yet: Minecraft writes it on first boot,
                so for a fresh sub-server this is the normal state. We do not
                create it ourselves, stale defaults would stomp the JAR's. */}
            {!loading && notFound && !filePath && (
                <div className="srv-surface flex flex-col items-center gap-3 px-6 py-14 text-center">
                    <FileQuestion size={32} className="text-(--base-05)" />
                    <div>
                        <p className="text-sm font-medium text-(--base-09)">No active sub-server selected</p>
                        <p className="mt-1 text-xs text-(--base-06)">Install or switch to a sub-server in the Setup tab to edit its server.properties.</p>
                    </div>
                </div>
            )}
            {!loading && notFound && !!filePath && (
                <div className="srv-surface flex flex-col items-center gap-3 px-6 py-14 text-center">
                    <FileQuestion size={32} className="text-(--base-05)" />
                    <div>
                        <p className="text-sm font-medium text-(--base-09)">
                            <code className="srv-num text-(--base-08)">server.properties</code> doesn&apos;t exist yet
                        </p>
                        <p className="mx-auto mt-1 max-w-md text-xs text-(--base-06)">
                            Minecraft generates this file the first time the server boots.
                            Start the server once from the power buttons; the tab will pick it up automatically.
                        </p>
                    </div>
                    <div className="mt-1 flex items-center gap-3">
                        <span className="inline-flex items-center gap-1.5 text-xs text-(--base-06)">
                            <Power size={14} /> Start the server from the header
                        </span>
                        <button type="button" onClick={reload} className="btn btn-secondary btn-sm">
                            <RotateCcw size={14} /> Check again
                        </button>
                    </div>
                </div>
            )}

            {!loading && !error && doc && mode === 'simple' && (
                <div className="grid grid-cols-1 gap-6 lg:grid-cols-[13rem_minmax(0,1fr)] lg:gap-8">
                    <aside className="srv-config-side" aria-label="Find a property">
                        <InputGroup>
                            <Input
                                type="search"
                                value={search}
                                onChange={e => setSearch(e.target.value)}
                                placeholder="Search properties…"
                                aria-label="Search properties"
                                className="input-field w-full"
                            />
                        </InputGroup>
                        <nav aria-label="Property categories" className="flex gap-1 overflow-x-auto lg:flex-col lg:overflow-visible">
                            {visibleGroups.map(group => (
                                <button key={group} type="button" onClick={() => jumpTo(`props-${group}`)} className="srv-config-side-link w-auto shrink-0 whitespace-nowrap lg:w-full">
                                    <span>{GROUP_LABELS[group]}</span>
                                    <span className="srv-num hidden text-xs text-(--base-05) lg:inline">{filteredGroups[group].length}</span>
                                </button>
                            ))}
                            {unknownEntries.length > 0 && (
                                <button type="button" onClick={() => jumpTo('props-unknown')} className="srv-config-side-link w-auto shrink-0 whitespace-nowrap lg:w-full">
                                    <span>Custom</span>
                                    <span className="srv-num hidden text-xs text-(--base-05) lg:inline">{unknownEntries.length}</span>
                                </button>
                            )}
                        </nav>
                    </aside>

                    <div className="flex min-w-0 flex-col gap-6">
                        {visibleGroups.length === 0 && unknownEntries.length === 0 && (
                            <div className="srv-surface flex flex-col items-center gap-2 px-6 py-12 text-center">
                                <Search size={24} className="text-(--base-05)" />
                                <p className="text-sm text-(--base-07)">No properties match &ldquo;{search}&rdquo;.</p>
                            </div>
                        )}
                        {visibleGroups.map(group => (
                            <section key={group} id={`props-${group}`} aria-labelledby={`props-${group}-h`} className="scroll-mt-4">
                                <div className="mb-3 flex items-baseline justify-between px-1">
                                    <h2 id={`props-${group}-h`} className="srv-config-card-title">{GROUP_LABELS[group]}</h2>
                                    <span className="srv-num text-xs text-(--base-05)">{filteredGroups[group].length}</span>
                                </div>
                                <div className="srv-surface overflow-hidden">
                                    {filteredGroups[group].map(def => (
                                        <PropertyRow
                                            key={def.key}
                                            def={def}
                                            raw={pending[def.key] ?? doc.values[def.key]}
                                            edited={def.key in pending}
                                            onChange={editProperty}
                                        />
                                    ))}
                                </div>
                            </section>
                        ))}

                        {/* Keys in the file that VANILLA_SCHEMA does not describe
                            (new MC versions, plugins, hand edits). Listed so they
                            stay editable here instead of only in Advanced. */}
                        {unknownEntries.length > 0 && (
                            <section id="props-unknown" aria-labelledby="props-unknown-h" className="scroll-mt-4">
                                <div className="mb-3 px-1">
                                    <div className="flex items-baseline justify-between">
                                        <h2 id="props-unknown-h" className="srv-config-card-title">Custom properties</h2>
                                        <span className="srv-num text-xs text-(--base-05)">{unknownEntries.length}</span>
                                    </div>
                                    <p className="mt-1 text-xs text-(--base-06)">Keys without a schema entry. They save with everything else.</p>
                                </div>
                                <div className="srv-surface overflow-hidden">
                                    {unknownEntries.map(entry => (
                                        <div key={entry.key} className="srv-config-row" data-edited={entry.key in pending ? 'true' : undefined}>
                                            <label htmlFor={`prop-custom-${entry.key}`} className="min-w-0">
                                                <code className="srv-num break-all text-xs text-(--base-08)">{entry.key}</code>
                                            </label>
                                            <div className="srv-config-control">
                                                <Input
                                                    id={`prop-custom-${entry.key}`}
                                                    type="text"
                                                    value={entry.value}
                                                    onChange={e => editProperty(entry.key, e.target.value)}
                                                    className="input-field srv-num w-full text-xs"
                                                    placeholder="(empty)"
                                                />
                                            </div>
                                        </div>
                                    ))}
                                </div>
                            </section>
                        )}
                    </div>
                </div>
            )}

            {!loading && !error && doc && mode === 'advanced' && (
                <div className="flex min-h-[32rem] flex-1 flex-col gap-3">
                    <p className="text-xs text-(--base-06)">
                        Raw <code className="srv-num text-(--base-08)">server.properties</code>. Changes are not applied until you save.
                    </p>
                    <div ref={cmRef} className="srv-surface min-h-0 flex-1 overflow-hidden bg-(--base-01)">
                        <Suspense fallback={<div className="flex h-full items-center justify-center text-sm text-(--base-07)">Loading editor…</div>}>
                            <CodeMirrorEditor
                                value={advancedText}
                                onChange={next => { setAdvancedText(next); setAdvancedDirty(true); }}
                                filename={filePath}
                                className="h-full"
                            />
                        </Suspense>
                    </div>
                </div>
            )}

            {/* Appears only while there is something to write. The navigation
                guard (useUnsavedChanges above) is what protects the edits; this
                bar is the display. */}
            {dirty && !error && (
                <div className="srv-config-savebar" role="region" aria-label="Unsaved changes">
                    <span>
                        {mode === 'advanced'
                            ? 'Unsaved changes'
                            : `${pendingCount} unsaved ${pendingCount === 1 ? 'change' : 'changes'}`}
                    </span>
                    <div className="flex items-center gap-2">
                        <SettingsCardSave form={form} />
                    </div>
                </div>
            )}
        </div>
    );
}
