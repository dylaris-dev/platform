"use client";

import React, { useState, useEffect, useRef, useCallback } from 'react';
import {
    getGatewaySettings, saveGatewaySettings, GatewaySettings, HosterDomain, HosterValidation,
    getRoutingMode, saveRoutingMode, getRoutingMigrationStatus,
    bulkDeleteRoutesBySuffix,
    RoutingMode, FileAccessMode,
} from '@/lib/api';
import { RefreshCw, Save, CircleCheck, CircleAlert, Router, AlertTriangle, EyeOff, Globe, Plus, Trash2, X, Copy, Check, Search, Network } from 'lucide-react';
import { SkeletonHeader, SkeletonCard, SkeletonTable } from '@/components/Skeleton';
import Spinner from '@/components/Spinner';
import { LimitField, LimitHelp } from '@/components/settings/LimitField';
import { useUnsavedChanges } from '@/components/settings/UnsavedChanges';
import SettingsLoadError from '@/components/settings/SettingsLoadError';
import { settingsLoadState } from '@/lib/settingsLoadState';
import { toast } from '@/components/ui/Toast';
import { checkDns, DnsCheckResult, DnsRecord, DnsRecordCategory, DnsRecordStatus } from '@/lib/api/dns';
import GatewayDnsCard from '@/components/settings/GatewayDnsCard';
import SettingsCard, { SettingsGroup } from '@/components/settings/SettingsCard';
import { useAppData } from '@/lib/AppDataContext';
import { useBusy } from '@/lib/useBusy';
import { cnameTargetsFor } from '@/lib/cnameTargets';
import HelpTip from '@/components/ui/HelpTip';
import SettingsPage from '@/components/settings/SettingsPage';

// ─────────────────────────────────────────────
// Gateway settings
// ─────────────────────────────────────────────

type LimitKey = 'global' | 'userDefault';
type ModeOption<T extends string> = { value: T; label: string; desc: string };

const ROUTING_OPTIONS: ModeOption<RoutingMode>[] = [
    { value: 'ip_port', label: 'IP : Port', desc: 'Direct host port binding — players connect via Node IP + port' },
    { value: 'both', label: 'Both', desc: 'Allow both IP:Port and Gateway routes simultaneously' },
    { value: 'gateway', label: 'Gateway', desc: 'Route-only mode — no host ports exposed, traffic via Gate → Link' },
];

const FILE_OPTIONS: ModeOption<FileAccessMode>[] = [
    { value: 'sftp', label: 'SFTP', desc: 'Users access server files via SFTP on the Node IP' },
    { value: 'both', label: 'Both', desc: 'Allow both SFTP and Beam file access' },
    { value: 'beam', label: 'Beam', desc: 'File access only via Beam relay — no direct Node IP needed' },
];

// ─────────────────────────────────────────────
// DNS & Domains check card
// ─────────────────────────────────────────────

// Plain-language, one-line explanation per record category. Shown under the
// record name so a non-expert operator knows what each row is for.
const DNS_CATEGORY_BLURB: Record<DnsRecordCategory, string> = {
    player: 'Player base domain — the address customers connect to.',
    wildcard: 'Wildcard so every server subdomain resolves automatically — you never touch DNS per customer.',
    cname: 'Custom-domain target — where customers point a CNAME for their own domain.',
    panel: 'Panel domain — where this admin/web interface is served.',
    api: 'API domain — what the panel in the browser calls for every request. Normally the panel domain itself, since Core serves both.',
    beam: 'Beam relay — the hostname the desktop client dials for REMOTE access. LAN and direct connections do not use it.',
};

const DNS_CATEGORY_LABEL: Record<DnsRecordCategory, string> = {
    player: 'Player base',
    wildcard: 'Wildcard',
    cname: 'Custom CNAME',
    panel: 'Panel',
    api: 'API',
    beam: 'Beam relay',
};

// Map the check verdict onto the shared .badge-* utilities + a label.
const DNS_STATUS_BADGE: Record<DnsRecordStatus, { cls: string; label: string }> = {
    ok: { cls: 'badge-success', label: 'OK' },
    mismatch: { cls: 'badge-warning', label: 'Mismatch' },
    missing: { cls: 'badge-error', label: 'Missing' },
    unresolved: { cls: 'badge-error', label: 'Unresolved' },
    info: { cls: 'badge-neutral', label: 'Optional' },
};

// One copyable record value (the expected DNS target). Shows a transient
// check mark on copy, matching the api-keys / Warp copy pattern.
function CopyValue({ value }: { value: string }) {
    const [copied, setCopied] = useState(false);
    const copy = () => {
        navigator.clipboard.writeText(value).then(() => {
            setCopied(true);
            setTimeout(() => setCopied(false), 1500);
        }).catch(() => { /* clipboard blocked — silent, value is still visible */ });
    };
    return (
        <div className="flex items-center gap-1.5">
            <code className="font-mono text-xs text-(--base-09) bg-(--base-03) px-1.5 py-0.5 rounded break-all">{value}</code>
            <button
                type="button"
                onClick={copy}
                className="text-(--base-06) hover:text-(--accent-light) transition-colors shrink-0"
                title="Copy value"
                aria-label={`Copy ${value}`}
            >
                {copied ? <Check size={12} className="text-(--success-light)" /> : <Copy size={12} />}
            </button>
        </div>
    );
}

function DnsCheckCard() {
    // The records table is populated by the check itself (the backend computes
    // the required records from the operator's config). null = before first
    // check, [] only happens once a check returns with zero records.
    const [result, setResult] = useState<DnsCheckResult | null>(null);
    const [checking, setChecking] = useState(false);
    const [error, setError] = useState<string | null>(null);

    const runCheck = async () => {
        setChecking(true);
        setError(null);
        const res = await checkDns();
        if (res.success) {
            setResult(res);
        } else {
            setError(res.message || 'DNS check failed. Try again.');
        }
        setChecking(false);
    };

    const checked = result && !checking;

    return (
        <div className="card p-5 space-y-5">
            <div className="flex items-start justify-between gap-4">
                <div className="flex items-center gap-3">
                    <div className="w-9 h-9 rounded-md bg-(--base-03) flex items-center justify-center">
                        <Network size={18} className="text-(--accent-light)" />
                    </div>
                    <div>
                        <div className="font-medium text-sm text-(--base-09)">DNS &amp; Domains</div>
                        <div className="text-xs text-(--base-06)">The records to create at your DNS provider — then verify they resolve and the ingress is reachable</div>
                    </div>
                </div>
                <button
                    type="button"
                    onClick={runCheck}
                    disabled={checking}
                    className="btn btn-primary btn-sm shrink-0 disabled:opacity-40"
                >
                    {checking ? <><Spinner size="xs" /> Checking…</> : <><Search size={13} /> Check DNS</>}
                </button>
            </div>

            {/* Error state */}
            {error && (
                <div className="alert alert-error text-xs">
                    <AlertTriangle size={14} className="shrink-0 mt-0.5" />
                    <span>{error}</span>
                </div>
            )}

            {/* Checking state — skeleton (lookups can take a few seconds) */}
            {checking && !result && (
                <div className="space-y-3">
                    <SkeletonTable rows={4} cols={4} />
                </div>
            )}

            {/* Empty state — before the first check */}
            {!checking && !result && !error && (
                <div className="flex flex-col items-center gap-2 px-4 py-8 rounded-md bg-(--base-02) border border-(--base-03) text-center">
                    <Network size={22} className="text-(--base-05)" />
                    <p className="text-sm text-(--base-08)">Run a check to see your required DNS records</p>
                    <p className="text-xs text-(--base-06) max-w-md">
                        The records are computed from your configured hoster domains, custom-domain CNAME target and panel URL. Each is resolved against a public resolver and the ingress is dialled for reachability.
                    </p>
                </div>
            )}

            {/* Results — records table + reachability */}
            {result && (
                <div className={`space-y-5 transition-opacity ${checking ? 'opacity-50' : 'opacity-100'}`}>
                    {result.records.length === 0 ? (
                        <div className="flex items-start gap-2 p-3 rounded-md bg-(--base-02) border border-(--base-03) text-xs text-(--base-07)">
                            <AlertTriangle size={14} className="text-(--warning-light) mt-0.5 shrink-0" />
                            <span>No DNS records to verify. Configure at least one hoster domain above (and the panel URL via <span className="font-mono">FRONTEND_URL</span>) to populate this table.</span>
                        </div>
                    ) : (
                        <div>
                            <h3 className="mono-label mb-3">Required Records</h3>
                            <div className="border border-(--base-03) rounded-md overflow-hidden">
                                {/* Header */}
                                <div className="hidden md:grid grid-cols-[auto_1fr_auto] gap-4 px-4 py-2.5 bg-(--base-02) border-b border-(--base-03)">
                                    <span className="mono-label">Type</span>
                                    <span className="mono-label">Name &amp; expected target</span>
                                    <span className="mono-label text-right">{checked ? 'Status' : ''}</span>
                                </div>
                                {result.records.map((rec, idx) => (
                                    <DnsRecordRow key={`${rec.name}-${rec.type}-${idx}`} rec={rec} checked={!!checked} />
                                ))}
                            </div>
                        </div>
                    )}

                    {/* Reachability */}
                    {result.reachability.length > 0 && (
                        <div>
                            <h3 className="mono-label mb-3">Reachability</h3>
                            <div className="space-y-2">
                                {result.reachability.map((r, idx) => (
                                    <div key={`${r.target}-${idx}`} className="flex items-start justify-between gap-4 p-3 rounded-md bg-(--base-02)">
                                        <div className="min-w-0">
                                            <code className="font-mono text-xs text-(--base-09) break-all">{r.target}</code>
                                            {r.hint && <p className="text-xs text-(--base-06) mt-0.5">{r.hint}</p>}
                                        </div>
                                        <span className={`badge ${r.ok ? 'badge-success' : 'badge-error'} shrink-0`}>
                                            {r.ok ? <><CircleCheck size={11} /> Reachable</> : <><CircleAlert size={11} /> Unreachable</>}
                                        </span>
                                    </div>
                                ))}
                            </div>
                        </div>
                    )}

                    {result.checkedAt && (
                        <p className="text-[11px] font-mono text-(--base-05)">
                            Last checked {new Date(result.checkedAt).toLocaleString()}
                        </p>
                    )}
                </div>
            )}
        </div>
    );
}

// One record row — name + expected target(s) with copy buttons + a category
// blurb. After a check, the actual resolved value(s), a status badge and the
// hint are shown. Before the first check the row only carries the expected
// config (status badge withheld).
function DnsRecordRow({ rec, checked }: { rec: DnsRecord; checked: boolean }) {
    const badge = DNS_STATUS_BADGE[rec.status];
    const showStatus = checked && badge;
    return (
        <div className="grid grid-cols-1 md:grid-cols-[auto_1fr_auto] gap-2 md:gap-4 px-4 py-3 border-b border-(--base-03) last:border-b-0 items-start">
            <span className="font-mono text-[11px] px-1.5 py-0.5 rounded bg-(--base-03) text-(--base-07) w-fit h-fit mt-0.5">{rec.type}</span>

            <div className="min-w-0 space-y-1.5">
                <div className="flex items-center gap-2 flex-wrap">
                    <code className="font-mono text-xs text-(--base-09) break-all">{rec.name}</code>
                    <span className="badge badge-neutral">{DNS_CATEGORY_LABEL[rec.category]}</span>
                </div>
                <p className="text-xs text-(--base-06)">{DNS_CATEGORY_BLURB[rec.category]}</p>

                {/* Expected targets — copyable */}
                {rec.expected.length > 0 && (
                    <div className="flex flex-col gap-1 pt-0.5">
                        <span className="text-[10px] font-mono uppercase tracking-[0.08em] text-(--base-05)">Expected</span>
                        {rec.expected.map((v, i) => <CopyValue key={i} value={v} />)}
                    </div>
                )}

                {/* Actual resolved values + hint (post-check) */}
                {checked && (
                    <div className="flex flex-col gap-1 pt-0.5">
                        <span className="text-[10px] font-mono uppercase tracking-[0.08em] text-(--base-05)">Resolved</span>
                        {rec.actual.length > 0 ? (
                            rec.actual.map((v, i) => (
                                <code key={i} className="font-mono text-xs text-(--base-08) break-all">{v}</code>
                            ))
                        ) : (
                            <code className="font-mono text-xs text-(--base-06) italic">no answer</code>
                        )}
                        {rec.hint && <p className="text-xs text-(--base-06) mt-0.5">{rec.hint}</p>}
                    </div>
                )}
            </div>

            <div className="md:text-right">
                {showStatus && <span className={`badge ${badge.cls}`}>{badge.label}</span>}
            </div>
        </div>
    );
}

// ─────────────────────────────────────────────
// Gateway panel
// ─────────────────────────────────────────────

// Inline notice shown above the operational gateway controls (routes)
// whenever Game Traffic is still on IP:Port. The routing-mode selector
// itself is never gated — it's the only way to turn the gateway on — so the
// copy points the operator back at it, right above.
function GatewayDisabledNotice() {
    return (
        <div className="alert alert-warning text-xs">
            <AlertTriangle size={14} className="shrink-0 mt-0.5" />
            <span>
                Gateway routing is disabled. Switch <span className="font-medium">Game Traffic</span> to{' '}
                <span className="font-medium">Gateway</span> or <span className="font-medium">Both</span>{' '}
                above to manage routes.
            </span>
        </div>
    );
}

function GatewayPanel({ showToast }: { showToast: (msg: string, ok?: boolean) => void }) {
    // Auto-move is gateway-only; switching back to IP:Port force-disables it
    // server-side. Surface that in the confirm step so the admin isn't
    // surprised that every server's auto-move opt-in gets cleared.
    const { featureFlags } = useAppData();

    const [settings, setSettings] = useState<GatewaySettings>({
        limits: {
            // null, not -1: the old sentinel for "unlimited" is not a value in
            // the limits convention any more, and every cap is enforced as
            // "count >= limit" - which a negative meets before the first route
            // exists. Saving this screen before its load landed used to write
            // that number into all four scopes and deny every route with
            // "You have used all -1 addresses".
            global: null, userDefault: null, portMcEnabled: true,
        },
        hosterDomains: [],
        customDomainsEnabled: false,
        cnameTarget: '',
        blockedRoutePrefixes: [],
    });
    // Preview of what users will actually be told to point their domain at:
    // the label combined with every hoster base, one target per region.
    const cnameTargets = cnameTargetsFor(settings.cnameTarget, settings.hosterDomains);
    const [loading, setLoading] = useState(true);
    const [saving, setSaving] = useState(false);

    const [routingMode, setRoutingMode] = useState<RoutingMode>('ip_port');
    const [fileMode, setFileMode] = useState<FileAccessMode>('sftp');
    const [origRoutingMode, setOrigRoutingMode] = useState<RoutingMode>('ip_port');
    const [origFileMode, setOrigFileMode] = useState<FileAccessMode>('sftp');
    const [confirmModal, setConfirmModal] = useState(false);
    const [applyingRouting, runSaveRouting] = useBusy();
    const [savingRouting, setSavingRouting] = useState(false);
    const [migration, setMigration] = useState<{ running: boolean; total: number; done: number; failed: number } | null>(null);
    const pollRef = useRef<ReturnType<typeof setInterval> | null>(null);

    // Snapshot of last-saved gateway settings for dirty detection. The routing
    // mode has its own explicit "Apply Routing" flow and is tracked separately.
    const snapshotRef = useRef<GatewaySettings | null>(null);
    const [loadFailed, setLoadFailed] = useState(false);

    // A load that FAILS must not render as a configuration. The form falls back
    // to its own defaults - no limits, no hoster domains, every toggle off -
    // which is exactly what an unconfigured platform looks like, and dirty is
    // measured against a snapshot a failed load never sets, so nothing typed in
    // could be saved and nothing would say so. useSettingsForm expresses this
    // through a blocked save bar; this screen is hand-rolled and has none.
    const load = useCallback(async () => {
        setLoading(true);
        try {
            const [gwRes, rmRes] = await Promise.all([getGatewaySettings(), getRoutingMode()]);
            if (!gwRes.success || !gwRes.settings) {
                setLoadFailed(true);
                return;
            }
            const loaded: GatewaySettings = {
                ...gwRes.settings,
                hosterDomains: gwRes.settings.hosterDomains || [],
                customDomainsEnabled: !!gwRes.settings.customDomainsEnabled,
                cnameTarget: gwRes.settings.cnameTarget || '',
                blockedRoutePrefixes: gwRes.settings.blockedRoutePrefixes || [],
            };
            setSettings(loaded);
            snapshotRef.current = loaded;
            setLoadFailed(false);
            if (rmRes.success) {
                const m: RoutingMode = rmRes.mode || 'ip_port';
                const f: FileAccessMode = rmRes.fileMode || 'sftp';
                setRoutingMode(m); setOrigRoutingMode(m);
                setFileMode(f); setOrigFileMode(f);
            }
        } catch {
            // A rejected promise took the same path as a failed response before:
            // straight past the setters and into finally, form rendered.
            setLoadFailed(true);
        } finally {
            setLoading(false);
        }
    }, []);

    useEffect(() => { load(); }, [load]);

    // The migration poll only stops itself once the run reports finished, so
    // leaving this tab mid-migration otherwise left it running: a request every
    // 3s and a setState into an unmounted component, for as long as the
    // migration lasts. The interval eight lines below already does this.
    useEffect(() => () => {
        if (pollRef.current) { clearInterval(pollRef.current); pollRef.current = null; }
    }, []);

    const startPolling = () => {
        if (pollRef.current) return;
        pollRef.current = setInterval(async () => {
            const res = await getRoutingMigrationStatus();
            if (res.success) {
                setMigration({ running: res.running, total: res.total, done: res.done, failed: res.failed });
                if (!res.running) { clearInterval(pollRef.current!); pollRef.current = null; }
            }
        }, 3000);
    };

    const handleSaveRouting = async () => {
        setSavingRouting(true);
        setConfirmModal(false);
        const res = await saveRoutingMode({ mode: routingMode, fileMode });
        if (res.success) {
            setOrigRoutingMode(routingMode);
            setOrigFileMode(fileMode);
            // The mode saves even when the fleet migration cannot start, and
            // "Routing mode saved." alone is indistinguishable from a platform
            // that had no servers to migrate - while every server is in fact
            // still on the old routing.
            if (res.migrationError) showToast(res.migrationError, false);
            else showToast(`Routing mode saved.${res.serversQueued > 0 ? ` Redeploying ${res.serversQueued} servers...` : ''}`);
            if (res.serversQueued > 0) {
                setMigration({ running: true, total: res.serversQueued, done: 0, failed: 0 });
                startPolling();
            }
        } else {
            showToast(res.message || 'Failed to save routing mode.', false);
        }
        setSavingRouting(false);
    };

    const handleSave = async (): Promise<boolean> => {
        setSaving(true);
        try {
            const res = await saveGatewaySettings(settings);
            showToast(res.success ? 'Gateway settings saved.' : (res.message || 'Save failed.'), res.success);
            if (res.success) snapshotRef.current = settings;
            return !!res.success;
        } finally {
            setSaving(false);
        }
    };

    const handleDiscard = () => {
        if (snapshotRef.current) setSettings(snapshotRef.current);
    };

    const setLimit = (key: LimitKey, value: number | null) =>
        setSettings(prev => ({ ...prev, limits: { ...prev.limits, [key]: value } }));

    const routingChanged = routingMode !== origRoutingMode || fileMode !== origFileMode;

    // Gate the operational route-management UI on the *applied* routing mode
    // (origRoutingMode), not the in-progress selection — the controls below
    // only do anything once Gateway/Both is actually live. The mode selector
    // above always stays usable.
    const gatewayOff = origRoutingMode === 'ip_port';

    const addHoster = () => {
        setSettings(prev => ({
            ...prev,
            hosterDomains: [...prev.hosterDomains, { domain: '', validation: 'alphanumeric' }],
        }));
    };

    // Removing a hoster domain — when the entry has a real domain value we
    // route through a confirm modal with an optional cascade-delete-routes
    // checkbox. Blank/unsaved entries (just-added rows) skip the prompt.
    const [removeTarget, setRemoveTarget] = useState<{ idx: number; domain: string } | null>(null);
    const [removeCascade, setRemoveCascade] = useState(false);
    const [removeCountdown, setRemoveCountdown] = useState(0);
    const [removeBusy, setRemoveBusy] = useState(false);

    useEffect(() => {
        if (!removeTarget || !removeCascade) {
            setRemoveCountdown(0);
            return;
        }
        setRemoveCountdown(5);
        const id = setInterval(() => {
            setRemoveCountdown(c => (c <= 1 ? 0 : c - 1));
        }, 1000);
        return () => clearInterval(id);
    }, [removeTarget, removeCascade]);

    const removeHoster = (idx: number) => {
        const target = settings.hosterDomains[idx];
        if (!target || !target.domain.trim()) {
            // Brand-new empty row — drop immediately, no confirm.
            setSettings(prev => ({
                ...prev,
                hosterDomains: prev.hosterDomains.filter((_, i) => i !== idx),
            }));
            return;
        }
        setRemoveCascade(false);
        setRemoveCountdown(0);
        setRemoveTarget({ idx, domain: target.domain });
    };

    const confirmRemoveHoster = async () => {
        if (!removeTarget) return;
        if (removeCascade && removeCountdown > 0) return; // timer still running
        setRemoveBusy(true);
        let cascadeMessage = '';
        if (removeCascade) {
            const res = await bulkDeleteRoutesBySuffix(removeTarget.domain);
            if (res.success) {
                cascadeMessage = ` (${res.deleted} route${res.deleted !== 1 ? 's' : ''} deleted)`;
            } else {
                cascadeMessage = ' (route cascade failed)';
            }
        }
        setSettings(prev => ({
            ...prev,
            hosterDomains: prev.hosterDomains.filter((_, i) => i !== removeTarget.idx),
        }));
        setRemoveBusy(false);
        setRemoveTarget(null);
        // Nothing has been written yet - the removal is a local edit, and the
        // card's own Save commits it along with everything else in it.
        showToast(`Removed ${removeTarget.domain}${cascadeMessage}. Save this card to apply it.`);
    };
    const updateHoster = (idx: number, patch: Partial<HosterDomain>) => {
        setSettings(prev => ({
            ...prev,
            hosterDomains: prev.hosterDomains.map((h, i) => i === idx ? { ...h, ...patch } : h),
        }));
    };

    const VALIDATION_LABELS: Record<HosterValidation, string> = {
        letters: 'Letters only',
        alphanumeric: 'Letters + numbers',
        dns: 'Full DNS (a-z, 0-9, -)',
    };

    // Both of the first two are a cap on ONE tenant's addresses, not a platform
    // total - the global one used to say "Total routes across all users and
    // servers", which is not what it does and would be read as a completely
    // different setting. They are asked in order: a user's own override, then
    // the default below, then this one.
    const allocationFields: { key: LimitKey; label: string; desc: string; unlimitedLabel?: string }[] = [
        { key: 'global', label: 'Fallback Per-User Max', desc: 'Used when neither a per-user override nor the default below is set' },
        {
            key: 'userDefault', label: 'Default Per-User Max', desc: 'Applies to every user without their own override',
            // Blank here does not mean uncapped: it hands the question to the
            // fallback above. Saying "No limit" would state the opposite
            // whenever that fallback carries a number.
            unlimitedLabel: 'Use fallback',
        },
    ];

    const dirty =
        snapshotRef.current !== null &&
        JSON.stringify(settings) !== JSON.stringify(snapshotRef.current);

    useUnsavedChanges({ dirty, save: handleSave, discard: handleDiscard, saving });

    const loadState = settingsLoadState(loading, loadFailed);
    if (loadState === 'loading') return (
        <div className="space-y-6">
            <SkeletonHeader />
            <SkeletonCard height="h-72" />
            <SkeletonCard height="h-80" />
            <SkeletonCard height="h-96" />
        </div>
    );
    if (loadState === 'failed') return <SettingsLoadError what="the gateway settings" onRetry={load} />;

    return (
        <SettingsPage
            title="Gateway configuration"
            icon={Router}
            width="full"
            description="Gateway routing, link defaults and route limits for gates and links."
        >
            {/* Routing Mode */}
            <div className="card p-5 space-y-5">
                <div className="flex items-center gap-3">
                    <div className="w-9 h-9 rounded-md bg-(--base-03) flex items-center justify-center">
                        <Router size={18} className="text-(--accent-light)" />
                    </div>
                    <div>
                        <div className="font-medium text-sm text-(--base-09)">Traffic Routing</div>
                        <div className="text-xs text-(--base-06)">How game traffic and file access are routed to servers</div>
                    </div>
                </div>

                <div>
                    <h3 className="mono-label mb-3 flex items-center gap-1.5">
                        Game Traffic
                        <HelpTip label="About the routing mode">
                            <p className="mb-2">
                                Where players connect. This is the highest-consequence setting on
                                this page: applying it <strong>redeploys every server</strong>, because
                                each container has to be recreated with different networking.
                            </p>
                            <p className="mb-2">
                                <strong>IP:Port</strong> - players connect straight to the node&apos;s
                                public address. Works with no gateway at all, and exposes the machine.
                                <br />
                                <strong>Gateway</strong> - players connect to a domain on 25565 and
                                the edge forwards it. The node&apos;s address is never given out.
                                <br />
                                <strong>Both</strong> - either route works. Useful while migrating;
                                the node address stays exposed while it is on.
                            </p>
                            <p>
                                The node IP is only fully hidden with Gateway <em>and</em> File
                                Access on Beam - SFTP would otherwise hand out the same address.
                            </p>
                        </HelpTip>
                    </h3>
                    <div className="grid grid-cols-3 gap-2">
                        {ROUTING_OPTIONS.map(opt => (
                            <button key={opt.value} type="button" onClick={() => setRoutingMode(opt.value)}
                                className={`p-3 rounded-md border text-left transition-colors ${routingMode === opt.value ? 'border-(--accent) bg-(--accent)/10' : 'border-(--base-03) bg-(--base-02) hover:border-(--base-05)'}`}>
                                <div className={`text-sm font-medium ${routingMode === opt.value ? 'text-(--accent-light)' : 'text-(--base-09)'}`}>{opt.label}</div>
                                <div className="text-xs text-(--base-06) mt-0.5">{opt.desc}</div>
                            </button>
                        ))}
                    </div>
                </div>

                <div>
                    <h3 className="mono-label mb-3 flex items-center gap-1.5">
                        File Access
                        <HelpTip label="About file access">
                            <p className="mb-2">
                                How people reach their files.
                            </p>
                            <p className="mb-2">
                                <strong>SFTP</strong> is the familiar one and works with any client -
                                but the client connects to the node directly, so it learns the
                                machine&apos;s address.
                                <br />
                                <strong>Beam</strong> is the desktop app. It works over the local
                                network without a gateway, and remotely through the relay, which is
                                what keeps the node address private.
                            </p>
                            <p>
                                Switching to Beam does not remove existing SFTP credentials; it stops
                                offering the SFTP route in the panel.
                            </p>
                        </HelpTip>
                    </h3>
                    <div className="grid grid-cols-3 gap-2">
                        {FILE_OPTIONS.map(opt => (
                            <button key={opt.value} type="button" onClick={() => setFileMode(opt.value)}
                                className={`p-3 rounded-md border text-left transition-colors ${fileMode === opt.value ? 'border-(--accent) bg-(--accent)/10' : 'border-(--base-03) bg-(--base-02) hover:border-(--base-05)'}`}>
                                <div className={`text-sm font-medium ${fileMode === opt.value ? 'text-(--accent-light)' : 'text-(--base-09)'}`}>{opt.label}</div>
                                <div className="text-xs text-(--base-06) mt-0.5">{opt.desc}</div>
                            </button>
                        ))}
                    </div>
                </div>

                <div className={`flex items-start gap-3 p-3 rounded-md border ${routingMode === 'gateway' && fileMode === 'beam' ? 'border-(--success)/30 bg-(--success)/5' : 'border-(--base-04) bg-(--base-02)'}`}>
                    <EyeOff size={15} className={`mt-0.5 shrink-0 ${routingMode === 'gateway' && fileMode === 'beam' ? 'text-(--success-light)' : 'text-(--base-06)'}`} />
                    <p className="text-xs text-(--base-07)">
                        The public Node IP is only fully hidden when both <span className="text-(--base-09) font-medium">Game Traffic</span> is set to <span className="text-(--base-09) font-medium">Gateway</span> and <span className="text-(--base-09) font-medium">File Access</span> is set to <span className="text-(--base-09) font-medium">Beam</span>.
                        {routingMode === 'gateway' && fileMode === 'beam' && <span className="text-(--success-light) font-medium ml-1">Node IPs are currently fully hidden.</span>}
                    </p>
                </div>

                {migration && (
                    <div className="p-3 rounded-md bg-(--base-02) border border-(--base-04) space-y-2">
                        <div className="flex items-center justify-between">
                            <div className="flex items-center gap-2">
                                {migration.running ? <Spinner size="xs" className="text-(--accent-light)" /> : <RefreshCw size={13} className="text-(--accent-light)" />}
                                <span className="text-xs text-(--base-09)">{migration.running ? 'Redeploying servers...' : 'Redeploy complete'}</span>
                            </div>
                            <span className="font-mono text-xs text-(--base-06)">{migration.done} / {migration.total} done{migration.failed > 0 ? ` · ${migration.failed} failed` : ''}</span>
                        </div>
                        <div className="h-1.5 rounded-full bg-(--base-03) overflow-hidden">
                            <div className={`h-full rounded-full transition-all duration-300 ${migration.failed > 0 ? 'bg-(--error-light)' : 'bg-(--accent)'}`}
                                style={{ width: migration.total > 0 ? `${Math.round((migration.done / migration.total) * 100)}%` : '0%' }} />
                        </div>
                    </div>
                )}

                <div className="flex items-center gap-3 pt-1 border-t border-(--base-03)">
                    <button onClick={() => setConfirmModal(true)} disabled={!routingChanged || savingRouting}
                        className="btn btn-primary disabled:opacity-40">
                        <Save size={14} />
                        {savingRouting ? 'Applying...' : 'Apply Routing'}
                    </button>
                    {routingChanged && (
                        <span className="text-xs text-(--base-06) flex items-center gap-1.5">
                            <AlertTriangle size={12} className="text-(--warning-light)" />
                            Changing routing mode will trigger a server redeploy
                        </span>
                    )}
                </div>
            </div>

            {/* Operational gateway content — only meaningful once Gateway/Both
                is the applied routing mode. Greyed + disabled otherwise; the
                mode selector above stays usable to turn it on. */}
            {gatewayOff && <GatewayDisabledNotice />}
            <fieldset disabled={gatewayOff} className="space-y-6 disabled:opacity-50 border-0 p-0 m-0">

            {/* Domains and route limits are ONE payload behind one endpoint, so
                they are one card with one save. They used to be two cards with
                the DNS diagnostics wedged between them, which read as three
                separate things and left no way to tell what the single Save
                button was about to write. */}
            <SettingsCard
                title="Domains and route limits"
                icon={Globe}
                form={{ dirty, saving, save: handleSave, discard: handleDiscard }}
                description="The domains users pick from, and how many routes and ports they may take."
            >
                <SettingsGroup title="Hoster domains" first>
                    <div>
                    <p className="text-xs text-(--base-06) mb-3">
                        Users only enter a subdomain — these base domains appear as a dropdown next to the input. Pick which characters are allowed in the subdomain per domain.
                    </p>
                    <div className="space-y-2">
                        {settings.hosterDomains.length === 0 && (
                            <p className="text-xs text-(--base-05) italic px-3 py-3 rounded-md bg-(--base-02)">
                                No hoster domains configured. Add one to enable the subdomain picker.
                            </p>
                        )}
                        {settings.hosterDomains.map((hd, idx) => (
                            <div key={idx} className="flex items-center gap-2 p-2 rounded-md bg-(--base-02)">
                                <input
                                    type="text"
                                    value={hd.domain}
                                    onChange={e => updateHoster(idx, { domain: e.target.value.toLowerCase().trim() })}
                                    placeholder="dylaris.com"
                                    className="input-field input-mono flex-1 text-sm"
                                />
                                <select
                                    value={hd.validation}
                                    onChange={e => updateHoster(idx, { validation: e.target.value as HosterValidation })}
                                    className="input-field text-sm w-52"
                                >
                                    {(Object.keys(VALIDATION_LABELS) as HosterValidation[]).map(k => (
                                        <option key={k} value={k}>{VALIDATION_LABELS[k]}</option>
                                    ))}
                                </select>
                                <button
                                    type="button"
                                    onClick={() => removeHoster(idx)}
                                    className="text-(--base-06) hover:text-(--error-light) transition-colors p-2"
                                    title="Remove"
                                >
                                    <Trash2 size={14} />
                                </button>
                            </div>
                        ))}
                    </div>
                    <button
                        type="button"
                        onClick={addHoster}
                        className="btn btn-secondary btn-sm mt-3"
                    >
                        <Plus size={12} /> Add domain
                    </button>
                </div>

                <div className="border-t border-(--base-03) pt-5 space-y-4">
                    <div className="flex items-center justify-between gap-4">
                        <div>
                            <h3 className="mono-label">Custom Domains</h3>
                            <p className="text-xs text-(--base-06) mt-1">Allow users to bring their own domain via a CNAME record.</p>
                        </div>
                        <button
                            type="button"
                            role="switch"
                            aria-checked={settings.customDomainsEnabled}
                            onClick={() => setSettings(prev => ({ ...prev, customDomainsEnabled: !prev.customDomainsEnabled }))}
                            className={`toggle-track ${settings.customDomainsEnabled ? 'toggle-track-on' : 'toggle-track-off'}`}
                        >
                            <span className={`toggle-knob ${settings.customDomainsEnabled ? 'toggle-knob-on' : 'toggle-knob-off'}`} />
                        </button>
                    </div>
                    {settings.customDomainsEnabled && (
                        <div className="flex flex-col gap-[5px]">
                            <label className="input-label">CNAME Label</label>
                            <input
                                type="text"
                                value={settings.cnameTarget}
                                onChange={e => setSettings(prev => ({ ...prev, cnameTarget: e.target.value.toLowerCase() }))}
                                placeholder="route"
                                className="input-field input-mono text-sm"
                            />
                            <p className="text-xs text-(--base-06)">
                                A single label, not a full domain. It is combined with every hoster domain above, so one
                                entry covers all regions and each user picks the target for the region they want.
                            </p>
                            {cnameTargets.length > 0 && (
                                <div className="mt-1 flex flex-col gap-1">
                                    <span className="text-xs text-(--base-06)">Users will be told to point their domain at:</span>
                                    {cnameTargets.map(t => (
                                        <code key={t} className="font-mono text-xs text-(--accent-light) bg-(--base-02) px-1.5 py-0.5 rounded w-fit">{t}</code>
                                    ))}
                                    <span className="text-xs text-(--base-06)">
                                        Each of these needs its own A record pointing at that region&apos;s edge IPs. The
                                        wildcard does not cover them.
                                    </span>
                                </div>
                            )}
                            {settings.cnameTarget.trim() !== '' && settings.hosterDomains.length === 0 && (
                                <span className="text-xs text-(--warning-light)">
                                    Add a hoster domain above — without one there is nothing to combine this label with.
                                </span>
                            )}
                        </div>
                    )}
                </div>

                <div className="border-t border-(--base-03) pt-5 space-y-3">
                    <div>
                        <h3 className="mono-label">Reserved Route Prefixes</h3>
                        <p className="text-xs text-(--base-06) mt-1">
                            Leftmost labels users cannot register (subdomain picker and the first label of custom domains). One per line.
                        </p>
                    </div>
                    <textarea
                        value={settings.blockedRoutePrefixes.join('\n')}
                        onChange={e => setSettings(prev => ({
                            ...prev,
                            blockedRoutePrefixes: e.target.value
                                .split(/[\n,]/)
                                .map(s => s.trim().toLowerCase())
                                .filter(Boolean),
                        }))}
                        rows={4}
                        spellCheck={false}
                        placeholder={'admin\ndylaris\napp\napi'}
                        className="input-field input-mono text-sm w-full resize-y"
                    />
                    <p className="text-xs text-(--base-06)">
                        Leave empty to allow everything. Saving an empty list disables the built-in defaults.
                    </p>
                </div>
                </SettingsGroup>

                <SettingsGroup title="Route limits" description="Maximum route allocations and port access.">

                <div>
                    <h3 className="mono-label mb-3 flex items-center gap-1.5">
                        Route Allocation
                        <HelpTip label="About route allocation">
                            <p className="mb-2">
                                Asked in order, most specific first: a user&apos;s own override, then
                                the per-user default, then this global figure. The <strong>first one
                                that exists wins outright</strong> and the rest are not consulted.
                            </p>
                            <p className="mb-2">
                                So &quot;Global Max Routes&quot; is not a ceiling over the others - it
                                is the fallback for users no more specific rule covers. A per-user
                                override REPLACES it for that user and can be higher.
                            </p>
                            <p className="mb-2">
                                A scope with no entry is skipped; one that exists has answered, even
                                when its answer is no limit.
                            </p>
                            {LimitHelp}
                            <p className="mt-2">
                                Only addresses on your own domains count. A customer pointing their
                                own domain at the gateway is neither counted nor capped here.
                            </p>
                        </HelpTip>
                    </h3>
                    <div className="space-y-3">
                        {allocationFields.map(({ key, label, desc, unlimitedLabel }) => (
                            <div key={key} className="flex items-center justify-between gap-4 p-3 rounded-md bg-(--base-02)">
                                <div className="flex-1 min-w-0">
                                    <p className="text-sm text-(--base-09)">{label}</p>
                                    <p className="text-xs text-(--base-06)">{desc}</p>
                                </div>
                                <LimitField value={settings.limits[key]} onChange={v => setLimit(key, v)} unlimitedLabel={unlimitedLabel} />
                            </div>
                        ))}
                    </div>
                    <p className="text-xs text-(--base-05) mt-2"><span className="font-mono">0</span> = no routes allowed; switch off <span className="font-mono">No limit</span> to type a number. Per-user overrides can be set in user settings.</p>
                </div>

                <div className="border-t border-(--base-03) pt-5">
                    <h3 className="mono-label mb-3">Port Configuration</h3>
                    <div className="space-y-3">
                        {/* MC Port */}
                        <div className={`p-3 rounded-md bg-(--base-02) ${!settings.limits.portMcEnabled ? 'opacity-60' : ''}`}>
                            <div className="flex items-center justify-between mb-2">
                                <div className="flex items-center gap-2">
                                    <span className="text-sm font-semibold text-(--base-09)">Minecraft</span>
                                    <span className="font-mono text-[10px] px-1.5 py-0.5 rounded bg-(--base-03) text-(--base-06)">25565</span>
                                </div>
                                <button type="button" role="switch" aria-checked={settings.limits.portMcEnabled}
                                    onClick={() => setSettings(prev => ({ ...prev, limits: { ...prev.limits, portMcEnabled: !prev.limits.portMcEnabled } }))}
                                    className={`toggle-track ${settings.limits.portMcEnabled ? 'toggle-track-on' : 'toggle-track-off'}`}>
                                    <span className={`toggle-knob ${settings.limits.portMcEnabled ? 'toggle-knob-on' : 'toggle-knob-off'}`} />
                                </button>
                            </div>
                        </div>
                    </div>
                    <p className="text-xs text-(--base-05) mt-2">Disabled ports block all route creation on that port.</p>
                </div>
                </SettingsGroup>
            </SettingsCard>

            {/* Diagnostics for the records derived from the hoster domains,
                CNAME target and panel URL configured above. Each owns its own
                state, so they sit after the settings rather than between them. */}
            <GatewayDnsCard />

            <DnsCheckCard />

            </fieldset>

            {/* Routing confirmation modal */}
            {confirmModal && (
                <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 backdrop-blur-sm">
                    <div className="card p-6 max-w-md w-full mx-4 space-y-4">
                        <div className="flex items-start gap-3">
                            <div className="w-9 h-9 rounded-md bg-(--warning)/10 border border-(--warning)/20 flex items-center justify-center shrink-0">
                                <AlertTriangle size={18} className="text-(--warning-light)" />
                            </div>
                            <div>
                                <h3 className="font-display font-bold text-(--base-09) text-base">Confirm Routing Change</h3>
                                <p className="text-xs text-(--base-06) mt-0.5">This action will redeploy all active servers</p>
                            </div>
                        </div>
                        <div className="space-y-2 text-sm text-(--base-07)">
                            {routingMode === 'gateway' && origRoutingMode !== 'gateway' && (
                                <p>Switching to <span className="text-(--base-09) font-medium">Gateway</span> mode: all host port bindings will be removed and servers will be redeployed without exposed ports.</p>
                            )}
                            {routingMode !== 'gateway' && origRoutingMode === 'gateway' && (
                                <p>Switching away from <span className="text-(--base-09) font-medium">Gateway</span> mode: new host ports will be assigned to all servers during redeploy.</p>
                            )}
                            {routingMode === 'both' && origRoutingMode !== 'both' && origRoutingMode !== 'gateway' && (
                                <p>Switching to <span className="text-(--base-09) font-medium">Both</span> mode: servers will keep or receive host ports while also supporting gateway routes.</p>
                            )}
                            {fileMode !== origFileMode && (
                                <p>File access mode is changing to <span className="text-(--base-09) font-medium">{FILE_OPTIONS.find(o => o.value === fileMode)?.label}</span>.</p>
                            )}
                            {routingMode === 'ip_port' && origRoutingMode !== 'ip_port' && featureFlags.autoMove && (
                                <div className="alert alert-warning text-xs">
                                    <AlertTriangle size={14} className="shrink-0 mt-0.5" />
                                    <span>Disabling the gateway will turn off Auto-Move and clear every server&apos;s auto-move opt-in.</span>
                                </div>
                            )}
                            <p className="text-(--base-06) text-xs pt-1">Servers are redeployed in batches of 4 with 15s between batches. Each container has a 60s timeout before a force-kill is issued.</p>
                        </div>
                        <div className="flex gap-3 pt-2">
                            <button onClick={() => runSaveRouting(handleSaveRouting)} disabled={applyingRouting} className="btn btn-primary flex-1 disabled:opacity-40">Confirm & Apply</button>
                            <button onClick={() => setConfirmModal(false)} className="btn px-5 py-2 text-sm flex-1">Cancel</button>
                        </div>
                    </div>
                </div>
            )}

            {/* Hoster-domain remove confirmation (optional cascade) */}
            {removeTarget && (
                <div className="modal-overlay animate-fade-in">
                    <div className="modal-panel w-full max-w-md">
                        <div className="modal-header flex items-center justify-between">
                            <h3 className="modal-title flex items-center gap-2 text-(--error-light)">
                                <AlertTriangle size={18} />
                                Remove Hoster Domain
                            </h3>
                            <button onClick={() => setRemoveTarget(null)} className="text-(--base-06) hover:text-(--error-light)" disabled={removeBusy}>
                                <X size={18} />
                            </button>
                        </div>
                        <div className="modal-body space-y-3">
                            <p className="text-sm text-(--base-08)">
                                Remove <code className="font-mono text-(--base-09) bg-(--base-02) px-1.5 py-0.5 rounded">{removeTarget.domain}</code> from the hoster-domain list?
                            </p>
                            <p className="text-xs text-(--base-06)">
                                Users will no longer be able to register new subdomains under it. Existing routes pointing to this domain stay active by default.
                            </p>

                            <label className="flex items-start gap-2 cursor-pointer pt-1">
                                <input
                                    type="checkbox"
                                    checked={removeCascade}
                                    onChange={e => setRemoveCascade(e.target.checked)}
                                    className="checkbox mt-0.5"
                                />
                                <span className="text-sm text-(--base-08)">
                                    Also delete all related routes ending in <code className="font-mono">.{removeTarget.domain}</code>
                                </span>
                            </label>
                            {removeCascade && (
                                <div className="alert alert-error text-xs">
                                    <AlertTriangle size={14} className="shrink-0 mt-0.5" />
                                    <span>
                                        Cascade is permanent. Every server route under this domain will be deleted from the gateway. The confirm button is locked for {removeCountdown}s so you can re-read this.
                                    </span>
                                </div>
                            )}
                        </div>
                        <div className="modal-footer">
                            <button
                                onClick={() => setRemoveTarget(null)}
                                disabled={removeBusy}
                                className="btn btn-secondary"
                            >
                                Cancel
                            </button>
                            <button
                                onClick={confirmRemoveHoster}
                                disabled={removeBusy || (removeCascade && removeCountdown > 0)}
                                className={`btn disabled:opacity-40 ${removeCascade ? 'btn-danger' : 'btn-primary'}`}
                            >
                                {removeBusy
                                    ? <><Spinner size="xs" /> Removing…</>
                                    : (removeCascade && removeCountdown > 0)
                                        ? `Confirm cascade (${removeCountdown}s)`
                                        : removeCascade
                                            ? <><Trash2 size={13} /> Remove + delete routes</>
                                            : 'Remove domain'}
                            </button>
                        </div>
                    </div>
                </div>
            )}
        </SettingsPage>
    );
}

// ─────────────────────────────────────────────
// Hub Admin panel (TP2b)
// ─────────────────────────────────────────────


// ─────────────────────────────────────────────
// Main export
// ─────────────────────────────────────────────

export default function GatewayTab() {
    return <GatewayPanel showToast={toast} />;
}
