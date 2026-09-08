"use client";

import { useState, useEffect, useRef } from 'react';
import { getFeatureSettings, saveFeatureSettings, FeatureSettings } from '@/lib/api';
import { getSystemFeaturesAdmin, updateSystemFeatures, FeatureFlagsAdminPayload } from '@/lib/api/featureFlags';
import { getTabProxySettings, setTabProxySettings, type TabProxySettings } from '@/lib/api/tabProxySettings';
import { getCoreStorage } from '@/lib/api/coreStorage';
import { canSaveCoreStorage } from '@/lib/coreStorage';
import { Network, Globe, AlertTriangle, Server, ChevronDown, ChevronRight, KeyRound, FolderOpen } from 'lucide-react';
import GuardedTabs from '@/components/settings/GuardedTabs';
import type { TabItem } from '@/components/ui/Tabs';
import { useTabParam } from '@/lib/useTabParam';
import { getCatalog, type CatalogScope } from '@/lib/api/authzCatalog';
import { SkeletonHeader, SkeletonCard } from '@/components/Skeleton';
import { useUnsavedChanges } from '@/components/settings/UnsavedChanges';
import SettingsPage from '@/components/settings/SettingsPage';
import SettingsCard, { SettingsGroup } from '@/components/settings/SettingsCard';
import MetricsDatabaseCard from '@/components/settings/MetricsDatabaseCard';
import { SwitchRow } from '@/components/ui/Switch';
import Checkbox from '@/components/ui/Checkbox';
import { useAppData } from '@/lib/AppDataContext';
import { toast } from '@/components/ui/Toast';
import HelpTip from '@/components/ui/HelpTip';

type FeatureTab = 'subsystems' | 'infrastructure' | 'apikeys' | 'tabs';

const FEATURE_TABS: readonly FeatureTab[] = ['subsystems', 'infrastructure', 'apikeys', 'tabs'];

export default function FeaturesTab() {
    const [tab, setTab] = useTabParam<FeatureTab>(FEATURE_TABS, 'subsystems');
    // Auto-move is gateway-only — gate its toggle on the live routing mode.
    // ip_port means the gateway is off, so enabling auto-move would 409 on the
    // backend; we disable the control instead of letting that happen.
    const { routingMode, coreInfo } = useAppData();
    const gatewayOff = routingMode === 'ip_port';

    // Seeded with the SERVER's default, not an optimistic one: Core reads proxy
    // as `val != "false"` (default on). This is only ever visible if the load
    // below fails - the normal path renders a skeleton until the fetch
    // resolves - but a settings screen that shows "on" for a subsystem that is
    // off is worse than one that errs the other way. Same reasoning as
    // storageConfigured's `null` below.
    const [settings, setSettings] = useState<FeatureSettings>({ proxyEnabled: true });
    const [loading, setLoading] = useState(true);
    const [saving, setSaving] = useState(false);

    // Platform-wide subsystem toggles (tickets, modpacks, ...), behind
    // /api/admin/settings/features.
    //
    // These used to persist on every click, and say nothing when they worked.
    // Three save models sat in one scroll on this page - these seven flags
    // autosaving silently, the two tab-proxy switches autosaving on click and
    // its numbers on blur, and everything else waiting for a save bar - on
    // controls that look identical to each other. All three are dirty states
    // now, and each is committed by the card it lives in.
    const [platformFlags, setPlatformFlags] = useState<FeatureFlagsAdminPayload>({ tickets: false, modpacks: true, modpackAuthoring: false, library: false, autoMove: false, byon: false, userApiKeys: false, userApiKeyAllowedCaps: '' });
    const [platformSaving, setPlatformSaving] = useState(false);
    const platformSnapshot = useRef<FeatureFlagsAdminPayload | null>(null);

    // What the NEXT flip of "open authoring to users" should do to per-user
    // overrides an admin set by hand. Component state on purpose, not a stored
    // setting: it describes one action, so persisting it would silently widen a
    // later toggle nobody connected to this checkbox.
    const [applyToManual, setApplyToManual] = useState(false);
    // Rows the last authoring flip actually rewrote, straight from the server, so
    // the admin sees what the switch did instead of inferring it.
    const [lastBulk, setLastBulk] = useState<number | null>(null);

    // Tickets requires Core file storage (attachments + backups need a durable
    // off-host home) - the backend 409s ("core_storage_required") on enable
    // otherwise. Fetched once so the toggle can be disabled with a hint
    // instead of letting the admin hit a failed save. Starts as `null`
    // (unknown) rather than defaulting to "configured", so there is no window
    // where the toggle is optimistically enabled before the fetch resolves;
    // `storageConfigured !== true` treats both "unknown" and "confirmed not
    // configured" as disabled. If the fetch itself fails, storageConfigured
    // is set to `true` (usable) rather than left stuck at "unknown" forever -
    // the backend still correctly 409s on enable if storage really isn't
    // configured, so failing open here just avoids a dead toggle.
    const [storageConfigured, setStorageConfigured] = useState<boolean | null>(null);
    // Enabled ticket categories. null while unknown, and unknown is NOT treated
    // as a problem - unlike storage this only warns, so a failed count must not
    // invent a warning that is not there.
    const [ticketCategories, setTicketCategories] = useState<number | null>(null);

    // WS5 custom-tab reverse proxy toggles - same save-on-click/blur pattern
    // as the platform flags above, but its own admin settings endpoint.
    const [tabProxy, setTabProxy] = useState<TabProxySettings>({ enabled: false, allowPublicLinks: false, audience: 'all', maxPerUserPerServer: 3, maxPerUserTotal: 10, maxShareLinksPerUser: 20 });
    const [tabProxySaving, setTabProxySaving] = useState(false);
    const tabProxySnapshot = useRef<TabProxySettings | null>(null);

    // The capability whitelist for user API keys, rendered from the authz
    // catalog rather than a hardcoded list, so a capability added to the
    // catalog shows up here without a second edit. PANEL caps are left out
    // because a key can never carry one (authz.ValidKeyCap rejects them).
    const [keyCatalog, setKeyCatalog] = useState<CatalogScope[]>([]);
    const [keyCapsOpen, setKeyCapsOpen] = useState(false);

    // Snapshot of last-saved settings for dirty detection.
    const snapshotRef = useRef<FeatureSettings | null>(null);

    const showToast = (msg: string, ok = true) => toast(msg, ok);

    useEffect(() => {
        getFeatureSettings().then(res => {
            if (res.success && res.settings) {
                setSettings(res.settings);
                snapshotRef.current = res.settings;
            } else {
                // Say so instead of rendering the seed values as if they were
                // fetched. snapshotRef stays null on this path, which keeps
                // `dirty` false and the card's Save inert - so the admin cannot
                // write these unconfirmed values back over the real ones - but
                // without a message the screen just quietly lies about what is
                // enabled.
                showToast(res.message || 'Failed to load feature settings - shown values are unconfirmed', false);
            }
            setLoading(false);
        });
        getSystemFeaturesAdmin().then(res => {
            if (typeof res.enabledTicketCategories === 'number') {
                setTicketCategories(res.enabledTicketCategories);
            }
            if (res.success && res.features) {
                setPlatformFlags(res.features);
                // Seeding this is what makes the card savable at all: `dirty`
                // is measured against it, so a snapshot left null keeps the
                // save bar disarmed and savePlatform's own `if (!prev)` returns
                // early. Nothing errors - the switches just never persist.
                platformSnapshot.current = res.features;
            } else {
                // Same reasoning as the loader above: leaving the card inert is
                // right (these values are unconfirmed and must not be written
                // back), but in silence it reads as a broken page.
                showToast(res.message || 'Failed to load feature switches - shown values are unconfirmed', false);
            }
        });
        getCoreStorage().then(res => {
            if (!res.success) {
                // Fetch/parse failed - fail open so the toggle isn't stuck
                // disabled indefinitely; the backend still enforces the 409.
                setStorageConfigured(true);
                return;
            }
            setStorageConfigured(!!res.settings && canSaveCoreStorage(res.settings));
        });
        getTabProxySettings().then(res => {
            if (res.success && res.settings) {
                setTabProxy(res.settings);
                tabProxySnapshot.current = res.settings;
            }
        });
        getCatalog().then(res => {
            if (res.success && res.catalog) {
                setKeyCatalog(res.catalog.filter(sc => sc.scope !== 'panel'));
            }
        });
    }, []);

    // An edit to a platform flag. Local; the card's Save commits it.
    const editPlatformFlag = (key: keyof FeatureFlagsAdminPayload, value: boolean | string) => {
        setPlatformFlags(prev => ({ ...prev, [key]: value }));
        setLastBulk(null);
    };

    // Each tab saves only the flags it shows.
    //
    // The page used to be one card with eleven switches behind a single Save,
    // and splitting it into tabs is only safe if a tab can commit its own half.
    // Sending the whole object from a tab that renders three of its fields would
    // write the other eight from whatever this browser happened to be holding -
    // so the payload is built from the tab's own keys and Core leaves the rest
    // alone.
    const subsetPayload = (keys: readonly (keyof FeatureFlagsAdminPayload)[]): FeatureFlagsAdminPayload => {
        const out: FeatureFlagsAdminPayload = {};
        for (const k of keys) {
            // The index write is the point: this copies a known-safe subset of
            // one typed object into another of the same type.
            (out as unknown as Record<string, unknown>)[k] = platformFlags[k];
        }
        return out;
    };

    const subsetDirty = (keys: readonly (keyof FeatureFlagsAdminPayload)[]) =>
        platformSnapshot.current !== null &&
        keys.some(k => platformFlags[k] !== platformSnapshot.current![k]);

    const savePlatformSubset = async (keys: readonly (keyof FeatureFlagsAdminPayload)[]): Promise<boolean> => {
        const prev = platformSnapshot.current;
        if (!prev) return false;
        // applyAuthoringToManual rides along only when the toggle it describes
        // actually moved; sending it otherwise would be a standing instruction
        // the admin never gave.
        const payload: FeatureFlagsAdminPayload =
            keys.includes('modpackAuthoring') && platformFlags.modpackAuthoring !== prev.modpackAuthoring
                ? { ...subsetPayload(keys), applyAuthoringToManual: applyToManual }
                : subsetPayload(keys);
        setPlatformSaving(true);
        try {
            const res = await updateSystemFeatures(payload);
            if (!res.success) {
                showToast(res.message || 'Save failed.', false);
                return false;
            }
            const stored = res.features ?? platformFlags;
            setPlatformFlags(stored);
            platformSnapshot.current = stored;
            if (typeof res.usersChanged === 'number') setLastBulk(res.usersChanged);
            showToast('Feature switches saved.');
            return true;
        } finally {
            setPlatformSaving(false);
        }
    };

    const discardPlatform = () => {
        if (platformSnapshot.current) setPlatformFlags(platformSnapshot.current);
        setLastBulk(null);
    };

    // The three groups the one payload is now cut into. Listed here rather than
    // inline so a new flag has exactly one place to be added and cannot end up
    // in no tab at all - which would make it unsavable while looking fine.
    const SUBSYSTEM_KEYS = ['tickets', 'modpacks', 'modpackAuthoring', 'library'] as const;
    const INFRA_KEYS = ['autoMove', 'byon'] as const;
    const API_KEY_KEYS = ['userApiKeys', 'userApiKeyAllowedCaps'] as const;

    // One registration covering all three: the unsaved-changes guard asks "is
    // anything unsaved on this screen", and a tab switch inside GuardedTabs is
    // what protects the individual halves.
    const platformDirty = subsetDirty([...SUBSYSTEM_KEYS, ...INFRA_KEYS, ...API_KEY_KEYS]);
    const saveAllPlatform = () => savePlatformSubset([...SUBSYSTEM_KEYS, ...INFRA_KEYS, ...API_KEY_KEYS]);
    useUnsavedChanges({ dirty: platformDirty, save: saveAllPlatform, discard: discardPlatform, saving: platformSaving });

    const tabProxyDirty =
        tabProxySnapshot.current !== null &&
        JSON.stringify(tabProxy) !== JSON.stringify(tabProxySnapshot.current);

    const saveTabProxy = async (): Promise<boolean> => {
        setTabProxySaving(true);
        try {
            const res = await setTabProxySettings(tabProxy);
            if (!res.success) {
                showToast(res.message || 'Save failed.', false);
                return false;
            }
            const stored = res.settings ?? tabProxy;
            setTabProxy(stored);
            tabProxySnapshot.current = stored;
            showToast('Tab proxy settings saved.');
            return true;
        } finally {
            setTabProxySaving(false);
        }
    };

    const discardTabProxy = () => {
        if (tabProxySnapshot.current) setTabProxy(tabProxySnapshot.current);
    };

    useUnsavedChanges({ dirty: tabProxyDirty, save: saveTabProxy, discard: discardTabProxy, saving: tabProxySaving });

    const handleSave = async (): Promise<boolean> => {
        setSaving(true);
        try {
            const res = await saveFeatureSettings(settings);
            if (res.success) {
                showToast('Feature settings saved.');
                snapshotRef.current = settings;
                return true;
            }
            showToast(res.message || 'Save failed.', false);
            return false;
        } finally {
            setSaving(false);
        }
    };

    const handleDiscard = () => {
        if (snapshotRef.current) setSettings(snapshotRef.current);
    };

    const dirty =
        snapshotRef.current !== null &&
        JSON.stringify(settings) !== JSON.stringify(snapshotRef.current);

    useUnsavedChanges({ dirty, save: handleSave, discard: handleDiscard, saving });

    // The whitelist travels as a comma-separated string because that is what the
    // setting stores; the picker works on a Set and writes it back the same way.
    const allowedKeyCaps = new Set(
        (platformFlags.userApiKeyAllowedCaps ?? '').split(',').map(c => c.trim()).filter(Boolean),
    );
    const toggleKeyCap = (id: string) => {
        const next = new Set(allowedKeyCaps);
        if (next.has(id)) next.delete(id); else next.add(id);
        editPlatformFlag('userApiKeyAllowedCaps', Array.from(next).join(','));
    };

    if (loading) return (
        <div className="max-w-2xl space-y-6">
            <SkeletonHeader />
            <SkeletonCard height="h-20" />
            <SkeletonCard height="h-20" />
        </div>
    );

    // A card per save, and now a tab per subject. The page used to be four cards
    // down one 900-line scroll, three of which shared a single payload - so
    // "which of these does this Save button write" had no answer on screen.
    const proxyForm = { dirty, saving, save: handleSave, discard: handleDiscard };
    const proxyTabForm = { dirty: tabProxyDirty, saving: tabProxySaving, save: saveTabProxy, discard: discardTabProxy };
    const platformSubsetForm = (keys: readonly (keyof FeatureFlagsAdminPayload)[]) => ({
        dirty: subsetDirty(keys),
        saving: platformSaving,
        save: () => savePlatformSubset(keys),
        discard: discardPlatform,
    });

    const TABS: TabItem<FeatureTab>[] = [
        { id: 'subsystems', label: 'Subsystems', icon: Server },
        { id: 'infrastructure', label: 'Infrastructure', icon: Network },
        { id: 'apikeys', label: 'User API keys', icon: KeyRound },
        { id: 'tabs', label: 'Custom tabs', icon: Globe },
    ];

    return (
        <div className="flex flex-col h-full min-h-0">
            <GuardedTabs items={TABS} active={tab} onChange={setTab} ariaLabel="Feature groups" />

            <div className="flex-1 overflow-y-auto pt-5">
                <div className="space-y-6 max-w-3xl">
                    {tab === 'subsystems' && (
                        <SettingsCard title="Subsystems" icon={Server} form={platformSubsetForm(SUBSYSTEM_KEYS)}>
                <SettingsGroup first>
                    <SwitchRow
                        label="Ticket system"
                        description={<>Enables the tickets module, inbox, attachments, canned responses and notifications. When off, all <code className="font-mono">/api/tickets/*</code> endpoints return 503 and the Tickets nav entry is hidden.</>}
                        checked={!!platformFlags.tickets}
                        disabled={!platformFlags.tickets && storageConfigured !== true}
                        onChange={v => editPlatformFlag('tickets', v)}
                    >
                        {!platformFlags.tickets && storageConfigured !== true && (
                            <p className="flex items-start gap-1.5 text-xs text-(--warning-light) mt-2">
                                <AlertTriangle size={12} className="mt-0.5 shrink-0" />
                                <span>Requires Core file storage. Configure and save it under Settings, Core storage first.</span>
                            </p>
                        )}
                        {/* A ticket must name an enabled category, so with none
                            defined the module accepts nothing: every customer
                            attempt fails with "Unknown or disabled category"
                            while this screen reports the feature as on. Shown
                            whether the switch is on or off, because it is just
                            as wrong afterwards - deleting the last category
                            breaks it again. */}
                        {ticketCategories === 0 && (
                            <p className="flex items-start gap-1.5 text-xs text-(--warning-light) mt-2">
                                <AlertTriangle size={12} className="mt-0.5 shrink-0" />
                                <span>
                                    No enabled ticket categories exist. Every ticket a customer
                                    submits will be rejected until you create one under Settings,
                                    Tickets.
                                </span>
                            </p>
                        )}
                    </SwitchRow>
                </SettingsGroup>
                {/* Two switches, one subsystem. "Modpacks" turns it on for ADMINS;
                    "Open authoring to users" widens it to everyone else. Nested
                    rather than side by side because the second is meaningless without
                    the first, and the backend folds it to false when Modpacks goes
                    off - so the nesting is the real relationship, not decoration. */}
                <SettingsGroup title="Modpacks">
                    <SwitchRow
                        label="Modpacks"
                        description={<>Turns on the modpack builder, storage and Solder endpoints for <strong>admins</strong>. When off, modpack write endpoints return 503 and the Modpacks nav entry is hidden. Existing modpacks stay readable and downloadable.</>}
                        checked={!!platformFlags.modpacks}
                        onChange={v => editPlatformFlag('modpacks', v)}
                    />
                    <div className={`pl-4 border-l-2 border-(--base-03) space-y-3 ${platformFlags.modpacks ? '' : 'opacity-60'}`}>
                        <SwitchRow
                            label="Open authoring to users"
                            description="Lets non-admin users create and publish their own modpacks. Off means admins only. You can still revoke a single user afterwards under Settings, Users."
                            checked={!!platformFlags.modpackAuthoring}
                            disabled={!platformFlags.modpacks}
                            onChange={v => editPlatformFlag('modpackAuthoring', v)}
                        />
                        {/* Checked = the next flip of the switch above also rewrites
                            users an admin set by hand. Unchecked (the default) keeps
                            those decisions. Deliberately NOT a stored setting: it
                            describes what one change should do, so remembering it
                            across sessions would silently widen a later toggle. */}
                        <Checkbox
                            checked={applyToManual}
                            disabled={!platformFlags.modpacks}
                            onChange={setApplyToManual}
                            label="Also apply to users I set by hand"
                            hint="Off keeps every per-user override; on resets them to follow this switch from now on."
                        />
                        {lastBulk !== null && (
                            <p className="text-xs font-mono text-(--base-06)">
                                {lastBulk === 0
                                    ? 'No user rows needed changing.'
                                    : `${lastBulk} user${lastBulk === 1 ? '' : 's'} updated.`}
                            </p>
                        )}
                    </div>
                </SettingsGroup>
                <SettingsGroup title="File library">
                    <SwitchRow
                        label="File library"
                        description={<>A shared catalog of server jars and archives you upload once, offered as an install source in the server wizard. When off, the library page and all <code className="font-mono">/api/library/*</code> endpoints return 503.</>}
                        checked={!!platformFlags.library}
                        onChange={v => editPlatformFlag('library', v)}
                    />
                    <p className="text-xs text-(--base-06)">
                        Who sees it - admins only, or everyone - stays in Settings, Modules.
                    </p>
                </SettingsGroup>
                        </SettingsCard>
                    )}

                    {tab === 'infrastructure' && (
                        <>
            <SettingsCard title="Proxy and network support" icon={Network} form={proxyForm}>
                <SwitchRow
                    label="Proxy support"
                    description="BungeeCord, Velocity and Waterfall proxy containers, and server linking."
                    checked={settings.proxyEnabled}
                    onChange={v => setSettings(prev => ({ ...prev, proxyEnabled: v }))}
                />
            </SettingsCard>
                            {/* No Gateway toggle here: the gateway is on exactly when
                                game traffic routes through it, so the routing-mode
                                selector in the Gateway sub-tab is its only switch. */}
                            <SettingsCard title="Gateway-dependent features" icon={Server} form={platformSubsetForm(INFRA_KEYS)}>
                {/* Auto-move and BYON are gateway-only. Both toggles are hard
                    disabled while routing is on IP:Port, since enabling either
                    then would 409 on the backend. */}

                <SettingsGroup title="Gateway-dependent">
                    <SwitchRow
                        label="Auto-move"
                        description="Lets the rebalance worker migrate opted-in servers to a less-loaded node when their current node is overloaded. Per-server opt-in lives in each server's resource settings. The route keeps the player address stable across the node change."
                        checked={!!platformFlags.autoMove}
                        disabled={gatewayOff}
                        onChange={v => editPlatformFlag('autoMove', v)}
                    />
                    {/* An external node FORCES gateway routing locally
                        (NODE_EXTERNAL), so with no gateway a tenant node has
                        nothing to join: the flag would switch on an enrolment
                        surface for machines that can never connect. The panel
                        therefore ANDs this flag with the live routing mode. */}
                    <SwitchRow
                        label="BYON (bring your own node)"
                        description="Turns on tenant node enrollment, traffic metering and the Usage and Billing admin settings, which stay hidden while it is off."
                        checked={!!platformFlags.byon}
                        disabled={gatewayOff}
                        onChange={v => editPlatformFlag('byon', v)}
                    />
                    {gatewayOff && (
                        <p className="flex items-start gap-1.5 text-xs text-(--warning-light)">
                            <AlertTriangle size={12} className="mt-0.5 shrink-0" />
                            <span>
                                Both require gateway routing. Switch game traffic to Gateway or Both
                                first: a tenant node forces gateway routing on its own side, so it has
                                nothing to join while game traffic is on IP:Port.
                            </span>
                        </p>
                    )}
                </SettingsGroup>
                            </SettingsCard>
                            <MetricsDatabaseCard />
                        </>
                    )}

                    {tab === 'apikeys' && (
                        <SettingsCard title="User API keys" icon={KeyRound} form={platformSubsetForm(API_KEY_KEYS)}>
                {/* Two controls, because "may users hold keys" and "which
                    capabilities may they put on one" are different decisions: an
                    operator can open the feature without opening the whole
                    capability catalogue. Both are enforced at MINT and at USE - a
                    key created before the switch was turned off stops working,
                    which is what an operator who turned it off means. */}
                <SettingsGroup title="User API keys">
                    <SwitchRow
                        label="User API keys"
                        description={<>Lets non-admins mint scoped keys for the <code className="font-mono">/api/external</code> automation surface. Admins can always mint their own.</>}
                        checked={!!platformFlags.userApiKeys}
                        onChange={v => editPlatformFlag('userApiKeys', v)}
                    />

                {platformFlags.userApiKeys && (
                    <div className="pt-3 border-t border-(--base-03)">
                        <button
                            type="button"
                            onClick={() => setKeyCapsOpen(o => !o)}
                            aria-expanded={keyCapsOpen}
                            className="flex items-center gap-2 w-full text-left group"
                        >
                            {keyCapsOpen
                                ? <ChevronDown size={14} className="text-(--base-06)" />
                                : <ChevronRight size={14} className="text-(--base-06)" />}
                            <span className="text-sm font-medium text-(--base-09) group-hover:text-(--accent-light) transition-colors">Allowed capabilities</span>
                            <HelpTip label="About allowed capabilities">
                                <p className="mb-2">
                                    <strong>Selecting none is not &quot;none allowed&quot;.</strong> An empty
                                    list means no EXTRA restriction: a user may put any capability on
                                    a key that they already hold themselves.
                                </p>
                                <p className="mb-2">
                                    Selecting some narrows that further - a key may then carry only
                                    what is ticked here, and still only what its creator holds. It can
                                    never exceed the person who made it either way.
                                </p>
                                <p>
                                    Enforced when a key is minted AND every time one is used, so
                                    tightening this stops keys that already exist.
                                </p>
                            </HelpTip>
                            <span className="ml-auto text-xs text-(--base-06)">
                                {allowedKeyCaps.size === 0 ? 'No restriction' : `${allowedKeyCaps.size} selected`}
                            </span>
                        </button>
                        <p className="text-xs text-(--base-06) mt-2 ml-6">
                            {allowedKeyCaps.size === 0
                                ? 'Users may put any capability on a key that they already hold themselves. Select some to narrow that further.'
                                : 'Users may only put these on a key, and still only ones they already hold themselves.'}
                        </p>

                        {keyCapsOpen && (
                            <div className="mt-3 ml-6 space-y-4">
                                {allowedKeyCaps.size > 0 && (
                                    <button
                                        type="button"
                                        
                                        onClick={() => editPlatformFlag('userApiKeyAllowedCaps', '')}
                                        className="btn btn-ghost btn-sm"
                                    >
                                        Clear restriction
                                    </button>
                                )}
                                {keyCatalog.map(sc => (
                                    <div key={sc.scope} className="space-y-3">
                                        <div className="text-xs font-semibold uppercase tracking-wide text-(--base-06)">
                                            {sc.scope === 'server' ? 'Per server' : 'Account-wide'}
                                        </div>
                                        {sc.categories.map(cat => (
                                            <div key={cat.category}>
                                                <div className="text-xs text-(--base-07) mb-1">{cat.category}</div>
                                                <div className="flex flex-wrap gap-x-4 gap-y-1.5">
                                                    {cat.capabilities.map(c => (
                                                        <label key={c.id} className="flex items-center gap-2 text-xs text-(--base-08) cursor-pointer">
                                                            <input
                                                                type="checkbox"
                            className="checkbox"
                                                                checked={allowedKeyCaps.has(c.id)}
                                                                
                                                                onChange={() => toggleKeyCap(c.id)}
                                                            />
                                                            <span>{c.label}</span>
                                                            <code className="font-mono text-(--base-06)">{c.id}</code>
                                                        </label>
                                                    ))}
                                                </div>
                                            </div>
                                        ))}
                                    </div>
                                ))}
                            </div>
                        )}
                    </div>
                )}
                </SettingsGroup>
                        </SettingsCard>
                    )}

                    {tab === 'tabs' && (
            <SettingsCard title="Custom-tab reverse proxy" icon={Globe} form={proxyTabForm}>
                <SettingsGroup first>
                    <SwitchRow
                        label="Custom-tab reverse proxy"
                        description="Streams a server container's web UI (BlueMap, squaremap, Dynmap) through Dylaris so no public port is needed. When off, proxied tabs and share links stop serving."
                        checked={tabProxy.enabled}
                        onChange={v => setTabProxy(t => ({ ...t, enabled: v }))}
                    />
                    <SwitchRow
                        label="Allow public share links"
                        description="Let owners publish anonymous, no-login share links."
                        checked={tabProxy.allowPublicLinks}
                        onChange={v => setTabProxy(t => ({ ...t, allowPublicLinks: v }))}
                    />
                </SettingsGroup>
                <SettingsGroup>
                {/* Same-origin security note (WS5 C1 follow-up, closed by spec B5):
                    when origin isolation is active, proxied content is served from
                    a dedicated, different-origin proxy host, so a compromised/
                    malicious container can no longer read the viewer's panel
                    session. When it is NOT active (older Core, or disabled),
                    proxied content still runs on the panel's own origin and the
                    original warning applies. */}
                {coreInfo?.tabProxyAvailable ? (
                    <p className="flex items-start gap-1.5 text-xs text-(--success-light)">
                        <AlertTriangle size={12} className="mt-0.5 shrink-0" />
                        <span>
                            Each proxied tab is served on its own host, so a container&apos;s
                            scripts run on an origin that is not the panel&apos;s and cannot read a
                            viewer&apos;s session. Public share links are safe to enable here.
                        </span>
                    </p>
                ) : (
                    <p className="flex items-start gap-1.5 text-xs text-(--warning-light)">
                        <AlertTriangle size={12} className="mt-0.5 shrink-0" />
                        <span>
                            Proxied tabs are unavailable: Core has no
                            <code className="mx-1">TAB_PROXY_HOST_SUFFIX</code>
                            set, so there is no host to serve a container on. Point a wildcard
                            record and certificate at Core and set it, then proxied tabs and share
                            links start working. Direct tabs are unaffected.
                        </span>
                    </p>
                )}
                </SettingsGroup>
                <SettingsGroup title="Who sees the Custom Tabs page">
                    <div className="flex bg-(--base-03) p-0.5 rounded-md w-fit">
                        {(['all', 'admin'] as const).map(a => (
                            <button
                                key={a}
                                type="button"
                                onClick={() => setTabProxy({ ...tabProxy, audience: a })}
                                className={`px-3 py-1 text-xs rounded-sm transition-colors ${
                                    tabProxy.audience === a
                                        ? 'bg-(--accent) text-white'
                                        : 'text-(--base-07) hover:text-(--base-09)'
                                }`}
                            >
                                {a === 'all' ? 'Everyone' : 'Admins only'}
                            </button>
                        ))}
                    </div>
                    <p className="text-xs text-(--base-06)">
                        Controls the Custom Tabs entry in the navigation bar. It is set here
                        rather than under Settings &rarr; Modules, because that row follows this
                        value - editing it there would be undone the next time this card is saved.
                        Individual tabs are still governed by who may see the server they belong to.
                    </p>
                </SettingsGroup>
                <SettingsGroup title="Per-user limits">
                    <div className="grid grid-cols-3 gap-3">
                        <div className="flex flex-col gap-[5px]">
                            <label className="input-label" htmlFor="tabproxy-max-per-server">Proxied tabs per server</label>
                            <input
                                id="tabproxy-max-per-server"
                                type="number" min={1} value={tabProxy.maxPerUserPerServer}
                                onChange={e => setTabProxy({ ...tabProxy, maxPerUserPerServer: Number(e.target.value) })}
                                className="input-field w-full"
                            />
                        </div>
                        <div className="flex flex-col gap-[5px]">
                            <label className="input-label" htmlFor="tabproxy-max-total">Proxied tabs in total</label>
                            <input
                                id="tabproxy-max-total"
                                type="number" min={1} value={tabProxy.maxPerUserTotal}
                                onChange={e => setTabProxy({ ...tabProxy, maxPerUserTotal: Number(e.target.value) })}
                                className="input-field w-full"
                            />
                        </div>
                        <div className="flex flex-col gap-[5px]">
                            <label className="input-label" htmlFor="tabproxy-max-links">Share links per user</label>
                            <input
                                id="tabproxy-max-links"
                                type="number" min={1} value={tabProxy.maxShareLinksPerUser}
                                onChange={e => setTabProxy({ ...tabProxy, maxShareLinksPerUser: Number(e.target.value) })}
                                className="input-field w-full"
                            />
                        </div>
                    </div>
                    <p className="text-xs text-(--base-06)">
                        Both counts are per user. The total is the real ceiling - without it,
                        someone with twenty servers holds twenty times the per-server figure.
                        A total below the per-server value is clamped to it, so the smaller
                        number always wins.
                    </p>
                </SettingsGroup>
            </SettingsCard>
                    )}
                </div>
            </div>
        </div>
    );
}
