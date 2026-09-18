"use client";

// Settings tab for the modpack-authoring subsystem. Houses:
//   - a read-only mirror of the platform-wide feature flag (owned by Features)
//   - the .mrpack storage provider (local mirror list, or a saved S3 connection)
//   - the public address a node downloads a built pack from
// S3 credentials are NOT edited here: they live in Settings -> Storage
// Connections and this screen only references one by id.
//
// It was five separate cards over ONE payload, which read as five independent
// things and gave no clue which of them the single Save button was about to
// write. Tabs would have been worse: a tab hides fields that the one save still
// writes. Groups inside one card keep everything the button touches on screen.

import { useEffect, useState } from 'react';
import { Package, Plus, X, AlertTriangle } from 'lucide-react';
import { useSettingsForm } from '@/lib/useSettingsForm';
import SettingsPage from '@/components/settings/SettingsPage';
import SettingsCard, { SettingsGroup, SettingsRow } from '@/components/settings/SettingsCard';
import ModCacheCard from '@/components/settings/ModCacheCard';
import { SwitchRow } from '@/components/ui/Switch';
import {
    getModpackSettings, setModpackSettings,
    type ModpackSettings,
} from '@/lib/api/modpackSettings';
import { listStorageConnections, type StorageConnection } from '@/lib/api';

const PROVIDER_OPTIONS = [
    { value: 'local' as const, label: 'Local paths', desc: 'Archives on disk, mirrored across the paths below' },
    { value: 's3' as const, label: 'S3 connection', desc: 'A bucket from Settings, Storage connections' },
];

const DEFAULTS: ModpackSettings = {
    featureEnabled: true,
    // Nothing preselected. "Local paths" looked like a working default while
    // the path list was empty, which is not a backend at all - the first upload
    // answered 424 and the screen had said nothing.
    provider: '',
    paths: [],
    s3Endpoint: '',
    s3Bucket: '',
    s3Region: '',
    s3AccessKey: '',
    s3SecretKey: '',
    updateCheckIntervalHours: 24,
    shareLinksEnabled: false,
    connectionId: 0,
    corePublicUrl: '',
};

export default function ModpacksTab() {
    const [connections, setConnections] = useState<StorageConnection[]>([]);

    const form = useSettingsForm<ModpackSettings>({
        load: async () => {
            const res = await getModpackSettings();
            if (!res.success || !res.settings) return null;
            const stored = res.settings;
            // A stored "local" with no paths is what an install that never
            // configured this looks like. Show it as unchosen so the operator
            // makes the decision rather than inheriting it.
            const unconfigured = stored.provider === 'local' && (stored.paths?.length ?? 0) === 0;
            // The secret is never returned; keeping the field blank is what makes
            // "unchanged" mean "nobody typed a new one".
            return { ...stored, provider: unconfigured ? '' : stored.provider, s3SecretKey: '' };
        },
        save: async value => {
            const res = await setModpackSettings(value);
            // Re-snapshot with the secret blanked: it was just persisted server
            // side, and leaving it in the snapshot keeps the form dirty forever.
            return { ok: res.success, message: res.message, value: { ...value, s3SecretKey: '' } };
        },
        successMessage: 'Modpack settings saved.',
    });

    useEffect(() => {
        listStorageConnections().then(res => {
            if (res.success && res.connections) setConnections(res.connections);
        });
    }, []);

    const settings = form.value ?? DEFAULTS;
    const patch = form.patch;
    const usingConnection = settings.provider === 's3' && (settings.connectionId ?? 0) > 0;
    const detectedUrl = (settings.detectedCorePublicUrl ?? '').trim();

    return (
        <SettingsPage
            title="Modpacks"
            icon={Package}
            description="Where modpack archives are stored, and the address a node downloads a built pack from. The on/off switches live under Settings, Features."
            loading={form.loading}
        >
            {/* Read-only mirror of the platform flag, NOT a second switch. This
                screen used to carry its own toggle writing the same
                feature_modpacks_enabled key as Settings -> Features, so the
                platform had one switch behind two controls. Features owns the
                flags; this shows the resulting state so the config has context. */}
            <div className="flex items-start justify-between gap-4 rounded-md border border-(--base-04) bg-(--base-01) px-4 py-3">
                <div className="min-w-0">
                    <div className="text-sm font-medium text-(--base-09)">Subsystem state</div>
                    <div className="text-xs text-(--base-06) mt-0.5">
                        {settings.featureEnabled
                            ? 'Modpacks are enabled. Whether users may author, rather than admins only, is the "Open authoring to users" switch under Settings, Features.'
                            : 'Modpacks are disabled: write endpoints return 503 and the Modpacks nav entry is hidden. Existing modpacks stay readable and downloadable. Enable it under Settings, Features.'}
                    </div>
                </div>
                <span className={`badge shrink-0 ${settings.featureEnabled ? 'badge-accent' : 'badge-neutral'}`}>
                    {settings.featureEnabled ? 'Enabled' : 'Disabled'}
                </span>
            </div>

            <SettingsCard
                title="Modpack storage"
                description="Everything below is written by one save."
                form={form}
                saveBlockedReason={settings.provider ? undefined : 'Pick where modpack archives are stored first'}
            >
                <SettingsGroup title="Behaviour" first>
                    <SwitchRow
                        label="Share links"
                        description="When on, pack authors can mint tokenized download links for a build (client .mrpack or server pack). Off hides the create UI and blocks minting new links; existing links keep serving while modpack authoring is on."
                        checked={settings.shareLinksEnabled}
                        onChange={v => patch({ shareLinksEnabled: v })}
                    />
                    <SettingsRow
                        label="Auto-update check interval"
                        htmlFor="modpack-update-interval"
                        description="How often the background worker re-checks Modrinth-linked mods for a newer version. Counted per mod since it was last checked."
                    >
                        <input
                            id="modpack-update-interval"
                            type="number"
                            min={1}
                            value={settings.updateCheckIntervalHours}
                            onChange={e => patch({ updateCheckIntervalHours: Math.max(1, parseInt(e.target.value || '24', 10)) })}
                            className="input-mono w-24 text-right"
                        />
                        <span className="text-xs text-(--base-06)">hours</span>
                    </SettingsRow>
                </SettingsGroup>

                <SettingsGroup title="Storage provider">
                    {!settings.provider && (
                        <p className="alert alert-warning text-xs">
                            <AlertTriangle size={14} className="shrink-0 mt-0.5" />
                            <span>
                                Not configured. Modpacks cannot be created until archives have
                                somewhere to go: pick one below.
                            </span>
                        </p>
                    )}
                    <div className="grid grid-cols-1 sm:grid-cols-2 gap-2">
                        {PROVIDER_OPTIONS.map(opt => {
                            const active = settings.provider === opt.value;
                            return (
                                <button
                                    key={opt.value}
                                    type="button"
                                    aria-pressed={active}
                                    onClick={() => patch({ provider: opt.value })}
                                    className={`p-3 rounded-md border text-left transition-colors ${
                                        active
                                            ? 'border-(--accent) bg-(--accent)/10'
                                            : 'border-(--base-03) bg-(--base-02) hover:border-(--base-05)'
                                    }`}
                                >
                                    <div className={`text-sm font-medium ${active ? 'text-(--accent-light)' : 'text-(--base-09)'}`}>{opt.label}</div>
                                    <div className="text-xs text-(--base-06) mt-0.5">{opt.desc}</div>
                                </button>
                            );
                        })}
                    </div>

                    {/* Local - mirrored paths */}
                    {settings.provider === 'local' && (
                        <div className="pt-1">
                            <div className="mono-label mb-1">Paths</div>
                            <p className="text-xs text-(--base-06) mb-2">
                                Absolute paths. The provider writes to ALL paths (mirror) and reads from the first hit.
                            </p>
                            <div className="space-y-2">
                                {settings.paths.length === 0 && (
                                    <div className="text-xs text-(--warning)">
                                        No paths configured. Publishing or downloading a non-draft .mrpack
                                        will fail until at least one path is set.
                                    </div>
                                )}
                                {settings.paths.map((p, i) => (
                                    <div key={i} className="flex gap-2">
                                        <input
                                            type="text"
                                            value={p}
                                            aria-label={`Path ${i + 1}`}
                                            onChange={e => form.update(prev => {
                                                const next = [...prev.paths];
                                                next[i] = e.target.value;
                                                return { ...prev, paths: next };
                                            })}
                                            placeholder="/var/lib/dylaris/modpacks"
                                            className="input-mono flex-1"
                                        />
                                        <button
                                            type="button"
                                            onClick={() => form.update(prev => ({
                                                ...prev,
                                                paths: prev.paths.filter((_, j) => j !== i),
                                            }))}
                                            className="btn btn-secondary btn-sm px-2"
                                            aria-label={`Remove path ${i + 1}`}
                                            title="Remove path"
                                        >
                                            <X size={13} />
                                        </button>
                                    </div>
                                ))}
                                <button
                                    type="button"
                                    onClick={() => form.update(prev => ({ ...prev, paths: [...prev.paths, ''] }))}
                                    className="btn btn-secondary btn-sm inline-flex items-center gap-1"
                                >
                                    <Plus size={12} />
                                    Add path
                                </button>
                            </div>
                        </div>
                    )}

                    {/* S3 - a saved connection, never inline credentials. One
                        place defines an S3 target: Settings, Storage connections.
                        This screen used to offer endpoint / bucket / region / key
                        / secret inline as well, which meant the same bucket got
                        typed in several tabs, each with its own copy of the
                        credentials and its own chance to be missed on a rotation. */}
                    {settings.provider === 's3' && (
                        <div className="space-y-3 pt-1">
                            <div className="flex flex-col gap-[5px]">
                                <label className="mono-label" htmlFor="modpack-connection">Storage connection</label>
                                <select
                                    id="modpack-connection"
                                    value={settings.connectionId ?? 0}
                                    onChange={e => patch({ connectionId: Number(e.target.value) })}
                                    className="input-field w-full"
                                >
                                    <option value={0}>Select a connection</option>
                                    {connections.map(c => <option key={c.id} value={c.id}>{c.name}</option>)}
                                </select>
                                <p className="text-xs text-(--base-06)">
                                    Buckets and credentials are defined once under Settings, Storage
                                    connections and referenced here, so a rotation happens in one place.
                                </p>
                            </div>

                            {usingConnection ? (
                                <div className="alert alert-info text-xs">
                                    Credentials come from the selected connection. Manage or test it on the
                                    Storage connections page.
                                </div>
                            ) : (
                                <div className="alert alert-warning text-xs">
                                    <AlertTriangle size={14} className="shrink-0 mt-0.5" />
                                    <span>
                                        {connections.length === 0
                                            ? 'No storage connections exist yet. Create one under Settings, Storage connections, then pick it here: modpack storage cannot use S3 until you do.'
                                            : 'Pick a connection above. S3 storage is not configured until you do.'}
                                    </span>
                                </div>
                            )}
                        </div>
                    )}
                </SettingsGroup>

                <SettingsGroup
                    title="Pack download address"
                    description="A node installing a pack built here downloads it from Core, so Core needs to know its own public address."
                >
                    <div className="space-y-1.5">
                        <Field
                            id="modpack-core-url"
                            label="Core public URL"
                            value={settings.corePublicUrl}
                            onChange={v => patch({ corePublicUrl: v })}
                            placeholder="https://api.example.com"
                        />
                        {/* Core reports the origin this very request came in on.
                            Offered, never applied on its own: an admin reaching
                            Core over localhost or a service name would otherwise
                            silently persist an address no node can resolve. */}
                        {detectedUrl && detectedUrl !== settings.corePublicUrl.trim() && (
                            <button
                                type="button"
                                onClick={() => patch({ corePublicUrl: detectedUrl })}
                                className="btn btn-secondary btn-sm"
                            >
                                Use the address you are on: <span className="font-mono ml-1">{detectedUrl}</span>
                            </button>
                        )}
                    </div>
                    {settings.corePublicUrl.trim() === '' && (
                        <div className="text-xs text-(--warning-light)">
                            Not set: a pack built here cannot be installed on a server, because the node downloads it from this address.
                        </div>
                    )}
                    <p className="text-xs text-(--base-06)">
                        Core serves built packs itself at <span className="font-mono">{'{url}'}/mirror/</span>,
                        so this is the origin a node reaches Core on, not an internal address.
                    </p>
                </SettingsGroup>
            </SettingsCard>

            {/* Its own card because it is its own payload: this writes where the
                metadata cache lives, which has nothing to do with where modpack
                archives are stored. One card, one save. */}
            <ModCacheCard />
        </SettingsPage>
    );
}

function Field({
    id,
    label,
    value,
    onChange,
    placeholder,
}: {
    id: string;
    label: string;
    value: string;
    onChange: (v: string) => void;
    placeholder?: string;
}) {
    return (
        <div className="flex flex-col gap-[5px]">
            <label className="mono-label" htmlFor={id}>{label}</label>
            <input
                id={id}
                type="text"
                value={value}
                onChange={e => onChange(e.target.value)}
                placeholder={placeholder}
                className="input-mono w-full"
            />
        </div>
    );
}
