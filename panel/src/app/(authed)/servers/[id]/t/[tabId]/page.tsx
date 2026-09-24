"use client";

import React, { useEffect, useState } from 'react';

import { AlertTriangle, ExternalLink, Link2 } from 'lucide-react';
import { listServerTabs, mintTabProxyAuth, type ServerTab } from '@/lib/api/serverTabs';
import { systemEvents } from '@/lib/systemEvents';
import { tabContentSrc } from '@/lib/tabProxy';
import { tabHost } from '@/lib/tabHost';
import { useAppData } from '@/lib/AppDataContext';
import { Skeleton } from '@/components/Skeleton';
import { useRouteId } from '@/lib/routeParams';

// dynamic renderer for custom tabs. Loads the tab metadata, then either:
//  - direct: embeds the configured URL in an iframe (open_in_panel=true) or
//    shows a landing card with a popout button (false).
//  - proxied, surface tab|both: gets a ticket from Core on this origin, has the
//    tab's OWN host store it as that host's cookie
//    (core/handlers/tab_proxy_host.go HostMint), and then embeds that host. The
//    iframe src never carries a token, and the container runs on an origin that
//    is not the panel's, so its JavaScript reaches neither these pages nor the
//    session cookie, which is host-only to the panel's host.
//  - proxied, surface page: not embeddable here, points at the share link.
// Reacts to server_tabs.changed SSE so edits in another tab refresh here
// without reload.

// The dyl_tabproxy ticket cookie Core mints is short-lived (~5min,
// tabProxyTicketTTL server-side); re-mint comfortably inside that window so
// the cookie never expires out from under an in-flight sub-request while the
// tab stays open. The iframe src itself does not need to change on refresh -
// keeping the cookie fresh is enough for its ongoing sub-requests.
const PROXY_AUTH_REFRESH_MS = 4 * 60 * 1000;

type ProxyAuthState = 'pending' | 'ready' | 'error';

export default function ServerCustomTabPage() {
    const paramId = useRouteId('servers');
    const paramTabId = useRouteId('t');
    const serverId = Number(paramId);
    const tabId = Number(paramTabId);
    // Each proxied tab carries its own content origin, computed by Core from
    // the tab's host label. There is no same-origin fallback: without one there
    // is nowhere safe to serve the container, and the page says so.
    const { coreInfo } = useAppData();
    const [tab, setTab] = useState<ServerTab | null | undefined>(undefined);
    const [proxyAuth, setProxyAuth] = useState<ProxyAuthState>('pending');
    const [proxyAuthError, setProxyAuthError] = useState<string | null>(null);
    const [loadError, setLoadError] = useState<string | null>(null);

    const refresh = async () => {
        try {
            const list = await listServerTabs(serverId);
            setTab(list.find(t => t.id === tabId) || null);
            setLoadError(null);
        } catch (e) {
            // null here would render "tab not found", which is a statement about
            // the tab. A request that failed is a statement about the request.
            setLoadError(e instanceof Error ? e.message : 'Could not load this tab.');
        }
    };

    useEffect(() => { refresh(); /* eslint-disable-next-line react-hooks/exhaustive-deps */ }, [serverId, tabId]);

    useEffect(() => {
        const unsub = systemEvents.on('server_tabs.changed', (evt) => {
            const sid = (evt.payload as any)?.serverId;
            if (sid === undefined || sid === serverId) refresh();
        });
        return () => { unsub(); /* eslint-disable-next-line react-hooks/exhaustive-deps */ };
    }, [serverId]);

    // Embeddable-proxied tab: mint the dyl_tabproxy cookie before the iframe
    // is allowed to load, then keep it fresh on an interval for as long as
    // this page stays mounted. Every other branch (not-found/disabled/
    // page-only/direct) leaves isEmbeddedProxy false, so no proxy-auth call
    // is ever made for a tab that isn't actually going to be proxy-embedded.
    // A proxied tab with no content origin is NOT embeddable: the feature is
    // unconfigured on this deployment, or this row predates its host. Refusing
    // here is what keeps the mint from being attempted against an empty URL.
    const isEmbeddedProxy = !!tab && tab.enabled && tab.mode === 'proxied' && tab.surface !== 'page' && !!tab.proxyOrigin;
    useEffect(() => {
        if (!isEmbeddedProxy) return;
        let cancelled = false;
        setProxyAuth('pending');
        setProxyAuthError(null);

        // isInitial distinguishes the first mint (before the iframe has ever
        // rendered) from the periodic background refresh. Only the initial
        // mint may flip the UI into the error card - a background refresh
        // that fails (transient network blip, brief backend hiccup) must
        // never tear down an iframe that is already loaded and working, so
        // it just leaves the state alone and lets the next interval tick retry.
        const mint = async (isInitial: boolean) => {
            if (!tab) return;
            const res = await mintTabProxyAuth(tab);
            if (cancelled) return;
            if (res.success) {
                setProxyAuth('ready');
                return;
            }
            if (!isInitial) return;
            setProxyAuth('error');
            setProxyAuthError(res.message || 'Failed to authorize this tab.');
        };

        mint(true);
        const interval = setInterval(() => mint(false), PROXY_AUTH_REFRESH_MS);
        return () => { cancelled = true; clearInterval(interval); };
    }, [isEmbeddedProxy, tab?.proxyOrigin]);

    // Every root below is h-full rather than flex-1, and all four have to be.
    // ServerShell renders a page inside a scrolling BLOCK, so flex-1 gives this
    // main no height and the iframe's own h-full then resolves to auto. Measured
    // against the real class chain: 150px tall inside a 764px box, which is the
    // whole custom tab. With h-full the same iframe is 716px.
    if (tab === undefined) {
        return (
            <main className="h-full overflow-hidden bg-(--base-01) p-4">
                <Skeleton className="w-full h-full rounded" />
            </main>
        );
    }
    // Before the not-found branch, because "we could not ask" must never be
    // answered with "it does not exist" - somebody would go and recreate a tab
    // that is still there.
    if (loadError) {
        return (
            <main className="h-full flex items-center justify-center p-6">
                <div className="card p-6 max-w-md text-center">
                    <AlertTriangle size={20} className="text-(--warning-light) mx-auto mb-2" />
                    <p className="text-sm text-(--base-07)">{loadError}</p>
                    <button type="button" onClick={refresh} className="btn btn-secondary btn-sm mt-3">
                        Try again
                    </button>
                </div>
            </main>
        );
    }
    if (tab === null) {
        return (
            <main className="flex-1 flex items-center justify-center p-6">
                <div className="card p-6 max-w-md text-center">
                    <AlertTriangle size={20} className="text-(--warning-light) mx-auto mb-2" />
                    <p className="text-sm text-(--base-07)">Tab not found. It may have been deleted.</p>
                </div>
            </main>
        );
    }
    if (!tab.enabled) {
        return (
            <main className="flex-1 flex items-center justify-center p-6">
                <div className="card p-6 max-w-md text-center">
                    <p className="text-sm text-(--base-07)">This tab is disabled.</p>
                </div>
            </main>
        );
    }

    if (tab.mode === 'proxied') {
        if (tab.surface === 'page') {
            // Published as a standalone page only - not embeddable here.
            return (
                <main className="flex-1 flex items-center justify-center p-6">
                    <div className="card p-6 max-w-md text-center space-y-2">
                        <Link2 size={20} className="text-(--accent-light) mx-auto" />
                        <p className="text-sm text-(--base-07)">
                            {tab.name} is published as a standalone page. Open it via its share link.
                        </p>
                    </div>
                </main>
            );
        }

        if (proxyAuth === 'pending') {
            return (
                <main className="h-full overflow-hidden bg-(--base-01) p-4">
                    <Skeleton className="w-full h-full rounded" />
                </main>
            );
        }
        if (proxyAuth === 'error') {
            return (
                <main className="flex-1 flex items-center justify-center p-6">
                    <div className="card p-6 max-w-md text-center">
                        <AlertTriangle size={20} className="text-(--warning-light) mx-auto mb-2" />
                        <p className="text-sm text-(--base-07)">{proxyAuthError}</p>
                    </div>
                </main>
            );
        }

        // proxyAuth === 'ready': the dyl_tabproxy cookie is minted and
        // path-scoped to exactly this proxy prefix, so the src below never
        // needs (and never gets) a token of its own.
        return (
            <main className="h-full overflow-hidden bg-(--base-01)">
                <iframe
                    src={tabContentSrc(tab.proxyOrigin) || undefined}
                    title={tab.name}
                    className="w-full h-full border-0"
                    referrerPolicy="no-referrer"
                    sandbox="allow-scripts allow-same-origin allow-forms allow-popups allow-downloads"
                />
            </main>
        );
    }

    if (!tab.openInPanel) {
        // Popout-style — surface the link instead of embedding it. Some
        // sites refuse to load in iframes via X-Frame-Options; this gives
        // the operator a clean way to declare that without ugly error pages.
        return (
            <main className="flex-1 flex items-center justify-center p-6">
                <div className="card p-6 max-w-md text-center space-y-3">
                    <p className="text-sm text-(--base-07)">{tab.name} opens in a new window.</p>
                    <ExternalOriginLine url={tab.url} />
                    <a
                        href={tab.url}
                        target="_blank"
                        rel="noopener noreferrer"
                        className="btn btn-primary inline-flex"
                    >
                        <ExternalLink size={13} />
                        Open {tab.name}
                    </a>
                </div>
            </main>
        );
    }

    return (
        // flex column rather than a bare h-full box: the notice takes its own
        // height and the frame takes the rest. See the h-full note above, a
        // frame sized with flex-1 alone inside this shell collapses to 150px.
        <main className="h-full overflow-hidden bg-(--base-01) flex flex-col">
            <ExternalOriginBar url={tab.url} />
            {/* sandbox kept permissive so JS-heavy minimap viewers function;
                referrer-policy keeps URLs from leaking the panel surface */}
            <iframe
                src={tab.url}
                title={tab.name}
                className="w-full flex-1 min-h-0 border-0"
                referrerPolicy="no-referrer"
                sandbox="allow-scripts allow-same-origin allow-forms allow-popups allow-downloads"
            />
        </main>
    );
}

// ExternalOriginBar names whose page fills the frame below it.
//
// A custom tab's URL is not necessarily the owner's: tabs.write is part of the
// "Server admin" preset, so anyone they delegated that to can point a tab
// anywhere, and the page then renders full width with scripts and forms
// enabled while the address bar still reads the panel's own domain. Nothing on
// screen said where the content came from, which is the whole ingredient list
// for a convincing "your session expired, sign in again" page.
//
// It is deliberately shown for EVERY direct tab, including the owner's own
// BlueMap. A warning that appears only on pages somebody decided were
// suspicious teaches people to trust its absence.
function ExternalOriginBar({ url }: { url: string }) {
    const host = tabHost(url);
    return (
        <div className="shrink-0 flex items-center gap-2 px-3 py-1.5 border-b border-(--base-03) bg-(--base-02)">
            <AlertTriangle size={13} className="text-(--warning-light) shrink-0" />
            {/* The host is its own element, and the sentence is what truncates.
                With both in one line the narrow-screen ellipsis fell on the one
                word the line exists to show. */}
            {host && (
                <span className="text-xs font-mono text-(--base-08) shrink-0 max-w-[45%] truncate" title={host}>
                    {host}
                </span>
            )}
            <p className="text-xs text-(--base-06) min-w-0 truncate">
                {host
                    ? 'is not part of DYLARIS. Never enter your password in it.'
                    : 'This page is loaded from another site and is not part of DYLARIS. Never enter your password in it.'}
            </p>
            <a
                href={url}
                target="_blank"
                rel="noopener noreferrer"
                className="ml-auto shrink-0 hidden sm:inline-flex items-center gap-1 text-xs text-(--accent-light) hover:underline focus-visible:underline"
            >
                Open directly
                <ExternalLink size={11} />
            </a>
        </div>
    );
}

// The same statement for the popout card, where there is no frame to sit above.
function ExternalOriginLine({ url }: { url: string }) {
    const host = tabHost(url);
    return (
        <p className="text-xs text-(--base-06)">
            {host
                ? <>It is hosted at <span className="font-mono text-(--base-08)">{host}</span> and is not part of DYLARIS.</>
                : <>It is hosted elsewhere and is not part of DYLARIS.</>}
        </p>
    );
}
