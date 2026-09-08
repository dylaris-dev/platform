'use client';

import { useState } from 'react';
import { Globe, CircleAlert, Loader2, Plug, Plus, X } from 'lucide-react';
import {
    getGatewayDns,
    saveGatewayDns,
    probeGatewayDns,
    GatewayDnsConfig,
    GatewayDnsProbe,
} from '@/lib/api/gatewayDns';
import { useSettingsForm } from '@/lib/useSettingsForm';
import { CONN_TEST_TIMEOUT_MS } from '@/lib/connectionTest';
import SettingsCard from '@/components/settings/SettingsCard';
import Switch from '@/components/ui/Switch';
import HelpTip from '@/components/ui/HelpTip';

// The DNS credential lives in the gateway Hub. This card is a form over it, not
// a second copy: Core forwards what is typed here and stores nothing, so there
// is one credential and one writer of records.

// The form's own shape. The stored config plus the two write-only fields, so the
// dirty compare is against something stable.
interface DnsForm {
    provider: string;
    token: string;
    zones: string[];
    enabled: boolean;
    acmeEnabled: boolean;
    acmeEmail: string;
    acmeDirectory: string;
    acmeAgreed: boolean;
}

function formOf(c: GatewayDnsConfig): DnsForm {
    return {
        provider: c.provider,
        // Never repopulated from the server, because the server never sends it.
        token: '',
        zones: [...c.zones],
        enabled: c.enabled,
        acmeEnabled: c.acme_enabled,
        acmeEmail: c.acme_email,
        acmeDirectory: c.acme_directory,
        acmeAgreed: c.acme_agreed,
    };
}

export default function GatewayDnsCard() {
    const [available, setAvailable] = useState(true);
    const [meta, setMeta] = useState<GatewayDnsConfig | null>(null);
    const [probe, setProbe] = useState<GatewayDnsProbe | null>(null);
    const [probing, setProbing] = useState(false);
    const [newZone, setNewZone] = useState('');

    const form = useSettingsForm<DnsForm>({
        load: async () => {
            const res = await getGatewayDns();
            if (!res.success) return null;
            if (res.available === false) {
                setAvailable(false);
                // Not a load failure - there is genuinely nothing to configure.
                // Returning a blank form keeps the card in a clean state behind
                // the "no gateway" message below.
                return formOf({
                    provider: '', zones: [], enabled: false, has_token: false, env_locked: false,
                    providers: [], acme_enabled: false, acme_email: '', acme_directory: '', acme_agreed: false,
                });
            }
            setAvailable(true);
            if (!res.config) return null;
            setMeta(res.config);
            return formOf(res.config);
        },
        save: async v => {
            const res = await saveGatewayDns({
                provider: v.provider,
                token: v.token,
                zones: v.zones,
                enabled: v.enabled,
                acme_enabled: v.acmeEnabled,
                acme_email: v.acmeEmail,
                acme_directory: v.acmeDirectory,
                acme_agreed: v.acmeAgreed,
            });
            if (res.config) setMeta(res.config);
            return {
                ok: !!res.success,
                message: res.message,
                value: res.config ? formOf(res.config) : undefined,
            };
        },
        successMessage: 'Gateway DNS settings saved',
    });

    const v = form.value;

    // The probe keeps its own state because its answer is more than a verdict:
    // the zone list below is built from it. What it did not have is a deadline
    // - this call crosses Core, the gateway and then the provider's API, and a
    // token that hangs one of them used to leave the button spinning for as
    // long as the tab stayed open.
    const runProbe = async () => {
        if (!v) return;
        setProbing(true);
        setProbe(null);
        const controller = new AbortController();
        const timer = setTimeout(() => controller.abort(), CONN_TEST_TIMEOUT_MS);
        try {
            const res = await probeGatewayDns(v.provider, v.token, controller.signal);
            if (!res.success || !res.probe) {
                setProbe({ ok: false, zone_listing: false, message: res.message || 'Could not reach the gateway.' });
                return;
            }
            setProbe(res.probe);
        } catch {
            setProbe({
                ok: false,
                zone_listing: false,
                message: `No answer within ${Math.round(CONN_TEST_TIMEOUT_MS / 1000)} seconds, so the test was ` +
                    'stopped. The call goes through the gateway to the provider, so any of the three could be ' +
                    'the one not answering.',
            });
        } finally {
            clearTimeout(timer);
            setProbing(false);
        }
    };

    const toggleZone = (zone: string, on: boolean) => {
        if (!v) return;
        form.patch({ zones: on ? [...v.zones, zone] : v.zones.filter(z => z !== zone) });
    };

    const addZone = () => {
        const z = newZone.trim().toLowerCase().replace(/\.$/, '');
        if (!z || !v) return;
        if (v.zones.includes(z)) { setNewZone(''); return; }
        form.patch({ zones: [...v.zones, z] });
        setNewZone('');
    };

    // Every zone worth showing a row for: what the credential can see, plus
    // anything already saved that it cannot. A saved zone the token no longer
    // covers is the case worth surfacing - it is why records stop being written.
    const detected = probe?.zone_listing ? (probe.zones ?? []) : [];
    const rows = Array.from(new Set([...detected, ...(v?.zones ?? [])])).sort();

    return (
        <SettingsCard
            title="Automatic DNS and certificates"
            icon={Globe}
            form={form}
            description="One provider token: the gateway keeps the edge and beam records pointed at whatever is online, and gets the beam relay its TLS certificate."
        >
            {form.loading && (
                <div className="flex items-center gap-2 text-xs text-(--base-06)">
                    <Loader2 size={14} className="animate-spin" />
                    Reading the gateway&apos;s settings…
                </div>
            )}

            {!form.loading && !available && (
                <p className="text-xs text-(--base-05) italic px-3 py-3 rounded-md bg-(--base-02)">
                    No gateway is connected to this platform, so there are no records to write.
                    Nodes need none at all, and the panel and API are yours to point at your reverse
                    proxy. Set <code className="font-mono">GATEWAY_HUB_URL</code> on Core if you run
                    the gateway.
                </p>
            )}

            {!form.loading && available && v && (
                <>
                    <p className="text-xs text-(--base-06)">
                        The credential is stored by the gateway, not here — this platform keeps no
                        copy of it. The same token also gets the beam relay its TLS certificate.
                    </p>

                    {meta?.env_locked && (
                        <div className="flex items-start gap-2 text-xs text-(--base-07) px-3 py-2 rounded-md bg-(--base-02)">
                            <CircleAlert size={14} className="shrink-0 mt-0.5 text-(--base-06)" />
                            <span>
                                The gateway has a credential mounted as an environment variable,
                                which wins over anything saved here.
                            </span>
                        </div>
                    )}

                    <div>
                        <label className="input-label">Provider</label>
                        <select
                            value={v.provider}
                            onChange={e => { form.patch({ provider: e.target.value }); setProbe(null); }}
                            className="input-field text-sm w-full"
                        >
                            <option value="">— none —</option>
                            {(meta?.providers ?? []).map(p => (
                                <option key={p.name} value={p.name}>{p.label}</option>
                            ))}
                        </select>
                    </div>

                    <div>
                        <label className="input-label">API token</label>
                        <input
                            type="password"
                            autoComplete="off"
                            value={v.token}
                            onChange={e => { form.patch({ token: e.target.value }); setProbe(null); }}
                            placeholder={meta?.has_token ? 'stored — leave blank to keep' : 'paste the token'}
                            className="input-field input-mono text-sm w-full"
                        />
                        <p className="text-xs text-(--base-06) mt-1.5">
                            Write-only. Leaving it blank keeps the stored one, so editing a zone does
                            not quietly erase the credential. A provider that needs more than one
                            value takes a JSON object here.
                        </p>
                    </div>

                    {/* Test before save. The credential is the one part of this
                        configuration that cannot be checked by reading it back:
                        it used to be saved, switched on, and left to a
                        reconciler tick that either wrote something or logged a
                        failure nobody was watching. */}
                    <div className="flex flex-wrap items-center gap-3">
                        <button
                            type="button"
                            onClick={runProbe}
                            disabled={probing || !v.provider}
                            className="btn btn-secondary btn-sm disabled:opacity-40 inline-flex items-center gap-1.5"
                        >
                            {probing ? <Loader2 size={13} className="animate-spin" /> : <Plug size={13} />}
                            {probing ? 'Testing…' : 'Test & detect zones'}
                        </button>
                        {probe && (
                            <span className={`text-xs ${probe.ok ? 'text-(--success-light)' : 'text-(--error-light)'}`}>
                                {probe.ok ? '' : 'Reached, but refused: '}{probe.message}
                            </span>
                        )}
                    </div>

                    <div>
                        <label className="input-label flex items-center gap-1.5">
                            Zones
                            <HelpTip label="About zones">
                                <p className="mb-2">
                                    The boundary of what this token is allowed to touch. A name
                                    outside every zone here is never written, whatever else is
                                    configured.
                                </p>
                                <p className="mb-2">
                                    Test above to fill the list from the credential itself. Providers
                                    that cannot list zones need them typed in — that is a property of
                                    the provider, not a problem with your token.
                                </p>
                                <p>
                                    A zone switched off is left completely alone: the reconciler
                                    neither writes nor deletes inside it.
                                </p>
                            </HelpTip>
                        </label>

                        {rows.length === 0 ? (
                            <p className="text-xs text-(--base-06) px-3 py-2.5 rounded-md bg-(--base-02)">
                                No zones yet. Test the credential above to list them, or add one by hand.
                            </p>
                        ) : (
                            <div className="space-y-1.5">
                                {rows.map(zone => {
                                    const on = v.zones.includes(zone);
                                    const unseen = detected.length > 0 && !detected.includes(zone);
                                    return (
                                        <div key={zone} className="flex items-center justify-between gap-3 px-3 py-2 rounded-md bg-(--base-02) border border-(--base-03)">
                                            <div className="min-w-0">
                                                <div className="font-mono text-xs text-(--base-09) break-all">{zone}</div>
                                                {unseen && (
                                                    <div className="text-[11px] text-(--warning-light) mt-0.5">
                                                        The token cannot see this zone, so nothing in it can be written.
                                                    </div>
                                                )}
                                            </div>
                                            <div className="flex items-center gap-1.5 shrink-0">
                                                <Switch checked={on} onChange={o => toggleZone(zone, o)} ariaLabel={`Manage ${zone}`} />
                                                {!detected.includes(zone) && (
                                                    <button
                                                        type="button"
                                                        onClick={() => form.patch({ zones: v.zones.filter(z => z !== zone) })}
                                                        aria-label={`Remove ${zone}`}
                                                        className="p-1 rounded text-(--base-06) hover:text-(--error-light)"
                                                    >
                                                        <X size={13} />
                                                    </button>
                                                )}
                                            </div>
                                        </div>
                                    );
                                })}
                            </div>
                        )}

                        <div className="flex gap-2 mt-2">
                            <input
                                type="text"
                                value={newZone}
                                onChange={e => setNewZone(e.target.value)}
                                onKeyDown={e => { if (e.key === 'Enter') { e.preventDefault(); addZone(); } }}
                                placeholder="example.com"
                                className="input-field input-mono text-sm flex-1"
                            />
                            <button
                                type="button"
                                onClick={addZone}
                                disabled={!newZone.trim()}
                                className="btn btn-secondary btn-sm disabled:opacity-40 inline-flex items-center gap-1.5"
                            >
                                <Plus size={13} /> Add
                            </button>
                        </div>
                    </div>

                    <div className="flex items-center justify-between gap-4">
                        <div>
                            <div className="text-sm text-(--base-09)">Write records automatically</div>
                            <div className="text-xs text-(--base-06) mt-0.5">
                                The reconciler keeps the switched-on zones matching what is online, and
                                deletes what it did not plan.
                            </div>
                        </div>
                        <Switch checked={v.enabled} onChange={o => form.patch({ enabled: o })} ariaLabel="Write records automatically" />
                    </div>

                    <div className="pt-4 border-t border-(--base-03) space-y-4">
                        <div>
                            <h3 className="mono-label">Certificates</h3>
                            <p className="text-xs text-(--base-06) mt-1.5">
                                The beam relay&apos;s client port is the one listener that needs a
                                publicly trusted certificate — the Beam app checks its chain and
                                hostname. It is obtained with the same token above, over the DNS-01
                                challenge, and renewed without a restart. Leave this off if you mount
                                a certificate on the relay yourself; that always wins.
                            </p>
                        </div>

                        <div>
                            <label className="input-label">Contact address</label>
                            <input
                                type="email"
                                value={v.acmeEmail}
                                onChange={e => form.patch({ acmeEmail: e.target.value })}
                                placeholder="ops@example.com"
                                className="input-field text-sm w-full"
                            />
                            <p className="text-xs text-(--base-06) mt-1.5">
                                The CA sends expiry warnings here. It is the last thing that tells you
                                this stopped working, so use an address someone reads.
                            </p>
                        </div>

                        <div>
                            <label className="input-label">Certificate authority</label>
                            <select
                                value={v.acmeDirectory}
                                onChange={e => form.patch({ acmeDirectory: e.target.value })}
                                className="input-field text-sm w-full"
                            >
                                <option value="">Let&apos;s Encrypt</option>
                                <option value="staging">Let&apos;s Encrypt (staging)</option>
                            </select>
                            <p className="text-xs text-(--base-06) mt-1.5">
                                Staging issues untrusted certificates against generous rate limits.
                                Use it to prove the DNS challenge works, then switch.
                            </p>
                        </div>

                        <div className="flex items-center justify-between gap-4">
                            <div className="text-sm text-(--base-09)">I accept the CA&apos;s subscriber agreement</div>
                            <Switch checked={v.acmeAgreed} onChange={o => form.patch({ acmeAgreed: o })} ariaLabel="Accept the subscriber agreement" />
                        </div>

                        <div className="flex items-center justify-between gap-4">
                            <div className="text-sm text-(--base-09)">Obtain and renew certificates</div>
                            <Switch checked={v.acmeEnabled} onChange={o => form.patch({ acmeEnabled: o })} ariaLabel="Obtain and renew certificates" />
                        </div>

                        {meta?.cert_status && (
                            <div className="space-y-2">
                                {meta.cert_status.error && (
                                    <p className="text-xs text-(--error-light)">{meta.cert_status.error}</p>
                                )}
                                {meta.cert_status.note && (
                                    <p className="text-xs text-(--base-06)">{meta.cert_status.note}</p>
                                )}
                                {(meta.cert_status.names ?? []).length > 0 && (
                                    <div className="overflow-x-auto">
                                        <table className="w-full text-xs">
                                            <thead>
                                                <tr className="text-(--base-06) text-left">
                                                    <th className="font-normal py-1 pr-3">Name</th>
                                                    <th className="font-normal py-1 pr-3">Certificate</th>
                                                    <th className="font-normal py-1 pr-3">Expires</th>
                                                    <th className="font-normal py-1">Last error</th>
                                                </tr>
                                            </thead>
                                            <tbody>
                                                {(meta.cert_status.names ?? []).map(n => (
                                                    <tr key={n.name} className="border-t border-(--base-03)">
                                                        <td className="py-1.5 pr-3 font-mono text-(--base-09)">{n.name}</td>
                                                        <td className="py-1.5 pr-3">
                                                            {n.have
                                                                ? <span className="text-(--success-light)">issued</span>
                                                                : <span className="text-(--base-06)">none yet</span>}
                                                        </td>
                                                        <td className="py-1.5 pr-3 text-(--base-07)">
                                                            {n.have && n.expires ? n.expires.slice(0, 10) : '—'}
                                                        </td>
                                                        <td className="py-1.5 text-(--base-06) break-all">{n.error || '—'}</td>
                                                    </tr>
                                                ))}
                                            </tbody>
                                        </table>
                                        <p className="text-xs text-(--base-06) mt-2">
                                            An error next to a certificate that already exists is a
                                            renewal failing. That is the one worth acting on now: the
                                            certificate is still valid, and will stop being so.
                                        </p>
                                    </div>
                                )}
                            </div>
                        )}
                    </div>

                    {form.error && (
                        <div className="flex items-start gap-2 text-xs text-(--error-light) px-3 py-2 rounded-md bg-(--base-02)">
                            <CircleAlert size={14} className="shrink-0 mt-0.5" />
                            <span>{form.error}</span>
                        </div>
                    )}
                </>
            )}
        </SettingsCard>
    );
}
