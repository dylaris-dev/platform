"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import '@/styles/server-content.css';

import { Package, Search, AlertTriangle, Filter, Box, X, RefreshCw, Info, ArrowUpRight } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import { visibleCategoriesFor, categoryLabel } from '@/lib/modrinthCategories';
import { Skeleton, SkeletonText } from '@/components/Skeleton';
import { systemEvents } from '@/lib/systemEvents';
import {
    searchModrinth, getModrinthProject, getModrinthVersions, getModrinthCategories,
    listInstalledMods, uninstallMod, setModUpdatesPaused, setModEnabled, pickPrimaryFile,
    getServerModpackInfo, getModHistory,
    getModrinthVersionsByIds, getModrinthProjectsByIds, getLatestVersionsFromHashes,
    planModInstall,
    type ModHistoryEntry, type InstallModPayload,
    type ModrinthSearchHit, type ModrinthSearchResult, type ModrinthProject, type ModrinthProjectName,
    type ModrinthVersion, type ModrinthCategory, type InstalledMod, type ServerModpackContent,
} from '@/lib/api/modrinth';
import { isPackMember } from '@/lib/modpackMembership';
import { clientSideNotice } from '@/lib/modpackWarnings';
import { matchesServerBucket, serverSideParam, type ServerBucketFilter } from '@/lib/modSide';
import { ServerSideFilter } from '@/components/mods/EnvironmentFilter';
import { browseNotice, envFilterName } from '@/lib/browseNotice';
import {
    pickPreferredVersion, rowUpdateTarget,
} from '@/lib/modVersionCompare';
import { installQueue, planConfirm, planRefused, updateQueue } from '@/lib/modInstallPlan';
import { MOD_LOADERS_FOR, PLUGIN_LOADERS, buildMismatch, loaderName, modFitKnown } from '@/lib/modFit';
import { LOADER_OPTIONS, isKnownLoader, isImportedServer } from '@/lib/serverLoaderMetadata';
import ServerVersionPanel from '@/components/mods/ServerVersionPanel';
import Callout from '@/components/server/Callout';
import Tabs, { type TabItem } from '@/components/ui/Tabs';
import { BrowseRow, InstalledRow, type RowStatus } from '@/components/server/content/ContentRows';
import ProjectDrawer from '@/components/server/content/ProjectDrawer';
import { UnmanagedJars } from '@/components/mods/UnmanagedJars';
import { isMcVersion } from '@/lib/validation';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import { declareServerLoaderMetadata } from '@/lib/api';
import { toast } from '@/components/ui/Toast';
import Select from '@/components/ui/Select';
import Switch from '@/components/ui/Switch';
import { useRouteId } from '@/lib/routeParams';
import { isOnServer } from '@/lib/installedState';
import {
    updatableMods, summarise, runWasClean, emptyTally, checkModUpdates,
    type BulkProgress, type UpdateCheckApi,
} from '@/lib/bulkModUpdate';
import { useBusy } from '@/lib/useBusy';
import { useModInstalls } from '@/lib/modInstallManager';
import { isPending } from '@/lib/modInstallJobs';
import { createCoalescer } from '@/lib/coalesce';
import { fmtDateTime } from '@/lib/time';

// Modrinth Content tab, search first: a search field with category, side and
// "Advanced" (loader + MC-version) filters above a result list; the project
// detail opens in a drawer (description with a short/full switch, plus the
// version list with the newest build for this server's MC version highlighted).

type Section = 'browse' | 'installed' | 'version';
const ALL_CATEGORIES = '__all__';

const PROJECT_TYPE_FOR_LOADER: Record<string, 'mod' | 'plugin'> = {
    paper: 'plugin', spigot: 'plugin', bukkit: 'plugin', purpur: 'plugin',
    velocity: 'plugin', waterfall: 'plugin', bungeecord: 'plugin',
    fabric: 'mod', forge: 'mod', quilt: 'mod', neoforge: 'mod',
};

const PROXY_LOADERS = new Set(['velocity', 'waterfall', 'bungeecord']);


const updateCheckApi: UpdateCheckApi = {
    versionsByIds: getModrinthVersionsByIds,
    latestFromHashes: getLatestVersionsFromHashes,
    projectVersions: getModrinthVersions,
    projectsByIds: getModrinthProjectsByIds,
};

export default function ServerContentPage() {
    const paramId = useRouteId('servers');
    const { servers, refreshServers } = useAppData();
    const serverId = Number(paramId);
    const server = servers.find(s => s.id === serverId);
    // Installs are followed by the app-level manager, not this page, so they
    // keep going and keep reporting when the reader navigates away.
    const installs = useModInstalls();
    const serverName = server?.name || `Server ${serverId}`;

    const defaultLoader = (server?.installerType || '').toLowerCase();
    // A proxy's recorded version is its own release ("3.5"), which no plugin on
    // Modrinth lists as a game version: filtering by it found nothing at all.
    const defaultMcVersion = PROXY_LOADERS.has(defaultLoader) ? '' : server?.minecraftVersion || '';

    const [section, setSection] = useState<Section>('browse');
    const [query, setQuery] = useState('');
    const [advanced, setAdvanced] = useState(false);
    const [filterLoaders, setFilterLoaders] = useState<string[]>(defaultLoader ? [defaultLoader] : []);
    const [filterVersions, setFilterVersions] = useState<string[]>(defaultMcVersion ? [defaultMcVersion] : []);
    const [searchResult, setSearchResult] = useState<ModrinthSearchResult | null>(null);
    // The server-side filter, shared by Browse and Installed: "Server only" or
    // "Client + Server", null = everything a server can run. Mods only: a
    // plugin or datapack has no client side to speak of.
    const [env, setEnv] = useState<ServerBucketFilter>(null);
    const [searchLoading, setSearchLoading] = useState(false);
    // The last search FAILED, as opposed to finding nothing.
    const [searchFailed, setSearchFailed] = useState(false);

    // Category sidebar (single-select). Categories are proxied from Modrinth.
    const [categories, setCategories] = useState<ModrinthCategory[]>([]);
    const [categoriesLoading, setCategoriesLoading] = useState(true);
    const [selectedCategory, setSelectedCategory] = useState<string | null>(null);

    const [installed, setInstalled] = useState<InstalledMod[]>([]);
    // False until the first answer: "No mods installed" before the list has
    // arrived is a claim the panel cannot make yet.
    // Kept per server, so switching to another one does not read the previous
    // server's answer as this one's.
    const [installedLoadedFor, setInstalledLoadedFor] = useState<number | null>(null);
    const installedLoaded = installedLoadedFor === serverId;
    const installedByProject = useMemo(() => {
        const m = new Map<string, InstalledMod>();
        for (const im of installed) m.set(im.modrinthProjectId, im);
        return m;
    }, [installed]);

    // Per-row "is there a newer matching build" state for BROWSE rows that are
    // already installed. Keyed by Modrinth project id; a missing entry means
    // "not fetched yet" (renders as 'checking'), distinct from an empty array
    // (fetched, nothing matched the current filter). Only installed rows are
    // fetched eagerly - not-installed rows resolve their install target
    // on-click, same lazy pattern as opening the detail column.
    const [installedRowVersions, setInstalledRowVersions] = useState<Map<string, ModrinthVersion[]>>(new Map());
    // Project ids with an install/remove/update request in flight, for
    // per-row spinner + disabled state.
    const [busyProjects, setBusyProjects] = useState<Set<string>>(new Set());

    // Modpack cross-check: projectId -> the pack's version of that mod. Non-empty
    // only for a server installed from a modpack; drives the banner + warnings.
    const [packByProject, setPackByProject] = useState<Map<string, { versionId: string; versionNumber: string }>>(new Map());
    const [packContents, setPackContents] = useState<ServerModpackContent[]>([]);
    // Wider than packByProject: a Technic or uploaded pack is a modpack even
    // when Modrinth knows none of its jars.
    const [isModpack, setIsModpack] = useState(false);

    const [selectedSlug, setSelectedSlug] = useState<string | null>(null);
    const [projectDetail, setProjectDetail] = useState<ModrinthProject | null>(null);
    const [projectVersions, setProjectVersions] = useState<ModrinthVersion[]>([]);
    const [projectLoading, setProjectLoading] = useState(false);
    const [descMode, setDescMode] = useState<'short' | 'full'>('short');


    const showToast = (msg: string, ok = true) => toast(msg, ok);

    // ----- Optional "declare loader + MC version" flow (imported servers) -----
    // Recommended, never forced: an imported/uploaded server keeps a blank
    // MinecraftVersion and a non-loader InstallerType, which silently disables
    // auto-filtering + version highlighting above. Declaring them here is a
    // metadata-only PATCH (core DeclareServerLoaderMetadata) - it never
    // reinstalls anything, and skipping it leaves the manual/Advanced filters
    // from Task 8 fully usable.
    const [declareDismissed, setDeclareDismissed] = useState(false);
    const [declareOpen, setDeclareOpen] = useState(false);
    const [declareLoader, setDeclareLoader] = useState('');
    const [declareMcVersion, setDeclareMcVersion] = useState('');
    const [declareSubmitting, setDeclareSubmitting] = useState(false);
    const [declareError, setDeclareError] = useState<string | null>(null);

    const handleDeclareOpen = () => {
        setDeclareLoader(defaultLoader && isKnownLoader(defaultLoader) ? defaultLoader : '');
        setDeclareMcVersion(defaultMcVersion || '');
        setDeclareError(null);
        setDeclareOpen(true);
    };

    const handleDeclareSubmit = async () => {
        const loader = declareLoader.trim().toLowerCase();
        const mcVersion = declareMcVersion.trim();
        if (!isKnownLoader(loader) || !isMcVersion(mcVersion)) {
            setDeclareError('Pick a loader and a valid Minecraft version (e.g. 1.20.4).');
            return;
        }
        setDeclareSubmitting(true);
        setDeclareError(null);
        try {
            const res = await declareServerLoaderMetadata(serverId, loader, mcVersion);
            if (res.success) {
                setDeclareOpen(false);
                // Re-sync the active filters to the just-declared values so browse
                // and per-row install work immediately, without waiting for a reload
                // to re-seed them from the refreshed server defaults.
                setFilterLoaders([loader]);
                setFilterVersions([mcVersion]);
                await refreshServers();
                showToast('Loader and Minecraft version declared', true);
            } else {
                showToast(res.message || 'Failed to declare loader/version', false);
            }
        } finally {
            setDeclareSubmitting(false);
        }
    };

    const projectType = useMemo<'mod' | 'plugin' | undefined>(() => {
        if (advanced || !defaultLoader) return undefined;
        return PROJECT_TYPE_FOR_LOADER[defaultLoader];
    }, [advanced, defaultLoader]);

    // ----- Categories -----

    useEffect(() => {
        let cancelled = false;
        setCategoriesLoading(true);
        getModrinthCategories().then(cats => {
            if (cancelled) return;
            setCategories(cats);
            setCategoriesLoading(false);
        });
        return () => { cancelled = true; };
    }, []);

    // Categories for this server's content type. See visibleCategoriesFor: the
    // full-list fallback it replaces is what put resourcepack resolutions
    // (8x/16x/32x...) in front of plugin servers.
    const browseProjectType = PROJECT_TYPE_FOR_LOADER[defaultLoader] || 'mod';
    // Not browseProjectType: that falls back to 'mod' for vanilla and unknown
    // loaders. Browse also drops it in Advanced, where results mix plugins,
    // shaders and packs.
    const isModServer = PROJECT_TYPE_FOR_LOADER[defaultLoader] === 'mod';
    const sideFilter: ServerBucketFilter = isModServer ? env : null;
    // Browse filters by side at all only on a mod server outside Advanced.
    const browseSides = isModServer && !advanced;
    const browseEnv: ServerBucketFilter = browseSides ? env : null;
    const visibleCategories = useMemo(
        () => visibleCategoriesFor(categories, browseProjectType),
        [categories, browseProjectType],
    );

    // ----- Browse search -----

    const runSearch = useCallback(async () => {
        setSearchLoading(true);
        const res = await searchModrinth({
            query: query.trim() || undefined,
            loaders: filterLoaders.length ? filterLoaders : undefined,
            versions: filterVersions.length ? filterVersions : undefined,
            categories: selectedCategory ? [selectedCategory] : undefined,
            projectType,
            side: browseSides ? serverSideParam(browseEnv) : undefined,
            limit: 20,
            index: 'relevance',
        });
        setSearchResult(res);
        setSearchFailed(res === null);
        setSearchLoading(false);
    }, [query, filterLoaders, filterVersions, selectedCategory, projectType, browseSides, browseEnv]);

    const visibleHits = useMemo(
        () => (searchResult?.hits ?? []).filter(h => !browseSides || matchesServerBucket(browseEnv, h, { browse: true })),
        [searchResult, browseSides, browseEnv],
    );
    const notice = browseNotice({
        failed: searchFailed,
        hits: searchResult?.hits.length ?? 0,
        visible: visibleHits.length,
        env: browseEnv,
    });

    // Debounce typed search input (and category/filter changes).
    useEffect(() => {
        const t = setTimeout(runSearch, 350);
        return () => clearTimeout(t);
    }, [runSearch]);

    // ----- Installed mods -----

    const [installedError, setInstalledError] = useState<string | null>(null);
    const [history, setHistory] = useState<ModHistoryEntry[]>([]);

    // Newest first, which is the order Core returns and the order a rollback
    // wants: the build you had immediately before is the one you almost always
    // mean.
    // Non-null while a bulk run is going. Read by the button labels, the
    // progress line and the close guard.
    const [bulk, setBulk] = useState<BulkProgress | null>(null);
    // The ref, not the state, is what the loop checks: state does not update
    // until the next render, and the loop is inside one tick.
    const bulkCancelled = useRef(false);
    // Mods the last Update all left alone, with why. Kept after the run so the
    // reader can act on them; the toast alone is gone in seconds.
    const [bulkSkipped, setBulkSkipped] = useState<{ title: string; reason: string }[]>([]);

    // Closing the tab mid-run leaves the rest un-issued. What HAS been issued is
    // queued on Core and finishes regardless, so this warns about the remainder
    // rather than pretending work would be lost.
    useEffect(() => {
        if (!bulk) return;
        const warn = (e: BeforeUnloadEvent) => { e.preventDefault(); };
        window.addEventListener('beforeunload', warn);
        return () => window.removeEventListener('beforeunload', warn);
    }, [bulk]);

    // Leaving the page in-app stops the loop rather than letting it run against
    // an unmounted component.
    useEffect(() => () => { bulkCancelled.current = true; }, []);

    // Modrinth project data of installed mods: the icon on every row, and on a
    // mod server the sides for the server-side tag and the filter. null = asked, Modrinth did not answer for it: the marker is what
    // stops this effect asking again on every render. A mod without sides (not
    // fetched yet, null, or the lookup failed) stays visible: hiding it would
    // read as "not installed".
    const [installedSides, setInstalledSides] = useState<Map<string, ModrinthProjectName | null>>(new Map());
    useEffect(() => {
        const missing = [...new Set(installed.map(m => m.modrinthProjectId))].filter(id => id && !installedSides.has(id));
        if (missing.length === 0) return;
        let cancelled = false;
        getModrinthProjectsByIds(missing).then(projects => {
            if (cancelled) return;
            setInstalledSides(prev => {
                const next = new Map(prev);
                for (const id of missing) next.set(id, null);
                for (const p of projects) next.set(p.id, p);
                return next;
            });
        }).catch(() => { /* fail-open: unfiltered rows stay visible */ });
        return () => { cancelled = true; };
    }, [installed, installedSides]);
    // The installed builds themselves, by version id, to mark the ones that do
    // not fit this server (a Forge jar left over on NeoForge, a 26.3 beta on
    // 1.21.1). Same null-marker guard as the sides above. Keyed by version, so
    // a swap fetches the new build on its own.
    const fitCheck = isModServer && modFitKnown(defaultLoader, defaultMcVersion);
    const [installedBuilds, setInstalledBuilds] = useState<Map<string, ModrinthVersion | null>>(new Map());
    useEffect(() => {
        if (!fitCheck) return;
        const missing = [...new Set(installed.filter(m => isOnServer(m)).map(m => m.modrinthVersionId))]
            .filter(id => id && !installedBuilds.has(id));
        if (missing.length === 0) return;
        let cancelled = false;
        getModrinthVersionsByIds(missing).then(builds => {
            if (cancelled) return;
            setInstalledBuilds(prev => {
                const next = new Map(prev);
                for (const id of missing) next.set(id, null);
                for (const b of builds) next.set(b.id, b);
                return next;
            });
        }).catch(() => { /* fail-open: an unchecked row is not marked */ });
        return () => { cancelled = true; };
    }, [fitCheck, installed, installedBuilds]);
    const mismatchFor = (m: InstalledMod): string | null => {
        const b = fitCheck && isOnServer(m) ? installedBuilds.get(m.modrinthVersionId) : null;
        return b ? buildMismatch(defaultLoader, defaultMcVersion, b) : null;
    };

    const visibleInstalled = useMemo(() => installed.filter(m => {
        const sides = installedSides.get(m.modrinthProjectId);
        return !isModServer || !sides || matchesServerBucket(sideFilter, sides, { browse: false });
    }), [isModServer, installed, installedSides, sideFilter]);

    const historyFor = useCallback(
        (projectId: string) => history.filter(h => h.modrinthProjectId === projectId),
        [history],
    );
    const [retryingInstalled, retryInstalled] = useBusy();

    // Through a ref: the server list arriving must not cost another GET.
    const serverNameRef = useRef(serverName);
    serverNameRef.current = serverName;
    const adoptInstalls = installs.adopt;
    const refreshInstalled = useCallback(async () => {
        if (!serverId) return;
        try {
            const rows = await listInstalledMods(serverId);
            setInstalled(rows);
            setInstalledLoadedFor(serverId);
            adoptInstalls(serverId, serverNameRef.current, rows);
            setInstalledError(null);
            // Fail-open, and the difference from the list above is deliberate:
            // the list decides what is on the server, the history only offers a
            // way back. Losing it hides a button; losing the list would invite
            // installing something twice.
            try {
                setHistory(await getModHistory(serverId));
            } catch {
                setHistory([]);
            }
        } catch (e) {
            // The list is not decoration here: it is what the browse rows read
            // to decide whether a mod is already on the server. An empty one
            // says "install it" about everything.
            setInstalledError(e instanceof Error ? e.message : 'Could not load the installed mods.');
            setInstalledLoadedFor(serverId);
        }
    }, [serverId, adoptInstalls]);

    useEffect(() => { refreshInstalled(); }, [refreshInstalled]);

    // Every event-driven re-read goes through here: "Update all" emits two
    // server_mods.changed frames per mod, and each used to cost two GETs.
    const coalesce = useRef(createCoalescer(300)).current;
    const refreshInstalledSoon = useCallback(
        () => coalesce(serverId, refreshInstalled),
        [coalesce, serverId, refreshInstalled],
    );

    // A job settling is also a reason to re-read: the manager may have learned
    // it by polling (an older Core publishes no event when the node reports).
    const settledJobs = installs.jobs
        .filter(j => j.serverId === serverId && !isPending(j))
        .map(j => j.id)
        .join(',');
    useEffect(() => { if (settledJobs) refreshInstalledSoon(); }, [settledJobs, refreshInstalledSoon]);

    // Load the modpack snapshot once per server. Fail-open: on any error the map
    // stays empty and the tab behaves exactly as a non-modpack server.
    useEffect(() => {
        if (!serverId) return;
        let cancelled = false;
        (async () => {
            const { contents, isModpack: modpack } = await getServerModpackInfo(serverId);
            if (cancelled) return;
            const next = new Map<string, { versionId: string; versionNumber: string }>();
            for (const c of contents) {
                next.set(c.modrinthProjectId, { versionId: c.modrinthVersionId, versionNumber: c.modrinthVersionNumber });
            }
            setPackByProject(next);
            setPackContents(contents);
            setIsModpack(modpack);
        })();
        return () => { cancelled = true; };
    }, [serverId]);

    // SSE — server_mods.changed fires when another session installs/uninstalls.
    useEffect(() => {
        const unsub = systemEvents.on('server_mods.changed', (evt) => {
            const sid = (evt.payload as any)?.serverId;
            if (sid === undefined || sid === serverId) refreshInstalledSoon();
        });
        return () => { unsub(); };
    }, [serverId, refreshInstalledSoon]);

    // For browse rows that are already installed, fetch the version list
    // filtered by the current loader + MC-version filter, so the row can show
    // "Update available" without the user having to open the mod first. Scoped
    // to the (small) installed-and-currently-visible subset - NOT every
    // visible row - since Modrinth version lists are otherwise only fetched
    // on demand (mod-open / install-click), and eagerly fetching for all ~20
    // search hits on every keystroke would be a heavy, mostly-wasted burst of
    // calls against not-yet-installed projects nobody asked about.
    useEffect(() => {
        if (!searchResult) return;
        const hitsToCheck = searchResult.hits.filter(h => installedByProject.has(h.project_id));
        if (hitsToCheck.length === 0) return;

        // Drop stale entries for these ids immediately so a filter change
        // shows 'checking' rather than a stale (possibly wrong) status while
        // the refetch is in flight.
        setInstalledRowVersions(prev => {
            const next = new Map(prev);
            for (const h of hitsToCheck) next.delete(h.project_id);
            return next;
        });

        let cancelled = false;
        const loaders = filterLoaders.length ? filterLoaders : undefined;
        const versions = filterVersions.length ? filterVersions : undefined;
        (async () => {
            const entries = await Promise.all(hitsToCheck.map(async h => {
                const list = await getModrinthVersions(h.slug, { loaders, versions });
                return [h.project_id, list] as const;
            }));
            if (cancelled) return;
            setInstalledRowVersions(prev => {
                const next = new Map(prev);
                for (const [id, list] of entries) next.set(id, list);
                return next;
            });
        })();
        return () => { cancelled = true; };
    }, [searchResult, installedByProject, filterLoaders, filterVersions]);

    // ----- Project detail column -----

    const openProjectDetail = useCallback(async (slug: string) => {
        setSelectedSlug(slug);
        setDescMode('short');
        setProjectLoading(true);
        setProjectDetail(null);
        setProjectVersions([]);
        // Fetch versions filtered by the active loader only (not MC version), so
        // the full version history for this loader is visible and we can
        // highlight the newest build matching the server's MC version.
        const [p, versions] = await Promise.all([
            getModrinthProject(slug),
            getModrinthVersions(slug, { loaders: advanced ? undefined : filterLoaders }),
        ]);
        setProjectDetail(p);
        setProjectVersions(versions);
        setProjectLoading(false);
    }, [advanced, filterLoaders]);

    const closeProjectDetail = () => {
        setSelectedSlug(null);
        setProjectDetail(null);
        setProjectVersions([]);
    };

    const sortedVersions = useMemo(
        () => [...projectVersions].sort((a, b) => b.date_published.localeCompare(a.date_published)),
        [projectVersions],
    );
    // Newest build (they are date-sorted) whose game_versions covers this
    // server's MC version — the one we recommend + highlight.
    const highlightVersionId = useMemo(() => {
        if (!defaultMcVersion) return null;
        return sortedVersions.find(v => v.game_versions.includes(defaultMcVersion))?.id ?? null;
    }, [sortedVersions, defaultMcVersion]);

    // ----- Install / uninstall -----

    // Advisory, non-blocking modpack cross-check. The tone is now carried by the
    // dialog itself - the same-version case is informational, the other two are
    // rendered as warnings - which is what the "Warning:" prefixes were standing
    // in for while this went through window.confirm. Returns false if the user
    // cancels. Shared by every install path (detail-pane version list, row
    // Install, row Update) so a cancelled confirm is always evaluated BEFORE any
    // destructive action (e.g. the row Update flow must not remove the old file
    // first).
    const confirmModpackCrossCheck = async (project: ModrinthProject, version: ModrinthVersion): Promise<boolean> => {
        const inPack = packByProject.get(project.id);
        if (inPack) {
            if (inPack.versionId === version.id) {
                return confirmDialog({
                    title: `"${project.title}" is already in this modpack`,
                    message: `This server's modpack already ships this exact version. Install it again anyway?`,
                    confirmLabel: 'Install anyway',
                    destructive: false,
                });
            }
            const packVer = inPack.versionNumber || 'a different version';
            return confirmDialog({
                title: `This overrides the modpack's version`,
                message: `This server's modpack ships ${packVer} of "${project.title}". Installing ${version.version_number} may stop the server from starting, or leave players on the pack version unable to connect.`,
                confirmLabel: 'Install anyway',
            });
        }
        if (isModpack && project.client_side === 'required') {
            return confirmDialog({
                title: `"${project.title}" is client-side too`,
                message: `Players need ${version.version_number} on their own client as well, or they cannot connect. The distributed modpack does not give it to them.`,
                confirmLabel: 'Install anyway',
            });
        }
        return true;
    };

    // The client-side line for an update or rollback on a modpack. Advisory, so
    // a failed lookup shows no line rather than blocking the action.
    const clientSideWarning = async (projectIds: string[]): Promise<string | null> => {
        if (!isModpack || projectIds.length === 0) return null;
        try {
            return clientSideNotice(await getModrinthProjectsByIds(projectIds), true);
        } catch {
            return null;
        }
    };

    // Without the server's Minecraft version nothing can tell a build for it
    // from one for another version, which is how a 26.3 beta landed on a
    // 1.21.1 server. Advanced is the explicit "I pick the build" way round.
    const installNeedsMc = !advanced && !PROXY_LOADERS.has(defaultLoader) && !isMcVersion(defaultMcVersion);
    const refuseUnknownMc = (): boolean => {
        if (!installNeedsMc) return false;
        setDeclareDismissed(false);
        showToast("This server's Minecraft version is unknown. Declare it with Declare now at the top, or turn on Advanced to pick a build by hand.", false);
        return true;
    };

    // The build whose dependency plan is being worked out, for its button.
    const [planningVersion, setPlanningVersion] = useState<string | null>(null);

    // Every new install brings its dependencies along, the way the Modrinth app
    // does: Core works out the plan, one confirm lists what else comes and what
    // cannot, then each install starts dependency-first as its own job. Nothing
    // extra and nothing to warn about installs straight away. A plan that could
    // not be worked out never quietly turns into an install without them.
    const handleInstall = async (project: ModrinthProject, version: ModrinthVersion) => {
        if (refuseUnknownMc()) return;
        if (!await confirmModpackCrossCheck(project, version)) return;
        const file = pickPrimaryFile(version);
        if (!file) { showToast('Version has no downloadable file', false); return; }
        const root: InstallModPayload = {
            projectId: project.id,
            projectSlug: project.slug,
            versionId: version.id,
            title: project.title,
            fileName: file.filename,
            downloadUrl: file.url,
            sha512: file.hashes.sha512,
        };
        let queue = [root];
        const withoutDeps = (title: string, message: string) => confirmDialog({
            title,
            message: `${message}\n\nInstalling without them may stop the server from starting.`,
            confirmLabel: 'Install without dependencies',
            destructive: true,
            focusCancel: true,
        });
        if (!modFitKnown(defaultLoader, defaultMcVersion)) {
            // Core cannot plan for a server that does not say what it runs.
            if (!await withoutDeps(
                `Install "${project.title}" without its dependencies?`,
                "Dependencies are installed automatically only once this server's loader and Minecraft version are declared. Declare them with Declare now at the top.",
            )) return;
        } else {
            setPlanningVersion(version.id);
            const res = await planModInstall(serverId, version.id);
            setPlanningVersion(null);
            if (!res.ok) {
                if (planRefused(res.status)) { showToast(res.message, false); return; }
                if (!await withoutDeps(`Dependencies of "${project.title}" could not be checked`, res.message)) return;
            } else {
                const ask = planConfirm(res.plan, root);
                if (ask && !await confirmDialog(ask)) return;
                queue = installQueue(res.plan, root);
            }
        }
        for (const p of queue) {
            const job = await installs.start(serverId, serverName, p);
            if (job.state === 'failed') {
                // The manager toasts the failure itself; this says what it cost.
                if (p !== root) showToast(`"${root.title}" was not installed, because ${p.title} did not install.`, false);
                break;
            }
        }
        refreshInstalledSoon();
    };

    // An installed build that does not fit the server, swapped for the newest
    // one that does (release first). It is an ordinary install: Core replaces
    // the old jar, files it in the history, and the plan brings what the new
    // build needs.
    const handleSwap = (m: InstalledMod) => withBusy(m.modrinthProjectId, async () => {
        const ref = m.modrinthProjectSlug || m.modrinthProjectId;
        // A plugin fits any patch of its line, so plugins are not filtered by the exact version.
        const versions = PROXY_LOADERS.has(defaultLoader) || PLUGIN_LOADERS.has(defaultLoader) ? undefined : [defaultMcVersion];
        const [project, candidates] = await Promise.all([
            getModrinthProject(ref),
            getModrinthVersions(ref, { loaders: MOD_LOADERS_FOR[defaultLoader], versions }),
        ]);
        if (!project) { showToast('Failed to load project details', false); return; }
        const target = pickPreferredVersion(candidates.filter(v => !buildMismatch(defaultLoader, defaultMcVersion, v)));
        if (!target) {
            showToast(`Modrinth has no ${loaderName(defaultLoader)} build of "${m.title || m.fileName}" for Minecraft ${defaultMcVersion}.`, false);
            return;
        }
        await handleInstall(project, target);
    });

    // Rolling back is an install of an older build, not a separate mechanism.
    //
    // Going through the same path is what makes it safe: the node downloads and
    // verifies before it swaps, deletes the jar that is there now because Core
    // passes it as the previous file, and reports the outcome. It also files the
    // build being rolled back FROM into the history, so rolling forward again is
    // the same button.
    //
    // The version is fetched by id rather than picked out of a list, because the
    // list the tab holds is filtered by this server's loader and Minecraft
    // version, and the build being returned to need not match today's filter.
    const handleRollback = async (m: InstalledMod, entry: ModHistoryEntry) => {
        // Both builds by number, so the reader confirms the move they mean. The
        // file names alone often differ only in a build suffix.
        let builds: ModrinthVersion[];
        try {
            builds = await getModrinthVersionsByIds([m.modrinthVersionId, entry.modrinthVersionId]);
        } catch (e) {
            showToast(e instanceof Error ? e.message : 'Could not load that build', false);
            return;
        }
        const version = builds.find(b => b.id === entry.modrinthVersionId);
        if (!version) { showToast('Modrinth no longer has that build', false); return; }
        const from = builds.find(b => b.id === m.modrinthVersionId)?.version_number ?? m.fileName;
        const notice = await clientSideWarning([m.modrinthProjectId]);
        if (!await confirmDialog({
            title: `Roll back ${m.title || m.fileName}?`,
            message: `From ${from} back to ${version.version_number} (${entry.fileName}), the build this server ran until ${fmtDateTime(entry.replacedAt)}.`
                + (notice ? `\n\n${notice}` : ''),
            confirmLabel: 'Roll back',
            destructive: !!notice,
        })) return;
        await withBusy(m.modrinthProjectId, async () => {
            const file = pickPrimaryFile(version);
            if (!file) { showToast('That build has no downloadable file', false); return; }
            const job = await installs.start(serverId, serverName, {
                projectId: m.modrinthProjectId,
                projectSlug: m.modrinthProjectSlug,
                versionId: version.id,
                title: m.title,
                fileName: file.filename,
                downloadUrl: file.url,
                sha512: file.hashes.sha512,
            });
            if (job.state !== 'failed') refreshInstalledSoon();
        });
    };

    // Update all is pinned to the server's own loader and Minecraft version,
    // never the Browse filter: a filter the reader cleared (or cannot see from
    // the Installed tab) must not widen the run to builds for other versions.
    const pinnedLoader = isKnownLoader(defaultLoader) ? defaultLoader : '';
    const pinnedMcVersion = isMcVersion(defaultMcVersion) ? defaultMcVersion : '';
    const updateAllBlocked = !pinnedLoader || !pinnedMcVersion
        ? "Update all needs this server's loader and Minecraft version. Declare them first."
        : null;

    // Update every installed mod that has a newer build for this server.
    //
    // Two passes: decide everything first, show what will happen (including
    // what is skipped and why), and only then install. Sequential on purpose.
    // Each install is a queued command plus a Modrinth version lookup, and
    // firing thirty of both at once buys nothing - the work happens on the node
    // afterwards either way - while making the failure of any one of them
    // harder to attribute and the progress meaningless.
    const handleUpdateAll = async () => {
        if (updateAllBlocked) { showToast(updateAllBlocked, false); return; }
        const targets = updatableMods(installed);
        const paused = installed.filter(m => isOnServer(m) && m.updatesPaused).map(m => m.title || m.fileName);
        if (targets.length === 0) {
            showToast(paused.length ? 'Every installed mod is paused for updates' : 'Nothing installed to update', false);
            return;
        }
        bulkCancelled.current = false;
        setBulkSkipped([]);
        const tally = emptyTally();
        setBulk({ done: 0, total: targets.length, current: '', checking: true });
        const check = await checkModUpdates(
            targets,
            { loader: pinnedLoader, mcVersion: pinnedMcVersion },
            updateCheckApi,
            (done, total) => setBulk({ done, total, current: '', checking: true }),
            () => bulkCancelled.current,
        );
        if (!check) { setBulk(null); return; }
        tally.current = check.current;
        tally.unknown = check.unknown;
        const planned = check.planned;
        const held = new Map(installed.filter(m => m.updatesPaused || m.disabled)
            .map(m => [m.modrinthProjectId, m.title || m.fileName] as [string, string]));
        const skipped = check.skipped.map(s => ({ title: s.mod.title || s.mod.fileName, reason: s.reason }));
        setBulkSkipped(skipped);
        setBulk(null);

        if (planned.length === 0) {
            const extra = skipped.length ? ` ${skipped.length} skipped, listed below.` : '';
            showToast(summarise(tally) + extra, runWasClean(tally));
            return;
        }
        const shown = skipped.slice(0, 8).map(s => `${s.title} (${s.reason})`).join('; ');
        const more = skipped.length > 8 ? `; and ${skipped.length - 8} more, listed on this tab` : '';
        const notice = await clientSideWarning(planned.map(p => p.mod.modrinthProjectId));
        const ok = await confirmDialog({
            title: `Update ${planned.length} ${planned.length === 1 ? 'mod' : 'mods'}?`,
            message: `${planned.length} ${planned.length === 1 ? 'mod moves' : 'mods move'} to a newer build for ${pinnedLoader} ${pinnedMcVersion}, never to a less stable channel. Dependencies a new build needs are installed first.`
                + (skipped.length ? ` ${skipped.length} skipped: ${shown}${more}.` : ' Nothing skipped.')
                + (tally.unknown ? ` ${tally.unknown} could not be checked.` : '')
                + (paused.length ? `\n\nPaused, staying on their build: ${paused.slice(0, 8).join(', ')}${paused.length > 8 ? ` and ${paused.length - 8} more` : ''}.` : '')
                + (notice ? ` ${notice}` : ''),
            confirmLabel: `Update ${planned.length}`,
            destructive: !!notice,
        });
        if (!ok) return;

        // Each update is planned like a single install: what the new build needs
        // is installed first. A build Core cannot plan, or one that would leave
        // a dependency unresolved or clash with an installed mod, is not
        // installed - the run must not leave a server that cannot start.
        const runSkipped: { title: string; reason: string }[] = [];
        for (const [i, { mod: m, build }] of planned.entries()) {
            if (bulkCancelled.current) break;
            const title = m.title || m.fileName;
            setBulk({ done: i, total: planned.length, current: title });
            const file = pickPrimaryFile(build);
            if (!file) { tally.failed++; continue; }
            const root: InstallModPayload = {
                projectId: m.modrinthProjectId,
                projectSlug: m.modrinthProjectSlug,
                versionId: build.id,
                title: m.title,
                fileName: file.filename,
                downloadUrl: file.url,
                sha512: file.hashes.sha512,
            };
            const res = await planModInstall(serverId, build.id);
            if (!res.ok) {
                // The session or the permission is gone: every further plan
                // would be refused the same way.
                if (res.status === 401 || res.status === 403) { showToast(res.message, false); break; }
                runSkipped.push({ title, reason: res.message });
                continue;
            }
            const step = updateQueue(res.plan, root, held);
            if ('skip' in step) { runSkipped.push({ title, reason: step.skip }); continue; }
            let ok = true;
            for (const p of step.queue) {
                const job = await installs.start(serverId, serverName, p);
                if (job.state === 'failed') { ok = false; break; }
            }
            if (ok) tally.updated++; else tally.failed++;
        }
        if (runSkipped.length) setBulkSkipped(prev => [...prev, ...runSkipped]);

        setBulk(null);
        refreshInstalled();
        const allSkipped = skipped.length + runSkipped.length;
        const extra = allSkipped ? ` ${allSkipped} skipped, listed below.` : '';
        showToast(summarise(tally) + extra, runWasClean(tally));
    };

    const handleToggleEnabled = async (m: InstalledMod) => {
        await withBusy(m.modrinthProjectId, async () => {
            const res = await setModEnabled(serverId, m.id, !!m.disabled);
            if (res.success) {
                showToast(`${m.title || m.fileName} switched ${m.disabled ? 'on' : 'off'}. Takes effect at the next restart.`, true);
                refreshInstalled();
            } else {
                showToast(res.message || 'Could not switch the mod', false);
            }
        });
    };

    const handleTogglePaused = async (m: InstalledMod) => {
        await withBusy(m.modrinthProjectId, async () => {
            const res = await setModUpdatesPaused(serverId, m.id, !m.updatesPaused);
            if (res.success) refreshInstalled();
            else showToast(res.message || 'Could not save the setting', false);
        });
    };

    const handleUninstall = async (m: InstalledMod) => {
        if (isPackMember(m, packContents) && !await confirmDialog({
            title: `Remove ${m.title || m.fileName}?`,
            message: 'This mod ships with the modpack. Removing it can stop the server from starting, and players on the pack may not be able to connect.',
            confirmLabel: 'Remove',
            destructive: true,
        })) return;
        const res = await uninstallMod(serverId, m.id);
        if (res.success) {
            showToast(`Removed ${m.title || m.fileName}`, true);
            refreshInstalled();
        } else {
            showToast(res.message || 'Uninstall failed', false);
        }
    };

    // ----- Per-row browse actions (install / remove / update) -----

    const withBusy = async (projectId: string, fn: () => Promise<void>) => {
        setBusyProjects(prev => new Set(prev).add(projectId));
        try {
            await fn();
        } finally {
            setBusyProjects(prev => {
                const next = new Set(prev);
                next.delete(projectId);
                return next;
            });
        }
    };

    // Row "Install": resolve the newest version matching the current filter
    // for this project (fetched lazily, on click - not eagerly for every
    // browse row), release first, and install it.
    const handleRowInstall = (hit: ModrinthSearchHit) => withBusy(hit.project_id, async () => {
        if (refuseUnknownMc()) return;
        const loaders = filterLoaders.length ? filterLoaders : undefined;
        const versions = filterVersions.length ? filterVersions : undefined;
        const [project, candidates] = await Promise.all([
            getModrinthProject(hit.slug),
            getModrinthVersions(hit.slug, { loaders, versions }),
        ]);
        if (!project) { showToast('Failed to load project details', false); return; }
        const latest = pickPreferredVersion(candidates, { loaders: filterLoaders, mcVersions: filterVersions });
        if (!latest) { showToast(`No version of "${hit.title}" matches the current loader / MC-version filter`, false); return; }
        await handleInstall(project, latest);
    });

    const handleRowRemove = (installedMod: InstalledMod) => withBusy(installedMod.modrinthProjectId, async () => {
        await handleUninstall(installedMod);
    });

    // Row "Update" is an install of the newer build, nothing more. Core passes
    // the current jar to the node as the file to replace, and the node swaps
    // only after the new one downloaded and verified. Removing first used to
    // drop the mod's history and rollback, and lost the mod outright whenever
    // the install then failed.
    const handleRowUpdate = (hit: ModrinthSearchHit, latest: ModrinthVersion) =>
        withBusy(hit.project_id, async () => {
            if (refuseUnknownMc()) return;
            const project = await getModrinthProject(hit.slug);
            if (!project) { showToast('Failed to load project details', false); return; }
            if (!await confirmModpackCrossCheck(project, latest)) return;
            // The same path as an install: Core plans it, and what the new build
            // needs is installed first.
            await handleInstall(project, latest);
        });

    // ----- Advanced toggle -----

    const handleAdvancedToggle = () => {
        setAdvanced(prev => {
            const next = !prev;
            if (!next) {
                // Turning Advanced off resets the loader + MC-version filters
                // back to the server's stored values (not just "clears" them).
                setFilterLoaders(defaultLoader ? [defaultLoader] : []);
                setFilterVersions(defaultMcVersion ? [defaultMcVersion] : []);
            }
            return next;
        });
    };

    if (!server) return null;

    const unknownLoader = !advanced && defaultLoader && PROJECT_TYPE_FOR_LOADER[defaultLoader] === undefined;
    const imported = isImportedServer(server);
    const categoryOptions = [
        { value: ALL_CATEGORIES, label: 'All categories' },
        ...visibleCategories.map(c => ({ value: c.name, label: categoryLabel(c.name) })),
    ];
    const sectionItems: TabItem<Section>[] = [
        { id: 'browse', label: 'Browse', icon: Search },
        { id: 'installed', label: `Installed (${installed.length})`, icon: Box },
        { id: 'version', label: 'Minecraft version', icon: ArrowUpRight },
    ];

    return (
        <main className="flex flex-col gap-6">
            <header className="flex flex-wrap items-end justify-between gap-x-6 gap-y-2">
                <div className="min-w-0">
                    <h1 className="font-display text-xl font-bold text-(--base-09)">Content</h1>
                    <p className="mt-1 text-[13px] text-(--base-06)">
                        Mods and plugins from Modrinth.
                        {!advanced && (defaultLoader || defaultMcVersion) && (
                            <> Filtered to <span className="srv-num text-(--base-07)">{[defaultLoader, defaultMcVersion].filter(Boolean).join(' · ')}</span>.</>
                        )}
                    </p>
                </div>
            </header>

            <Tabs items={sectionItems} active={section} onChange={setSection} ariaLabel="Content sections" />

            {imported && !declareDismissed && (
                <Callout
                    tone="info"
                    icon={<Info size={16} />}
                    title="Declare this server's loader and Minecraft version"
                    actions={
                        <button onClick={() => setDeclareDismissed(true)} className="btn btn-ghost btn-icon btn-sm" aria-label="Dismiss">
                            <X size={14} />
                        </button>
                    }
                >
                    {!declareOpen ? (
                        <>
                            <p>
                                This looks like an imported server. It is missing loader and version info, so auto-filtering and
                                version highlighting are off. Declaring them is optional, recommended, and does not
                                reinstall anything; manual/Advanced filtering keeps working either way.
                            </p>
                            <button onClick={handleDeclareOpen} className="btn btn-primary btn-sm mt-3">
                                Declare now
                            </button>
                        </>
                    ) : (
                        <form
                            onSubmit={e => { e.preventDefault(); handleDeclareSubmit(); }}
                            className="mt-1 space-y-2"
                        >
                            <div className="flex flex-wrap items-end gap-3">
                                <div>
                                    <label className="srv-label" htmlFor="declare-loader">Loader</label>
                                    <Select
                                        id="declare-loader"
                                        ariaLabel="Loader"
                                        className="mt-1 min-w-32"
                                        value={declareLoader}
                                        onChange={setDeclareLoader}
                                        disabled={declareSubmitting}
                                        placeholder="Select…"
                                        options={LOADER_OPTIONS.map(l => ({ value: l, label: l }))}
                                    />
                                </div>
                                <div>
                                    <label className="srv-label" htmlFor="declare-mc">Minecraft version</label>
                                    <input
                                        id="declare-mc"
                                        type="text"
                                        value={declareMcVersion}
                                        onChange={e => setDeclareMcVersion(e.target.value)}
                                        disabled={declareSubmitting}
                                        placeholder="1.20.4"
                                        className="input-field input-mono mt-1 block w-28 text-xs"
                                    />
                                </div>
                                <button
                                    type="submit"
                                    disabled={declareSubmitting || !declareLoader || !isMcVersion(declareMcVersion.trim())}
                                    className="btn btn-primary btn-sm"
                                >
                                    {declareSubmitting && <RefreshCw size={14} className="animate-spin motion-reduce:animate-none" />}
                                    Save
                                </button>
                                <button
                                    type="button"
                                    onClick={() => setDeclareOpen(false)}
                                    disabled={declareSubmitting}
                                    className="btn btn-secondary btn-sm"
                                >
                                    Cancel
                                </button>
                            </div>
                            {declareError && <p className="text-(--error-light)">{declareError}</p>}
                        </form>
                    )}
                </Callout>
            )}

            {isModpack && (
                <Callout tone="info" icon={<Package size={16} />}>
                    This server runs a modpack. Mods you add here are not part of the distributed pack;
                    players who lack a required client-side mod may fail to connect.
                </Callout>
            )}

            {section === 'browse' && (
                <section className="srv-content-browse" aria-label="Browse content">
                    {/* Filter sidebar, Modrinth style: environment, categories, then the
                        Advanced loader + MC-version filters. Stacks above the results on
                        narrow screens. */}
                    <aside className="srv-content-filters" aria-label="Filters">
                        {isModServer && !advanced && (
                            <div className="srv-content-filter-group">
                                {/* ServerSideFilter brings its own "Environment" heading. */}
                                <ServerSideFilter value={env} onChange={setEnv} />
                            </div>
                        )}

                        <div className="srv-content-filter-group">
                            <span className="srv-label" id="content-categories-label">Categories</span>
                            {categoriesLoading ? (
                                <div className="flex flex-col gap-2" aria-busy="true">
                                    {Array.from({ length: 6 }, (_, i) => <SkeletonText key={i} width="w-3/4" className="h-4" />)}
                                </div>
                            ) : (
                                <div className="srv-content-cats" role="group" aria-labelledby="content-categories-label">
                                    {categoryOptions.map(c => {
                                        const on = (selectedCategory ?? ALL_CATEGORIES) === c.value;
                                        return (
                                            <button
                                                key={c.value}
                                                type="button"
                                                className="srv-content-cat"
                                                aria-pressed={on}
                                                onClick={() => setSelectedCategory(c.value === ALL_CATEGORIES ? null : c.value)}
                                            >
                                                {c.label}
                                            </button>
                                        );
                                    })}
                                </div>
                            )}
                        </div>

                        <div className="srv-content-filter-group">
                            <div className="flex items-center justify-between gap-3">
                                <span className="flex items-center gap-2 text-[13px] font-medium text-(--base-08)">
                                    <Filter size={14} aria-hidden="true" /> Advanced
                                </span>
                                <Switch checked={advanced} onChange={handleAdvancedToggle} ariaLabel="Advanced filters" />
                            </div>
                            <p className="text-xs text-(--base-06)">Pick loaders and Minecraft versions by hand.</p>
                            {/* Turning Advanced off resets both back to the server's stored values. */}
                            {advanced && (
                                <>
                                    <div>
                                        <span className="srv-label">Loaders</span>
                                        <div className="mt-2 flex flex-wrap gap-2" role="group" aria-label="Loaders">
                                            {LOADER_OPTIONS.map(l => {
                                                const on = filterLoaders.includes(l);
                                                return (
                                                    <button
                                                        key={l}
                                                        type="button"
                                                        className="srv-content-toggle"
                                                        aria-pressed={on}
                                                        onClick={() => setFilterLoaders(prev => on ? prev.filter(x => x !== l) : [...prev, l])}
                                                    >
                                                        {l}
                                                    </button>
                                                );
                                            })}
                                        </div>
                                    </div>
                                    <div>
                                        <label className="srv-label" htmlFor="content-game-versions">Game versions (comma separated)</label>
                                        <input
                                            id="content-game-versions"
                                            type="text"
                                            value={filterVersions.join(',')}
                                            onChange={e => setFilterVersions(e.target.value.split(',').map(s => s.trim()).filter(Boolean))}
                                            className="input-field input-mono mt-1 w-full text-xs"
                                            placeholder="1.20.2, 1.21"
                                        />
                                    </div>
                                </>
                            )}
                        </div>
                    </aside>

                    <div className="flex min-w-0 flex-col gap-4">
                    <div className="relative">
                        <Search size={16} className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-(--base-06)" aria-hidden="true" />
                        <input
                            type="search"
                            value={query}
                            onChange={e => setQuery(e.target.value)}
                            placeholder="Search mods, plugins, datapacks…"
                            aria-label="Search content"
                            className="input-field input-icon-left w-full"
                        />
                    </div>

                    {/* Results scroll inside their own box so the filter sidebar and the
                        page around them keep their own scroll. */}
                    <div className="srv-content-results">
                    {unknownLoader && (
                        <Callout tone="warning" icon={<AlertTriangle size={16} />}>
                            Unknown loader <span className="srv-num">{defaultLoader}</span>. Results are not pre-filtered by mod or plugin type.
                        </Callout>
                    )}

                    {searchLoading ? (
                        <div className="srv-content-list" aria-busy="true" aria-label="Searching">
                            {Array.from({ length: 6 }, (_, i) => (
                                <div key={i} className="srv-content-row">
                                    <Skeleton className="size-11 shrink-0 rounded-(--radius-md)" />
                                    <div className="flex-1 space-y-2">
                                        <SkeletonText width="w-1/3" className="h-3.5" />
                                        <SkeletonText width="w-2/3" className="h-3" />
                                    </div>
                                    <Skeleton className="h-8 w-24 rounded-(--radius-md)" />
                                </div>
                            ))}
                        </div>
                    ) : notice?.kind === 'failed' ? (
                        <Callout
                            tone="warning"
                            icon={<AlertTriangle size={16} />}
                            role="alert"
                            actions={
                                <button type="button" onClick={runSearch} className="btn btn-secondary btn-sm">
                                    <RefreshCw size={14} />
                                    Try again
                                </button>
                            }
                        >
                            {notice.message}
                        </Callout>
                    ) : notice ? (
                        <div className="srv-surface flex flex-col items-center gap-2 px-6 py-12 text-center">
                            <Search size={20} className="text-(--base-05)" aria-hidden="true" />
                            <p className="text-sm text-(--base-07)">{notice.message}</p>
                        </div>
                    ) : (
                        <div className="srv-content-list animate-content-in">
                            {visibleHits.map(hit => {
                                // isOnServer, not "a row exists": a queued install has not
                                // happened yet and a failed one never will, so neither is a
                                // thing to compare a newer build against - and neither should
                                // wear the badge that says the server has this mod.
                                const row = installedByProject.get(hit.project_id);
                                const installedMod = isOnServer(row) ? row : undefined;
                                const candidates = installedMod ? installedRowVersions.get(hit.project_id) : undefined;
                                // Release first; a pre-release only when nothing newer is
                                // released (Update all never offers one), so the button says
                                // which channel it moves to.
                                const rowLatest = installedMod && candidates
                                    ? rowUpdateTarget(installedMod.modrinthVersionId, candidates, { loaders: filterLoaders, mcVersions: filterVersions })
                                    : null;
                                const status: RowStatus = !installedMod
                                    ? 'not-installed'
                                    : candidates === undefined
                                        ? 'checking'
                                        : rowLatest ? 'update-available' : 'up-to-date';
                                return (
                                    <BrowseRow
                                        key={hit.project_id}
                                        hit={hit}
                                        status={status}
                                        sides={isModServer ? hit : null}
                                        updateChannel={rowLatest?.version_type}
                                        installed={!!installedMod}
                                        busy={busyProjects.has(hit.project_id)}
                                        job={installs.jobFor(serverId, hit.project_id)}
                                        selected={selectedSlug === hit.slug}
                                        fitsVersion={!advanced && defaultMcVersion && hit.versions?.includes(defaultMcVersion) ? defaultMcVersion : undefined}
                                        onOpen={() => openProjectDetail(hit.slug)}
                                        onInstall={() => handleRowInstall(hit)}
                                        onRemove={() => installedMod && handleRowRemove(installedMod)}
                                        onUpdate={() => {
                                            if (rowLatest) handleRowUpdate(hit, rowLatest);
                                        }}
                                    />
                                );
                            })}
                        </div>
                    )}
                    </div>
                    </div>
                </section>
            )}

            <ProjectDrawer
                open={selectedSlug !== null}
                onClose={closeProjectDetail}
                loading={projectLoading}
                project={projectDetail}
                versions={sortedVersions}
                highlightVersionId={highlightVersionId}
                mcVersion={defaultMcVersion}
                descMode={descMode}
                onDescMode={setDescMode}
                installedMod={projectDetail ? installedByProject.get(projectDetail.id) : undefined}
                jobFor={pid => installs.jobFor(serverId, pid)}
                planningVersion={planningVersion}
                onInstall={handleInstall}
            />

            {section === 'version' && server && (
                <ServerVersionPanel
                    server={server}
                    onChanged={refreshInstalled}
                    showToast={showToast}
                />
            )}

            {section === 'installed' && (
                <section className="flex flex-col gap-4" aria-label="Installed content">
                    {/* Before the branch below, not inside its non-empty arm: a server
                        whose jars were all placed by hand has NO managed mods, which is
                        the likeliest way to end up looking for this - and the empty
                        branch would have won. Renders nothing when there are none. */}
                    {server && (
                        <UnmanagedJars
                            serverId={serverId}
                            serverUuid={server.uuid}
                            activeSubServer={server.activeSubServer}
                            isModpack={isModpack}
                            onChanged={refreshInstalled}
                            showToast={showToast}
                        />
                    )}
                    {installedError ? (
                        <Callout
                            tone="warning"
                            icon={<AlertTriangle size={16} />}
                            role="alert"
                            actions={
                                <button
                                    type="button"
                                    onClick={() => retryInstalled(refreshInstalled)}
                                    disabled={retryingInstalled}
                                    className="btn btn-secondary btn-sm"
                                >
                                    {retryingInstalled ? 'Trying…' : 'Try again'}
                                </button>
                            }
                        >
                            {installedError}
                        </Callout>
                    ) : !installedLoaded ? (
                        <div className="srv-content-list" aria-busy="true" aria-label="Loading installed mods">
                            {Array.from({ length: 4 }, (_, i) => (
                                <div key={i} className="srv-content-row">
                                    <Skeleton className="size-11 shrink-0 rounded-(--radius-md)" />
                                    <div className="flex-1 space-y-2">
                                        <SkeletonText width="w-1/4" className="h-3.5" />
                                        <SkeletonText width="w-1/2" className="h-3" />
                                    </div>
                                </div>
                            ))}
                        </div>
                    ) : installed.length === 0 ? (
                        <div className="srv-surface flex animate-content-in flex-col items-center gap-2 px-6 py-12 text-center">
                            <Box size={20} className="text-(--base-05)" aria-hidden="true" />
                            <p className="text-sm text-(--base-07)">No mods installed.</p>
                            <button type="button" onClick={() => setSection('browse')} className="btn btn-secondary btn-sm mt-2">
                                Browse content
                            </button>
                        </div>
                    ) : (
                        <div className="flex animate-content-in flex-col gap-4">
                            <div className="flex flex-wrap items-center justify-between gap-3">
                                <div className="min-w-0 text-[13px] text-(--base-06)">
                                    {bulk
                                        ? bulk.checking
                                            ? `Checked ${bulk.done} of ${bulk.total}`
                                            : `Working through ${bulk.done + 1} of ${bulk.total} - ${bulk.current}`
                                        : updateAllBlocked
                                            ?? `Updates are limited to ${pinnedLoader} · MC ${pinnedMcVersion}, and never move a mod to a less stable channel.`}
                                </div>
                                <div className="flex shrink-0 flex-wrap items-center gap-3">
                                    {isModServer && (
                                        <ServerSideFilter value={env} onChange={setEnv} inline />
                                    )}
                                    <button
                                        onClick={handleUpdateAll}
                                        disabled={bulk !== null || updateAllBlocked !== null}
                                        title={updateAllBlocked ?? undefined}
                                        className="btn btn-primary btn-sm"
                                    >
                                        <RefreshCw size={14} className={bulk ? 'animate-spin motion-reduce:animate-none' : ''} />
                                        {bulk ? 'Working…' : 'Update all'}
                                    </button>
                                </div>
                            </div>
                            {/* Said once, next to the button that starts it, rather than in a
                                dialog nobody reads: the run is issued from this tab, so closing
                                it stops whatever has not been issued yet. What HAS been issued
                                is queued on Core and finishes either way. */}
                            {bulk && (
                                <Callout tone="warning" icon={<AlertTriangle size={16} />} role="status">
                                    Keep this tab open. Closing it stops the mods that have not been
                                    started yet; the ones already started will finish.
                                </Callout>
                            )}
                            {bulkSkipped.length > 0 && (
                                <Callout tone="warning" icon={<AlertTriangle size={16} />} title={`Skipped by Update all (${bulkSkipped.length})`}>
                                    <ul className="mt-1 space-y-0.5">
                                        {bulkSkipped.map((s, i) => (
                                            <li key={`${s.title}-${i}`}>{s.title}: {s.reason}</li>
                                        ))}
                                    </ul>
                                </Callout>
                            )}
                            {visibleInstalled.length < installed.length && (
                                <p className="text-xs text-(--base-06)">
                                    {installed.length - visibleInstalled.length} hidden by the {envFilterName(sideFilter)} filter under Environment. Update all still covers every mod.
                                </p>
                            )}
                            <div className="srv-content-list">
                                {visibleInstalled.map(m => {
                                    const job = installs.jobFor(serverId, m.modrinthProjectId);
                                    // Only a job this session follows animates. A row left at
                                    // installing by a lost report keeps the static label instead
                                    // of pulsing forever.
                                    const followed = m.status === 'installing' && !!job && isPending(job) && job.versionId === m.modrinthVersionId;
                                    return (
                                        <InstalledRow
                                            key={m.id}
                                            m={m}
                                            iconUrl={installedSides.get(m.modrinthProjectId)?.icon_url}
                                            sides={installedSides.get(m.modrinthProjectId)}
                                            showSides={isModServer}
                                            mismatch={mismatchFor(m)}
                                            busy={busyProjects.has(m.modrinthProjectId)}
                                            installJob={followed ? job : null}
                                            downloading={followed}
                                            history={historyFor(m.modrinthProjectId)}
                                            onSwap={() => handleSwap(m)}
                                            onRollback={h => handleRollback(m, h)}
                                            onToggleEnabled={() => handleToggleEnabled(m)}
                                            onTogglePaused={() => handleTogglePaused(m)}
                                            onUninstall={() => handleUninstall(m)}
                                        />
                                    );
                                })}
                            </div>
                        </div>
                    )}
                </section>
            )}
        </main>
    );
}
