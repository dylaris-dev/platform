'use client';

import { useCallback, useEffect, useState } from 'react';
import { AlertTriangle, Globe, KeyRound, Loader2, Pencil, Plug, Plus, RefreshCw, Trash2, Users, X } from 'lucide-react';
import SettingsCard, { SettingsGroup } from '@/components/settings/SettingsCard';
import SettingsLoadError from '@/components/settings/SettingsLoadError';
import { LimitField, LimitHelp } from '@/components/settings/LimitField';
import { Badge } from '@/components/ui/Badge';
import Select from '@/components/ui/Select';
import ModalPanel from '@/components/ui/ModalPanel';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import { toast } from '@/components/ui/Toast';
import { SkeletonCard } from '@/components/Skeleton';
import { useSettingsForm } from '@/lib/useSettingsForm';
import { getUsers, type User } from '@/lib/api';
import { useAppData } from '@/lib/AppDataContext';
import { listPanelRoles, type PanelRole } from '@/lib/api/panelRoles';
import {
    addManualDnsZone, createDnsAccount, deleteDnsAccount, deleteServerDomain, getDnsAdmin, refreshDnsZones, saveDnsSettings,
    setDnsZoneAccess, testDnsAccount, updateDnsAccount,
    type DnsAccount, type DnsAdmin, type DnsKind, type DnsZone, type DnsZoneDomain,
} from '@/lib/api/serverDomains';
import {
    credentialFieldsFor, formatLimit, intervalError, orphanMessage, parseLimit, principalOf, splitPrincipal,
    MIN_INTERVAL_SECONDS, type PrincipalKind,
} from '@/components/settings/dnsTabHelpers';

// A spinner for work that usually finishes at once is flicker. Only show it
// once the call has been running for the delay.
function useSlow(active: boolean, delayMs = 400): boolean {
    const [slow, setSlow] = useState(false);
    useEffect(() => {
        if (!active) { setSlow(false); return; }
        const t = setTimeout(() => setSlow(true), delayMs);
        return () => clearTimeout(t);
    }, [active, delayMs]);
    return slow;
}

export default function DnsTab() {
    const { routingMode } = useAppData();
    const [data, setData] = useState<DnsAdmin | null>(null);
    const [failed, setFailed] = useState('');
    const [account, setAccount] = useState<DnsAccount | 'new' | null>(null);
    const [accessZone, setAccessZone] = useState<DnsZone | null>(null);
    const [manualFor, setManualFor] = useState<DnsAccount | null>(null);
    const [orphans, setOrphans] = useState<string[]>([]);

    const load = useCallback(async () => {
        const res = await getDnsAdmin();
        if (!res.success) { setFailed(res.message || 'Core did not answer.'); return; }
        setFailed('');
        setData({ ...res, kinds: res.kinds ?? [], accounts: res.accounts ?? [] });
    }, []);
    useEffect(() => { void load(); }, [load]);

    if (failed) return <SettingsLoadError what="the DNS settings" onRetry={() => { setFailed(''); void load(); }} />;
    if (!data) return <div className="max-w-3xl space-y-6"><SkeletonCard height="h-32" /><SkeletonCard height="h-24" /></div>;

    if (!data.enabled) {
        return (
            <div className="max-w-3xl">
                <SettingsCard title="DNS updater (direct mode)" icon={Globe}
                    description="Lets servers get their own domain, kept pointed at the node by Core.">
                    <p className="text-xs text-(--base-06) px-3 py-3 rounded-md bg-(--base-02)">
                        {routingMode === 'gateway'
                            ? 'The DNS updater is unavailable while game traffic is routed through the gateway. It works with direct routing (IP and port).'
                            : 'The DNS updater works with direct routing. Turn it on under Features. Nothing is written to any DNS provider until then.'}
                    </p>
                </SettingsCard>
            </div>
        );
    }

    const deleteAccount = async (a: DnsAccount) => {
        const names = a.zones.flatMap(z => z.domains.map(d => d.fqdn));
        const ok = await confirmDialog({
            title: `Delete account "${a.name}"?`,
            message: names.length === 0
                ? 'No server uses this account. Its stored credentials are erased.'
                : `Core removes the records it wrote for these domains, then removes the domains from their servers:\n\n${names.join('\n')}\n\nA record changed by hand at the provider is left alone.`,
            confirmLabel: 'Delete account',
        });
        if (!ok) return;
        const res = await deleteDnsAccount(a.id);
        if (!res.success) { toast(res.message || 'The account could not be deleted.', false); return; }
        toast('Account deleted');
        setOrphans(res.orphanedNames ?? []);
        void load();
    };

    return (
        <div className="max-w-3xl space-y-6">
            {routingMode === 'gateway' && (
                <div role="status" className="flex items-start gap-2 text-xs text-(--warning-light) px-3 py-3 rounded-md bg-(--warning-ghost)">
                    <AlertTriangle size={14} className="shrink-0 mt-0.5" />
                    <div className="flex-1">
                        Game traffic is routed through the gateway, so the DNS updater does nothing right now: Core writes,
                        changes and checks no record, and every domain shows as paused. Existing records stay as they are.
                        It resumes once the routing mode is direct (IP and port) or both.
                    </div>
                </div>
            )}
            {orphans.length > 0 && (
                <div role="alert" className="flex items-start gap-2 text-xs text-(--warning-light) px-3 py-3 rounded-md bg-(--warning-ghost)">
                    <AlertTriangle size={14} className="shrink-0 mt-0.5" />
                    <div className="flex-1 break-words">{orphanMessage(orphans)}</div>
                    <button type="button" onClick={() => setOrphans([])} aria-label="Dismiss warning"
                        className="p-1 rounded text-(--base-06) hover:text-(--base-09) focus-visible:outline-2">
                        <X size={13} />
                    </button>
                </div>
            )}

            <SettingsCard
                title="DNS accounts"
                icon={Globe}
                description="Provider credentials Core uses to write A and SRV records for server domains. Core never changes a record it did not create."
                actions={
                    <button type="button" onClick={() => setAccount('new')} className="btn btn-secondary btn-sm inline-flex items-center gap-1.5">
                        <Plus size={13} /> Add account
                    </button>
                }
            >
                {data.accounts.length === 0 && (
                    <p className="text-xs text-(--base-06) px-3 py-3 rounded-md bg-(--base-02)">
                        No account yet. Add one to let users give their servers a subdomain.
                    </p>
                )}
                {data.accounts.map(a => (
                    <AccountRow key={a.id} account={a} kind={data.kinds.find(k => k.id === a.provider)}
                        onEdit={() => setAccount(a)} onDelete={() => deleteAccount(a)}
                        onAccess={setAccessZone} onManual={() => setManualFor(a)} onChanged={load} />
                ))}
            </SettingsCard>

            <DnsSettingsCard />

            {account && (
                <AccountModal kinds={data.kinds} account={account === 'new' ? null : account}
                    onClose={() => setAccount(null)}
                    onSaved={() => { setAccount(null); void load(); }} />
            )}
            {accessZone && (
                <ZoneAccessModal zone={accessZone} onClose={() => setAccessZone(null)}
                    onSaved={() => { setAccessZone(null); void load(); }} />
            )}
            {manualFor && (
                <ManualZoneModal account={manualFor} onClose={() => setManualFor(null)}
                    onSaved={() => { setManualFor(null); void load(); }} />
            )}
        </div>
    );
}

function AccountRow({ account: a, kind, onEdit, onDelete, onAccess, onManual, onChanged }: {
    account: DnsAccount; kind?: DnsKind;
    onEdit: () => void; onDelete: () => Promise<void>; onAccess: (z: DnsZone) => void; onManual: () => void; onChanged: () => void;
}) {
    const [testing, setTesting] = useState(false);
    const [refreshing, setRefreshing] = useState(false);
    const [result, setResult] = useState<{ ok: boolean; message: string } | null>(null);
    const slowTest = useSlow(testing);
    const slowRefresh = useSlow(refreshing);
    const [deleting, setDeleting] = useState(false);
    const remove = async () => { setDeleting(true); try { await onDelete(); } finally { setDeleting(false); } };
    const canList = kind?.zoneListing !== false;

    const test = async () => {
        setTesting(true); setResult(null);
        const res = await testDnsAccount(a.id);
        setTesting(false);
        setResult(res.success
            ? { ok: true, message: 'Connection works.' }
            : { ok: false, message: res.message || 'The provider did not accept the credentials.' });
    };
    const refresh = async () => {
        setRefreshing(true);
        const res = await refreshDnsZones(a.id);
        setRefreshing(false);
        if (!res.success) { toast(res.message || 'The zones could not be refreshed.', false); return; }
        toast('Zones refreshed');
        onChanged();
    };

    return (
        <div className="rounded-md border border-(--base-03) bg-(--base-02) p-3 space-y-3">
            <div className="flex flex-wrap items-center justify-between gap-2">
                <div className="min-w-0">
                    <div className="text-sm text-(--base-09) flex items-center gap-2 flex-wrap">
                        <span className="break-all">{a.name}</span>
                        <Badge>{kind?.label ?? a.provider}</Badge>
                        <Badge variant={a.hasCredentials ? 'success' : 'warning'} icon={<KeyRound size={10} />}>
                            Credentials {a.hasCredentials ? 'set' : 'not set'}
                        </Badge>
                    </div>
                    {a.lastError && <div className="text-xs text-(--error-light) mt-1 break-words">{a.lastError}</div>}
                </div>
                <div className="flex items-center gap-1.5">
                    <button type="button" onClick={test} disabled={testing} className="btn btn-secondary btn-sm inline-flex items-center gap-1.5 disabled:opacity-40">
                        {slowTest ? <Loader2 size={13} className="animate-spin" /> : <Plug size={13} />} Test connection
                    </button>
                    <button type="button" onClick={onEdit} aria-label={`Edit ${a.name}`} className="btn btn-secondary btn-sm"><Pencil size={13} /></button>
                    <button type="button" onClick={remove} disabled={deleting} aria-label={`Delete ${a.name}`} className="btn btn-secondary btn-sm hover:text-(--error-light) disabled:opacity-40"><Trash2 size={13} /></button>
                </div>
            </div>
            {result && <div role="status" className={`text-xs ${result.ok ? 'text-(--success-light)' : 'text-(--error-light)'}`}>{result.message}</div>}

            <div className="space-y-2">
                <div className="flex items-center justify-between gap-2">
                    <h4 className="mono-label">Zones</h4>
                    <div className="flex gap-1.5">
                        {canList && (
                            <button type="button" onClick={refresh} disabled={refreshing} className="btn btn-secondary btn-sm inline-flex items-center gap-1.5 disabled:opacity-40">
                                {slowRefresh ? <Loader2 size={13} className="animate-spin" /> : <RefreshCw size={13} />} Refresh zones
                            </button>
                        )}
                        {/* A provider that can list zones may still miss one the
                            token is scoped away from, so manual add stays. */}
                        <button type="button" onClick={onManual} className="btn btn-secondary btn-sm inline-flex items-center gap-1.5">
                            <Plus size={13} /> Add zone manually
                        </button>
                    </div>
                </div>
                {!canList && (
                    <p className="text-xs text-(--base-06)">This provider cannot list zones. Add each zone by name.</p>
                )}
                {a.zones.length === 0 && <p className="text-xs text-(--base-06)">No zones yet.</p>}
                {a.zones.map(z => <ZoneBlock key={z.id} zone={z} onAccess={() => onAccess(z)} onChanged={onChanged} />)}
            </div>
        </div>
    );
}

function ZoneBlock({ zone: z, onAccess, onChanged }: { zone: DnsZone; onAccess: () => void; onChanged: () => void }) {
    const [removing, setRemoving] = useState<number | null>(null);
    const removeDomain = async (d: DnsZoneDomain) => {
        const ok = await confirmDialog({
            title: 'Remove domain?',
            message: `Remove ${d.fqdn} from ${d.serverName}? Core deletes the records it created for it.`,
            confirmLabel: 'Remove domain',
        });
        if (!ok) return;
        setRemoving(d.id);
        const res = await deleteServerDomain(String(d.serverId), d.id);
        setRemoving(null);
        if (!res.success) { toast(res.message || 'The domain could not be removed.', false); return; }
        toast('Domain removed');
        onChanged();
    };
    return (
        <div className="rounded-md bg-(--base-01) border border-(--base-03) px-3 py-2 space-y-1.5">
            <div className="flex flex-wrap items-center justify-between gap-2">
                <div className="font-mono text-xs text-(--base-09) break-all">
                    {z.name} {z.manual && <Badge className="ml-1">manual</Badge>}
                    {!z.listed && <Badge variant="warning" className="ml-1">not listed by the provider</Badge>}
                </div>
                <button type="button" onClick={onAccess} className="btn btn-secondary btn-sm inline-flex items-center gap-1.5">
                    <Users size={13} /> Access ({z.access.length})
                </button>
            </div>
            {!z.listed && (
                <div className="text-[11px] text-(--base-06)">
                    The last zone refresh did not list it. It is kept because domains or Core&apos;s records use it, and those
                    keep working; no new subdomain can go in. Add it manually to keep it for good.
                </div>
            )}
            {z.access.length === 0 && (
                <div className="text-[11px] text-(--base-06)">No one may use this zone yet. Admins always can.</div>
            )}
            {z.domains.map(d => (
                <div key={d.fqdn} className="flex flex-wrap items-center justify-between gap-2 text-xs">
                    <div className="min-w-0">
                        <span className="font-mono text-(--base-09) break-all">{d.fqdn}</span>
                        <span className="text-(--base-06)"> {d.serverName} by {d.createdByName || d.createdBy}</span>
                    </div>
                    {d.accessLost && (
                        <div className="flex items-center gap-2">
                            <Badge variant="warning">creator lost access</Badge>
                            <button type="button" onClick={() => removeDomain(d)} disabled={removing === d.id}
                                aria-label={`Remove ${d.fqdn} from ${d.serverName}`}
                                className="btn btn-secondary btn-sm hover:text-(--error-light) disabled:opacity-40 inline-flex items-center gap-1.5">
                                {removing === d.id ? <Loader2 size={13} className="animate-spin" /> : <Trash2 size={13} />} Remove
                            </button>
                        </div>
                    )}
                </div>
            ))}
        </div>
    );
}

function DnsSettingsCard() {
    interface F { interval: string; perUser: number | null; perServer: number | null }
    const form = useSettingsForm<F>({
        load: async () => {
            const r = await getDnsAdmin();
            if (!r.success) return null;
            return { interval: String(r.intervalSeconds), perUser: parseLimit(r.limits?.perUser ?? ''), perServer: parseLimit(r.limits?.perServer ?? '') };
        },
        save: async v => {
            const res = await saveDnsSettings({
                intervalSeconds: Number(v.interval), perUser: formatLimit(v.perUser), perServer: formatLimit(v.perServer),
            });
            return { ok: !!res.success, message: res.message };
        },
        successMessage: 'DNS settings saved',
    });
    const slowLoad = useSlow(form.loading);
    const v = form.value;
    const err = v ? intervalError(v.interval) : '';

    return (
        <SettingsCard title="Reconcile and limits" icon={Globe} form={form}
            saveBlockedReason={err || undefined}
            description="How often Core checks records, and how many subdomains one owner and one server may hold.">
            {slowLoad && <Loader2 size={14} className="animate-spin text-(--base-06)" />}
            {v && (
                <>
                    <SettingsGroup first title="Interval">
                        <label className="input-label" htmlFor="dns-interval">Check every (seconds)</label>
                        <input id="dns-interval" type="number" min={MIN_INTERVAL_SECONDS} step={1} value={v.interval}
                            aria-invalid={!!err} aria-describedby="dns-interval-hint"
                            onChange={e => form.patch({ interval: e.target.value })}
                            className="input-field input-mono w-32" />
                        <p id="dns-interval-hint" className={`text-xs mt-1.5 ${err ? 'text-(--error-light)' : 'text-(--base-06)'}`}>
                            {err || 'Recommended: 30 seconds'}
                        </p>
                    </SettingsGroup>
                    <SettingsGroup title="Limits" help={LimitHelp}>
                        <div className="flex items-center justify-between gap-4">
                            <div className="text-sm text-(--base-09)">Subdomains per user</div>
                            <LimitField value={v.perUser} onChange={n => form.patch({ perUser: n })} />
                        </div>
                        <div className="flex items-center justify-between gap-4 mt-3">
                            <div className="text-sm text-(--base-09)">Subdomains per server</div>
                            <LimitField value={v.perServer} onChange={n => form.patch({ perServer: n })} />
                        </div>
                    </SettingsGroup>
                </>
            )}
        </SettingsCard>
    );
}

function AccountModal({ kinds, account, onClose, onSaved }: {
    kinds: DnsKind[]; account: DnsAccount | null; onClose: () => void; onSaved: () => void;
}) {
    const editing = account !== null;
    const [name, setName] = useState(account?.name ?? '');
    const [provider, setProvider] = useState(account?.provider ?? '');
    // Never prefilled: Core does not send credentials back.
    const [values, setValues] = useState<Record<string, string>>({});
    const [saving, setSaving] = useState(false);
    const [error, setError] = useState('');
    const slow = useSlow(saving);
    const fields = credentialFieldsFor(kinds, provider);

    const missing = fields.filter(f => !f.optional && !editing && !(values[f.key] ?? '').trim()).map(f => f.label);
    const blocked = !name.trim() || !provider || missing.length > 0;

    const save = async () => {
        setSaving(true); setError('');
        const body = {
            name: name.trim(), provider,
            credentials: Object.fromEntries(Object.entries(values).map(([k, x]) => [k, x.trim()])),
        };
        const res = editing ? await updateDnsAccount(account.id, body) : await createDnsAccount(body);
        setSaving(false);
        if (!res.success) { setError(res.message || 'The account could not be saved.'); return; }
        toast(editing ? 'Account updated' : 'Account added');
        onSaved();
    };

    return (
        <div className="modal-overlay animate-fade-in" onClick={onClose}>
            <ModalPanel onClose={onClose} className="modal-panel w-full max-w-lg" onClick={e => e.stopPropagation()}>
                <div className="modal-header flex items-center justify-between">
                    <h3 className="modal-title">{editing ? 'Edit DNS account' : 'Add DNS account'}</h3>
                    <button type="button" onClick={onClose} aria-label="Close" className="p-1 rounded text-(--base-06) hover:text-(--base-09) focus-visible:outline-2 focus-visible:outline-(--accent-light)"><X size={16} /></button>
                </div>
                <div className="modal-body space-y-4">
                    <div className="form-group">
                        <label className="input-label" htmlFor="dns-acc-name">Name</label>
                        <input id="dns-acc-name" type="text" value={name} onChange={e => setName(e.target.value)} className="input-field" placeholder="Main Cloudflare" />
                    </div>
                    <div className="form-group">
                        <label className="input-label">Provider</label>
                        <Select ariaLabel="Provider" value={provider} disabled={editing}
                            onChange={p => { setProvider(p); setValues({}); }}
                            options={kinds.map(k => ({ value: k.id, label: k.label }))} placeholder="Choose a provider" />
                        {editing && <p className="text-xs text-(--base-06) mt-1.5">The provider cannot be changed. Delete the account and add a new one.</p>}
                    </div>
                    {fields.map(f => (
                        <div className="form-group" key={f.key}>
                            <label className="input-label flex items-center gap-2" htmlFor={`dns-f-${f.key}`}>
                                {f.label}{f.optional && <span className="text-(--base-06)">(optional)</span>}
                                {f.secret && <Badge variant={editing && account.hasCredentials ? 'success' : 'neutral'}>{editing && account.hasCredentials ? 'Set' : 'Not set'}</Badge>}
                            </label>
                            <input id={`dns-f-${f.key}`} type={f.secret ? 'password' : 'text'} autoComplete="off"
                                value={values[f.key] ?? ''} onChange={e => setValues(prev => ({ ...prev, [f.key]: e.target.value }))}
                                placeholder={editing ? 'leave blank to keep the stored value' : ''}
                                className="input-field input-mono" />
                        </div>
                    ))}
                    {error && <p role="alert" className="text-xs text-(--error-light) break-words">{error}</p>}
                </div>
                <div className="modal-footer">
                    <button type="button" onClick={onClose} className="btn btn-secondary">Cancel</button>
                    <button type="button" onClick={save} disabled={blocked || saving}
                        title={missing.length ? `Fill in: ${missing.join(', ')}` : undefined}
                        className="btn btn-primary disabled:opacity-40 inline-flex items-center gap-1.5">
                        {slow && <Loader2 size={13} className="animate-spin" />} {editing ? 'Save' : 'Add account'}
                    </button>
                </div>
            </ModalPanel>
        </div>
    );
}

function ManualZoneModal({ account, onClose, onSaved }: { account: DnsAccount; onClose: () => void; onSaved: () => void }) {
    const [zone, setZone] = useState('');
    const [saving, setSaving] = useState(false);
    const [error, setError] = useState('');
    const slow = useSlow(saving);
    const clean = zone.trim().toLowerCase().replace(/\.$/, '');

    const save = async () => {
        setSaving(true); setError('');
        const res = await addManualDnsZone(account.id, clean);
        setSaving(false);
        if (!res.success) { setError(res.message || 'The zone could not be added.'); return; }
        toast('Zone added');
        onSaved();
    };

    return (
        <div className="modal-overlay animate-fade-in" onClick={onClose}>
            <ModalPanel onClose={onClose} className="modal-panel w-full max-w-md" onClick={e => e.stopPropagation()}>
                <div className="modal-header flex items-center justify-between">
                    <h3 className="modal-title">Add zone to {account.name}</h3>
                    <button type="button" onClick={onClose} aria-label="Close" className="p-1 rounded text-(--base-06) hover:text-(--base-09) focus-visible:outline-2 focus-visible:outline-(--accent-light)"><X size={16} /></button>
                </div>
                <div className="modal-body space-y-3">
                    <label className="input-label" htmlFor="dns-zone-name">Zone name</label>
                    <input id="dns-zone-name" type="text" value={zone} onChange={e => setZone(e.target.value)}
                        onKeyDown={e => { if (e.key === 'Enter' && clean && !saving) void save(); }}
                        className="input-field input-mono" placeholder="example.com" />
                    {error && <p role="alert" className="text-xs text-(--error-light) break-words">{error}</p>}
                </div>
                <div className="modal-footer">
                    <button type="button" onClick={onClose} className="btn btn-secondary">Cancel</button>
                    <button type="button" onClick={save} disabled={!clean || saving} className="btn btn-primary disabled:opacity-40 inline-flex items-center gap-1.5">
                        {slow && <Loader2 size={13} className="animate-spin" />} Add zone
                    </button>
                </div>
            </ModalPanel>
        </div>
    );
}

function ZoneAccessModal({ zone, onClose, onSaved }: { zone: DnsZone; onClose: () => void; onSaved: () => void }) {
    const [principals, setPrincipals] = useState<string[]>(zone.access);
    const [users, setUsers] = useState<User[]>([]);
    const [roles, setRoles] = useState<PanelRole[]>([]);
    const [kind, setKind] = useState<PrincipalKind>('user');
    const [pick, setPick] = useState('');
    const [saving, setSaving] = useState(false);
    const [error, setError] = useState('');
    const slow = useSlow(saving);

    useEffect(() => {
        void getUsers().then(r => { if (r?.success && r.users) setUsers(r.users); });
        void listPanelRoles().then(r => { if (r.success && r.roles) setRoles(r.roles); });
    }, []);

    const label = (p: string) => {
        const s = splitPrincipal(p);
        if (!s) return p;
        if (s.kind === 'user') return `User: ${users.find(u => String(u.id) === s.id)?.username ?? s.id}`;
        return `Role: ${roles.find(r => String(r.id) === s.id)?.name ?? s.id}`;
    };
    const options = (kind === 'user'
        ? users.map(u => ({ value: String(u.id), label: u.username }))
        : roles.map(r => ({ value: String(r.id), label: r.name })))
        .filter(o => !principals.includes(principalOf(kind, o.value)));

    const add = () => {
        if (!pick) return;
        setPrincipals(prev => [...prev, principalOf(kind, pick)]);
        setPick('');
    };
    const save = async () => {
        setSaving(true); setError('');
        const res = await setDnsZoneAccess(zone.id, principals);
        setSaving(false);
        if (!res.success) { setError(res.message || 'The access list could not be saved.'); return; }
        toast('Zone access saved');
        onSaved();
    };

    return (
        <div className="modal-overlay animate-fade-in" onClick={onClose}>
            <ModalPanel onClose={onClose} className="modal-panel w-full max-w-lg" onClick={e => e.stopPropagation()}>
                <div className="modal-header flex items-center justify-between">
                    <h3 className="modal-title">Who may use {zone.name}</h3>
                    <button type="button" onClick={onClose} aria-label="Close" className="p-1 rounded text-(--base-06) hover:text-(--base-09) focus-visible:outline-2 focus-visible:outline-(--accent-light)"><X size={16} /></button>
                </div>
                <div className="modal-body space-y-4">
                    <p className="text-xs text-(--base-06)">
                        Listed users and roles can add subdomains in this zone. Removing someone keeps the
                        domains they already made and only blocks new ones.
                    </p>
                    {principals.length === 0 ? (
                        <p className="text-xs text-(--base-06) px-3 py-2 rounded-md bg-(--base-02)">No one yet. Only admins can use this zone.</p>
                    ) : (
                        <ul className="space-y-1.5">
                            {principals.map(p => (
                                <li key={p} className="flex items-center justify-between gap-2 px-3 py-1.5 rounded-md bg-(--base-02) text-sm text-(--base-09)">
                                    <span className="break-all">{label(p)}</span>
                                    <button type="button" aria-label={`Remove ${label(p)}`}
                                        onClick={() => setPrincipals(prev => prev.filter(x => x !== p))}
                                        className="p-1 rounded text-(--base-06) hover:text-(--error-light)"><X size={13} /></button>
                                </li>
                            ))}
                        </ul>
                    )}
                    <div className="flex flex-wrap items-center gap-2">
                        <div className="w-28">
                            <Select ariaLabel="Principal type" value={kind} onChange={k => { setKind(k as PrincipalKind); setPick(''); }}
                                options={[{ value: 'user', label: 'User' }, { value: 'role', label: 'Role' }]} />
                        </div>
                        <div className="flex-1 min-w-40">
                            <Select ariaLabel={kind === 'user' ? 'User' : 'Role'} searchable value={pick} onChange={setPick}
                                options={options} placeholder={kind === 'user' ? 'Choose a user' : 'Choose a role'} />
                        </div>
                        <button type="button" onClick={add} disabled={!pick} className="btn btn-secondary btn-sm inline-flex items-center gap-1.5 disabled:opacity-40">
                            <Plus size={13} /> Add
                        </button>
                    </div>
                    {error && <p role="alert" className="text-xs text-(--error-light) break-words">{error}</p>}
                </div>
                <div className="modal-footer">
                    <button type="button" onClick={onClose} className="btn btn-secondary">Cancel</button>
                    <button type="button" onClick={save} disabled={saving} className="btn btn-primary disabled:opacity-40 inline-flex items-center gap-1.5">
                        {slow && <Loader2 size={13} className="animate-spin" />} Save access
                    </button>
                </div>
            </ModalPanel>
        </div>
    );
}
