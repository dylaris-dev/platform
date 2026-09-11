"use client";

import { useState } from 'react';
import Link from 'next/link';
import { Copy, Check, Terminal, Lock, ShoppingCart, ExternalLink, Link2 as LinkIcon } from 'lucide-react';
import {
    nodeCompose, routeOnlyCompose, deployCli, deployIntro, composeFileName,
    DEPLOY_PORTAINER_NOTE,
} from '@/lib/warpDeploy';
import type { DeployPlatform } from '@/lib/warpDeploy';
import type { WarpDeployConfig } from '@/lib/api/warpDeployConfig';

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
            ? 'The node drives the host’s Docker socket to run your servers, which needs host networking and NET_ADMIN.'
            : 'The tunnel uses kernel WireGuard, which needs host networking and NET_ADMIN.';
    }
    return kind === 'node'
        ? 'Host networking on Docker Desktop joins the WSL2 VM, not Windows. Your servers run inside that VM alongside the node, so bind a Windows path in the snippet if you want the files where you can see them.'
        : 'Host networking on Docker Desktop joins the WSL2 VM, not Windows, so the snippet points the link at host.docker.internal. Your Minecraft server keeps running on Windows as it does now.';
}

export function DeployKit({ kind, warpKey, enrollUrl, nodeEnrollToken, grpcTlsFingerprint, nodeId, config, linkBesideNode }: {
    kind: 'node' | 'route-only';
    warpKey: string | null;
    enrollUrl: string;
    nodeEnrollToken?: string;
    grpcTlsFingerprint?: string;
    nodeId?: string;
    config?: WarpDeployConfig | null;
    /** See WarpDeployInput.linkBesideNode: only for a key bound (or about to be) to its machine. */
    linkBesideNode?: boolean;
}) {
    // Both kinds run on Docker Desktop. The node was Linux-only here for longer
    // than it needed to be: it drives the host's Docker socket, and on Docker
    // Desktop that socket, the tunnel and the Minecraft containers are all
    // inside the same WSL2 VM - so they reach each other exactly as on Linux.
    // What genuinely differs is where the server FILES land, which the snippet
    // says in the place it matters, at the bind mount.
    const [platform, setPlatform] = useState<DeployPlatform>('linux');
    const input = {
        apiKey: warpKey ?? '<your-warp-key>',
        enrollUrl,
        nodeEnrollToken,
        grpcTlsFingerprint,
        nodeId,
        platform,
        // Undetermined values stay undefined so the snippet keeps its
        // placeholder: a blank tells the reader something is missing, an empty
        // string looks like a setting that was deliberately cleared.
        tunnelSubnets: config?.tunnelSubnets || undefined,
        linkBesideNode,
    };
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
            {/* nodeCompose keeps the node-managed Link on Docker Desktop, so the
                reader must not go looking for a link service that is not there. */}
            {kind === 'node' && linkBesideNode && platform === 'windows' && (
                <p className="text-xs text-(--base-06)">
                    On Docker Desktop the node still starts the Link itself, so this file is the same as before.
                </p>
            )}
            <Snippet title={composeFileName(kind)} body={compose} />
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
                      : `Add ${what} to your plan, or ask an admin to enable it for you.`}
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

/** "3 of 5 in use", or "3 in use" when the plan sets no cap (limit <= 0). */
export function usageLabel(used: number, limit: number | undefined): string {
    if (limit === undefined || limit <= 0) return `${used} in use`;
    return `${used} of ${limit} in use`;
}
