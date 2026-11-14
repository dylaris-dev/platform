'use client';

import { useCallback, useEffect, useState } from 'react';
import { Cloud, CircleAlert, CircleCheck, Loader2, Plug, RefreshCw } from 'lucide-react';
import SettingsCard, { SettingsGroup } from '@/components/settings/SettingsCard';
import { Skeleton } from '@/components/Skeleton';
import { useSettingsForm } from '@/lib/useSettingsForm';
import { CONN_TEST_TIMEOUT_MS } from '@/lib/connectionTest';
import { formatDecimalBytes } from '@/lib/format';
import { fmtDateTime } from '@/lib/time';
import {
    getGatewayProvider, saveGatewayProvider, testGatewayProvider, getProviderUsage, getOrphanKits, revokeOrphanKit,
    providerSaveBody, providerTestBody,
    type ProviderSettings, type ProviderAccount, type ProviderUsage, type OrphanKit,
} from '@/lib/api/gatewayProvider';
import {
    providerErrorText, outboxWarning, barPercent, switchConfirm, purchaseNote, usageFinalLabel, providerGbToBytes,
    productLabel, testedAccount, routingModeWarning, refusalLine,
    ORPHAN_INTRO, showOrphanSection, orphanRevokeConfirm,
} from '@/lib/gatewayProvider';
import { useAppData } from '@/lib/AppDataContext';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import { toast } from '@/components/ui/Toast';
import { linkState } from '@/lib/linkState';
import LinkBadge from '@/components/infra/LinkBadge';

// Which gateway this platform uses. The settings GET calls the provider on
// every load (uncached for a rolled key, rate limited), so it is read on mount
// and on the refresh button only, never on an interval. The same goes for
// the usage read.

type Mode = 'local' | 'remote';

interface ProviderFormValue {
    mode: Mode;
    url: string;
    // Write-only: never filled from the server, which never sends it.
    key: string;
}

const MODES: { value: Mode; label: string; desc: string }[] = [
    { value: 'local', label: "This platform's own gateway", desc: 'Your Hub, edges and links carry the players.' },
    { value: 'remote', label: 'DYLARIS Managed Gateway', desc: 'DYLARIS runs the gateway; each node runs one link.' },
];

// A spinner for work that usually finishes at once is flicker.
function useSlow(active: boolean, delayMs = 400): boolean {
    const [slow, setSlow] = useState(false);
    useEffect(() => {
        if (!active) { setSlow(false); return; }
        const t = setTimeout(() => setSlow(true), delayMs);
        return () => clearTimeout(t);
    }, [active, delayMs]);
    return slow;
}

// The limits convention: null = no cap, 0 = none, n = the cap.
function limitText(n: number | null | undefined): string {
    if (n === null || n === undefined) return 'No limit';
    if (n === 0) return 'None';
    return String(n);
}

// Core's own sentence wins; ours is only for an answer that carries none.
function problemText(p: { code: string; message?: string }): string {
    return p.message || providerErrorText(p.code);
}

function AccountSummary({ account }: { account: ProviderAccount }) {
    const rows: [string, string][] = [
        ['Product', productLabel(account.product)],
        ['Links', limitText(account.limits?.maxLinks)],
        ['Routes', limitText(account.limits?.maxRoutes)],
        ['Custom domains', limitText(account.limits?.maxCustomDomains)],
    ];
    return (
        <dl className="grid grid-cols-2 sm:grid-cols-4 gap-2">
            {rows.map(([k, v]) => (
                <div key={k} className="p-3 rounded-md bg-(--base-02) min-w-0">
                    <dt className="mono-label">{k}</dt>
                    <dd className="text-sm text-(--base-09) mt-1 truncate" title={v}>{v}</dd>
                </div>
            ))}
        </dl>
    );
}

function UsagePanel() {
    const [usage, setUsage] = useState<ProviderUsage | null>(null);
    const [error, setError] = useState('');
    const [loading, setLoading] = useState(true);
    const slow = useSlow(loading);

    const load = useCallback(async () => {
        setLoading(true);
        setError('');
        const res = await getProviderUsage();
        if (res.success) setUsage(res);
        else setError(res.message);
        setLoading(false);
    }, []);

    useEffect(() => { void load(); }, [load]);

    const total = (usage?.edgeBytes ?? 0) + (usage?.relayBytes ?? 0);
    const pool = usage?.pool ?? null;
    const pct = barPercent(pool?.percent);

    return (
        <SettingsGroup title="Usage this month">
            <div className="flex items-start justify-between gap-3">
                <div className="min-w-0" aria-live="polite">
                    {loading && slow && <Skeleton className="h-5 w-40" />}
                    {!loading && error && (
                        <p className="flex items-start gap-1.5 text-xs text-(--error-light)">
                            <CircleAlert size={13} className="shrink-0 mt-0.5" /><span>{error}</span>
                        </p>
                    )}
                    {!loading && !error && usage && (
                        <p className="text-sm text-(--base-09)">
                            <span className="font-mono">{formatDecimalBytes(total)}</span>
                            <span className="text-xs text-(--base-06) ml-2">
                                {usageFinalLabel(usage.final)}
                            </span>
                        </p>
                    )}
                </div>
                <button
                    type="button"
                    onClick={() => void load()}
                    disabled={loading}
                    className="btn btn-secondary btn-sm shrink-0 disabled:opacity-40 inline-flex items-center gap-1.5"
                    aria-label="Refresh usage"
                >
                    <RefreshCw size={13} className={loading ? 'animate-spin motion-reduce:animate-none' : ''} /> Refresh
                </button>
            </div>
            {!loading && !error && pool && (
                <div className="space-y-1.5">
                    <div
                        className="h-1.5 rounded-full bg-(--base-03) overflow-hidden"
                        role="progressbar"
                        aria-label="Traffic pool used"
                        aria-valuemin={0}
                        aria-valuemax={100}
                        aria-valuenow={pct}
                    >
                        <div className={`h-full rounded-full ${pct >= 90 ? 'bg-(--warning)' : 'bg-(--accent)'}`} style={{ width: `${pct}%` }} />
                    </div>
                    {/* The provider counts in decimal units, so these do too: a
                        5000 GB plan reads 5 TB, as it was bought. */}
                    <p className={`text-xs ${pct >= 90 ? 'text-(--warning-light)' : 'text-(--base-06)'}`}>
                        {pct}% of the pool used: {formatDecimalBytes(pool.usedBytes)} of {formatDecimalBytes(providerGbToBytes(pool.includedGb))} included.{' '}
                        {purchaseNote(pool.maxPurchaseGb, formatDecimalBytes)}
                    </p>
                </div>
            )}
        </SettingsGroup>
    );
}

// Links the provider holds that no node here has. Loaded with the card and on
// the refresh button only: every load reads the provider's kit list.
function OrphanKitsPanel() {
    const [items, setItems] = useState<OrphanKit[] | null>(null);
    const [error, setError] = useState('');
    const [loading, setLoading] = useState(true);
    const [revoking, setRevoking] = useState('');

    const load = useCallback(async () => {
        setLoading(true);
        setError('');
        const res = await getOrphanKits();
        if (res.success) setItems(res.items ?? []);
        else setError(res.message);
        setLoading(false);
    }, []);

    useEffect(() => { void load(); }, [load]);

    const revoke = async (kit: OrphanKit) => {
        if (!(await confirmDialog(orphanRevokeConfirm(kit.name)))) return;
        setRevoking(kit.linkId);
        const res = await revokeOrphanKit(kit.linkId);
        setRevoking('');
        if (!res.success) { toast(res.message, false); return; }
        toast(res.queued ? 'The provider could not be reached. The revoke is retried on its own.' : 'Link revoked');
        if (!res.queued) setItems(list => (list ? list.filter(k => k.linkId !== kit.linkId) : list));
    };

    if (!showOrphanSection(items, error)) return null;

    return (
        <SettingsGroup title="Links the provider holds that this platform does not know">
            <div className="flex items-start justify-between gap-3">
                <p className="text-xs text-(--base-06) min-w-0">{ORPHAN_INTRO}</p>
                <button
                    type="button"
                    onClick={() => void load()}
                    disabled={loading || !!revoking}
                    className="btn btn-secondary btn-sm shrink-0 disabled:opacity-40 inline-flex items-center gap-1.5"
                    aria-label="Refresh the unknown links"
                >
                    <RefreshCw size={13} className={loading ? 'animate-spin motion-reduce:animate-none' : ''} /> Refresh
                </button>
            </div>
            {error && (
                <p className="flex items-start gap-1.5 text-xs text-(--error-light)" role="alert">
                    <CircleAlert size={13} className="shrink-0 mt-0.5" /><span>{error}</span>
                </p>
            )}
            {!error && items && (
                <ul className="space-y-2" aria-busy={loading}>
                    {items.map(kit => {
                        const state = linkState(kit.online);
                        return (
                            <li key={kit.linkId} className="flex flex-wrap items-center justify-between gap-3 p-3 rounded-md bg-(--base-02)">
                                <div className="min-w-0">
                                    <div className="flex items-center gap-2 text-sm text-(--base-09)">
                                        <LinkBadge state={state} />
                                        <span className="truncate">{kit.name || 'Unnamed link'}</span>
                                    </div>
                                    <div className="text-xs text-(--base-06) mt-0.5">
                                        <span className="font-mono" title={kit.linkId}>
                                            {kit.linkId.length > 20 ? `${kit.linkId.slice(0, 20)}…` : kit.linkId}
                                        </span>
                                        {kit.region && <span> · {kit.region}</span>}
                                        {kit.retireAt && <span className="text-(--warning-light)"> · being replaced until {fmtDateTime(kit.retireAt)}</span>}
                                    </div>
                                </div>
                                <button
                                    type="button"
                                    onClick={() => void revoke(kit)}
                                    disabled={!!revoking || loading}
                                    className="btn btn-danger btn-sm shrink-0 disabled:opacity-40 inline-flex items-center gap-1.5"
                                >
                                    {revoking === kit.linkId && <Loader2 size={13} className="animate-spin motion-reduce:animate-none" />}
                                    Revoke
                                </button>
                            </li>
                        );
                    })}
                </ul>
            )}
        </SettingsGroup>
    );
}

/**
 * reloadToken: bumped by the page after a routing-mode save. That save sends
 * no features.changed, so without it the routing warning stays as loaded.
 */
export default function GatewayProviderCard({ reloadToken = 0 }: { reloadToken?: number }) {
    const [view, setView] = useState<ProviderSettings | null>(null);
    const { refreshFeatureFlags, gatewayVisibility } = useAppData();
    const [testing, setTesting] = useState(false);
    const [test, setTest] = useState<{ ok: boolean; text: string; account?: ProviderAccount } | null>(null);
    const [loadError, setLoadError] = useState('');

    const form = useSettingsForm<ProviderFormValue>({
        load: async () => {
            const res = await getGatewayProvider();
            if (!res.success) { setLoadError(res.message); return null; }
            setLoadError('');
            setView(res);
            return { mode: res.mode, url: res.url, key: '' };
        },
        save: async v => {
            if (!view) return { ok: false, message: 'The settings are not loaded.' };
            const res = await saveGatewayProvider(providerSaveBody({
                ...v, savedUrl: view.url, urlFromEnv: view.urlFromEnv, keyFromEnv: view.keyFromEnv,
            }));
            if (!res.success) return { ok: false, message: res.message };
            setView(res);
            setTest(null);
            // This tab switches its surfaces now; other tabs follow the
            // features.changed event the save sends.
            void refreshFeatureFlags();
            return { ok: true, value: { mode: res.mode, url: res.url, key: '' } };
        },
        successMessage: 'Gateway provider saved',
        // Here rather than around the card's button, so the leave-with-unsaved-
        // changes dialog asks too.
        confirmBeforeSave: switchConfirm,
    });

    // Only the view (routingModeOk) is re-read: reloading the form would drop
    // what the operator typed into it.
    useEffect(() => {
        if (!reloadToken) return;
        let alive = true;
        void getGatewayProvider().then(res => { if (alive && res.success) setView(res); });
        return () => { alive = false; };
    }, [reloadToken]);

    const v = form.value;
    const slow = useSlow(form.loading);

    const runTest = async () => {
        if (!v || !view) return;
        setTesting(true);
        setTest(null);
        const controller = new AbortController();
        const timer = setTimeout(() => controller.abort(), CONN_TEST_TIMEOUT_MS);
        try {
            const res = await testGatewayProvider(providerTestBody({
                ...v, savedUrl: view.url, urlFromEnv: view.urlFromEnv, keyFromEnv: view.keyFromEnv,
            }), controller.signal);
            if (!res.success) setTest({ ok: false, text: res.message });
            else if (res.ok) setTest({ ok: true, text: 'Connected. The account has a managed gateway plan.', account: res.account });
            else setTest({ ok: false, text: res.error ? problemText(res.error) : providerErrorText(''), account: res.account });
        } finally {
            clearTimeout(timer);
            setTesting(false);
        }
    };

    const remoteChosen = v?.mode === 'remote';
    const remoteSaved = view?.mode === 'remote';
    const keyTyped = !!v && v.key.trim() !== '';
    // An unreadable stored key is no key: Core cannot send it anywhere.
    const keyAvailable = keyTyped || !!view?.keyFromEnv || (!!view?.keySet && !view?.keyUnreadable);
    // A hint only; Core decides. Saves the confirm-then-refuse round trip.
    const saveBlockedReason = remoteChosen && !keyAvailable ? 'Enter the API key first' : undefined;
    const shownAccount = testedAccount(test, remoteSaved ? view?.account : undefined);
    const outbox = outboxWarning(view?.outbox, Date.now());
    const refused = view?.outbox?.refused ?? [];
    const routingWarning = routingModeWarning(view?.routingModeOk, remoteChosen, remoteSaved);

    return (
        <SettingsCard
            title="Gateway provider"
            icon={Cloud}
            form={form}
            saveBlockedReason={saveBlockedReason}
            description="Run the gateway yourself, or let DYLARIS run it for this platform."
            loadFailedMessage={<>The gateway provider settings could not be loaded{loadError ? `: ${loadError}` : '.'} Nothing has changed.</>}
            actions={form.loadFailed ? (
                <button type="button" onClick={() => void form.reload()} className="btn btn-secondary btn-sm inline-flex items-center gap-1.5">
                    <RefreshCw size={13} /> Retry
                </button>
            ) : undefined}
        >
            {/* The space is held from the first paint so the cards below do not
                jump; the skeleton fills it only once the load is slow. */}
            {form.loading && (
                <div className="space-y-3 min-h-28" aria-busy="true">
                    {slow && (
                        <>
                            <Skeleton className="h-16 w-full" />
                            <Skeleton className="h-9 w-full" />
                        </>
                    )}
                </div>
            )}

            {!form.loading && v && view && (
                <>
                    <div className="grid grid-cols-1 sm:grid-cols-2 gap-2" role="group" aria-label="Gateway provider">
                        {MODES.map(opt => {
                            const on = v.mode === opt.value;
                            return (
                                <button
                                    key={opt.value}
                                    type="button"
                                    aria-pressed={on}
                                    onClick={() => { form.patch({ mode: opt.value }); setTest(null); }}
                                    className={`p-3 rounded-md border text-left transition-colors focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)] ${on ? 'border-(--accent) bg-(--accent)/10' : 'border-(--base-03) bg-(--base-02) hover:border-(--base-05)'}`}
                                >
                                    <div className={`text-sm font-medium ${on ? 'text-(--accent-light)' : 'text-(--base-09)'}`}>{opt.label}</div>
                                    <div className="text-xs text-(--base-06) mt-0.5">{opt.desc}</div>
                                </button>
                            );
                        })}
                    </div>

                    {remoteSaved && view.error && (
                        <div className="alert alert-error text-xs" role="alert">
                            <CircleAlert size={14} className="shrink-0 mt-0.5" />
                            <span>{problemText(view.error)}</span>
                        </div>
                    )}

                    {routingWarning && (
                        <div className="alert alert-warning text-xs" role="status">
                            <CircleAlert size={14} className="shrink-0 mt-0.5" />
                            <span>
                                {routingWarning} The routing mode is under{' '}
                                <a href="#traffic-routing" className="underline rounded-sm hover:text-(--base-09) focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]">
                                    Traffic Routing
                                </a>{' '}
                                below.
                            </span>
                        </div>
                    )}

                    {(outbox || refused.length > 0) && (
                        <div className="alert alert-warning text-xs" role="status">
                            <CircleAlert size={14} className="shrink-0 mt-0.5" />
                            <div className="min-w-0 space-y-1.5">
                                {outbox && (
                                    <p>
                                        {outbox}
                                        {view.outbox?.oldestAt && ` The oldest is from ${fmtDateTime(view.outbox.oldestAt)}.`}
                                    </p>
                                )}
                                {refused.length > 0 && (
                                    <>
                                        <p>The provider refused:</p>
                                        <ul className="list-disc pl-4 space-y-0.5">
                                            {refused.map((it, i) => (
                                                <li key={`${it.op}-${it.domain}-${i}`} className="break-words">{refusalLine(it)}</li>
                                            ))}
                                        </ul>
                                    </>
                                )}
                            </div>
                        </div>
                    )}

                    {remoteChosen && (
                        <>
                            <div>
                                <label className="input-label" htmlFor="gw-provider-url">Provider address</label>
                                {view.urlFromEnv ? (
                                    <input id="gw-provider-url" type="text" disabled value="Set by environment" className="input-field text-sm w-full opacity-60 cursor-not-allowed" />
                                ) : (
                                    <input
                                        id="gw-provider-url"
                                        type="url"
                                        inputMode="url"
                                        spellCheck={false}
                                        value={v.url}
                                        onChange={e => { form.patch({ url: e.target.value }); setTest(null); }}
                                        placeholder="https://..."
                                        className="input-field input-mono text-sm w-full"
                                    />
                                )}
                                {!view.urlFromEnv && (
                                    <p className="text-xs text-(--base-06) mt-1.5">
                                        https://... with the host only. A new address needs the key typed in again.
                                    </p>
                                )}
                            </div>

                            <div>
                                <label className="input-label" htmlFor="gw-provider-key">API key</label>
                                {view.keyFromEnv ? (
                                    <input id="gw-provider-key" type="text" disabled value="Set by environment" className="input-field text-sm w-full opacity-60 cursor-not-allowed" />
                                ) : (
                                    <input
                                        id="gw-provider-key"
                                        type="password"
                                        autoComplete="off"
                                        spellCheck={false}
                                        value={v.key}
                                        onChange={e => { form.patch({ key: e.target.value }); setTest(null); }}
                                        placeholder={view.keyUnreadable ? 'Enter the key again' : view.keySet ? 'Stored. Leave blank to keep it.' : 'Paste the key from your DYLARIS account'}
                                        aria-describedby={view.keyUnreadable ? 'gw-provider-key-unreadable' : undefined}
                                        className="input-field input-mono text-sm w-full"
                                    />
                                )}
                                {view.keyUnreadable && (
                                    <p id="gw-provider-key-unreadable" className="flex items-start gap-1.5 text-xs text-(--warning-light) mt-1.5">
                                        <CircleAlert size={12} className="shrink-0 mt-0.5" />
                                        <span>A key is stored but could not be read. Enter it again.</span>
                                    </p>
                                )}
                            </div>

                            <div className="flex flex-wrap items-center gap-3">
                                <button
                                    type="button"
                                    onClick={() => void runTest()}
                                    disabled={testing || !keyAvailable}
                                    title={keyAvailable ? undefined : 'Enter a key first'}
                                    className="btn btn-secondary btn-sm disabled:opacity-40 inline-flex items-center gap-1.5"
                                >
                                    {testing ? <Loader2 size={13} className="animate-spin motion-reduce:animate-none" /> : <Plug size={13} />}
                                    {testing ? 'Testing…' : 'Test connection'}
                                </button>
                                <span aria-live="polite" className="min-w-0">
                                    {test && (
                                        <span className={`inline-flex items-start gap-1.5 text-xs ${test.ok ? 'text-(--success-light)' : 'text-(--error-light)'}`}>
                                            {test.ok ? <CircleCheck size={13} className="shrink-0 mt-0.5" /> : <CircleAlert size={13} className="shrink-0 mt-0.5" />}
                                            <span>{test.text}</span>
                                        </span>
                                    )}
                                </span>
                            </div>

                            {shownAccount && <AccountSummary account={shownAccount} />}
                        </>
                    )}

                    {form.error && (
                        <div className="alert alert-error text-xs" role="alert">
                            <CircleAlert size={14} className="shrink-0 mt-0.5" />
                            <span>{form.error}</span>
                        </div>
                    )}

                    {remoteSaved && gatewayVisibility.providerUsage && <UsagePanel />}
                    {remoteSaved && gatewayVisibility.managedLinkKit && <OrphanKitsPanel />}
                </>
            )}

        </SettingsCard>
    );
}
