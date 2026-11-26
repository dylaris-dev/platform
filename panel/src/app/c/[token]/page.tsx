"use client";

import { useEffect, useState } from 'react';

import { AlertTriangle } from 'lucide-react';
import { resolveShareLink } from '@/lib/api/serverTabs';
import { tabContentSrc } from '@/lib/tabProxy';
import { useRouteId } from '@/lib/routeParams';

// Standalone, Dylaris-branded page for a shared custom tab. Lives OUTSIDE the
// (authed) group so a public link loads without the panel's auth redirect.
//
// This page and the tab it frames are DIFFERENT ORIGINS on purpose. The page is
// ours; the frame is a tenant's container. Same-origin would let that container
// reach parent.document and rewrite the header below - swapping the link back to
// the panel for a phishing target, say - which is a smaller hole than reading
// the session token but the same kind of hole.
//
// The flow is deliberately short: resolve the token to its content host, then
// render the frame. That is all this page can do, and the reason is worth
// stating because it looks like something is missing.
//
// This page is served on the share host, NOT the panel host. Everything that
// identifies a visitor is scoped to the panel's origin - the session cookie is
// host-only, and storage is per-origin - so nothing here can prove who is
// looking, and a ticket cannot be minted from this page for a PRIVATE tab. A
// public link needs none and works. A private one gets Core's own "open it from
// the panel" card inside the frame, which is the honest answer and comes from
// the side that can actually tell.

type State = 'checking' | 'ready' | 'notfound' | 'expired';

export default function StandaloneTabProxyPage() {
    const paramToken = useRouteId('c');
    const token = String(paramToken || '');
    const [state, setState] = useState<State>('checking');
    const [contentOrigin, setContentOrigin] = useState('');

    useEffect(() => {
        let cancelled = false;

        (async () => {
            const res = await resolveShareLink(token);
            if (cancelled) return;
            if (!res.success || !res.data?.contentOrigin) {
                setState(res.status === 410 ? 'expired' : 'notfound');
                return;
            }
            setContentOrigin(res.data.contentOrigin);
            setState('ready');
        })();

        return () => { cancelled = true; };
    }, [token]);

    const iframeSrc = tabContentSrc(contentOrigin);

    return (
        <div className="flex flex-col h-screen bg-(--base-00) text-(--base-09) font-body overflow-hidden">
            <header className="shrink-0 h-12 bg-(--base-01) flex items-center px-4">
                <a
                    href="/"
                    className="rounded-md inline-flex items-center focus-visible:outline-hidden focus-visible:shadow-(--focus-ring)"
                >
                    <span className="text-xl font-logo tracking-widest select-none">
                        <span className="text-(--accent-light)">D</span>
                        <span className="text-(--base-09)">ylaris</span>
                    </span>
                </a>
                <span className="ml-3 text-xs text-(--base-06)">running through your Dylaris panel</span>
            </header>
            <main className="flex-1 overflow-hidden bg-(--base-01)">
                {state === 'checking' && (
                    <div className="h-full flex items-center justify-center text-(--base-06) text-sm">Loading...</div>
                )}
                {state === 'notfound' && (
                    <div className="h-full flex items-center justify-center p-6">
                        <div className="app-auth-card text-center">
                            <AlertTriangle size={20} className="text-(--warning-light) mx-auto mb-2" />
                            <p className="text-sm text-(--base-07)">This link is not valid.</p>
                        </div>
                    </div>
                )}
                {state === 'expired' && (
                    <div className="h-full flex items-center justify-center p-6">
                        <div className="app-auth-card text-center">
                            <AlertTriangle size={20} className="text-(--warning-light) mx-auto mb-2" />
                            <p className="text-sm text-(--base-07)">This share link has expired.</p>
                        </div>
                    </div>
                )}
                {state === 'ready' && iframeSrc && (
                    <iframe
                        src={iframeSrc}
                        title="Dylaris tab"
                        className="w-full h-full border-0"
                        referrerPolicy="no-referrer"
                        sandbox="allow-scripts allow-same-origin allow-forms allow-popups allow-downloads"
                    />
                )}
            </main>
        </div>
    );
}
