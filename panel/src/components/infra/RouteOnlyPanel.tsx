"use client";

import React, { useCallback, useEffect, useState } from 'react';
import { Plus, Trash2, Loader2, Server, Link2, Copy, Check, ShieldCheck, Pencil, X, RefreshCw } from 'lucide-react';
import RouteDomainPicker, { DomainAvailability } from '@/components/RouteDomainPicker';
import {
    CreateRouteRequest, LinkRoute, LinkKit, MintedLinkKit,
    getLinkRoutes, createLinkRoute, deleteLinkRoute,
    listLinkKits, mintLinkKit, revokeLinkKit, rollLinkKit,
} from '@/lib/api';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import { SkeletonCard } from '@/components/Skeleton';
import { DeployKit, DEPLOY_ASIDE_STICKY, DEPLOY_GRID, NotIncluded, usageLabel } from '@/components/infra/DeployKit';
import { routeSubmitRequest } from '@/lib/routeSubmit';
import { singleLocalTarget } from '@/lib/warpDeploy';
import type { WarpDeployConfig } from '@/lib/api/warpDeployConfig';

// Named RouteOnlyPanel, not RoutesPanel: views/infrastructure/RoutesPanel is
// the ADMIN one (fleet routes across edges) and the two are easy to import by
// mistake for each other.
//
// Route-only ("via Link"): a DDoS-protected address pointed at a server the user
// runs on their OWN machine, reached through their own outbound Link tunnel. No
// managed node, no open ports - the customer runs warp + link with a "link kit"
// and the edge proxies through that tunnel to their LOCAL server.
//
// This was its own page at /routes while /nodes carried a SECOND, partial copy
// of the same mint flow: one product, minted in two places, with the deploy
// snippet on one page and the address form on the other. It is one tab now, and
// the whole sequence - create a link, deploy it, point an address at it - reads
// top to bottom in one place.
export default function RouteOnlyPanel({ enrollUrl, config, storeUrl, allowed, entitlementKnown, suspended, storeLinked }: {
    enrollUrl: string;
    config: WarpDeployConfig | null;
    storeUrl: string | null;
    /**
     * Resolved route-only entitlement. The old /routes page never checked this
     * at all, so someone without it got a fully working-looking form and only
     * found out at mint time.
     */
    allowed: boolean;
    entitlementKnown: boolean;
    suspended: boolean;
    /** null = not known yet; see NotIncluded for why that differs from false. */
    storeLinked?: boolean | null;
}) {
    const [kits, setKits] = useState<LinkKit[]>([]);
    const [used, setUsed] = useState(0);
    // null is Core's "no cap at all" and 0 is a cap of NONE (the platform's
    // limits convention). Collapsing them into 0 showed a tenant entitled to no
    // link that they had unlimited ones, and the refusal arrived as a 403.
    const [limit, setLimit] = useState<number | null | undefined>(undefined);
    const [routes, setRoutes] = useState<LinkRoute[]>([]);
    const [loading, setLoading] = useState(true);

    // Mint-link flow
    const [linkName, setLinkName] = useState('');
    const [minting, setMinting] = useState(false);
    const [minted, setMinted] = useState<MintedLinkKit | null>(null);
    // A roll reuses the same reveal - it produces the same one secret - so only
    // the wording differs, and this is what selects it.
    const [mintedWasRoll, setMintedWasRoll] = useState(false);
    const [rollingLink, setRollingLink] = useState('');

    // Create-route flow
    const [linkId, setLinkId] = useState('');
    const [domainReq, setDomainReq] = useState<CreateRouteRequest>({ targetPort: 25565 });
    const [availability, setAvailability] = useState<DomainAvailability>('idle');
    const [targetHost, setTargetHost] = useState('127.0.0.1');
    const [targetPort, setTargetPort] = useState(25565);
    const [creating, setCreating] = useState(false);
    const [error, setError] = useState('');
    const [toast, setToast] = useState('');

    // The domain of the route being edited, or null while creating a new one.
    //
    // The domain itself is NOT editable in this mode, and that is the point.
    // Saving posts the same domain again, which is the only overwrite Core
    // permits for a route you own; posting a DIFFERENT one would leave the old
    // route in place and quietly spend a second address.
    const [editing, setEditing] = useState<string | null>(null);
    // True while editing a route whose link Core could not name. See startEdit.
    const [linkUnknown, setLinkUnknown] = useState(false);

    const load = useCallback(async () => {
        try {
            const [k, r] = await Promise.all([listLinkKits(), getLinkRoutes()]);
            const kitList = k?.kits ?? [];
            setKits(kitList);
            setUsed(k?.used ?? kitList.length);
            setLimit(k?.limit);
            setRoutes(Array.isArray(r) ? r : []);
            // Default the route form to the first link if none chosen yet.
            setLinkId(prev => prev || (kitList[0]?.link_id ?? ''));
        } finally {
            setLoading(false);
        }
    }, []);

    // Only fetch once the caller is allowed to have any of this: an unentitled
    // tenant would otherwise fire two requests to render a refusal.
    useEffect(() => { if (allowed) load(); }, [allowed, load]);

    const flashToast = (msg: string) => {
        setToast(msg);
        setTimeout(() => setToast(''), 3000);
    };

    const mint = async () => {
        setMinting(true);
        try {
            const res = await mintLinkKit(linkName.trim());
            // Mirrors revoke's res.success check below: fetchAPI resolves (does not
            // throw) on an HTTP error with a JSON body, so an unchecked res here
            // would render the one-time-secret panel with WARP_API_KEY=undefined -
            // looking like success for a security-critical mint.
            if (!res.success) throw new Error((res as { message?: string }).message || 'Failed to create link');
            setMinted(res);
            setMintedWasRoll(false);
            setLinkName('');
            await load();
            setLinkId(res.link_id);
        } catch (e) {
            flashToast(e instanceof Error ? e.message : 'Failed to create link');
        } finally {
            setMinting(false);
        }
    };

    // New secret, same link. The link_id, its routes and its protected address all
    // stay; only WARP_API_KEY changes. The tunnel is left up on purpose - the
    // machine keeps serving until it is redeployed with the new key, and revoke
    // beside this is still the immediate cutoff for a key that leaked.
    const roll = async (linkIdToRoll: string, name: string) => {
        if (!(await confirmDialog({
            title: 'Roll this link key?',
            message: `The current key for ${name} stops working for new connections straight away, and you get a replacement shown once. `
                + 'The address and its routes do not change, and the link keeps running until you redeploy it with the new key.',
            confirmLabel: 'Roll the key',
            destructive: false,
        }))) return;
        setRollingLink(linkIdToRoll);
        try {
            // Same reason mint and revoke check it: fetchAPI resolves on an HTTP
            // error with a JSON body, so an unchecked res would reveal a panel
            // reading WARP_API_KEY=undefined after a roll that never happened.
            const res = await rollLinkKit(linkIdToRoll);
            if (!res.success || !res.warp_key) throw new Error((res as { message?: string }).message || 'Failed to roll link key');
            setMinted(res);
            setMintedWasRoll(true);
            setLinkId(res.link_id);
            await load();
        } catch (e) {
            flashToast(e instanceof Error ? e.message : 'Failed to roll link key');
        } finally {
            setRollingLink('');
        }
    };

    const revoke = async (linkIdToRevoke: string) => {
        if (!(await confirmDialog({ title: 'Revoke link', message: 'Revoke this link? Its tunnel drops and it can no longer connect.', confirmLabel: 'Revoke' }))) return;
        try {
            // fetchAPI resolves (does not throw) on an HTTP error whose body is JSON, so a
            // 404/500 arrives here as { success: false }. Check it, or a failed revoke of a
            // destructive, security-relevant action would falsely report "Link revoked".
            const res = await revokeLinkKit(linkIdToRevoke);
            if (!res.success) throw new Error((res as { message?: string }).message || 'Failed to revoke link');
            flashToast('Link revoked');
            await load();
        } catch (e) {
            flashToast(e instanceof Error ? e.message : 'Failed to revoke link');
        }
    };

    const submitRoute = async () => {
        setError('');
        if (!linkId) { setError('Create a link first, then select it'); return; }
        if (!targetHost.trim()) { setError('Enter the local address of your server'); return; }
        // "Taken" is the right refusal for a new address and the wrong one for
        // an edit: the domain being edited is taken BY THE PERSON EDITING IT.
        if (!editing && availability === 'taken') { setError('That domain is already taken'); return; }
        setCreating(true);
        try {
            const req = routeSubmitRequest(editing, domainReq, targetPort);
            const res = await createLinkRoute({ ...req, linkId, targetHost: targetHost.trim() });
            if (!res.success) throw new Error((res as { message?: string }).message || `Failed to ${editing ? 'save' : 'create'} route`);
            // On a custom domain the route is accepted but provisional: a four-hour
            // clock is now running and missing it deletes the route. Core says so in
            // ownershipNotice, and a fixed "Route created" would hide it.
            const notice = (res as { ownershipNotice?: string }).ownershipNotice;
            const what = editing ? 'Route updated' : 'Route created';
            flashToast(notice ? `${what}. ${notice}` : what);
            if (editing) cancelEdit();
            await load();
        } catch (e) {
            setError(e instanceof Error ? e.message : `Failed to ${editing ? 'save' : 'create'} route`);
        } finally {
            setCreating(false);
        }
    };

    const startEdit = (rt: LinkRoute) => {
        setEditing(rt.domain);
        setError('');
        setDomainReq({ domain: rt.domain, targetPort: rt.target_port });
        setTargetHost(rt.target_ip);
        setTargetPort(rt.target_port);
        // Only when Core could name it. Falling back to the currently selected
        // link would move the route to a different one on save without anyone
        // asking for that.
        if (rt.link_id) setLinkId(rt.link_id);
        // And say so when it could not. A route whose link kit was revoked
        // keeps running on the old tunnel and its link_id resolves to nothing,
        // so the select shows some OTHER kit and saving would rebind the route
        // to it. The one case where the form cannot be trusted to describe what
        // it is editing is the one case that has to be said out loud.
        setLinkUnknown(!rt.link_id);
        setAvailability('idle');
    };

    const cancelEdit = () => {
        setEditing(null);
        setLinkUnknown(false);
        setError('');
        setDomainReq({ targetPort: 25565 });
        setTargetHost('127.0.0.1');
        setTargetPort(25565);
        setAvailability('idle');
    };

    const removeRoute = async (domain: string) => {
        if (!(await confirmDialog({ title: 'Delete route', message: `Delete the route ${domain}?` }))) return;
        try {
            const res = await deleteLinkRoute(domain);
            if (!res.success) throw new Error((res as { message?: string }).message || 'Failed to delete route');
            await load();
        } catch (e) {
            flashToast(e instanceof Error ? e.message : 'Failed to delete route');
        }
    };

    // null means "not fetched yet". Rendering a refusal during that window tells
    // an entitled tenant they have nothing and then takes it back.
    if (!entitlementKnown) return <SkeletonCard height="h-24" />;
    if (!allowed) return <NotIncluded what="route only" storeUrl={storeUrl} suspended={suspended} storeLinked={storeLinked} />;

    return (
        <div className="space-y-6">
            <p className="text-sm text-(--base-07) max-w-2xl">
                <strong>You</strong> run the Minecraft server, on your own panel or none at all. Dylaris gives it
                a protected address and takes the attack traffic. Your machine makes outbound connections
                only — nothing is exposed.
            </p>

            {/* The deploy kit gets a column of its own rather than a fold-out.
                It is not a detail of creating a link: it is the thing the reader
                came to run, they need it again on every rebuild, and while it
                was behind "Show the deploy steps again" the commonest question
                was where the compose file had gone. */}
            <div className={DEPLOY_GRID}>
            <div className="space-y-6 min-w-0">

            {/* Links */}
            <div className="card p-5 space-y-4">
                <div className="flex items-center gap-2">
                    <Link2 size={16} className="text-(--accent-light)" />
                    <h2 className="font-medium text-(--base-09)">Your links</h2>
                </div>

                {kits.length > 0 && (
                    <div className="space-y-2">
                        <p className="text-xs text-(--base-06) font-mono">
                            {/* The same wording the machines tab uses, from the same
                                helper, so one convention is read one way on both. */}
                            {usageLabel(used, limit)}
                        </p>
                        {kits.map(k => (
                            <div key={k.id} className="flex items-center gap-3 px-3 py-2 rounded-md bg-(--base-01)">
                                <ShieldCheck size={14} className="text-(--success-light) shrink-0" />
                                <span className="text-sm text-(--base-09) truncate">{k.name}</span>
                                <span className="text-xs text-(--base-06) font-mono truncate ml-auto">{k.link_id}</span>
                                <button
                                    onClick={() => roll(k.link_id, k.name)}
                                    disabled={rollingLink === k.link_id}
                                    title="Replace this key with a new one, keeping the address and its routes"
                                    className="p-1.5 text-(--base-06) hover:text-(--base-09) transition-colors shrink-0 disabled:opacity-40 disabled:cursor-not-allowed"
                                >
                                    <RefreshCw size={14} className={rollingLink === k.link_id ? 'animate-spin' : undefined} />
                                </button>
                                <button
                                    onClick={() => revoke(k.link_id)}
                                    title="Revoke link"
                                    className="p-1.5 text-(--base-06) hover:text-(--error-light) transition-colors shrink-0"
                                >
                                    <Trash2 size={14} />
                                </button>
                            </div>
                        ))}
                    </div>
                )}

                <div className="grid grid-cols-[1fr_auto] gap-3 max-w-md">
                    <div className="flex flex-col gap-1.5">
                        <label className="input-label">New link name</label>
                        <input
                            type="text"
                            value={linkName}
                            onChange={e => setLinkName(e.target.value)}
                            placeholder="Home PC"
                            disabled={suspended}
                            className="input-field text-sm w-full"
                        />
                    </div>
                    <div className="flex flex-col gap-1.5 justify-end">
                        <button onClick={mint} disabled={minting || suspended} className="btn btn-secondary inline-flex items-center gap-2 disabled:opacity-60">
                            {minting ? <Loader2 size={16} className="animate-spin" /> : <Plus size={16} />}
                            {minting ? 'Creating…' : 'Create link'}
                        </button>
                    </div>
                </div>
                <p className="text-xs text-(--base-06)">A link is the kit you run on your machine (warp + link). It opens an outbound tunnel — nothing is exposed.</p>
            </div>

            {/* Create or edit route */}
            <div id="route-form" className="card p-5 space-y-4">
                <div className="flex items-center justify-between gap-3">
                    <h2 className="font-medium text-(--base-09)">{editing ? 'Edit route' : 'New route'}</h2>
                    {editing && (
                        <button onClick={cancelEdit} className="btn btn-secondary btn-sm inline-flex items-center gap-1.5">
                            <X size={13} /> Cancel
                        </button>
                    )}
                </div>

                <div className="flex flex-col gap-1.5">
                    <label className="input-label">Link</label>
                    <select
                        value={linkId}
                        onChange={e => setLinkId(e.target.value)}
                        disabled={kits.length === 0}
                        className="input-field text-sm w-full max-w-md disabled:opacity-60"
                    >
                        {kits.length === 0 && <option value="">Create a link first</option>}
                        {kits.map(k => <option key={k.id} value={k.link_id}>{k.name}</option>)}
                    </select>
                    {editing && linkUnknown && (
                        <p className="text-xs text-(--warning-light)">
                            This route&apos;s link could not be identified &mdash; its kit was probably revoked.
                            Saving will move the route to the link selected above.
                        </p>
                    )}
                </div>

                <div className="flex flex-col gap-1.5">
                    <label className="input-label">Your domain</label>
                    {editing ? (
                        <>
                            <div className="input-field text-sm font-mono text-(--base-08) opacity-70 cursor-not-allowed select-all">{editing}</div>
                            <p className="text-xs text-(--base-06)">The address stays as it is. To use a different one, cancel and create a second route.</p>
                        </>
                    ) : (
                        <RouteDomainPicker value={domainReq} onChange={setDomainReq} onAvailabilityChange={setAvailability} />
                    )}
                </div>

                <div className="grid grid-cols-[1fr_auto] gap-3 max-w-md">
                    <div className="flex flex-col gap-1.5">
                        <label className="input-label">Local server address</label>
                        <div className="flex items-center gap-2">
                            <Server size={14} className="text-(--base-06) shrink-0" />
                            <input
                                type="text"
                                value={targetHost}
                                onChange={e => setTargetHost(e.target.value)}
                                placeholder="127.0.0.1 or 192.168.1.50"
                                className="input-field text-sm w-full"
                            />
                        </div>
                    </div>
                    <div className="flex flex-col gap-1.5">
                        <label className="input-label">Port</label>
                        <input
                            type="number"
                            value={targetPort}
                            onChange={e => setTargetPort(Number(e.target.value))}
                            className="input-field text-sm w-24 text-center"
                        />
                    </div>
                </div>
                <p className="text-xs text-(--base-06)">This is the address your link dials on its own machine — loopback or LAN. Your link only dials addresses you allow it to (LINK_ALLOWED_TARGETS).</p>

                {error && <p className="text-sm text-(--error-light)">{error}</p>}

                <div className="flex items-center gap-2">
                    <button onClick={submitRoute} disabled={creating || kits.length === 0 || suspended} className="btn btn-primary inline-flex items-center gap-2 w-fit disabled:opacity-60">
                        {creating ? <Loader2 size={16} className="animate-spin" /> : editing ? <Check size={16} /> : <Plus size={16} />}
                        {creating ? (editing ? 'Saving…' : 'Creating…') : editing ? 'Save changes' : 'Create route'}
                    </button>
                    {editing && (
                        <button onClick={cancelEdit} disabled={creating} className="btn btn-secondary w-fit disabled:opacity-60">Cancel</button>
                    )}
                </div>
            </div>

            {/* List routes */}
            <div className="space-y-2">
                <h2 className="font-medium text-(--base-09)">Your routes</h2>
                {loading ? (
                    <div className="card p-5 text-sm text-(--base-06)">Loading…</div>
                ) : routes.length === 0 ? (
                    <div className="card p-5 text-sm text-(--base-06)">No routes yet.</div>
                ) : (
                    routes.map(rt => (
                        <div
                            key={rt.domain}
                            className={`card px-4 py-3 flex items-center justify-between gap-3 ${editing === rt.domain ? 'border-(--accent)' : ''}`}
                        >
                            <div className="min-w-0">
                                <div className="font-mono text-sm text-(--base-09) truncate">{rt.domain}</div>
                                <div className="text-xs text-(--base-06) font-mono">→ {rt.target_ip}:{rt.target_port}</div>
                            </div>
                            <div className="flex items-center gap-1 shrink-0">
                                {editing === rt.domain ? (
                                    <span className="badge badge-accent">Editing</span>
                                ) : (
                                    <button
                                        onClick={() => startEdit(rt)}
                                        title="Edit"
                                        aria-label={`Edit ${rt.domain}`}
                                        className="p-2 text-(--base-06) hover:text-(--base-09) transition-colors"
                                    >
                                        <Pencil size={16} />
                                    </button>
                                )}
                                <button
                                    onClick={() => removeRoute(rt.domain)}
                                    title="Delete"
                                    aria-label={`Delete ${rt.domain}`}
                                    className="p-2 text-(--base-06) hover:text-(--error-light) transition-colors"
                                >
                                    <Trash2 size={16} />
                                </button>
                            </div>
                        </div>
                    ))
                )}
            </div>

            </div>

            {/* Sticky, because the reader works through the compose file while
                scrolling the form and the route list beside it. */}
            <aside className={`space-y-3 min-w-0 ${DEPLOY_ASIDE_STICKY}`}>
                {minted && <MintReveal kit={minted} rolled={mintedWasRoll} onCopy={flashToast} onClose={() => setMinted(null)} />}
                {!minted && (
                    <p className="text-xs text-(--base-06)">
                        {kits.length > 0
                            ? <>The key itself cannot be shown again — only its hash is stored. Paste the one you saved where the file says <code className="font-mono">&lt;your-warp-key&gt;</code>, or revoke the link and create a new one.</>
                            : <>This is what you will run. Create a link on the left and its key is filled in for you.</>}
                    </p>
                )}
                <DeployKit
                    kind="route-only"
                    warpKey={minted?.warp_key ?? null}
                    enrollUrl={enrollUrl}
                    config={config}
                    // What their routes already dial, so the file allows exactly
                    // that. Core accepts any local address in the route form, and
                    // the link compares LINK_ALLOWED_TARGETS as an exact string,
                    // so a file stuck on 127.0.0.1 refuses every player of a
                    // server that runs on another machine, silently.
                    localTarget={singleLocalTarget(routes.map(rt => rt.target_ip))}
                />
            </aside>
            </div>

            {toast && (
                <div className="fixed bottom-6 left-1/2 -translate-x-1/2 px-4 py-2.5 rounded-md bg-(--success) text-white text-sm shadow-lg">{toast}</div>
            )}
        </div>
    );
}

// MintReveal shows the one-time secrets for a freshly minted link kit. The warp
// key is hashed server-side and can never be shown again, so we make copying it
// prominent and warn the user.
function MintReveal({ kit, rolled, onCopy, onClose }: { kit: MintedLinkKit; rolled?: boolean; onCopy: (m: string) => void; onClose: () => void }) {
    // The link fetches everything else (its tunnel token + Redis credential) from
    // Core at boot using this warp key, so the only secret to paste is WARP_API_KEY.
    return (
        <div className="rounded-md border border-(--accent)/30 bg-(--accent)/5 p-4 space-y-3">
            <div className="flex items-center justify-between">
                <span className="text-sm font-medium text-(--base-09)">
                    {rolled ? 'New key - copy this now' : 'Link created - copy this now'}
                </span>
                <button onClick={onClose} className="text-xs text-(--base-06) hover:text-(--base-09)">Dismiss</button>
            </div>
            <p className="text-xs text-(--base-07) leading-relaxed">
                Shown once. It is already filled into the compose file below. The link fetches its tunnel token
                and Redis credential from Core on its own. The warp key cannot be retrieved later.
                {rolled && ' Only the key changed - the address and its routes are untouched, and the link keeps running until you redeploy it.'}
            </p>
            <CopyRow label="WARP_API_KEY" value={kit.warp_key} onCopy={onCopy} />
            <button
                onClick={() => { navigator.clipboard?.writeText(`WARP_API_KEY=${kit.warp_key}`); onCopy('Copied .env line'); }}
                className="btn btn-secondary inline-flex items-center gap-2 text-xs"
            >
                <Copy size={13} /> Copy .env line
            </button>
        </div>
    );
}

function CopyRow({ label, value, onCopy }: { label: string; value: string; onCopy: (m: string) => void }) {
    const [copied, setCopied] = useState(false);
    const copy = () => {
        navigator.clipboard?.writeText(value);
        setCopied(true);
        onCopy(`Copied ${label}`);
        setTimeout(() => setCopied(false), 1500);
    };
    return (
        <div className="flex items-center gap-2">
            <span className="input-label w-32 shrink-0">{label}</span>
            <code className="text-xs font-mono text-(--base-08) bg-(--base-01) px-2 py-1 rounded truncate flex-1">{value}</code>
            <button onClick={copy} title={`Copy ${label}`} className="p-1.5 text-(--base-06) hover:text-(--accent-light) transition-colors shrink-0">
                {copied ? <Check size={14} className="text-(--success-light)" /> : <Copy size={14} />}
            </button>
        </div>
    );
}
