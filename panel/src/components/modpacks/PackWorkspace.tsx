'use client';

import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import Link from 'next/link';
import { usePathname } from 'next/navigation';
import { Plus, Users, History, RefreshCw, Package, Search, PanelLeftClose, PanelLeftOpen, ChevronDown, ChevronRight } from 'lucide-react';
import { getPackDetails, listBuilds, listPacksOverview, type Pack, type PackBuild, type PackPermissions, type PacksOverview } from '@/lib/api/packs';
import { routeIdFrom } from '@/lib/routeParams';
import { currentPackBuild, packBuildStatus } from '@/lib/packWorkspace';
import { systemEvents } from '@/lib/systemEvents';
import { useAppData } from '@/lib/AppDataContext';
import { useSidebarCollapse } from '@/lib/SidebarCollapse';
import { fmtDate } from '@/lib/time';
import { SkeletonList } from '@/components/Skeleton';
import PackCollaboratorsModal from '@/components/modpacks/PackCollaboratorsModal';
import PackActivity from '@/components/modpacks/PackActivity';
import ProjectIcon, { useProjectMetadata } from '@/components/modpacks/ProjectIcon';
import ModalPanel from '@/components/ui/ModalPanel';
import { Badge } from '@/components/ui/Badge';
import styles from './workspace.module.css';

type Workspace = { pack: Pack | null; builds: PackBuild[]; permissions: PackPermissions; loading: boolean; refresh: () => Promise<void>; detailsRoot: HTMLDivElement | null };
const WorkspaceContext = createContext<Workspace | null>(null);
export function usePackWorkspace() { const value = useContext(WorkspaceContext); if (!value) throw new Error('Missing modpack workspace'); return value; }
export function WorkspaceDetails({ children }: { children: ReactNode }) { const { detailsRoot } = usePackWorkspace(); return detailsRoot ? createPortal(children, detailsRoot) : null; }

export default function PackWorkspace({ children }: { children: ReactNode }) {
    const pathname = usePathname() || '';
    const packId = Number(routeIdFrom(pathname, 'modpacks')) || 0;
    const buildId = Number(routeIdFrom(pathname, 'builds')) || 0;
    const creating = pathname.endsWith('/builds/new');
    const { featureFlags, user } = useAppData();
    const { collapsed, toggle, width } = useSidebarCollapse();
    const [search, setSearch] = useState('');
    const [folded, setFolded] = useState<Record<string, boolean>>({});
    const [overview, setOverview] = useState<PacksOverview>({ packs: [], shared: [], createOn: [] });
    const [summaries, setSummaries] = useState<Record<number, PackBuild[]>>({});
    const [pack, setPack] = useState<Pack | null>(null);
    const [builds, setBuilds] = useState<PackBuild[]>([]);
    const [permissions, setPermissions] = useState<PackPermissions>({ canEdit: false, canDelete: false, isOwner: false });
    const [loading, setLoading] = useState(true);
    const [detailsRoot, setDetailsRoot] = useState<HTMLDivElement | null>(null);
    const [collaborators, setCollaborators] = useState(false);
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
    useEffect(() => { let live = true; generation.current++; setLoading(true); setPack(null); setBuilds([]); setPermissions({ canEdit: false, canDelete: false, isOwner: false }); const load = async () => {
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
    return <WorkspaceContext.Provider value={{ pack, builds, permissions, loading, refresh, detailsRoot }}>
        <div className={styles.workspace}>
            <aside className={`${styles.packs} ${width}`} aria-label="Modpacks">
                <div className={`flex gap-1 p-2 ${collapsed ? 'flex-col' : 'items-center'}`}>
                    <Link href="/modpacks" title="Modpacks" className={`btn btn-secondary btn-sm ${collapsed ? 'justify-center' : 'flex-1 justify-start'}`}><Package size={15} />{!collapsed && 'Modpacks'}</Link>
                    {!collapsed && <Link href="/modpacks?create=1" className="btn btn-secondary btn-sm" aria-label="New pack"><Plus size={14} /></Link>}
                    <button onClick={toggle} className="btn btn-ghost btn-sm" aria-label={collapsed ? 'Expand modpack sidebar' : 'Collapse modpack sidebar'}>{collapsed ? <PanelLeftOpen size={15} /> : <PanelLeftClose size={15} />}</button>
                </div>
                {!collapsed && <div className="relative mx-3 mb-3"><Search size={13} className="absolute left-3 top-1/2 -translate-y-1/2 text-(--base-06)" /><input aria-label="Search modpacks" placeholder="Search modpacks..." className="input-field input-icon-left w-full text-xs" value={search} onChange={event => setSearch(event.target.value)} /></div>}
                <nav className={styles.packList}>{([{ key: 'own', label: 'My modpacks', packs: overview.packs }, { key: 'invited', label: 'Invited / collaborating', packs: overview.shared }] as const).map(group => {
                    const matches = group.packs.filter(p => !search.trim() || `${p.internalName} ${p.summary}`.toLowerCase().includes(search.trim().toLowerCase()));
                    const open = !!search.trim() || !folded[group.key];
                    return <section key={group.key} aria-label={group.label} className="mb-4">
                        {!collapsed && <button onClick={() => setFolded(previous => ({ ...previous, [group.key]: !previous[group.key] }))} aria-expanded={open} className="flex items-center gap-2 w-full px-1 py-2 text-[11px] uppercase tracking-wider text-(--base-06)">{open ? <ChevronDown size={12} /> : <ChevronRight size={12} />}<span className="flex-1 text-left">{group.label}</span><span>{matches.length}</span></button>}
                        {(collapsed || open) && matches.map(p => { const current = currentPackBuild(summaries[p.id] || []); return <Link key={p.id} href={`/modpacks/${p.id}`} title={p.internalName} className={`${styles.packLink} ${collapsed ? '!p-1 justify-center' : ''}`} aria-current={packId === p.id ? 'page' : undefined}>
                            <ProjectIcon name={p.internalName} url={metadata.projects[p.modrinthProjectId]?.icon_url} />
                            {!collapsed && <span className="min-w-0"><span className="text-sm font-medium text-(--base-09) block truncate">{p.internalName}</span><span className="text-[11px] text-(--base-06) block mt-1">{current ? `${current.versionString} · ${current.channel === 'release' ? 'Release' : 'Latest'}` : 'No builds'}</span></span>}
                        </Link>; })}
                        {!collapsed && open && matches.length === 0 && <p className="text-xs text-(--base-06) px-2 py-3">{search ? 'No matching modpacks.' : group.key === 'own' ? 'Create your first modpack.' : 'Packs shared with you appear here.'}</p>}
                    </section>;
                })}</nav>
                <div className={`${styles.packFooter} p-2 border-t border-(--base-03) space-y-1`}><button onClick={() => setCollaborators(true)} title="Collaborators" className="btn btn-ghost btn-sm w-full justify-start"><Users size={14} />{!collapsed && 'Collaborators'}</button><button onClick={() => setActivity(true)} title="Activity" className="btn btn-ghost btn-sm w-full justify-start"><History size={14} />{!collapsed && 'Activity'}</button></div>
            </aside>
            <div className={styles.body}>
                {packId > 0 && <header className={styles.heading}><ProjectIcon name={pack?.internalName || 'Modpack'} url={metadata.projects[pack?.modrinthProjectId || '']?.icon_url} className="w-11 h-11" /><div className="min-w-0 flex-1"><p className="mono-label text-(--base-06)">Modpack workspace</p><h1 className="font-display text-lg font-semibold text-(--base-09) truncate">{loading ? 'Loading pack...' : pack?.internalName || 'Pack not found'}</h1><p className="text-xs text-(--base-06) truncate">{pack?.summary || 'Build, manage and share your modpack.'}</p></div><div className={styles.tools}>{permissions.isOwner && <button onClick={() => setCollaborators(true)} className="btn btn-secondary btn-sm" title="Manage modpack access"><Users size={13} /><span className="hidden xl:inline">Collaborators</span></button>}<button onClick={refresh} className="btn btn-secondary btn-sm" aria-label="Refresh pack"><RefreshCw size={13} /></button>{pack && permissions.canEdit && featureFlags.modpacks && <Link href={`/modpacks/${packId}/builds/new`} className="btn btn-primary btn-sm"><Plus size={13} /> New build</Link>}</div></header>}
                {packId > 0 && !featureFlags.modpacks && <p className="text-xs text-(--warning-light) px-5 py-3 border-b border-(--base-03)">Modpack authoring is disabled. Existing builds remain readable and downloadable.</p>}
                <div className={styles.columns}>
                    {pack && !creating && <nav className={styles.builds} aria-label="Pack builds"><div className={`${styles.buildLabel} mono-label text-(--base-06) mb-3 px-1`}>Builds · {builds.length}</div>{builds.map(b => <Link key={b.id} href={`/modpacks/${packId}/builds/${b.id}`} className={styles.buildLink} aria-current={buildId === b.id ? 'page' : undefined}><strong className="font-mono text-sm text-(--base-09) break-all">{b.versionString}</strong><Badge variant={b.channel === 'release' ? 'success' : 'neutral'}>{packBuildStatus(b, builds)}</Badge><span className="text-[10px] text-(--base-06)">{fmtDate(b.createdAt)}</span></Link>)}{builds.length === 0 && <p className="text-xs text-(--base-06)">No builds yet.</p>}</nav>}
                    <div className={styles.content} key={`${packId}:${buildId}:${creating}`}>{children}</div>
                    {pack && <aside className={styles.details} aria-label="Build details"><div ref={setDetailsRoot} className="flex flex-col flex-1 min-h-0">{!buildId && !creating && <><p className="mono-label text-(--base-06) mb-4">Pack details</p><dl className={styles.metadata}><div><dt>Builds</dt><dd>{builds.length}</dd></div><div><dt>Latest version</dt><dd>{currentPackBuild(builds)?.versionString || 'None'}</dd></div><div><dt>Modrinth</dt><dd>{pack.modrinthProjectId ? 'Linked' : 'Not linked'}</dd></div></dl></>}</div>{canAudit && <div className="mt-auto pt-6"><PackActivity packId={packId} compact limit={2} /><button onClick={() => setActivity(true)} className="text-xs text-(--accent-light) inline-flex items-center gap-2 mt-3"><History size={12} /> View all activity</button></div>}</aside>}
                </div>
            </div>
        </div>
        {collaborators && <PackCollaboratorsModal packs={overview.packs} initialPackId={permissions.isOwner ? packId || undefined : undefined} onClose={() => { setCollaborators(false); refreshOverview(); refresh(); }} />}
        {activity && <div className="modal-overlay" onClick={() => setActivity(false)}><ModalPanel onClose={() => setActivity(false)} className="modal-panel max-w-2xl" onClick={event => event.stopPropagation()}><div className="modal-header"><h2 className="modal-title">Modpack activity</h2><button onClick={() => setActivity(false)} className="btn btn-secondary btn-sm">Close</button></div><div className="modal-body">{!packId || canAudit ? <PackActivity packId={packId || undefined} showPack={!packId} /> : <p className="text-sm text-(--base-06)">Only the owner and administrators can view this pack&apos;s activity.</p>}</div></ModalPanel></div>}
    </WorkspaceContext.Provider>;
}
