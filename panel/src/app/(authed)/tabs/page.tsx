"use client";

import { Suspense, useCallback, useEffect, useMemo, useState } from 'react';
import { useRouter, useSearchParams } from 'next/navigation';
import { AlertTriangle, ChevronDown, ChevronRight, LayoutDashboard, Search, ShieldCheck } from 'lucide-react';
import { navGroups, parseTabParams, tabHref, type NavGroup } from '@/lib/customTabsNav';
import { listServerTabs, mintTabProxyAuth, type ServerTab } from '@/lib/api/serverTabs';
import { tabContentSrc, tabRunsOnActiveSubServer } from '@/lib/tabProxy';
import { useAppData } from '@/lib/AppDataContext';
import { DynamicIcon } from '@/lib/icons';
import { Skeleton } from '@/components/Skeleton';
import { systemEvents } from '@/lib/systemEvents';

// Every proxied custom tab the viewer may see, full width, grouped by server.
//
// The per-server tab page still exists and still works; this is where the same
// content gets the whole window instead of the strip left over beside the server
// navigation, which is what most of these are actually for - a map, a dashboard,
// a status board.
//
// The server list comes from AppDataContext, which is the SAME list the Servers
// page renders and therefore the same authorization Core already applied. That
// is deliberate: a second surface onto the same data with a query of its own is
// how this repository has produced the same defect three times (SFTP, players,
// the tab share plane). There is no server query in this file.

// Matches the in-dashboard tab page: the ticket is short-lived (~5min
// server-side), so re-mint comfortably inside that window while a tab is open.
const PROXY_AUTH_REFRESH_MS = 4 * 60 * 1000;

type TabsByServer = Record<number, ServerTab[]>;

// Collapsed server groups, per browser. A convenience only, so storage that is
// missing or refuses is not an error.
const COLLAPSED_KEY = 'dyl.tabs.collapsed';
function readCollapsed(): Set<number> {
    try { return new Set(JSON.parse(localStorage.getItem(COLLAPSED_KEY) || '[]') as number[]); } catch { return new Set(); }
}
function writeCollapsed(ids: Set<number>) {
    try { localStorage.setItem(COLLAPSED_KEY, JSON.stringify([...ids])); } catch { /* not kept */ }
}

const GROUP_LABEL: Record<NavGroup, string> = { mine: 'My servers', invited: 'Invited', admin: 'All servers (admin)' };

// useSearchParams needs a Suspense boundary under the static export.
export default function CustomTabsPage() {
    return (
        <Suspense fallback={<div className="flex-1 p-4"><Skeleton className="w-full h-full rounded" /></div>}>
            <CustomTabsInner />
        </Suspense>
    );
}

function CustomTabsInner() {
    const { servers, coreInfo, user } = useAppData();
    const router = useRouter();
    const searchParams = useSearchParams();
    const isAdmin = !!user?.isAdmin;
    const [tabsByServer, setTabsByServer] = useState<TabsByServer>({});
    const [loading, setLoading] = useState(true);
    const [adminMode, setAdminMode] = useState(false);
    const [query, setQuery] = useState('');
    const [collapsed, setCollapsed] = useState<Set<number>>(() => new Set());
    useEffect(() => { setCollapsed(readCollapsed()); }, []);
    const toggleCollapsed = (id: number) => setCollapsed(prev => {
        const next = new Set(prev);
        if (next.has(id)) next.delete(id); else next.add(id);
        writeCollapsed(next);
        return next;
    });
    const [authState, setAuthState] = useState<'pending' | 'ready' | 'error'>('pending');
    const [authError, setAuthError] = useState<string | null>(null);

    // The dependency is the ID LIST, not the servers array.
    //
    // AppDataContext replaces that array on every servers.changed event, which
    // fires on any status flip - a single server starting emits several. Keying
    // the reload on the array identity therefore re-fetched every server's tabs
    // several times per start, one request each. A tab list does not change when
    // a server's status does; it changes on server_tabs.changed, subscribed
    // below.
    const serverIds = useMemo(() => servers.map(s => s.id), [servers]);
    const serverKey = serverIds.join(',');

    // One request per server, in parallel. A tab list is a small read and the
    // sidebar has to show which servers have tabs before anything is clicked;
    // loading on expand instead would leave every group looking identical until
    // opened. If a fleet ever makes this heavy, the fix is a single
    // list-my-tabs endpoint, not lazier groups.
    const load = useCallback(async (ids: number[]) => {
        if (ids.length === 0) {
            setTabsByServer({});
            setLoading(false);
            return;
        }
        const lists = await Promise.all(ids.map(id => listServerTabs(id).catch(() => [] as ServerTab[])));
        const next: TabsByServer = {};
        ids.forEach((id, i) => {
            const proxied = lists[i].filter(t => t.enabled && t.mode === 'proxied' && t.surface !== 'page');
            if (proxied.length > 0) next[id] = proxied;
        });
        setTabsByServer(next);
        setLoading(false);
    }, []);

    useEffect(() => { load(serverIds); }, [serverKey, load]);

    useEffect(() => {
        const unsub = systemEvents.on('server_tabs.changed', () => { load(serverIds); });
        return () => { unsub(); };
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [serverKey, load]);


    // A tab pinned to a sub-server that is not the running one addresses a port
    // that only exists while that sub-server is up, and Core refuses to serve it.
    // Filtered HERE rather than in load() because the pin is compared against
    // live server state: switching sub-servers has to move a tab in and out of
    // this list without refetching anything.
    const visibleByServer = useMemo(() => {
        const out: TabsByServer = {};
        for (const s of servers) {
            const list = (tabsByServer[s.id] ?? []).filter(t => tabRunsOnActiveSubServer(t.subServerName, s.activeSubServer));
            if (list.length > 0) out[s.id] = list;
        }
        return out;
    }, [servers, tabsByServer]);

    // The tab is the URL's (?server=&tab=), so a refresh or a shared panel
    // link opens it again. Only a tab from the reader's own list can be
    // selected: a link to anything else answers "not found", never a frame.
    const wantedServer = searchParams.get('server');
    const wantedTab = searchParams.get('tab');
    const wanted = useMemo(() => parseTabParams(wantedServer, wantedTab), [wantedServer, wantedTab]);
    const selected: ServerTab | null = useMemo(
        () => (wanted ? (visibleByServer[wanted.serverId] ?? []).find(t => t.id === wanted.tabId) ?? null : null),
        [wanted, visibleByServer],
    );
    const wantedMissing = !!wanted && !loading && !selected;
    // A link into another user's server opens the admin group it lives in.
    useEffect(() => {
        if (!wanted || !isAdmin) return;
        const s = servers.find(x => x.id === wanted.serverId);
        if (s?.role === 'admin' && !s.invitedHere) setAdminMode(true);
    }, [wanted, isAdmin, servers]);
    const select = (serverId: number, tab: ServerTab) => router.push(tabHref(serverId, tab.id));

    const groups = useMemo(
        () => navGroups(servers, id => visibleByServer[id] ?? [], query, adminMode),
        [servers, visibleByServer, query, adminMode],
    );
    const anyListed = groups.mine.length + groups.invited.length + groups.admin.length > 0;

    // Mint the ticket for the selected tab before its frame is allowed to load,
    // then keep it fresh. A failing background refresh must never tear down a
    // frame that is already working, so only the first mint may show the error.
    useEffect(() => {
        const tab = selected;
        if (!tab?.proxyOrigin) return;
        let cancelled = false;
        setAuthState('pending');
        setAuthError(null);

        const mint = async (isInitial: boolean) => {
            const res = await mintTabProxyAuth(tab);
            if (cancelled) return;
            if (res.success) { setAuthState('ready'); return; }
            if (!isInitial) return;
            setAuthState('error');
            setAuthError(res.message || 'Failed to authorize this tab.');
        };
        mint(true);
        const interval = setInterval(() => mint(false), PROXY_AUTH_REFRESH_MS);
        return () => { cancelled = true; clearInterval(interval); };
    }, [selected?.proxyOrigin]);

    if (!coreInfo?.tabProxyAvailable) {
        return (
            <main className="flex-1 flex items-center justify-center p-6">
                <div className="card p-6 max-w-md text-center space-y-2">
                    <AlertTriangle size={20} className="text-(--warning-light) mx-auto" />
                    <p className="text-sm text-(--base-07)">
                        Proxied tabs are not available on this instance. An admin has to give Core a
                        proxy host before a tab can be served.
                    </p>
                </div>
            </main>
        );
    }

    return (
        <div className="flex-1 flex min-h-0">
            <nav aria-label="Custom tabs" className="w-64 shrink-0 border-r border-(--base-03) bg-(--base-01) overflow-y-auto flex flex-col">
                <div className="px-4 pt-4 pb-2 flex items-center justify-between gap-2">
                    <h1 className="text-xs font-medium uppercase tracking-wide text-(--base-06)">Custom tabs</h1>
                    {isAdmin && (
                        <button
                            type="button"
                            onClick={() => setAdminMode(m => !m)}
                            aria-pressed={adminMode}
                            title="Show every server's tabs"
                            className={`btn text-xs px-2.5 py-1 ${adminMode ? 'btn-primary' : 'btn-secondary'}`}
                        >
                            <ShieldCheck size={12} /> Admin
                        </button>
                    )}
                </div>
                <div className="px-3 pb-2">
                    <div className="relative">
                        <Search size={13} className="absolute left-2.5 top-1/2 -translate-y-1/2 text-(--base-06) pointer-events-none" aria-hidden="true" />
                        <input
                            value={query}
                            onChange={e => setQuery(e.target.value)}
                            placeholder={adminMode ? 'Server, user, UUID or tab' : 'Server or tab'}
                            aria-label="Search custom tabs"
                            className="input-field input-icon-left w-full text-xs py-1.5"
                        />
                    </div>
                </div>
                {loading && (
                    <div className="px-4 space-y-2">
                        <Skeleton className="h-4 w-32 rounded" />
                        <Skeleton className="h-4 w-24 rounded" />
                        <Skeleton className="h-4 w-28 rounded" />
                    </div>
                )}
                {!loading && !anyListed && (
                    <p className="px-4 py-3 text-xs text-(--base-06)">
                        {query.trim()
                            ? 'Nothing matches the search.'
                            : <>None of your servers has a proxied tab yet. Add one under a server&apos;s Config &rarr; Tabs.</>}
                    </p>
                )}
                {!loading && (['mine', 'invited', 'admin'] as NavGroup[]).map(g => groups[g].length > 0 && (
                    <div key={g} className="pb-1">
                        {(groups.invited.length > 0 || groups.admin.length > 0) && (
                            <p className="px-4 pt-2 pb-1 mono-label text-(--base-05)">{GROUP_LABEL[g]}</p>
                        )}
                        {groups[g].map(server => {
                            const open = !collapsed.has(server.id) || !!query.trim();
                            const list = visibleByServer[server.id] ?? [];
                            return (
                                <section key={server.id} className="pb-1">
                                    <button
                                        type="button"
                                        onClick={() => toggleCollapsed(server.id)}
                                        aria-expanded={open}
                                        className="w-full flex items-center gap-1.5 px-3 py-1.5 text-xs font-medium text-(--base-07) hover:text-(--base-09) transition-colors focus-visible:outline-hidden focus-visible:shadow-(--focus-ring)"
                                        title={g === 'admin' && server.owner ? `${server.name} (${server.owner})` : server.name}
                                    >
                                        {open ? <ChevronDown size={12} className="shrink-0" /> : <ChevronRight size={12} className="shrink-0" />}
                                        <span className="truncate">{server.name}</span>
                                        {g === 'admin' && server.owner && <span className="truncate text-(--base-05) font-normal">{server.owner}</span>}
                                        <span className="ml-auto mono-label text-(--base-05)">{list.length}</span>
                                    </button>
                                    {open && (
                                        <ul>
                                            {list.map(tab => {
                                                const active = selected?.id === tab.id;
                                                return (
                                                    <li key={tab.id}>
                                                        <a
                                                            href={tabHref(server.id, tab.id)}
                                                            onClick={e => { e.preventDefault(); select(server.id, tab); }}
                                                            aria-current={active ? 'page' : undefined}
                                                            className={`w-full flex items-center gap-2 pl-7 pr-4 py-1.5 text-sm text-left transition-colors
                                                                focus-visible:outline-hidden focus-visible:shadow-(--focus-ring)
                                                                ${active
                                                                    ? 'bg-(--accent-dim) text-(--base-09) border-l-2 border-(--accent-light)'
                                                                    : 'text-(--base-07) border-l-2 border-transparent hover:bg-(--base-02) hover:text-(--base-09)'}`}
                                                        >
                                                            <DynamicIcon name={tab.icon || 'layout-dashboard'} size={13} className="shrink-0" />
                                                            <span className="truncate">{tab.name}</span>
                                                        </a>
                                                    </li>
                                                );
                                            })}
                                        </ul>
                                    )}
                                </section>
                            );
                        })}
                    </div>
                ))}
            </nav>

            <main className="flex-1 min-w-0 bg-(--base-01)">
                {!selected && (
                    <div className="h-full flex items-center justify-center p-6">
                        <div className="text-center space-y-2">
                            {wantedMissing ? (
                                <>
                                    <AlertTriangle size={22} className="text-(--warning-light) mx-auto" />
                                    <p className="text-sm text-(--base-07)">This tab does not exist, is not running, or you have no access to it.</p>
                                </>
                            ) : (
                                <>
                                    <LayoutDashboard size={22} className="text-(--base-05) mx-auto" />
                                    <p className="text-sm text-(--base-06)">Pick a tab to open it here.</p>
                                </>
                            )}
                        </div>
                    </div>
                )}
                {selected && authState === 'pending' && (
                    <div className="h-full p-4"><Skeleton className="w-full h-full rounded" /></div>
                )}
                {selected && authState === 'error' && (
                    <div className="h-full flex items-center justify-center p-6">
                        <div className="card p-6 max-w-md text-center space-y-2">
                            <AlertTriangle size={20} className="text-(--warning-light) mx-auto" />
                            <p className="text-sm text-(--base-07)">{authError}</p>
                        </div>
                    </div>
                )}
                {selected && authState === 'ready' && tabContentSrc(selected.proxyOrigin) && (
                    <iframe
                        // Keyed on the tab so switching swaps the document
                        // instead of navigating inside the previous container.
                        key={selected.id}
                        src={tabContentSrc(selected.proxyOrigin) as string}
                        title={selected.name}
                        className="w-full h-full border-0"
                        referrerPolicy="no-referrer"
                        sandbox="allow-scripts allow-same-origin allow-forms allow-popups allow-downloads"
                    />
                )}
            </main>
        </div>
    );
}
