'use client';

import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import Link from 'next/link';
import { usePathname, useRouter } from 'next/navigation';
import { Plus, Users, History, RefreshCw, Package, Search, PanelLeftClose, PanelLeftOpen, ChevronDown, ChevronRight, BookOpen, Star, Bell, Layers, PanelRight, X, Globe, Download, CircleAlert } from 'lucide-react';
import { clearModrinthMemberships, getPackDetails, listBuilds, listPacksOverview, type Pack, type PackBuild, type PackPermissions, type PacksOverview } from '@/lib/api/packs';
import { routeIdFrom } from '@/lib/routeParams';
import { currentPackBuild, newestPackBuild, packBuildStatus } from '@/lib/packWorkspace';
import { systemEvents } from '@/lib/systemEvents';
import { useAppData } from '@/lib/AppDataContext';
import { useSidebarCollapse } from '@/lib/SidebarCollapse';
import { SkeletonList } from '@/components/Skeleton';
import { toast } from '@/components/ui/Toast';
import PackActivity from '@/components/modpacks/PackActivity';
import PackRecommendation from '@/components/modpacks/PackRecommendation';
import ModrinthSyncDialog from '@/components/modpacks/ModrinthSyncDialog';
import { useModrinthAutoSync } from '@/components/modpacks/useModrinthAutoSync';
import HelpTourButton from '@/components/help/HelpTourButton';
import { ModrinthWatchDialog } from '@/components/mods/ModrinthLibrary';
import ProjectIcon, { useProjectMetadata } from '@/components/modpacks/ProjectIcon';
import ModalPanel from '@/components/ui/ModalPanel';
import styles from './workspace.module.css';

type Workspace = { pack: Pack | null; builds: PackBuild[]; permissions: PackPermissions; loading: boolean; refresh: () => Promise<void>; detailsRoot: HTMLDivElement | null };
const WorkspaceContext = createContext<Workspace | null>(null);
const noPermissions: PackPermissions = { canEdit: false, canDelete: false, isOwner: false, canEditPage: false, canDeletePack: false, tokenInvalid: false };
export function usePackWorkspace() { const value = useContext(WorkspaceContext); if (!value) throw new Error('Missing modpack workspace'); return value; }
export function WorkspaceDetails({ children }: { children: ReactNode }) { const { detailsRoot } = usePackWorkspace(); return detailsRoot ? createPortal(children, detailsRoot) : null; }

export default function PackWorkspace({ children }: { children: ReactNode }) {
    const pathname = usePathname() || '';
    const packId = Number(routeIdFrom(pathname, 'modpacks')) || 0;
    const buildId = Number(routeIdFrom(pathname, 'builds')) || 0;
    const creating = pathname.endsWith('/builds/new');
    const allBuilds = pathname === `/modpacks/${packId}` || pathname === `/modpacks/${packId}/`;
    const alertsSelected = pathname === `/modpacks/${packId}/alerts`;
    const modrinthSelected = pathname === `/modpacks/${packId}/modrinth`;
    const [detailsOpen, setDetailsOpen] = useState(false);
    const [refreshing, setRefreshing] = useState(false);
    const [watchOpen, setWatchOpen] = useState(false);
    const [syncOpen, setSyncOpen] = useState(false);
    useEffect(() => { setDetailsOpen(false); setWatchOpen(false); }, [pathname]);
    const { featureFlags, user } = useAppData();
    const { collapsed, toggle, width } = useSidebarCollapse();
    const [search, setSearch] = useState('');
    const [folded, setFolded] = useState<Record<string, boolean>>({});
    const [overview, setOverview] = useState<PacksOverview>({ packs: [], shared: [], modrinth: { connected: true, tokenInvalid: false } });
    const [summaries, setSummaries] = useState<Record<number, PackBuild[]>>({});
    const [pack, setPack] = useState<Pack | null>(null);
    const [builds, setBuilds] = useState<PackBuild[]>([]);
    const [permissions, setPermissions] = useState<PackPermissions>(noPermissions);
    const [loading, setLoading] = useState(true);
    const [detailsRoot, setDetailsRoot] = useState<HTMLDivElement | null>(null);
    const [clearing, setClearing] = useState(false);
    const router = useRouter();
    const [activity, setActivity] = useState(false);
    const generation = useRef(0);
    const allPacks = [...overview.packs, ...overview.shared];
    const metadata = useProjectMetadata(allPacks.map(p => p.modrinthProjectId));
    const refreshOverview = useCallback(async () => { setOverview(await listPacksOverview()); }, []);
    const refresh = useCallback(async () => {
        const requestGeneration = generation.current;
        if (!packId) { setPack(null); setBuilds([]); setLoading(false); return; }
        const [details, versions] = await Promise.all([getPackDetails(packId), listBuilds(packId)]);
        if (requestGeneration !== generation.current) return;
        setPack(details.pack); setPermissions(details.permissions); setBuilds(versions);
        setSummaries(previous => ({ ...previous, [packId]: versions }));
        setLoading(false);
    }, [packId]);
    useEffect(() => { refreshOverview(); }, [refreshOverview]);
    // A pack opened right after it was created (or shared) may predate the list; the live event can lag or be missed.
    // Once per pack: an admin inspecting another user's pack never finds it in their own list.
    const listChecked = useRef(0);
    useEffect(() => { if (pack && listChecked.current !== pack.id && !overview.packs.some(p => p.id === pack.id) && !overview.shared.some(p => p.id === pack.id)) { listChecked.current = pack.id; refreshOverview(); } }, [pack, overview, refreshOverview]);
    useEffect(() => { let live = true; generation.current++; setLoading(true); setPack(null); setBuilds([]); setPermissions(noPermissions); const load = async () => {
        if (!packId) { if (live) setLoading(false); return; }
        const [details, versions] = await Promise.all([getPackDetails(packId), listBuilds(packId)]);
        if (live) { setPack(details.pack); setPermissions(details.permissions); setBuilds(versions); setSummaries(previous => ({ ...previous, [packId]: versions })); setLoading(false); }
    }; load(); return () => { live = false; generation.current++; }; }, [packId]);
    // Bound concurrency for installations with many packs; do not issue one request per render.
    useEffect(() => { let live = true; let next = 0; const packs = [...overview.packs, ...overview.shared];
        const worker = async () => { while (live && next < packs.length) { const current = packs[next++]; const versions = await listBuilds(current.id); if (live) setSummaries(previous => ({ ...previous, [current.id]: versions })); } };
        Promise.all([worker(), worker(), worker()]); return () => { live = false; };
    }, [overview]);
    useEffect(() => { const dispose = ['packs.changed', 'pack_builds.changed'].map(event => systemEvents.on(event, () => { refreshOverview(); refresh(); })); return () => dispose.forEach(fn => fn()); }, [refresh, refreshOverview]);
    const canAudit = permissions.isOwner || user?.isAdmin;
    // Drops the packs the caller can no longer prove access to; this one may be among them.
    const clearLapsed = async () => {
        setClearing(true);
        try {
            const res = await clearModrinthMemberships();
            if (!res.success) { toast(res.message || 'Clear failed', false); return; }
            toast(res.removed ? `Removed ${res.removed} pack${res.removed === 1 ? '' : 's'} from your list.` : 'Nothing to clear.', true);
            await refreshOverview();
            router.push('/modpacks');
        } finally { setClearing(false); }
    };
    const autoSync = useModrinthAutoSync(pack, permissions.canEdit && featureFlags.modpacks, () => { void refresh(); });
    const alertSource = builds.find(build => build.id === pack?.recommendedBuildId) || newestPackBuild(builds.filter(build => build.channel === 'release'));
    return <WorkspaceContext.Provider value={{ pack, builds, permissions, loading, refresh, detailsRoot }}>
        <div className={styles.workspace}>
            <aside className={`${styles.packs} ${width}`} aria-label="Modpacks">
                <div className={`flex gap-1 p-2 ${collapsed ? 'flex-col' : 'items-center'}`}>
                    <Link href="/modpacks" title="Modpacks" className={`btn btn-secondary btn-sm ${collapsed ? 'justify-center' : 'flex-1 justify-start'}`}><Package size={15} />{!collapsed && 'Modpacks'}</Link>
                    {!collapsed && <Link href="/modpacks?create=1" className="btn btn-secondary btn-sm" aria-label="New pack"><Plus size={14} /></Link>}
                    <button onClick={toggle} className="btn btn-ghost btn-sm" aria-label={collapsed ? 'Expand modpack sidebar' : 'Collapse modpack sidebar'}>{collapsed ? <PanelLeftOpen size={15} /> : <PanelLeftClose size={15} />}</button>
                </div>
                {!collapsed && <div className="relative mx-3 mb-3"><Search size={13} className="absolute left-3 top-1/2 -translate-y-1/2 text-(--base-06)" /><input aria-label="Search modpacks" placeholder="Search modpacks..." className="input-field input-icon-left w-full text-xs" value={search} onChange={event => setSearch(event.target.value)} /></div>}
                <nav className={styles.packList}>{([{ key: 'own', label: 'My modpacks', packs: overview.packs }, { key: 'team', label: 'Modrinth team', packs: overview.shared }] as const).map(group => {
                    const matches = group.packs.filter(p => !search.trim() || `${p.internalName} ${p.summary}`.toLowerCase().includes(search.trim().toLowerCase()));
                    const open = !!search.trim() || !folded[group.key];
                    return <section key={group.key} aria-label={group.label} className="mb-4">
                        {!collapsed && <button onClick={() => setFolded(previous => ({ ...previous, [group.key]: !previous[group.key] }))} aria-expanded={open} className="flex items-center gap-2 w-full px-1 py-2 text-[11px] uppercase tracking-wider text-(--base-06)">{open ? <ChevronDown size={12} /> : <ChevronRight size={12} />}<span className="flex-1 text-left">{group.label}</span><span>{matches.length}</span></button>}
                        {(collapsed || open) && matches.map(p => { const current = currentPackBuild(summaries[p.id] || [],p.recommendedBuildId); return <Link key={p.id} href={`/modpacks/${p.id}`} title={p.internalName} className={`${styles.packLink} ${collapsed ? '!p-1 justify-center' : ''}`} aria-current={packId === p.id ? 'page' : undefined}>
                            <ProjectIcon name={p.internalName} url={metadata.projects[p.modrinthProjectId]?.icon_url} />
                            {!collapsed && <span className="min-w-0"><span className="text-sm font-medium text-(--base-09) block truncate">{p.internalName}</span><span className={`text-[11px] block mt-1 ${current?.id === p.recommendedBuildId ? 'text-(--accent-light)' : current?.channel === 'release' ? 'text-(--success-light)' : 'text-(--base-06)'}`}>{current ? `${current.versionString} · ${packBuildStatus(current, summaries[p.id] || [], p.recommendedBuildId)}` : 'No builds'}</span></span>}
                        </Link>; })}
                        {!collapsed && open && matches.length === 0 && <p className="text-xs text-(--base-06) px-2 py-3">{search ? 'No matching modpacks.' : group.key === 'own' ? 'Create your first modpack.' : 'Packs of Modrinth teams you are in appear here.'}</p>}
                    </section>;
                })}</nav>
                <div className={`${styles.packFooter} p-2 border-t border-(--base-03) space-y-1`}><Link href="/modpacks/guide" title="Modpack how-to" className="btn btn-ghost btn-sm w-full justify-start"><BookOpen size={14}/>{!collapsed && 'How to build a pack'}</Link><Link href="/modpacks/import" title="Import from Modrinth" aria-current={pathname === '/modpacks/import' ? 'page' : undefined} className="btn btn-ghost btn-sm w-full justify-start"><Download size={14}/>{!collapsed && 'Import from Modrinth'}</Link><Link href="/favorites" title="Favorites and version alerts" className="btn btn-ghost btn-sm w-full justify-start"><Star size={14}/>{!collapsed && 'Favorites & alerts'}</Link><button onClick={() => setActivity(true)} title="Activity" className="btn btn-ghost btn-sm w-full justify-start"><History size={14} />{!collapsed && 'Activity'}</button></div>
            </aside>
            <div className={styles.body}>
                {packId > 0 && <>
                    <header className={styles.heading}>
                        <ProjectIcon name={pack?.internalName || 'Modpack'} url={metadata.projects[pack?.modrinthProjectId || '']?.icon_url} className="w-11 h-11" />
                        <div className="min-w-0 flex-1"><p className="mono-label text-(--base-06)">Modpack workspace</p><h1 className="font-display text-lg font-semibold text-(--base-09) truncate">{loading ? 'Loading pack...' : pack?.internalName || 'Pack not found'}</h1>{autoSync.phase !== 'idle' && <p role="status" className={`text-[11px] ${autoSync.phase === 'failed' ? 'text-(--warning-light)' : 'text-(--accent-light)'}`}>{autoSync.phase === 'running' ? `Importing new Modrinth versions (${autoSync.done + 1} of ${autoSync.total})...` : autoSync.phase === 'done' ? `Imported ${autoSync.imported} new Modrinth version${autoSync.imported === 1 ? '' : 's'}.` : `Modrinth sync stopped at ${autoSync.message}`}</p>}<p className="text-xs text-(--base-06) truncate">{pack?.summary || 'Build, manage and share your modpack.'}</p></div>
                        <div className={styles.tools}>
                            {(buildId > 0 || creating || modrinthSelected) && <Link href={`/modpacks/${packId}`} className="btn btn-secondary btn-sm"><Layers size={13}/>All builds</Link>}
                            <button onClick={async () => { setRefreshing(true); try { await refresh(); } finally { setRefreshing(false); } }} disabled={refreshing} className="btn btn-ghost btn-sm"><RefreshCw size={13} className={refreshing ? 'animate-spin' : ''} />{refreshing ? 'Refreshing...' : 'Refresh'}</button>
                            {canAudit && <button onClick={() => setActivity(true)} className="btn btn-ghost btn-sm"><History size={13} />Activity</button>}
                            {buildId > 0 && permissions.canEdit && featureFlags.modpacks && <Link href={`/modpacks/${packId}/builds/new`} className="btn btn-primary btn-sm"><Plus size={13} />New build</Link>}
                            <HelpTourButton topic="modpacks" />
                            {pack && (buildId > 0 || creating || allBuilds) && <button onClick={() => setDetailsOpen(value => !value)} aria-expanded={detailsOpen} aria-controls="pack-details" className={`btn btn-secondary btn-sm ${styles.detailsToggle}`}><PanelRight size={14} />{buildId || creating ? 'Build details' : 'Pack details'}</button>}
                        </div>
                    </header>
                    {permissions.tokenInvalid && <div role="status" className="flex items-start gap-2 px-5 py-3 border-b border-(--base-03) bg-(--warning)/10 text-xs text-(--base-09)">
                        <CircleAlert size={15} className="text-(--warning) mt-0.5 shrink-0" />
                        <p className="flex-1">Modrinth no longer accepts your token, so this pack is read only. Renew the token to work on it again, or clear the packs you no longer have access to from your list.</p>
                        <Link href="/account/modrinth" className="btn btn-primary btn-sm">Renew token</Link>
                        <button onClick={clearLapsed} disabled={clearing} className="btn btn-secondary btn-sm disabled:opacity-40">{clearing ? 'Clearing...' : 'Clear'}</button>
                    </div>}
                    {/* Pack-level settings live on the pack overview; inside a build the content needs the height. */}
                    {pack && (allBuilds || alertsSelected) && <div className={styles.packToolbar} aria-label="Pack actions">
                        {builds.length > 0 && <div className={styles.recommendation}><PackRecommendation pack={pack} builds={builds} canEdit={permissions.canEdit && featureFlags.modpacks} onChanged={refresh} /></div>}
                        <div className={`${styles.tools} ml-auto`}>
                            {builds.length > 0 && <button onClick={() => setWatchOpen(true)} aria-haspopup="dialog" aria-expanded={watchOpen} className={`btn btn-sm ${alertsSelected || watchOpen ? 'btn-primary' : 'btn-secondary'}`}><Bell size={14} />Pack version alerts</button>}
                            {pack.modrinthProjectId && permissions.canEditPage && <Link href={`/modpacks/${packId}/modrinth`} className="btn btn-secondary btn-sm"><Globe size={14} />Modrinth page</Link>}
                            {pack.modrinthProjectId && permissions.canEdit && featureFlags.modpacks && <button onClick={() => setSyncOpen(true)} className="btn btn-secondary btn-sm" title="Import Modrinth versions this pack does not have yet"><RefreshCw size={14} />Sync from Modrinth</button>}
                            {pack.modrinthProjectId && <a href={`https://modrinth.com/project/${encodeURIComponent(pack.modrinthProjectId)}/settings/members`} target="_blank" rel="noopener noreferrer" className="btn btn-secondary btn-sm" title="Who may work on this pack is its Modrinth team"><Users size={14} />Team on Modrinth</a>}
                            {permissions.canEdit && featureFlags.modpacks && <Link href={`/modpacks/${packId}/builds/new`} aria-current={creating ? 'page' : undefined} className="btn btn-primary btn-sm"><Plus size={14} />New build</Link>}
                        </div>
                    </div>}
                </>}
                {packId > 0 && !featureFlags.modpacks && <p className="text-xs text-(--warning-light) px-5 py-3 border-b border-(--base-03)">Modpack authoring is disabled. Existing builds remain readable and downloadable.</p>}
                <div className={styles.columns}>
                    
                    <div className={styles.content} key={`${packId}:${buildId}:${creating}`}>{children}</div>
                    {pack && detailsOpen && <button className={styles.detailsBackdrop} onClick={() => setDetailsOpen(false)} aria-label="Close details" />}
                    {pack && (buildId > 0 || creating || allBuilds) && <aside id="pack-details" className={`${styles.details} ${detailsOpen ? styles.detailsOpen : ''}`} aria-label={buildId || creating ? 'Build details' : 'Pack details'}><button onClick={() => setDetailsOpen(false)} className={`btn btn-ghost btn-sm ${styles.detailsClose}`}><X size={14} />Close details</button><div ref={setDetailsRoot} className="flex flex-col shrink-0">{allBuilds && <><p className="mono-label text-(--base-06) mb-4">Pack details</p><dl className={styles.metadata}><div><dt>Builds</dt><dd>{builds.length}</dd></div><div><dt>Latest version</dt><dd>{currentPackBuild(builds,pack.recommendedBuildId)?.versionString || 'None'}</dd></div><div><dt>Modrinth</dt><dd>{pack.modrinthProjectId ? 'Linked' : 'Not linked'}</dd></div></dl></>}</div></aside>}
                </div>
            </div>
        </div>
        {syncOpen && pack && <ModrinthSyncDialog packId={pack.id} title={pack.internalName} onClose={imported => { setSyncOpen(false); if (imported) void refresh(); }} />}
        {watchOpen && pack && <ModrinthWatchDialog packId={pack.id} title={pack.internalName} loader={alertSource?.loader} onClose={() => setWatchOpen(false)} />}
        {activity && <div className="modal-overlay" onClick={() => setActivity(false)}><ModalPanel onClose={() => setActivity(false)} className="modal-panel max-w-2xl" onClick={event => event.stopPropagation()}><div className="modal-header flex items-center justify-between gap-3"><h2 className="modal-title">Modpack activity</h2><button onClick={() => setActivity(false)} className="btn btn-secondary btn-sm">Close</button></div><div className="modal-body">{!packId || canAudit ? <PackActivity packId={packId || undefined} showPack={!packId} /> : <p className="text-sm text-(--base-06)">Only team members who may delete the Modrinth project, and administrators, can view this pack&apos;s activity.</p>}</div></ModalPanel></div>}
    </WorkspaceContext.Provider>;
}
