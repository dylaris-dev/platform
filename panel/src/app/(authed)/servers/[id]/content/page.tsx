"use client";

import { useCallback, useEffect, useMemo, useRef, useState, type CSSProperties, type KeyboardEvent, type MouseEvent } from 'react';

import { Package, Search, Download, Trash2, ExternalLink, AlertTriangle, Filter, Box, X, RefreshCw, Info, ArrowUpRight, RotateCcw } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import { visibleCategoriesFor, categoryLabel } from '@/lib/modrinthCategories';
import { Skeleton, SkeletonText, SkeletonCard } from '@/components/Skeleton';
import { ModDescription } from '@/components/mods/ModDescription';
import { systemEvents } from '@/lib/systemEvents';
import {
    searchModrinth, getModrinthProject, getModrinthVersions, getModrinthCategories,
    listInstalledMods, uninstallMod, pickPrimaryFile,
    getServerModpackContents, getModHistory, getModrinthVersion,
    type ModHistoryEntry,
    type ModrinthSearchHit, type ModrinthSearchResult, type ModrinthProject,
    type ModrinthVersion, type ModrinthCategory, type InstalledMod,
} from '@/lib/api/modrinth';
import { pickNewestMatchingVersion, compareInstalledVsLatest, type ModStatus } from '@/lib/modVersionCompare';
import { LOADER_OPTIONS, isKnownLoader, isImportedServer } from '@/lib/serverLoaderMetadata';
import ServerVersionPanel from '@/components/mods/ServerVersionPanel';
import { UnmanagedJars } from '@/components/mods/UnmanagedJars';
import { isMcVersion } from '@/lib/validation';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import { declareServerLoaderMetadata } from '@/lib/api';
import { toast } from '@/components/ui/Toast';
import { useRouteId } from '@/lib/routeParams';
import { installedState, isOnServer } from '@/lib/installedState';
import {
    updatableMods, nextBuildFor, summarise, runWasClean, emptyTally,
    updateScopeLabel,
    type BulkProgress,
} from '@/lib/bulkModUpdate';
import { useBusy } from '@/lib/useBusy';
import { useModInstalls } from '@/lib/modInstallManager';
import { isPending, jobLabel, type InstallJob } from '@/lib/modInstallJobs';
import { InstallBar } from '@/components/ModInstallWidget';
import { createCoalescer } from '@/lib/coalesce';

// Modrinth Content tab, Modrinth-style layout: an always-visible category
// sidebar (with the loader + MC-version filters below it, gated behind an
// "Advanced" toggle), a single-column result list, and a detail column on the
// right (description with a short/full switch, plus the version list with the
// newest build for this server's MC version highlighted).

type Section = 'browse' | 'installed' | 'version';
// Row status shown in the browse list: 'checking' is a UI-only state (the
// matching-version fetch for an installed row is still in flight) layered on
// top of the pure ModStatus from lib/modVersionCompare.
type RowStatus = ModStatus | 'checking';

const PROJECT_TYPE_FOR_LOADER: Record<string, 'mod' | 'plugin'> = {
    paper: 'plugin', spigot: 'plugin', bukkit: 'plugin', purpur: 'plugin',
    velocity: 'plugin', waterfall: 'plugin', bungeecord: 'plugin',
    fabric: 'mod', forge: 'mod', quilt: 'mod', neoforge: 'mod',
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
    const defaultMcVersion = server?.minecraftVersion || '';

    const [section, setSection] = useState<Section>('browse');
    const [query, setQuery] = useState('');
    const [advanced, setAdvanced] = useState(false);
    const [filterLoaders, setFilterLoaders] = useState<string[]>(defaultLoader ? [defaultLoader] : []);
    const [filterVersions, setFilterVersions] = useState<string[]>(defaultMcVersion ? [defaultMcVersion] : []);
    const [searchResult, setSearchResult] = useState<ModrinthSearchResult | null>(null);
    const [searchLoading, setSearchLoading] = useState(false);

    // Category sidebar (single-select). Categories are proxied from Modrinth.
    const [categories, setCategories] = useState<ModrinthCategory[]>([]);
    const [categoriesLoading, setCategoriesLoading] = useState(true);
    const [selectedCategory, setSelectedCategory] = useState<string | null>(null);

    const [installed, setInstalled] = useState<InstalledMod[]>([]);
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
            limit: 20,
            index: 'relevance',
        });
        setSearchResult(res);
        setSearchLoading(false);
    }, [query, filterLoaders, filterVersions, selectedCategory, projectType]);

    // Debounce typed search input (and category/filter changes).
    useEffect(() => {
        const t = setTimeout(runSearch, 350);
        return () => clearTimeout(t);
    }, [runSearch]);

    // ----- Installed mods -----

    const [installedError, setInstalledError] = useState<string | null>(null);
    const [history, setHistory] = useState<ModHistoryEntry[]>([]);
    const [rollbackFor, setRollbackFor] = useState<string | null>(null);

    // Newest first, which is the order Core returns and the order a rollback
    // wants: the build you had immediately before is the one you almost always
    // mean.
    // Non-null while a bulk run is going. Read by the button labels, the
    // progress line and the close guard.
    const [bulk, setBulk] = useState<BulkProgress | null>(null);
    // The ref, not the state, is what the loop checks: state does not update
    // until the next render, and the loop is inside one tick.
    const bulkCancelled = useRef(false);

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

    const historyFor = useCallback(
        (projectId: string) => history.filter(h => h.modrinthProjectId === projectId),
        [history],
    );
    const [retryingInstalled, retryInstalled] = useBusy();

    const refreshInstalled = useCallback(async () => {
        if (!serverId) return;
        try {
            setInstalled(await listInstalledMods(serverId));
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
        }
    }, [serverId]);

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
            const contents = await getServerModpackContents(serverId);
            if (cancelled) return;
            const next = new Map<string, { versionId: string; versionNumber: string }>();
            for (const c of contents) {
                next.set(c.modrinthProjectId, { versionId: c.modrinthVersionId, versionNumber: c.modrinthVersionNumber });
            }
            setPackByProject(next);
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
        if (packByProject.size > 0 && project.client_side === 'required') {
            return confirmDialog({
                title: `"${project.title}" is client-side too`,
                message: `It must run on each player's client as well, or they will not be able to connect, and it is not part of the distributed modpack.`,
                confirmLabel: 'Install anyway',
            });
        }
        return true;
    };

    // Fires the actual install call (no confirm - callers run
    // confirmModpackCrossCheck first). Progress and the outcome show on the
    // button and in the navbar widget; a failure also toasts from the manager.
    const doInstall = async (project: ModrinthProject, version: ModrinthVersion) => {
        const file = pickPrimaryFile(version);
        if (!file) { showToast('Version has no downloadable file', false); return; }
        const job = await installs.start(serverId, serverName, {
            projectId: project.id,
            projectSlug: project.slug,
            versionId: version.id,
            title: project.title,
            fileName: file.filename,
            downloadUrl: file.url,
            sha512: file.hashes.sha512,
        });
        if (job.state !== 'failed') refreshInstalledSoon();
    };

    const handleInstall = async (project: ModrinthProject, version: ModrinthVersion) => {
        if (!await confirmModpackCrossCheck(project, version)) return;
        await doInstall(project, version);
    };

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
        setRollbackFor(null);
        await withBusy(m.modrinthProjectId, async () => {
            let version;
            try {
                version = await getModrinthVersion(entry.modrinthVersionId);
            } catch (e) {
                showToast(e instanceof Error ? e.message : 'Could not load that build', false);
                return;
            }
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

    // Update every installed mod that has a newer matching build.
    //
    // Sequential on purpose. Each install is a queued command plus a Modrinth
    // version lookup, and firing thirty of both at once buys nothing - the work
    // happens on the node afterwards either way - while making the failure of
    // any one of them harder to attribute and the progress meaningless.
    const handleUpdateAll = async () => {
        const targets = updatableMods(installed);
        if (targets.length === 0) { showToast('Nothing installed to update', false); return; }
        bulkCancelled.current = false;
        const tally = emptyTally();
        const loaders = filterLoaders.length ? filterLoaders : undefined;
        const mcVersions = filterVersions.length ? filterVersions : undefined;

        for (const [i, m] of targets.entries()) {
            if (bulkCancelled.current) break;
            setBulk({ done: i, total: targets.length, current: m.title || m.fileName });
            const slug = m.modrinthProjectSlug || m.modrinthProjectId;
            const candidates = await getModrinthVersions(slug, { loaders, versions: mcVersions });
            // getModrinthVersions returns [] both for "no build matches" and for
            // a request that failed, and those are not the same answer. An empty
            // list is counted as unchecked rather than as current, so the summary
            // cannot report a mod as up to date on the strength of a lookup that
            // never happened.
            if (candidates.length === 0) { tally.unknown++; continue; }
            const next = nextBuildFor(m, candidates, { loaders, mcVersions });
            if (!next) { tally.current++; continue; }
            const file = pickPrimaryFile(next);
            if (!file) { tally.failed++; continue; }
            const job = await installs.start(serverId, serverName, {
                projectId: m.modrinthProjectId,
                projectSlug: m.modrinthProjectSlug,
                versionId: next.id,
                title: m.title,
                fileName: file.filename,
                downloadUrl: file.url,
                sha512: file.hashes.sha512,
            });
            if (job.state !== 'failed') tally.updated++; else tally.failed++;
        }

        setBulk(null);
        refreshInstalled();
        showToast(summarise(tally), runWasClean(tally));
    };

    // Put every mod back to the build it had before its last update.
    //
    // The counterpart to the run above and the reason it can be offered at all:
    // each update files the build it replaced, so a bulk update that breaks the
    // server has a bulk way back.
    const handleRollbackAll = async () => {
        const targets = updatableMods(installed).filter(m => historyFor(m.modrinthProjectId).length > 0);
        if (targets.length === 0) { showToast('No earlier builds to go back to', false); return; }
        bulkCancelled.current = false;
        const tally = emptyTally();

        for (const [i, m] of targets.entries()) {
            if (bulkCancelled.current) break;
            setBulk({ done: i, total: targets.length, current: m.title || m.fileName });
            const entry = historyFor(m.modrinthProjectId)[0];
            let version;
            try {
                version = await getModrinthVersion(entry.modrinthVersionId);
            } catch { tally.unknown++; continue; }
            const file = pickPrimaryFile(version);
            if (!file) { tally.failed++; continue; }
            const job = await installs.start(serverId, serverName, {
                projectId: m.modrinthProjectId,
                projectSlug: m.modrinthProjectSlug,
                versionId: version.id,
                title: m.title,
                fileName: file.filename,
                downloadUrl: file.url,
                sha512: file.hashes.sha512,
            });
            if (job.state !== 'failed') tally.updated++; else tally.failed++;
        }

        setBulk(null);
        refreshInstalled();
        showToast(summarise(tally), runWasClean(tally));
    };

    const handleUninstall = async (m: InstalledMod) => {
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
    // browse row) and install it.
    const handleRowInstall = (hit: ModrinthSearchHit) => withBusy(hit.project_id, async () => {
        const loaders = filterLoaders.length ? filterLoaders : undefined;
        const versions = filterVersions.length ? filterVersions : undefined;
        const [project, candidates] = await Promise.all([
            getModrinthProject(hit.slug),
            getModrinthVersions(hit.slug, { loaders, versions }),
        ]);
        if (!project) { showToast('Failed to load project details', false); return; }
        const latest = pickNewestMatchingVersion(candidates, { loaders: filterLoaders, mcVersions: filterVersions });
        if (!latest) { showToast(`No version of "${hit.title}" matches the current loader / MC-version filter`, false); return; }
        await handleInstall(project, latest);
    });

    const handleRowRemove = (installedMod: InstalledMod) => withBusy(installedMod.modrinthProjectId, async () => {
        await handleUninstall(installedMod);
    });

    // Row "Update": remove the old file THEN install the newer one, so the
    // mods/plugins folder never ends up holding both jars for the same
    // project. The modpack cross-check confirm runs BEFORE the removal so a
    // cancelled confirm never leaves the server with the mod uninstalled.
    const handleRowUpdate = (hit: ModrinthSearchHit, installedMod: InstalledMod, latest: ModrinthVersion) =>
        withBusy(hit.project_id, async () => {
            const project = await getModrinthProject(hit.slug);
            if (!project) { showToast('Failed to load project details', false); return; }
            if (!await confirmModpackCrossCheck(project, latest)) return;
            const removeRes = await uninstallMod(serverId, installedMod.id);
            if (!removeRes.success) {
                showToast(removeRes.message || 'Update failed: could not remove the old version', false);
                return;
            }
            await doInstall(project, latest);
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

    // h-full, not flex-1: ServerShell wraps every server page in a scrolling
    // `flex-1 overflow-y-auto p-6` box, which is a BLOCK, so flex-1 here sized
    // nothing and the page was as tall as its content. The whole window
    // scrolled, the three columns below never reached their own overflow, and
    // the p-6 sat inside the shell's p-6. Taking the shell's height instead is
    // what files/page.tsx already does.
    return (
        <main className="h-full flex flex-col gap-4 overflow-hidden">
            <header className="flex items-center gap-3 shrink-0">
                <Package size={20} className="text-(--accent-light)" />
                <h1 className="text-base font-display font-semibold text-(--base-09)">Content</h1>
                <span className="text-xs text-(--base-06) hidden sm:inline">
                    Modrinth-powered mods + plugins
                    {!advanced && (defaultLoader || defaultMcVersion) && (
                        <> · auto-filter: <code className="font-mono">{[defaultLoader, defaultMcVersion].filter(Boolean).join(' · ')}</code></>
                    )}
                </span>
            </header>

            {/* Section strip */}
            <nav className="flex gap-1 shrink-0 border-b border-(--base-03)">
                {([
                    { id: 'browse' as const, label: 'Browse', Icon: Search },
                    { id: 'installed' as const, label: `Installed (${installed.length})`, Icon: Box },
                    { id: 'version' as const, label: 'Minecraft version', Icon: ArrowUpRight },
                ]).map(({ id, label, Icon }) => (
                    <button
                        key={id}
                        onClick={() => setSection(id)}
                        className={`flex items-center gap-1.5 px-3 py-2 text-sm font-medium border-b-2 transition-colors ${
                            section === id
                                ? 'border-(--accent) text-(--accent-light)'
                                : 'border-transparent text-(--base-07) hover:text-(--base-09) hover:border-(--base-04)'
                        }`}
                    >
                        <Icon size={12} />
                        {label}
                    </button>
                ))}
            </nav>

            {imported && !declareDismissed && (
                <div className="shrink-0 flex items-start gap-2 px-3 py-2.5 rounded-md border border-(--accent-border) bg-(--accent-ghost) text-xs text-(--base-07)">
                    <Info size={13} className="mt-0.5 shrink-0 text-(--accent-light)" />
                    <div className="flex-1 min-w-0">
                        {!declareOpen ? (
                            <>
                                <p className="text-(--base-09) font-medium">Declare this server's loader + Minecraft version</p>
                                <p className="mt-0.5">
                                    This looks like an imported server - it's missing loader/version info, so auto-filtering and
                                    version highlighting above are off. Declaring them is optional, recommended, and does not
                                    reinstall anything; manual/Advanced filtering keeps working either way.
                                </p>
                                <button onClick={handleDeclareOpen} className="btn btn-primary btn-sm mt-2">
                                    Declare now
                                </button>
                            </>
                        ) : (
                            <form
                                onSubmit={e => { e.preventDefault(); handleDeclareSubmit(); }}
                                className="space-y-2"
                            >
                                <div className="flex items-end gap-2 flex-wrap">
                                    <div>
                                        <label className="input-label mb-0">Loader</label>
                                        <select
                                            value={declareLoader}
                                            onChange={e => setDeclareLoader(e.target.value)}
                                            disabled={declareSubmitting}
                                            className="input-field text-xs mt-1"
                                        >
                                            <option value="">Select…</option>
                                            {LOADER_OPTIONS.map(l => <option key={l} value={l}>{l}</option>)}
                                        </select>
                                    </div>
                                    <div>
                                        <label className="input-label mb-0">Minecraft version</label>
                                        <input
                                            type="text"
                                            value={declareMcVersion}
                                            onChange={e => setDeclareMcVersion(e.target.value)}
                                            disabled={declareSubmitting}
                                            placeholder="1.20.4"
                                            className="input-field input-mono text-xs mt-1 w-28"
                                        />
                                    </div>
                                    <button
                                        type="submit"
                                        disabled={declareSubmitting || !declareLoader || !isMcVersion(declareMcVersion.trim())}
                                        className="btn btn-primary btn-sm"
                                    >
                                        {declareSubmitting && <RefreshCw size={11} className="animate-spin" />}
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
                    </div>
                    <button
                        onClick={() => setDeclareDismissed(true)}
                        className="text-(--base-06) hover:text-(--base-09) transition-colors shrink-0"
                        title="Dismiss"
                    >
                        <X size={14} />
                    </button>
                </div>
            )}

            {packByProject.size > 0 && (
                <div className="shrink-0 flex items-start gap-2 px-3 py-2 rounded-md border border-(--base-04) bg-(--base-03) text-xs text-(--base-07)">
                    <Package size={13} className="mt-0.5 shrink-0 text-(--accent-light)" />
                    <span>
                        This server runs a modpack. Mods you add here are not part of the distributed pack;
                        players who lack a required client-side mod may fail to connect.
                    </span>
                </div>
            )}

            {section === 'browse' && (
                <div className="flex-1 flex gap-4 overflow-hidden min-h-0">
                    {/* Category sidebar (always visible) + Advanced-gated filters */}
                    <aside className="w-60 shrink-0 overflow-y-auto card p-3 space-y-4">
                        <div>
                            <label className="input-label">Categories</label>
                            {/* One bordered button per category. The old flush 0.5-gap
                                stack of borderless rows read as a single block of text
                                rather than as a list of controls. */}
                            <div className="mt-2 flex flex-col gap-1.5">
                                {categoriesLoading ? (
                                    <div className="space-y-1.5 py-1">
                                        {[0, 1, 2, 3, 4, 5].map(i => <SkeletonText key={i} width="w-full" className="h-6" />)}
                                    </div>
                                ) : visibleCategories.length === 0 ? (
                                    <p className="text-xs text-(--base-06) italic">No categories for this server type.</p>
                                ) : (
                                    visibleCategories.map(cat => {
                                        const on = selectedCategory === cat.name;
                                        return (
                                            <button
                                                key={cat.name}
                                                type="button"
                                                aria-pressed={on}
                                                onClick={() => setSelectedCategory(on ? null : cat.name)}
                                                className={`w-full flex items-center gap-2 px-2.5 py-1.5 rounded-md border text-[13px] capitalize transition-colors ${
                                                    on
                                                        ? 'bg-(--accent-ghost) border-(--accent-border) text-(--accent-light)'
                                                        : 'bg-(--base-02) border-(--base-03) text-(--base-07) hover:bg-(--base-03) hover:border-(--base-04) hover:text-(--base-09)'
                                                }`}
                                            >
                                                {/* The icon is an SVG from Modrinth's API. It used to be
                                                    inserted as raw HTML on the panel's origin, which put
                                                    whatever that response held - a style block, a form, a
                                                    full-page link - into the panel. Used as a mask it is an
                                                    image: it cannot run, style or link anything, and it still
                                                    takes the text colour the way the inline SVG did. */}
                                                <span
                                                    className="shrink-0 w-4 h-4 bg-current"
                                                    aria-hidden="true"
                                                    style={categoryIconMask(cat.icon)}
                                                />
                                                <span className="truncate">{categoryLabel(cat.name)}</span>
                                            </button>
                                        );
                                    })
                                )}
                            </div>
                        </div>

                        {/* Advanced toggle - gates the loader / MC-version filters below.
                            Turning it off resets both back to the server's stored values. */}
                        <div className="border-t border-(--base-03) pt-3 flex items-center justify-between gap-2">
                            <div className="min-w-0">
                                <div className="flex items-center gap-1.5 text-xs font-medium text-(--base-09)">
                                    <Filter size={11} className="text-(--base-06)" />
                                    Advanced
                                </div>
                                <p className="text-[10px] text-(--base-06) mt-0.5">Edit loader / MC-version filters</p>
                            </div>
                            <button
                                type="button"
                                role="switch"
                                aria-checked={advanced}
                                onClick={handleAdvancedToggle}
                                className={`toggle-track shrink-0 ${advanced ? 'toggle-track-on' : 'toggle-track-off'}`}
                                title="Toggle loader / version filters"
                            >
                                <span className={`toggle-knob ${advanced ? 'toggle-knob-on' : 'toggle-knob-off'}`} />
                            </button>
                        </div>

                        {/* Loader + MC-version filters, greyed until Advanced is on. */}
                        <div className={advanced ? '' : 'opacity-50 pointer-events-none select-none'}>
                            <label className="input-label mb-0">Loaders</label>
                            {/* No max-height and no scroller of its own: the whole
                                sidebar scrolls now, and a scroll area nested inside a
                                scroll area is the one that eats the wheel event the
                                reader meant for the other. */}
                            <div className="mt-1.5 space-y-1">
                                {LOADER_OPTIONS.map(l => {
                                    const on = filterLoaders.includes(l);
                                    return (
                                        <label key={l} className="flex items-center gap-2 text-xs cursor-pointer hover:text-(--base-09)">
                                            <input
                                                type="checkbox"
                                                checked={on}
                                                disabled={!advanced}
                                                onChange={() => setFilterLoaders(prev => on ? prev.filter(x => x !== l) : [...prev, l])}
                                                className="checkbox"
                                            />
                                            {l}
                                        </label>
                                    );
                                })}
                            </div>
                            <label className="input-label mt-3">Game versions (comma)</label>
                            <input
                                type="text"
                                value={filterVersions.join(',')}
                                onChange={e => setFilterVersions(e.target.value.split(',').map(s => s.trim()).filter(Boolean))}
                                disabled={!advanced}
                                className="input-field input-mono w-full text-xs"
                                placeholder="1.20.2, 1.21"
                            />
                        </div>
                    </aside>

                    {/* Result list (single column). Real min-width so it never
                        collapses to unusable once the detail column claims its share. */}
                    <div className="flex-1 min-w-[360px] flex flex-col overflow-hidden">
                        <div className="relative shrink-0 mb-3">
                            <Search size={13} className="absolute left-3 top-1/2 -translate-y-1/2 text-(--base-05)" />
                            <input
                                type="text"
                                value={query}
                                onChange={e => setQuery(e.target.value)}
                                placeholder="Search mods, plugins, datapacks…"
                                className="input-field w-full pl-8"
                            />
                        </div>

                        {unknownLoader && (
                            <div className="shrink-0 mb-2 px-3 py-2 rounded-md bg-(--warning-ghost) border border-(--warning)/30 text-(--warning-light) text-xs flex items-start gap-2">
                                <AlertTriangle size={12} className="mt-0.5 shrink-0" />
                                Unknown loader <code className="font-mono">{defaultLoader}</code> — results aren't pre-filtered by mod/plugin type.
                            </div>
                        )}

                        <div className="flex-1 overflow-y-auto space-y-2">
                            {searchLoading ? (
                                <div className="text-center py-12 text-sm text-(--base-06)">Searching…</div>
                            ) : !searchResult || searchResult.hits.length === 0 ? (
                                <div className="text-center py-12 text-sm text-(--base-06)">No projects match.</div>
                            ) : (
                                searchResult.hits.map(hit => {
                                    // isOnServer, not "a row exists": a queued install has not
                                    // happened yet and a failed one never will, so neither is a
                                    // thing to compare a newer build against - and neither should
                                    // wear the badge that says the server has this mod.
                                    const row = installedByProject.get(hit.project_id);
                                    const installedMod = isOnServer(row) ? row : undefined;
                                    const candidates = installedMod ? installedRowVersions.get(hit.project_id) : undefined;
                                    const status: RowStatus = !installedMod
                                        ? 'not-installed'
                                        : candidates === undefined
                                            ? 'checking'
                                            : compareInstalledVsLatest(installedMod.modrinthVersionId, candidates, { loaders: filterLoaders, mcVersions: filterVersions });
                                    return (
                                        <ModListRow
                                            key={hit.project_id}
                                            hit={hit}
                                            status={status}
                                            installed={!!installedMod}
                                            busy={busyProjects.has(hit.project_id)}
                                            job={installs.jobFor(serverId, hit.project_id)}
                                            selected={selectedSlug === hit.slug}
                                            onOpen={() => openProjectDetail(hit.slug)}
                                            onInstall={() => handleRowInstall(hit)}
                                            onRemove={() => installedMod && handleRowRemove(installedMod)}
                                            onUpdate={() => {
                                                if (!installedMod || !candidates) return;
                                                const latest = pickNewestMatchingVersion(candidates, { loaders: filterLoaders, mcVersions: filterVersions });
                                                if (latest) handleRowUpdate(hit, installedMod, latest);
                                            }}
                                        />
                                    );
                                })
                            )}
                        </div>
                    </div>

                    {/* Detail column - hidden below `xl` so it never squeezes the
                        list to unusable on narrow windows; roughly half the
                        remaining width from `xl` up, capped so it doesn't grow
                        absurdly wide on very large screens. */}
                    {(projectDetail || projectLoading) && (
                        <aside className="hidden xl:flex xl:w-[46%] xl:max-w-2xl shrink-0 flex-col card overflow-hidden">
                            {projectLoading || !projectDetail ? (
                                <div className="p-4 space-y-3">
                                    <div className="flex items-start gap-3">
                                        <Skeleton className="w-12 h-12 rounded-md shrink-0" />
                                        <div className="flex-1 space-y-2">
                                            <SkeletonText width="w-1/2" className="h-4" />
                                            <SkeletonText width="w-3/4" className="h-2.5" />
                                        </div>
                                    </div>
                                    <SkeletonCard height="h-40" />
                                </div>
                            ) : (
                                <>
                                    <div className="shrink-0 p-4 border-b border-(--base-03) flex items-start gap-3">
                                        {projectDetail.icon_url && (
                                            // eslint-disable-next-line @next/next/no-img-element
                                            <img src={projectDetail.icon_url} alt="" className="w-12 h-12 rounded-md shrink-0" />
                                        )}
                                        <div className="min-w-0 flex-1">
                                            <h3 className="text-sm font-semibold text-(--base-09) truncate">{projectDetail.title}</h3>
                                            <p className="mt-1 text-[10px] font-mono text-(--base-06)">{projectDetail.downloads.toLocaleString()} downloads</p>
                                            {/* The description below is what Modrinth renders on its own page, but
                                                only what a description can be: no version history, no gallery, no
                                                comments. This is the way out to the rest, so it is a button rather
                                                than the 10px text link it used to be next to the download count. */}
                                            <a
                                                href={`https://modrinth.com/project/${projectDetail.slug}`}
                                                target="_blank"
                                                rel="noopener noreferrer"
                                                className="btn btn-secondary btn-sm mt-2"
                                            >
                                                Open on Modrinth <ExternalLink size={12} />
                                            </a>
                                        </div>
                                        <button onClick={closeProjectDetail} className="text-(--base-06) hover:text-(--base-09) transition-colors shrink-0" title="Close">
                                            <X size={16} />
                                        </button>
                                    </div>

                                    <div className="flex-1 overflow-y-auto p-4 space-y-4">
                                        {/* Description with a short/full switch */}
                                        <div>
                                            <div className="flex items-center justify-between mb-1.5">
                                                <label className="input-label mb-0">Description</label>
                                                <div className="flex bg-(--base-03)/60 rounded-md p-0.5">
                                                    {(['short', 'full'] as const).map(m => (
                                                        <button
                                                            key={m}
                                                            onClick={() => setDescMode(m)}
                                                            className={`px-2 py-0.5 rounded-sm text-[10px] font-medium capitalize transition-colors ${
                                                                descMode === m ? 'bg-(--base-02) text-(--base-09)' : 'text-(--base-06) hover:text-(--base-09)'
                                                            }`}
                                                        >
                                                            {m}
                                                        </button>
                                                    ))}
                                                </div>
                                            </div>
                                            {descMode === 'short' ? (
                                                <p className="text-sm text-(--base-07) leading-relaxed">{projectDetail.description}</p>
                                            ) : projectDetail.body ? (
                                                <ModDescription body={projectDetail.body} />
                                            ) : (
                                                <p className="text-xs text-(--base-06) italic">No full description provided.</p>
                                            )}
                                        </div>

                                        {/* Versions — newest build for this MC version highlighted */}
                                        <div>
                                            <label className="input-label">Versions</label>
                                            <div className="mt-1.5 space-y-1.5">
                                                {sortedVersions.length === 0 ? (
                                                    <p className="text-xs text-(--base-06)">No compatible versions found.</p>
                                                ) : (
                                                    sortedVersions.slice(0, 30).map(v => {
                                                        const highlight = v.id === highlightVersionId;
                                                        // Which build is on the server. The list used to say only
                                                        // which build is NEWEST, so the one question you open it
                                                        // with - what have I got - had no answer here.
                                                        const state = installedState(installedByProject.get(projectDetail.id), v.id);
                                                        const vJob = installs.jobFor(serverId, projectDetail.id);
                                                        const job = vJob && vJob.versionId === v.id ? vJob : undefined;
                                                        const jobBusy = !!job && isPending(job);
                                                        return (
                                                            <div
                                                                key={v.id}
                                                                className={`flex items-center gap-2 p-2 rounded-md border ${
                                                                    state === 'installed' ? 'border-(--success)/40 bg-(--success-ghost)'
                                                                        : state === 'failed' ? 'border-(--warning)/40 bg-(--warning-ghost)'
                                                                        : highlight ? 'border-(--accent-border) bg-(--accent-ghost)'
                                                                        : 'border-(--base-04)'
                                                                }`}
                                                            >
                                                                <div className="min-w-0 flex-1">
                                                                    <div className="flex items-center gap-1.5 flex-wrap">
                                                                        <span className="text-sm font-medium text-(--base-09) truncate">{v.version_number}</span>
                                                                        {state === 'installed' && <span className="mono-label text-(--success) shrink-0">on this server</span>}
                                                                        {state === 'installing' && <span className="mono-label text-(--base-06) shrink-0">installing…</span>}
                                                                        {state === 'failed' && <span className="mono-label text-(--warning-light) shrink-0">install failed</span>}
                                                                        {highlight && state === null && <span className="mono-label text-(--accent-light) shrink-0">newest · {defaultMcVersion}</span>}
                                                                    </div>
                                                                    <div className="text-[10px] font-mono text-(--base-06) truncate">
                                                                        {v.version_type} · {v.loaders.join(', ')} · MC {v.game_versions.join(', ')}
                                                                    </div>
                                                                    {job && jobBusy && <InstallBar job={job} />}
                                                                    {job && (job.state === 'failed' || job.state === 'unknown') && job.message && (
                                                                        <p className="mt-1 text-[11px] text-(--warning-light)">{job.message}</p>
                                                                    )}
                                                                </div>
                                                                <button
                                                                    onClick={() => handleInstall(projectDetail, v)}
                                                                    disabled={state === 'installing' || jobBusy}
                                                                    aria-busy={jobBusy}
                                                                    className="btn btn-secondary btn-sm shrink-0"
                                                                >
                                                                    {jobBusy ? <RefreshCw size={11} className="animate-spin motion-reduce:animate-none" /> : <Download size={11} />}
                                                                    {job && jobBusy ? jobLabel(job) : state === 'installed' ? 'Reinstall' : state === 'failed' ? 'Try again' : 'Install'}
                                                                </button>
                                                            </div>
                                                        );
                                                    })
                                                )}
                                            </div>
                                        </div>
                                    </div>
                                </>
                            )}
                        </aside>
                    )}
                </div>
            )}

            {section === 'version' && server && (
                <ServerVersionPanel
                    server={server}
                    onChanged={refreshInstalled}
                    showToast={showToast}
                />
            )}

            {section === 'installed' && (
                <div className="flex-1 overflow-y-auto space-y-2">
                    {/* Before the branch below, not inside its non-empty arm: a server
                        whose jars were all placed by hand has NO managed mods, which is
                        the likeliest way to end up looking for this - and the empty
                        branch would have won. Renders nothing when there are none. */}
                    {server && (
                        <UnmanagedJars
                            serverId={serverId}
                            serverUuid={server.uuid}
                            activeSubServer={server.activeSubServer}
                            onChanged={refreshInstalled}
                            showToast={showToast}
                        />
                    )}
                    {installedError ? (
                        <div className="text-center py-12 text-sm text-(--warning-light)">
                            <AlertTriangle size={20} className="mx-auto mb-2" />
                            {installedError}
                            <div>
                                <button
                                    type="button"
                                    onClick={() => retryInstalled(refreshInstalled)}
                                    disabled={retryingInstalled}
                                    className="btn btn-secondary btn-sm mt-3"
                                >
                                    {retryingInstalled ? 'Trying…' : 'Try again'}
                                </button>
                            </div>
                        </div>
                    ) : installed.length === 0 ? (
                        <div className="text-center py-12 text-sm text-(--base-06)">No mods installed.</div>
                    ) : (
                        <div className="space-y-2">
                            <div className="flex items-center justify-between gap-3 flex-wrap pb-1">
                                <div className="text-xs text-(--base-06)">
                                    {bulk
                                        ? `Working through ${bulk.done + 1} of ${bulk.total} - ${bulk.current}`
                                        : updateScopeLabel(filterLoaders, filterVersions)}
                                </div>
                                <div className="flex items-center gap-1.5 shrink-0">
                                    <button
                                        onClick={handleUpdateAll}
                                        disabled={bulk !== null}
                                        className="btn btn-secondary btn-sm"
                                    >
                                        <RefreshCw size={12} className={bulk ? 'animate-spin' : ''} />
                                        {bulk ? 'Working…' : 'Update all'}
                                    </button>
                                    {installed.some(m => historyFor(m.modrinthProjectId).length > 0) && (
                                        <button
                                            onClick={handleRollbackAll}
                                            disabled={bulk !== null}
                                            className="btn btn-secondary btn-sm"
                                            title="Put every mod back to the build it had before its last update"
                                        >
                                            <RotateCcw size={12} />
                                            Roll back all
                                        </button>
                                    )}
                                </div>
                            </div>
                            {/* Said once, next to the button that starts it, rather than in a
                                dialog nobody reads: the run is issued from this tab, so closing
                                it stops whatever has not been issued yet. What HAS been issued
                                is queued on Core and finishes either way. */}
                            {bulk && (
                                <p className="text-[11px] text-(--warning-light) pb-1">
                                    Keep this tab open. Closing it stops the mods that have not been
                                    started yet; the ones already started will finish.
                                </p>
                            )}
                            {installed.map(m => (
                                <article key={m.id} className="card p-3 flex items-start gap-3">
                                    <div className="w-10 h-10 rounded-md bg-(--base-03) flex items-center justify-center shrink-0">
                                        <Box size={16} className="text-(--accent-light)" />
                                    </div>
                                    <div className="min-w-0 flex-1">
                                        <div className="text-sm font-medium text-(--base-09)">{m.title || m.fileName}</div>
                                        <div className="text-xs text-(--base-06) font-mono">{m.fileName}</div>
                                        <div className="text-xs text-(--base-06) mt-0.5">
                                            {m.status === 'installing'
                                                ? 'Installing - the node is downloading it'
                                                : m.status === 'failed'
                                                ? null
                                                : `Installed ${new Date(m.installedAt).toLocaleString()}`}
                                        </div>
                                        {/* The node reported a reason and this is the only place it can
                                            reach. Core used to write this row before dispatching and never
                                            revisit it, so a download that 404ed listed as an installed mod. */}
                                        {/* Only a job this session is following animates. A row
                                            left at installing by a lost report keeps the static label
                                            above instead of pulsing forever. */}
                                        {(() => {
                                            const job = installs.jobFor(serverId, m.modrinthProjectId);
                                            return m.status === 'installing' && job && isPending(job) && job.versionId === m.modrinthVersionId
                                                ? <InstallBar job={job} />
                                                : null;
                                        })()}
                                        {m.status === 'failed' && (
                                            <div className="mt-1 flex items-start gap-1.5 text-xs text-(--warning-light)">
                                                <AlertTriangle size={12} className="mt-0.5 shrink-0" />
                                                <span>
                                                    This did not install. The server is still running whatever it
                                                    had before.{m.statusMessage ? ` ${m.statusMessage}` : ''}
                                                </span>
                                            </div>
                                        )}
                                    </div>
                                    <div className="flex items-center gap-1 shrink-0">
                                        {/* Only the builds this server actually ran. Modrinth's own
                                            version list is one click away and offers everything; what it
                                            cannot tell you is which build you had before the update that
                                            broke something. */}
                                        {historyFor(m.modrinthProjectId).length > 0 && (
                                            <div className="relative">
                                                <button
                                                    onClick={() => setRollbackFor(rollbackFor === m.modrinthProjectId ? null : m.modrinthProjectId)}
                                                    disabled={busyProjects.has(m.modrinthProjectId)}
                                                    className="btn btn-secondary btn-sm"
                                                    title="Go back to an earlier build"
                                                >
                                                    <RotateCcw size={12} />
                                                    Roll back
                                                </button>
                                                {rollbackFor === m.modrinthProjectId && (
                                                    <div className="dropdown-menu right-0 mt-1 w-72 animate-fade-in origin-top-right">
                                                        <div className="dropdown-label px-3 pt-2 pb-1">Builds this server had</div>
                                                        {historyFor(m.modrinthProjectId).map(h => (
                                                            <button
                                                                key={h.id}
                                                                onClick={() => handleRollback(m, h)}
                                                                className="dropdown-item flex-col items-start gap-0"
                                                            >
                                                                <span className="font-mono text-xs truncate w-full text-left">{h.fileName}</span>
                                                                <span className="text-[10px] text-(--base-06)">
                                                                    replaced {new Date(h.replacedAt).toLocaleString()}
                                                                </span>
                                                            </button>
                                                        ))}
                                                    </div>
                                                )}
                                            </div>
                                        )}
                                        <a
                                            href={`https://modrinth.com/project/${m.modrinthProjectSlug || m.modrinthProjectId}`}
                                            target="_blank"
                                            rel="noopener noreferrer"
                                            className="btn btn-secondary btn-sm"
                                            title="View on Modrinth"
                                        >
                                            <ExternalLink size={12} />
                                        </a>
                                        <button onClick={() => handleUninstall(m)} className="btn btn-secondary btn-sm">
                                            <Trash2 size={12} className="text-(--error)" />
                                            Remove
                                        </button>
                                    </div>
                                </article>
                            ))}
                        </div>
                    )}
                </div>
            )}

        </main>
    );
}

function ModListRow({
    hit,
    status,
    installed,
    busy,
    job,
    selected,
    onOpen,
    onInstall,
    onRemove,
    onUpdate,
}: {
    hit: ModrinthSearchHit;
    status: RowStatus;
    installed: boolean;
    busy: boolean;
    job?: InstallJob;
    selected: boolean;
    onOpen: () => void;
    onInstall: () => void;
    onRemove: () => void;
    onUpdate: () => void;
}) {
    // The row opens the detail column on click, but also hosts a nested real
    // <button> for install/remove/update - a <button> can't nest another
    // <button>, so the row itself is a div with button semantics (role +
    // keyboard handling) and the action button stops propagation.
    const handleKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
        if (e.key === 'Enter' || e.key === ' ') {
            e.preventDefault();
            onOpen();
        }
    };
    // The install job outlives the click: busy covers resolving the version,
    // the job covers Core and the node until they answer.
    const jobBusy = !!job && isPending(job);
    const actionBusy = busy || jobBusy;
    const stopAnd = (fn: () => void) => (e: MouseEvent) => {
        e.stopPropagation();
        fn();
    };

    return (
        <div
            role="button"
            tabIndex={0}
            onClick={onOpen}
            onKeyDown={handleKeyDown}
            className={`card p-3 w-full flex items-start gap-3 transition-colors cursor-pointer focus:outline-none focus:border-(--accent-border) ${
                selected ? 'border-(--accent-border)' : 'hover:border-(--base-04)'
            }`}
        >
            {hit.icon_url ? (
                // eslint-disable-next-line @next/next/no-img-element
                <img src={hit.icon_url} alt="" className="w-11 h-11 rounded-md shrink-0" />
            ) : (
                <div className="w-11 h-11 rounded-md bg-(--base-03) flex items-center justify-center shrink-0">
                    <Package size={16} className="text-(--base-05)" />
                </div>
            )}
            <div className="min-w-0 flex-1">
                <div className="flex items-center gap-2">
                    <span className="font-medium text-sm text-(--base-09) truncate">{hit.title}</span>
                    <span className="text-[10px] font-mono text-(--base-06) truncate">by {hit.author}</span>
                </div>
                <p className="text-xs text-(--base-07) line-clamp-2 mt-0.5">{hit.description}</p>
                <div className="text-[10px] font-mono text-(--base-06) flex items-center gap-1.5 mt-1">
                    <Download size={10} />
                    {hit.downloads.toLocaleString()}
                </div>
                {job && jobBusy && <InstallBar job={job} />}
                {job && (job.state === 'failed' || job.state === 'unknown') && job.message && (
                    <p className="mt-1 text-[11px] text-(--warning-light) flex items-start gap-1">
                        <AlertTriangle size={11} className="mt-0.5 shrink-0" />
                        <span>{job.message}</span>
                    </p>
                )}
            </div>

            <div className="flex flex-col items-end gap-1.5 shrink-0">
                {status === 'up-to-date' && (
                    <span className="mono-label bg-(--success-ghost) text-(--success-light) px-1.5 rounded-sm">installed</span>
                )}
                {status === 'update-available' && (
                    <span className="mono-label bg-(--warning-ghost) text-(--warning-light) px-1.5 rounded-sm">update available</span>
                )}
                {status === 'checking' && (
                    <span className="mono-label bg-(--base-03) text-(--base-06) px-1.5 rounded-sm">checking…</span>
                )}

                {status === 'not-installed' && (
                    <button onClick={stopAnd(onInstall)} disabled={actionBusy} aria-busy={actionBusy} className="btn btn-primary btn-sm">
                        {actionBusy ? <RefreshCw size={11} className="animate-spin motion-reduce:animate-none" /> : <Download size={11} />}
                        {job && jobBusy ? jobLabel(job) : job?.state === 'failed' || job?.state === 'unknown' ? 'Try again' : 'Install'}
                    </button>
                )}
                {status === 'update-available' && (
                    <button onClick={stopAnd(onUpdate)} disabled={actionBusy} aria-busy={actionBusy} className="btn btn-primary btn-sm">
                        {actionBusy ? <RefreshCw size={11} className="animate-spin motion-reduce:animate-none" /> : <Download size={11} />}
                        {job && jobBusy ? jobLabel(job) : 'Update'}
                    </button>
                )}
                {installed && (
                    <button onClick={stopAnd(onRemove)} disabled={busy} className="btn btn-secondary btn-sm" title="Remove">
                        {busy ? <RefreshCw size={11} className="animate-spin" /> : <Trash2 size={11} className="text-(--error)" />}
                        Remove
                    </button>
                )}
            </div>
        </div>
    );
}

// categoryIconMask turns an SVG string into a CSS mask: an image the browser
// draws, never markup it parses into the page.
function categoryIconMask(svg: string): CSSProperties {
    const url = `url("data:image/svg+xml;utf8,${encodeURIComponent(svg)}")`;
    return {
        maskImage: url,
        WebkitMaskImage: url,
        maskSize: 'contain',
        WebkitMaskSize: 'contain',
        maskRepeat: 'no-repeat',
        WebkitMaskRepeat: 'no-repeat',
        maskPosition: 'center',
        WebkitMaskPosition: 'center',
    };
}
