"use client";

import { useState } from 'react';
import { useAppData } from '@/lib/AppDataContext';
import { AlertTriangle, Info } from 'lucide-react';
import { API_URL } from '@/lib/api';
import { SkeletonHeader, SkeletonCard } from '@/components/Skeleton';
import { useSettingsForm } from '@/lib/useSettingsForm';
import { SwitchRow } from '@/components/ui/Switch';
import SettingsPage from '@/components/settings/SettingsPage';
import SettingsCard, { SettingsGroup } from '@/components/settings/SettingsCard';
import HelpTip from '@/components/ui/HelpTip';
import { LimitField } from '@/components/settings/LimitField';
import ModalPanel from '@/components/ui/ModalPanel';

// ─────────────────────────────────────────────
// Beam settings
// ─────────────────────────────────────────────

interface BeamRelayInfo {
    id?: string;
    publicHost?: string;
    clientPort?: number;
    region?: string;
}

interface BeamSettings {
    // relayAddress is the EFFECTIVE relay: the override if one is set, otherwise
    // whatever discovery found. Read-only here. It used to be the field this
    // screen edited, which is how saving anything on this page pinned a
    // discovered relay as a permanent override and silently ended failover.
    relayAddress: string;
    // manualOverride is the setting. Empty means "use discovery".
    manualOverride: string;
    publicHost?: string;
    discoveredRelays?: BeamRelayInfo[];
    bwLimit: number;
    enabled: boolean;
    downloadLink: string;

    // There is no force-update floor here: it comes from the signed release
    // manifest, which Core verifies before enforcing it. See effectiveMinVersion.

    // Per-direction throttle splits (bytes/sec, 0 = unlimited). The internal
    // pair is enforced by the NODE, the external pair by the RELAY; bwLimit is
    // the legacy single value Core still derives for older nodes.
    bwUpInternal?: number;
    bwDownInternal?: number;
    bwUpExternal?: number;
    bwDownExternal?: number;

    // Operator-recorded host hardware references. Pure informational —
    // never enforced. Surfaced as captions on the throttle inputs so
    // admins can size limits against what the host actually provides.
    refUpInternal?: number;
    refDownInternal?: number;
    refUpExternal?: number;
    refDownExternal?: number;

    // Upload limits (bytes), enforced on the beam upload path, on the platform
    // limit convention: null = no cap, 0 = none (uploads not allowed), n = the
    // cap. maxUploadBytes is an absolute per-upload cap; dailyUploadBytes is a
    // per-user daily total.
    maxUploadBytes?: number | null;
    dailyUploadBytes?: number | null;
}

// Empty: the session is an HttpOnly cookie and fetch sends it on same-origin
// requests by itself. Kept as a function so the call sites below read the same
// as the rest of the panel.
function authHeader(): Record<string, string> {
    return {};
}

async function getBeamSettings(): Promise<BeamSettings | null> {
    try {
        const res = await fetch(`${API_URL}/settings/beam`, { headers: authHeader() });
        const body = await res.json();
        if (!body?.success || !body.settings) return null;
        // Normalise the optional numerics up front so the dirty compare is
        // against a stable shape rather than against undefined-vs-0.
        const s = body.settings as BeamSettings;
        return {
            ...s,
            manualOverride: s.manualOverride ?? '',
            bwUpInternal: s.bwUpInternal ?? 0,
            bwDownInternal: s.bwDownInternal ?? 0,
            bwUpExternal: s.bwUpExternal ?? 0,
            bwDownExternal: s.bwDownExternal ?? 0,
            refUpInternal: s.refUpInternal ?? 0,
            refDownInternal: s.refDownInternal ?? 0,
            refUpExternal: s.refUpExternal ?? 0,
            refDownExternal: s.refDownExternal ?? 0,
            // ?? null, not ?? 0: an absent limit is NO cap, and defaulting it to
            // 0 would render as "uploads are not allowed".
            maxUploadBytes: s.maxUploadBytes ?? null,
            dailyUploadBytes: s.dailyUploadBytes ?? null,
        };
    } catch {
        return null;
    }
}

async function saveBeamSettings(settings: BeamSettings): Promise<{ ok: boolean; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/settings/beam`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json', ...authHeader() },
            body: JSON.stringify(settings),
        });
        const body = await res.json();
        return { ok: !!body?.success, message: body?.message };
    } catch {
        return { ok: false, message: 'Network error' };
    }
}

// ─────────────────────────────────────────────
// Unit helpers
// ─────────────────────────────────────────────

// Bandwidth values flow as bytes/sec through the API but the operator
// thinks in Mbit/s — this is the canonical UI unit. 1 Mbit/s = 125,000
// bytes/sec (decimal Mbit, matches what hosters advertise on uplinks).
const MBIT_TO_BPS = 125_000;
function mbitToBps(mbit: number): number {
    if (!Number.isFinite(mbit) || mbit <= 0) return 0;
    return Math.round(mbit * MBIT_TO_BPS);
}
function bpsToMbit(bps: number | undefined): number {
    if (!bps || bps <= 0) return 0;
    return Math.round(bps / MBIT_TO_BPS);
}

// Upload limits are stored in bytes; the UI edits them in GiB. They are on the
// platform limit convention - null is no cap, 0 is a cap of none - and the null is
// carried through by uploadLimitGiB rather than by these two helpers.
const GIB_TO_BYTES = 1024 * 1024 * 1024;
function bytesToGiB(bytes: number | undefined): number {
    if (!bytes || bytes <= 0) return 0;
    return Math.round((bytes / GIB_TO_BYTES) * 100) / 100; // 2 decimals
}
function giBToBytes(gib: number): number {
    if (!gib || gib <= 0) return 0;
    return Math.round(gib * GIB_TO_BYTES);
}

// BwField is one cell of the per-direction throttle grid: a single
// Mbit/s number input with the operator's matching hardware-reference
// value (refValue) surfaced as a caption underneath, so caps get
// chosen against real link capacity instead of typed blind.
function BwField({
    label,
    value,
    refValue,
    onChange,
}: {
    label: string;
    value: number;
    refValue?: number;
    onChange: (v: number) => void;
}) {
    const refMbit = refValue ? Math.round(refValue / MBIT_TO_BPS) : 0;
    return (
        <div className="flex flex-col gap-[5px]">
            <label className="input-label">{label}</label>
            <div className="flex items-center gap-2">
                <input
                    type="number"
                    min={0}
                    step={1}
                    value={value || ''}
                    onChange={e => onChange(Math.max(0, parseInt(e.target.value) || 0))}
                    placeholder="0"
                    className="input-field w-28 text-right"
                />
                <span className="text-[11px] font-mono text-(--base-06)">Mbit/s</span>
            </div>
            <p className="text-[10px] text-(--base-06)">
                {value === 0 ? 'Unlimited' : `Capped at ${value} Mbit/s`}
                {refMbit > 0 ? ` · host: ${refMbit} Mbit/s` : ''}
            </p>
        </div>
    );
}

function RefField({
    label,
    value,
    onChange,
}: {
    label: string;
    value: number;
    onChange: (v: number) => void;
}) {
    return (
        <div className="flex flex-col gap-[5px]">
            <label className="input-label">{label}</label>
            <div className="flex items-center gap-2">
                <input
                    type="number"
                    min={0}
                    step={1}
                    value={value || ''}
                    onChange={e => onChange(Math.max(0, parseInt(e.target.value) || 0))}
                    placeholder="0"
                    className="input-field w-28 text-right"
                />
                <span className="text-[11px] font-mono text-(--base-06)">Mbit/s</span>
            </div>
        </div>
    );
}

// ─────────────────────────────────────────────
// Beam panel
// ─────────────────────────────────────────────

export default function BeamTab() {
    const { gatewayEnabled } = useAppData();
    // The download-link field stays read-only until the admin acknowledges what a
    // custom link costs. Session-scoped on purpose: it guards the decision at the
    // moment it is made, so there is nothing worth persisting.
    const [downloadLinkUnlocked, setDownloadLinkUnlocked] = useState(false);
    const [showDownloadLinkWarning, setShowDownloadLinkWarning] = useState(false);

    const form = useSettingsForm<BeamSettings>({
        load: getBeamSettings,
        save: saveBeamSettings,
        successMessage: 'Beam settings saved',
        // Pinning a relay by hand is the setting on this page that can quietly
        // take remote access away: every client is told to dial that one host,
        // and discovery stops choosing a live one.
        confirmBeforeSave: (next, prev) =>
            next.manualOverride.trim() && next.manualOverride !== prev.manualOverride
                ? {
                      title: 'Pin the relay by hand?',
                      message:
                          'Every Beam client will be told to use this address and nothing else. ' +
                          'Discovery and failover to another relay stop while it is set. Leave it ' +
                          'empty to go back to picking whichever relay is online.',
                      confirmLabel: 'Pin it',
                      destructive: false,
                  }
                : null,
    });

    const settings = form.value;

    type BwKey = 'bwUpInternal' | 'bwDownInternal' | 'bwUpExternal' | 'bwDownExternal';
    type RefKey = 'refUpInternal' | 'refDownInternal' | 'refUpExternal' | 'refDownExternal';
    const setBwField = (k: BwKey, mbit: number) => form.patch({ [k]: mbitToBps(mbit) } as Partial<BeamSettings>);
    const setRefField = (k: RefKey, mbit: number) => form.patch({ [k]: mbitToBps(mbit) } as Partial<BeamSettings>);
    const setUploadLimit = (k: 'maxUploadBytes' | 'dailyUploadBytes', gib: number | null) =>
        form.patch({ [k]: gib === null ? null : giBToBytes(gib) } as Partial<BeamSettings>);
    const uploadLimitGiB = (v: number | null | undefined) =>
        v === null || v === undefined ? null : bytesToGiB(v);

    if (form.loading) return (
        <div className="space-y-6">
            <SkeletonHeader />
            <SkeletonCard height="h-72" />
            <SkeletonCard height="h-64" />
            <SkeletonCard height="h-64" />
            <SkeletonCard height="h-64" />
        </div>
    );

    if (!settings) {
        return (
            <div className="card p-5 flex items-start gap-2">
                <AlertTriangle size={16} className="mt-0.5 shrink-0 text-(--error-light)" />
                <div>
                    <p className="text-sm text-(--base-09)">Could not read the Beam settings.</p>
                    <p className="text-xs text-(--base-06) mt-1">
                        Nothing is shown rather than showing defaults: an empty form saved over a
                        real configuration is worse than no form.
                    </p>
                    <button onClick={() => void form.reload()} className="btn btn-secondary btn-sm mt-3">
                        Try again
                    </button>
                </div>
            </div>
        );
    }

    const discovered = settings.discoveredRelays ?? [];

    return (
        <>
        <SettingsPage
            title="Beam desktop client"
            description="Beam is DYLARIS's built-in desktop client for file transfer and server console. It always works over the local network or a direct connection, with no gateway required: Core mints the connection ticket and the node authenticates it. The gateway only adds an optional encrypted relay so users can reach their servers remotely."
        >
            {/* One endpoint writes all of this, so it is one card. It was four,
                which read as four independent things above a single save. */}
            <SettingsCard title="Beam" form={form}>
            <SettingsGroup title="General" first>

                <SwitchRow
                    label="Offer Beam to users"
                    description="Advisory only. Shows or hides the Download Beam button and reports availability to the desktop app. It does not disable Beam itself - Core still mints connection tickets and the node still authenticates, so clients that already have Beam keep working either way."
                    checked={settings.enabled}
                    onChange={v => form.patch({ enabled: v })}
                />

                <div className="flex flex-col gap-[5px]">
                    <label className="input-label flex items-center gap-1.5">
                        Relay address
                        <HelpTip label="About the relay address">
                            <p className="mb-2">
                                The Beam app asks Core where the relay is when it logs in. Left empty,
                                Core answers with whichever relay is registered and online, which is
                                what makes failover and multi-region work.
                            </p>
                            <p className="mb-2">
                                Setting a value here overrides that for every client, permanently,
                                including when that host is down. Use it when the relay sits behind a
                                name Core cannot discover - a load balancer, or your own DNS in front
                                of several relays.
                            </p>
                            <p>
                                Only used for REMOTE access through the gateway. LAN and direct
                                connections never touch it.
                            </p>
                        </HelpTip>
                    </label>

                    {/* What discovery currently answers, stated separately from
                        the override. These used to be one field, so the screen
                        offered a discovered value for editing and stored the edit
                        as an override - saving anything on this page pinned it. */}
                    <div className="rounded-md bg-(--base-02) px-3 py-2 mb-1">
                        <div className="mono-label text-[10px] mb-1">In use right now</div>
                        <div className="font-mono text-xs text-(--base-09) break-all">
                            {settings.relayAddress?.trim() || 'none - no relay is registered'}
                        </div>
                        <div className="text-[11px] text-(--base-06) mt-1">
                            {settings.manualOverride?.trim()
                                ? 'Pinned by the override below.'
                                : discovered.length > 0
                                    ? `Chosen automatically from ${discovered.length} registered relay${discovered.length === 1 ? '' : 's'}.`
                                    : 'Nothing discovered yet.'}
                        </div>
                    </div>

                    <label className="input-label">Override (optional)</label>
                    <input
                        type="text"
                        value={settings.manualOverride}
                        onChange={e => form.patch({ manualOverride: e.target.value })}
                        placeholder="empty - pick whichever relay is online"
                        className="input-field input-mono"
                    />
                    <p className="text-xs text-(--base-06) mt-0.5">
                        Host and port, e.g. <span className="font-mono">beam.example.com:25550</span>.
                        Leave it empty unless discovery cannot see your relay.
                    </p>
                    {/* A relay only exists inside the gateway subsystem, so with
                        routing on ip_port there is nothing missing. */}
                    {!settings.relayAddress?.trim() && (
                        gatewayEnabled ? (
                            <p className="flex items-start gap-1.5 text-xs text-(--base-06) mt-1">
                                <AlertTriangle size={12} className="mt-0.5 shrink-0 text-(--warning-light)" />
                                <span>No relay is available. Remote access through the gateway is unavailable until one registers or you set an override; LAN and direct connections are unaffected.</span>
                            </p>
                        ) : (
                            <p className="text-xs text-(--base-06) mt-1">
                                Not needed while Game Traffic is on IP:Port — Beam connects over LAN or directly.
                            </p>
                        )
                    )}
                </div>

                <div className="flex flex-col gap-[5px]">
                    <label className="input-label">Beam download link</label>
                    <p className="text-xs text-(--base-06) mb-1">
                        Where the Files tab sends users to install Beam Desktop. Leave it EMPTY unless you build
                        your own Beam: empty means users get the official signed build, which this Core verifies
                        against the release manifest before it will let a client connect.
                    </p>
                    <input
                        type="text"
                        value={settings.downloadLink}
                        // Locked until acknowledged. Pointing this at your own URL
                        // replaces a signed, verified download with one nothing
                        // checks, and that is not visible from a text field with a
                        // URL placeholder - which is all it was.
                        readOnly={!downloadLinkUnlocked}
                        onFocus={() => { if (!downloadLinkUnlocked) setShowDownloadLinkWarning(true); }}
                        onClick={() => { if (!downloadLinkUnlocked) setShowDownloadLinkWarning(true); }}
                        onChange={e => form.patch({ downloadLink: e.target.value })}
                        placeholder="Empty — users get the official build"
                        aria-describedby="beam-download-link-help"
                        className={`input-field ${downloadLinkUnlocked ? '' : 'cursor-pointer'}`}
                    />
                    <p id="beam-download-link-help" className="text-xs text-(--base-06) mt-0.5">
                        {downloadLinkUnlocked
                            ? 'Custom link active. Core cannot verify a build it does not publish — you are responsible for what this URL serves.'
                            : 'Only needed if you ship your own Beam build or a fork. Click the field to change it.'}
                    </p>
                </div>
                {/* The force-update floor deliberately has no control here. It is
                    baked into the SIGNED release manifest, which is the same
                    artifact the app verifies before self-updating, so the release
                    decides it. A hand-typed floor could only disagree with the
                    binary being shipped - and a typo in it locks every client out. */}
                <p className="flex items-start gap-1.5 text-xs text-(--base-06)">
                    <Info size={12} className="mt-0.5 shrink-0" />
                    <span>
                        The minimum Beam version is read from the signed release manifest and
                        verified before it is enforced — there is nothing to set. Clients below
                        that floor are asked to update before they can connect.
                    </span>
                </p>
            </SettingsGroup>

            {/* ─── Throttle (enforced) ─── */}
            <SettingsGroup>
                <div>
                    <h3 className="text-sm font-display font-semibold text-(--base-08) mb-1 flex items-center gap-1.5">
                        Bandwidth throttle
                        <HelpTip label="About the bandwidth throttle">
                            <p className="mb-2">
                                A ceiling on how fast Beam moves data. <strong>0 means unlimited</strong>,
                                which is the default for all four.
                            </p>
                            <p className="mb-2">
                                <strong>Internal</strong> is the hop between the node and the relay,
                                and the <strong>node</strong> enforces it.
                                <strong> External</strong> is the hop between the relay and the Beam
                                app on someone&apos;s desktop, and the <strong>relay</strong> enforces
                                it. A transfer crosses both, so the slower of the two decides.
                            </p>
                            <p className="mb-2">
                                The cap is per direction and <em>shared</em>: ten concurrent transfers
                                divide one limit between them rather than getting it each. Nobody is
                                singled out - a big transfer simply slows down while others run.
                            </p>
                            <p className="mb-2">
                                <code>1 Mbit/s = 125,000 bytes/s</code>, the decimal Mbit hosters
                                advertise. A 1 Gbit uplink is 1000 here.
                            </p>
                            <p>
                                Nodes and relays re-read these every 10 seconds, so a change takes
                                effect on its own. Nothing needs restarting.
                            </p>
                        </HelpTip>
                    </h3>
                    <p className="text-xs text-(--base-06)">
                        Values in <span className="font-mono">Mbit/s</span> — <strong>0 = unlimited</strong>.
                        Each cap is shared across all transfers in that direction, so concurrent transfers
                        divide it rather than each getting it.
                    </p>
                </div>

                <div>
                    <h4 className="mono-label mb-2">Internal — node to relay, enforced by the node</h4>
                    <div className="grid grid-cols-2 gap-3">
                        <BwField
                            label="Up (into the node)"
                            value={bpsToMbit(settings.bwUpInternal)}
                            refValue={settings.refUpInternal}
                            onChange={v => setBwField('bwUpInternal', v)}
                        />
                        <BwField
                            label="Down (out of the node)"
                            value={bpsToMbit(settings.bwDownInternal)}
                            refValue={settings.refDownInternal}
                            onChange={v => setBwField('bwDownInternal', v)}
                        />
                    </div>
                </div>

                <div>
                    <h4 className="mono-label mb-2">External — relay to the Beam app, enforced by the relay</h4>
                    <div className="grid grid-cols-2 gap-3">
                        <BwField
                            label="Up (from the user)"
                            value={bpsToMbit(settings.bwUpExternal)}
                            refValue={settings.refUpExternal}
                            onChange={v => setBwField('bwUpExternal', v)}
                        />
                        <BwField
                            label="Down (to the user)"
                            value={bpsToMbit(settings.bwDownExternal)}
                            refValue={settings.refDownExternal}
                            onChange={v => setBwField('bwDownExternal', v)}
                        />
                    </div>
                </div>
            </SettingsGroup>

            {/* ─── Upload limits (enforced) ─── */}
            <SettingsGroup>
                <div>
                    <h3 className="text-sm font-display font-semibold text-(--base-08) mb-1 flex items-center gap-1.5">
                        Upload limits
                        <HelpTip label="About upload limits">
                            <p className="mb-2">
                                Two different questions. <strong>Max per upload</strong> refuses one
                                file that is too big, before it starts. <strong>Daily per user</strong>{' '}
                                is a running total per account, counted from midnight UTC.
                            </p>
                            <p className="mb-2">
                                Both apply to every way a file reaches a node: the Beam app, SFTP, and
                                the browser file manager. Downloads are never counted.
                            </p>
                            <p className="mb-2">
                                The daily total is added up when an upload finishes, so several
                                uploads running at once can overshoot the limit slightly rather than
                                queueing behind each other. That is deliberate: this is a fair-use
                                brake, not a quota to the byte.
                            </p>
                            <p>
                                Both <strong>fail open</strong>. If the counter is unreachable the
                                upload is allowed - a storage outage should not also stop people
                                working.
                            </p>
                        </HelpTip>
                    </h3>
                    <p className="text-xs text-(--base-06)">
                        Caps on files pushed to a node over Beam, SFTP and the file manager.
                        Values in <span className="font-mono">GiB</span>. Switch to
                        <strong> No limit</strong> to leave an axis uncapped, or set
                        <strong> 0</strong> to refuse uploads on it entirely.
                        Downloads are not counted.
                    </p>
                </div>
                <div className="grid grid-cols-2 gap-3">
                    <div className="flex flex-col gap-[5px]">
                        <label className="input-label" htmlFor="beam-max-upload">Max per upload</label>
                        <LimitField
                            id="beam-max-upload"
                            value={uploadLimitGiB(settings.maxUploadBytes)}
                            onChange={v => setUploadLimit('maxUploadBytes', v)}
                            unit="GiB"
                            step={0.1}
                        />
                        <p className="text-xs text-(--base-06)">Refuses any single file larger than this, before the transfer starts.</p>
                    </div>
                    <div className="flex flex-col gap-[5px]">
                        <label className="input-label" htmlFor="beam-daily-upload">Daily per user</label>
                        <LimitField
                            id="beam-daily-upload"
                            value={uploadLimitGiB(settings.dailyUploadBytes)}
                            onChange={v => setUploadLimit('dailyUploadBytes', v)}
                            unit="GiB"
                            step={0.1}
                        />
                        <p className="text-xs text-(--base-06)">Running total per account, reset at midnight UTC.</p>
                    </div>
                </div>
            </SettingsGroup>

            {/* ─── Reference (host hardware — informational only) ─── */}
            <SettingsGroup>
                <div>
                    <h3 className="text-sm font-display font-semibold text-(--base-08) mb-1 flex items-center gap-1.5">
                        Host hardware reference
                        <HelpTip label="About the hardware reference">
                            <p className="mb-2">
                                <strong>These enforce nothing.</strong> Nothing in the node, the relay
                                or Core reads them. They are a note to yourself.
                            </p>
                            <p className="mb-2">
                                What you type here appears as{' '}
                                <span className="font-mono">host: N Mbit/s</span> under the matching
                                throttle field above, so a cap can be chosen against what the link
                                really does instead of guessed.
                            </p>
                            <p>
                                Worth saying plainly, because a number on a settings page normally
                                does something. Leave them at 0 if you do not know.
                            </p>
                        </HelpTip>
                    </h3>
                    <p className="text-xs text-(--base-06)">
                        What the host actually provides. <strong>Never enforced</strong> — the only thing
                        these do is caption the throttle fields above so you can size a cap against real
                        capacity. Values in <span className="font-mono">Mbit/s</span>, leave at 0 if unknown.
                    </p>
                </div>

                <div>
                    <h4 className="mono-label mb-2">Internal (datacenter network)</h4>
                    <div className="grid grid-cols-2 gap-3">
                        <RefField
                            label="Up"
                            value={bpsToMbit(settings.refUpInternal)}
                            onChange={v => setRefField('refUpInternal', v)}
                        />
                        <RefField
                            label="Down"
                            value={bpsToMbit(settings.refDownInternal)}
                            onChange={v => setRefField('refDownInternal', v)}
                        />
                    </div>
                </div>

                <div>
                    <h4 className="mono-label mb-2">External (public uplink)</h4>
                    <div className="grid grid-cols-2 gap-3">
                        <RefField
                            label="Up"
                            value={bpsToMbit(settings.refUpExternal)}
                            onChange={v => setRefField('refUpExternal', v)}
                        />
                        <RefField
                            label="Down"
                            value={bpsToMbit(settings.refDownExternal)}
                            onChange={v => setRefField('refDownExternal', v)}
                        />
                    </div>
                </div>
            </SettingsGroup>
            </SettingsCard>
        </SettingsPage>

        {showDownloadLinkWarning && (
            <div className="modal-overlay animate-fade-in" onClick={() => setShowDownloadLinkWarning(false)}>
                <ModalPanel onClose={() => setShowDownloadLinkWarning(false)} className="modal-panel max-w-md" onClick={e => e.stopPropagation()}>
                    <div className="modal-header">
                        <h3 className="modal-title flex items-center gap-2 text-(--warning-light)">
                            <AlertTriangle size={16} /> Use your own download link?
                        </h3>
                    </div>
                    <div className="modal-body space-y-3 text-sm text-(--base-07)">
                        <p>
                            You only need this if you build and host Beam Desktop yourself, or run a fork.
                        </p>
                        <p>
                            Leaving it empty is the normal setup: users get the official build, and Core verifies
                            it against the signed release manifest. A custom URL bypasses that check — Core cannot
                            verify a build it does not publish, and everyone who installs from here trusts whatever
                            that URL serves.
                        </p>
                    </div>
                    <div className="modal-footer">
                        <button onClick={() => setShowDownloadLinkWarning(false)} className="btn btn-secondary">
                            Cancel
                        </button>
                        <button
                            onClick={() => { setDownloadLinkUnlocked(true); setShowDownloadLinkWarning(false); }}
                            className="btn btn-primary"
                        >
                            I build my own Beam
                        </button>
                    </div>
                </ModalPanel>
            </div>
        )}
        </>
    );
}
