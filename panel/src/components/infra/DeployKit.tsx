"use client";

import { useCallback, useEffect, useState } from 'react';
import Link from 'next/link';
import { Copy, Check, Terminal, Lock, ShoppingCart, ExternalLink, Link2 as LinkIcon, CircleAlert, Loader2, RefreshCcw } from 'lucide-react';
import {
    nodeCompose, routeOnlyCompose, deployCli, deployIntro, composeFileName,
    DEPLOY_PORTAINER_NOTE, kitInput, watchtowerNote, managedLinkCompose, LINK_IMAGE,
} from '@/lib/warpDeploy';
import { getManagedLink, retireManagedLink, type ManagedLinkKit as ManagedLinkKitData } from '@/lib/api/gatewayProvider';
import { isDockerNetworkName, canReplaceLink, retiringLine, RETIRE_CONFIRM } from '@/lib/gatewayProvider';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import { toast } from '@/components/ui/Toast';
import { fmtDateTime } from '@/lib/time';
import { linkState } from '@/lib/linkState';
import LinkBadge from '@/components/infra/LinkBadge';
import { Skeleton } from '@/components/Skeleton';
import { useAppData } from '@/lib/AppDataContext';
import type { KitProps } from '@/lib/warpDeploy';
import type { DeployPlatform } from '@/lib/warpDeploy';

// Shared by both halves of "my infrastructure". They used to be two pages with
// their own copies, which is how route-only ended up with a mint flow on each
// of them.

// The two-column shape both halves use: what you configure on the left, the
// compose file you run on the right. Both columns have a floor, and the grid
// stacks the moment the container cannot hold both floors plus the gap - the
// breakpoint IS 32 + 34 + 1.5, so the two numbers can only disagree if someone
// changes one and not the other.
//
// 42rem on the right is measured, not picked: the widest line the snippet has
// to show is a 64-hex key at 87 characters, and at the pre's 11px mono that is
// ~575px of text, which 42rem clears once the card's p-4 and the pre's p-3 are
// taken off. Below that the pre scrolls horizontally rather than wrapping a
// value someone is about to copy.
//
// A CONTAINER query, not a viewport breakpoint: this page renders inside a flex
// area, so the viewport width does not describe the space the grid actually has.
// Requires an ancestor carrying `@container` - the page shell does.
export const DEPLOY_GRID =
    'grid gap-6 items-start @min-[67.5rem]:grid-cols-[minmax(32rem,1fr)_minmax(34rem,42rem)]';

// Sticky only while the columns are side by side. Stacked, the compose file is
// the last thing on the page and pinning it does nothing but shorten it.
export const DEPLOY_ASIDE_STICKY = '@min-[67.5rem]:sticky @min-[67.5rem]:top-6';

export function CopyButton({ value, label, className }: { value: string; label?: string; className?: string }) {
    const [copied, setCopied] = useState(false);
    return (
        <button
            type="button"
            onClick={async () => {
                try {
                    await navigator.clipboard.writeText(value);
                    setCopied(true);
                    setTimeout(() => setCopied(false), 1800);
                } catch { /* clipboard blocked (insecure context); the text is selectable anyway */ }
            }}
            className={className || 'btn btn-secondary btn-sm shrink-0'}
        >
            {copied ? <><Check size={13} /> Copied</> : <><Copy size={13} /> {label || 'Copy'}</>}
        </button>
    );
}

/**
 * A secret shown exactly once. Blurred until asked for, because it sits on a
 * page someone may well have open while screen-sharing a deploy - and because
 * it is not the thing they came for: both keys are already filled into the
 * compose file below, so the normal path never reads them at all.
 *
 * Still fully retrievable in the moment: only a hash is stored, so this is the
 * one chance to save it.
 */
export function SecretField({ label, value, note }: { label: string; value: string; note?: string }) {
    const [shown, setShown] = useState(false);
    return (
        <div className="space-y-1">
            <span className="mono-label">{label}</span>
            <div className="flex items-center gap-2">
                <button
                    type="button"
                    onClick={() => setShown(s => !s)}
                    aria-label={shown ? `Hide ${label}` : `Reveal ${label}`}
                    aria-pressed={shown}
                    title={shown ? 'Hide' : 'Click to reveal'}
                    className="group flex-1 min-w-0 text-left rounded-md bg-(--base-02) border border-(--base-03) px-3 py-2 hover:border-(--base-04) transition-colors cursor-pointer"
                >
                    <code
                        className={`input-mono block break-all text-xs text-(--base-08) transition-[filter] motion-reduce:transition-none ${
                            shown ? 'select-all' : 'blur-[5px] select-none'
                        }`}
                    >
                        {value}
                    </code>
                </button>
                <CopyButton value={value} />
            </div>
            {note && <p className="text-xs text-(--base-07)">{note}</p>}
        </div>
    );
}

/**
 * A copyable code block. Used for both the compose file and the CLI steps.
 *
 * The title is the FILE NAME the reader has to save the block as, so it is set
 * like a heading rather than like a caption: it used to render in the same
 * small grey as every other label on the page, which made the one word they
 * have to type the hardest one to read.
 */
export function Snippet({ title, body, note }: { title: string; body: string; note?: string }) {
    return (
        <div className="space-y-1.5">
            <div className="flex items-center justify-between gap-2">
                <span className="font-mono text-sm font-medium text-(--base-09)">{title}</span>
                <CopyButton value={body} />
            </div>
            {note && <p className="text-xs text-(--base-07)">{note}</p>}
            {/* Bounded and scrollable. The node compose file is ~60 lines of
                commented YAML: at full height it pushed everything below it off
                the screen and made the two columns on this page different
                lengths for no reason anyone chose. Nobody reads it top to bottom
                anyway - it is copied. */}
            <pre className="max-h-72 overflow-auto overscroll-contain p-3 rounded-md bg-(--base-02) border border-(--base-03) font-mono text-[11px] leading-relaxed text-(--base-08)">
                {body}
            </pre>
        </div>
    );
}

/**
 * The deploy instructions for one machine: what to run, in what order, and what
 * to check afterwards.
 *
 * `warpKey` is null whenever the secret is not (or no longer) available - it is
 * stored as a hash, so it is shown exactly once at mint time. The snippet then
 * carries an obvious placeholder instead of a plausible-looking wrong value.
 */
/**
 * What differs about the chosen machine, in one sentence.
 *
 * Four combinations and no shared sentence between them: on Linux the answer is
 * about privileges, and on Docker Desktop it is about the WSL2 VM - which means
 * something different to a route-only link (where your own server is elsewhere)
 * than to a node (where your servers run inside that VM with it).
 */
export function platformNote(kind: 'node' | 'route-only', platform: DeployPlatform): string {
    if (platform === 'linux') {
        return kind === 'node'
            ? 'The node drives the host’s Docker socket to run your servers, which needs host networking; warp needs NET_ADMIN for the tunnel.'
            : 'Host networking lets the link reach your server on this machine or your LAN. It needs no extra privileges and opens no port.';
    }
    return kind === 'node'
        ? 'Host networking on Docker Desktop joins the WSL2 VM, not Windows. Your servers run inside that VM alongside the node, so bind a Windows path in the snippet if you want the files where you can see them.'
        : 'Host networking on Docker Desktop joins the WSL2 VM, not Windows, so the snippet points the link at host.docker.internal. Your Minecraft server keeps running on Windows as it does now.';
}

const PLATFORM_KEY = 'dylaris:deployPlatform';

/**
 * The target machine the reader picked, remembered per browser.
 *
 * It reset to Linux on every visit, and on the route-only file the wrong tab is
 * not cosmetic: it decides LOCAL_HOST, and left on Linux on Docker Desktop the
 * link dials the WSL2 VM's loopback while the server runs on Windows. Nothing
 * in the panel said so - only a dial error in the container log.
 *
 * Read in an effect, as SidebarCollapse does: the server render has no storage,
 * and reading during render would not match it.
 */
export function useDeployPlatform(): [DeployPlatform, (p: DeployPlatform) => void] {
    const [platform, setPlatformState] = useState<DeployPlatform>('linux');
    useEffect(() => {
        try {
            const v = localStorage.getItem(PLATFORM_KEY);
            if (v === 'linux' || v === 'windows') setPlatformState(v);
        } catch { /* private mode: the default stands */ }
    }, []);
    const setPlatform = useCallback((p: DeployPlatform) => {
        setPlatformState(p);
        try {
            localStorage.setItem(PLATFORM_KEY, p);
        } catch { /* private mode: the choice lasts this page only */ }
    }, []);
    return [platform, setPlatform];
}

export function DeployKit({ kind, ...props }: { kind: 'node' | 'route-only' } & KitProps) {
    // Both kinds run on Docker Desktop. The node was Linux-only here for longer
    // than it needed to be: it drives the host's Docker socket, and on Docker
    // Desktop that socket, the tunnel and the Minecraft containers are all
    // inside the same WSL2 VM - so they reach each other exactly as on Linux.
    // What genuinely differs is where the server FILES land, which the snippet
    // says in the place it matters, at the bind mount.
    const [platform, setPlatform] = useDeployPlatform();
    // Spread, never field by field: listing the props here by hand is exactly
    // how linkBesideNode went missing and every customer file came out without
    // its link service. KitProps is kitInput's own parameter type, so a new
    // field cannot be forgotten on the way in. See kitInput.
    const input = kitInput({ ...props, platform });
    const compose = kind === 'node' ? nodeCompose(input) : routeOnlyCompose(input);

    return (
        <div className="space-y-3 rounded-md border border-(--base-03) bg-(--base-01) p-4">
            <div className="flex items-center gap-2 text-sm font-medium text-(--base-09)">
                <Terminal size={15} className="text-(--accent-light)" />
                Deploy it on your machine
            </div>
            <div className="flex items-center gap-1" role="group" aria-label="Target machine">
                {(['linux', 'windows'] as const).map((p) => (
                    <button
                        key={p}
                        type="button"
                        onClick={() => setPlatform(p)}
                        aria-pressed={platform === p}
                        className={`rounded-md px-2.5 py-1 text-xs transition-colors focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)] ${
                            platform === p
                                ? 'bg-(--accent) text-(--base-00)'
                                : 'bg-(--base-02) text-(--base-07) hover:bg-(--base-03) hover:text-(--base-09)'
                        }`}
                    >
                        {p === 'linux' ? 'Linux' : 'Windows (Docker Desktop)'}
                    </button>
                ))}
            </div>
            <p className="text-xs text-(--base-06)">{platformNote(kind, platform)}</p>
            {/* Docker Desktop used to be held back from this shape, and said so
                here. It no longer is: the Link works the overlay proxy's address
                out from the network it is on, which is inside the VM alongside
                warp. Saying nothing beats saying the opposite of what the file
                above now contains. */}
            <Snippet title={composeFileName(kind)} body={compose} />
            <p className="text-xs text-(--base-06)">{watchtowerNote(kind)}</p>
            <Snippet title="Commands" body={deployCli(kind)} note={deployIntro(kind, platform)} />
            <p className="text-xs text-(--base-06)">{DEPLOY_PORTAINER_NOTE}</p>
        </div>
    );
}

/** Shown in place of a tab's controls when the account does not include it. */
export function NotIncluded({ what, storeUrl, suspended, storeLinked }: {
    what: string;
    storeUrl: string | null;
    suspended: boolean;
    /** null = not known yet. See below for why that is not the same as false. */
    storeLinked?: boolean | null;
}) {
    // Three different problems with three different fixes, and telling someone
    // to buy a plan they already bought because the ACCOUNTS are not joined is
    // the worst of them: the store shows a paid subscription, the panel shows
    // nothing, and neither screen mentions the join that is missing.
    //
    // storeLinked === null means we have not been told yet (or the storefront
    // could not be reached). That falls through to the generic message rather
    // than claiming a disconnection we cannot support.
    const notLinked = storeLinked === false;
    const hosted = useAppData().featureFlags.hosted;
    return (
        <div className="rounded-md border border-(--base-03) bg-(--base-02) p-4 space-y-2.5">
            <div className="flex items-center gap-2 text-sm font-medium text-(--base-08)">
                <Lock size={14} className="text-(--base-06)" />
                {suspended ? 'Paused' : notLinked ? 'Connect your account first' : 'Not on your account'}
            </div>
            <p className="text-sm text-(--base-07)">
                {suspended
                    ? 'Your account is suspended, which pauses this until it is reactivated.'
                    : notLinked
                      ? 'Your panel account is not connected to the Dylaris store yet. A subscription raises no limits here until it is, so connect it first and this unlocks on its own.'
                      : hosted
                        ? `Add ${what} to your plan, or ask an admin to enable it for you.`
                        : `${what.charAt(0).toUpperCase()}${what.slice(1)} is not enabled by the administrator.`}
            </p>
            {!suspended && notLinked && (
                <Link href="/account/store" className="btn btn-primary btn-sm inline-flex w-fit">
                    <LinkIcon size={13} /> Connect the store
                </Link>
            )}
            {!suspended && !notLinked && storeUrl && (
                <a href={storeUrl} target="_blank" rel="noopener noreferrer" className="btn btn-primary btn-sm inline-flex w-fit">
                    <ShoppingCart size={13} /> Get {what} <ExternalLink size={11} />
                </a>
            )}
        </div>
    );
}

/**
 * "3 of 5 in use", "3 in use" when there is no cap, "none allowed" at a cap of 0.
 *
 * The platform's limits convention: nothing (null / undefined) is no cap at all,
 * 0 is none, n is the cap - see services.Limits. This used to read `limit <= 0`
 * as "no cap", so the one number an operator can type to say "none" was shown to
 * the customer as unlimited, and the refusal arrived only as a 403 afterwards.
 */
export function usageLabel(used: number, limit: number | undefined | null): string {
    if (limit === undefined || limit === null || limit < 0) return `${used} in use`;
    if (limit === 0) return 'none allowed';
    return `${used} of ${limit} in use`;
}

/**
 * The managed gateway's link for one node: the compose file the operator runs
 * on the node's host.
 *
 * Fetched on open, never before: the first fetch mints the kit at the provider
 * and the answer carries the link's key. The key lives in this component's state
 * only, so closing the kit (unmounting it) is what forgets it.
 */
export function ManagedLinkKit({ nodeId }: { nodeId: number }) {
    const [kit, setKit] = useState<ManagedLinkKitData | null>(null);
    const [error, setError] = useState('');
    const [loading, setLoading] = useState(true);
    const [slow, setSlow] = useState(false);
    const [network, setNetwork] = useState('');
    const [retiring, setRetiring] = useState(false);
    const [retireError, setRetireError] = useState('');

    const replace = async () => {
        if (!(await confirmDialog(RETIRE_CONFIRM))) return;
        setRetiring(true);
        setRetireError('');
        const res = await retireManagedLink(nodeId);
        setRetiring(false);
        if (!res.success) { setRetireError(res.message); return; }
        setKit(k => (k ? { ...k, retireAt: res.retireAt } : k));
        toast("The node's link is being replaced.");
    };

    useEffect(() => {
        let cancelled = false;
        const t = setTimeout(() => setSlow(true), 400);
        getManagedLink(nodeId).then(res => {
            if (cancelled) return;
            if (res.success) {
                setKit(res);
                // Core cannot see the node's Docker network, so its answer is a
                // guess to start the field with, not a value.
                setNetwork(res.networkKnown && res.network ? res.network : (res.networkHint ?? ''));
            } else {
                // Core's own sentence; the module falls back to ours when it sent none.
                setError(res.message);
            }
            setLoading(false);
        });
        return () => { cancelled = true; clearTimeout(t); };
    }, [nodeId]);

    if (loading) {
        return slow ? (
            <div className="space-y-3" aria-busy="true">
                <Skeleton className="h-5 w-40" />
                <Skeleton className="h-40 w-full" />
            </div>
        ) : null;
    }
    if (error || !kit) {
        return (
            <div className="alert alert-error text-xs" role="alert">
                <CircleAlert size={14} className="shrink-0 mt-0.5" />
                <span>{error || 'The deploy kit could not be loaded.'}</span>
            </div>
        );
    }

    const known = !!kit.networkKnown && !!kit.network;
    const net = network.trim();
    const netOk = isDockerNetworkName(net);
    const state = linkState(kit.online);
    const hintId = `managed-link-net-hint-${nodeId}`;
    const warnId = `managed-link-net-warn-${nodeId}`;
    const describedBy = [!netOk && warnId, !known && hintId].filter(Boolean).join(' ') || undefined;

    return (
        <div className="space-y-3">
            <div className="flex items-center gap-2 text-sm text-(--base-08)" role="status">
                <LinkBadge state={state} />
                <span>{state.label}</span>
            </div>
            {kit.retireAt && (
                <div className="alert alert-warning text-xs" role="status">
                    <CircleAlert size={14} className="shrink-0 mt-0.5" />
                    <span>{retiringLine(kit.retireAt, v => fmtDateTime(v))} Move this node&apos;s servers to another node before then.</span>
                </div>
            )}
            {canReplaceLink(kit) && (
                <div className="flex flex-wrap items-center gap-3">
                    <button
                        type="button"
                        onClick={() => void replace()}
                        disabled={retiring}
                        className="btn btn-secondary btn-sm disabled:opacity-40 inline-flex items-center gap-1.5"
                    >
                        {retiring ? <Loader2 size={13} className="animate-spin motion-reduce:animate-none" /> : <RefreshCcw size={13} />}
                        Replace this node&apos;s link
                    </button>
                    {retireError && (
                        <span className="inline-flex items-start gap-1.5 text-xs text-(--error-light)" role="alert">
                            <CircleAlert size={13} className="shrink-0 mt-0.5" /><span>{retireError}</span>
                        </span>
                    )}
                </div>
            )}
            <p className="text-xs text-(--base-06)">Run this on the node&apos;s host. It connects the node to the gateway.</p>
            <div>
                <label className="input-label" htmlFor={`managed-link-net-${nodeId}`}>Docker network</label>
                <input
                    id={`managed-link-net-${nodeId}`}
                    type="text"
                    spellCheck={false}
                    readOnly={known}
                    value={network}
                    onChange={e => setNetwork(e.target.value)}
                    aria-invalid={!netOk}
                    aria-describedby={describedBy}
                    className={`input-field input-mono text-sm w-full ${netOk ? '' : 'input-field-error'} ${known ? 'opacity-60 cursor-default' : ''}`}
                />
                {/* Directly under the field, so the error sits where the eye is. */}
                {!netOk && (
                    <p id={warnId} className="text-xs text-(--error-light) mt-1.5">
                        Enter the name of a Docker network: letters, digits and _ . - only, no spaces. Words like true or null and numbers alone are not accepted. The name usually ends in <code className="font-mono">dylaris_net</code>.
                    </p>
                )}
                {!known && (
                    <p id={hintId} className="text-xs text-(--base-06) mt-1.5">
                        The network your game servers run on. Find it with: <code className="font-mono break-all">docker network ls --filter name=dylaris_net</code>
                    </p>
                )}
            </div>
            {/* No file without a valid network, so nothing that points at one. */}
            {netOk && (
                <>
                    <SecretField label="Link key" value={kit.linkKey} note="Already filled into the file below." />
                    <Snippet
                        title="managed-link.yml"
                        body={managedLinkCompose({ linkKey: kit.linkKey, coreUrl: kit.coreUrl, network: net, image: LINK_IMAGE })}
                    />
                    <Snippet title="Commands" body="docker compose -f managed-link.yml up -d" />
                </>
            )}
        </div>
    );
}
