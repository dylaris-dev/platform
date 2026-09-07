"use client";

import React, { useState, useEffect, useMemo, useCallback } from 'react';
import { Server, setupServer, updateServerRuntime, switchSubServer, getFiles, getLibraryFiles, deleteSubServer, createServerRoute, getServerSettings, getServerRoutes, GatewayRoute, CreateRouteRequest } from '@/lib/api';
import { createBeamAdapter } from '@/lib/adapters';
import { useAppData } from '@/lib/AppDataContext';
import { AlertTriangle, Trash2, RefreshCw } from 'lucide-react';
import { recommendJavaForVersion, effectiveMcVersion } from './setup/JavaVersionPicker';
import { JAVA_21 } from '@/lib/javaVersion';
import { VersionEntry, compareVersionsDesc } from './setup/VersionPicker';
import SubServerSidebar from './setup/SubServerSidebar';
import SetupViewMode from './setup/SetupViewMode';
import SetupNewWizard from './setup/SetupNewWizard';
import SetupEditMode from './setup/SetupEditMode';
import RoutesModal from '@/components/RoutesModal';
import { getSubServerInstalls, type SubServerInstall } from '@/lib/api/subServerInstalls';
import WipeChoiceDialog from '@/views/setup/WipeChoiceDialog';
import { classifyInstallChange, type InstallChange, type WipeToken } from '@/lib/installWipe';
import { API_URL } from '@/lib/api/core';
import { isSubServerName } from '@/lib/validation';
const DEFAULT_GC_FLAGS = '-XX:+UseG1GC -XX:MaxHeapFreeRatio=40 -XX:MinHeapFreeRatio=15 -XX:-ShrinkHeapInSteps';

const PROXY_GC_FLAGS = '-XX:+UseG1GC -XX:+ParallelRefProcEnabled -XX:MaxGCPauseMillis=200 ' +
    '-XX:+UnlockExperimentalVMOptions -XX:+DisableExplicitGC -XX:+AlwaysPreTouch ' +
    '-XX:G1NewSizePercent=30 -XX:G1MaxNewSizePercent=40 -XX:G1HeapRegionSize=8M ' +
    '-XX:G1ReservePercent=20 -XX:G1HeapWastePercent=5 -XX:G1MixedGCCountTarget=4 ' +
    '-XX:InitiatingHeapOccupancyPercent=15 -XX:G1MixedGCLiveThresholdPercent=90 ' +
    '-XX:G1RSetUpdatingPauseTimePercent=5 -XX:SurvivorRatio=32 ' +
    '-XX:+PerfDisableSharedMem -XX:MaxTenuringThreshold=1';

// The sub-server name rule lives in @/lib/validation, which mirrors
// validate.SubServerName. The copy that used to sit here was the same alphabet
// WITHOUT the 1-50 length bound, so the field accepted a name Core rejects. The
// canonical constant existed the whole time and had no callers - a rule nothing
// reads is not a rule.
function sanitizeName(raw: string): string {
    return raw.replace(/ /g, '_').replace(/[^a-zA-Z0-9\-_+]/g, '');
}

type FormMode = 'view' | 'edit' | 'new';

interface SetupViewProps {
    server: Server;
    onSetupComplete: () => void;
    libraryEnabled?: boolean;
}

export default function SetupView({ server, onSetupComplete, libraryEnabled }: SetupViewProps) {
    const { refreshServers } = useAppData();
    const [subServers, setSubServers] = useState<string[]>([]);
    const [maxSubServers, setMaxSubServers] = useState<number>(3);
    const [formMode, setFormMode] = useState<FormMode>('view');
    const [switchTarget, setSwitchTarget] = useState<string | null>(null);
    const [activeServerMissing, setActiveServerMissing] = useState(false);

    // Form fields
    const [subName, setSubName] = useState('');
    const [subNameError, setSubNameError] = useState('');
    const [javaImage, setJavaImage] = useState(JAVA_21);
    const [extraFlags, setExtraFlags] = useState('');
    const [installTab, setInstallTab] = useState<'online' | 'library' | 'upload' | 'backup' | 'modpack' | 'pack'>('online');
    // The backup archive to import. Separate from uploadFile: the two tabs mean
    // different installers, and one field for both would carry a .zip into a
    // backup import as easily as the other way round.
    const [backupFile, setBackupFile] = useState<File | null>(null);
    // Selected Modrinth modpack (project + version + .mrpack URL).
    // Cleared on tab change or on submit.
    const [modpackSelection, setModpackSelection] = useState<import('@/views/setup/ModpackPicker').ModpackSelection | null>(null);
    // Selected unified-builder pack + build (Core pack/build IDs).
    // Cleared on tab change or on submit.
    const [packSelection, setPackSelection] = useState<import('@/views/setup/PackPicker').PackSelection | null>(null);

    // Software list from API
    const [softwareCatalog, setSoftwareCatalog] = useState<{ name: string; type: string }[]>([]);
    const isProxy = server.serverType === 'proxy';
    const filteredSoftware = useMemo(() =>
        softwareCatalog.filter(s => s.type === (isProxy ? 'proxy' : 'game')).map(s => s.name),
        [softwareCatalog, isProxy]
    );

    // Version picker
    const defaultSoftware = isProxy ? 'velocity' : 'paper';
    const [software, setSoftware] = useState(defaultSoftware);
    const [allVersions, setAllVersions] = useState<VersionEntry[]>([]);
    const [selectedMajor, setSelectedMajor] = useState('');
    const [selectedBuild, setSelectedBuild] = useState('');
    const [loadingVersions, setLoadingVersions] = useState(false);

    // Auto-select Java image when MC version changes. We use the build when it
    // is a true MC patch version (Paper "1.20.11" / Vanilla "1.20.6"), and fall
    // back to the major when the build is a loader version (Fabric/Forge).
    useEffect(() => {
        if (formMode === 'view') return;
        // A modpack carries its own Minecraft version, and on the modpack tabs
        // the online version pickers are empty - so keying this on them alone
        // meant picking a modpack recommended nothing and the form silently kept
        // whatever Java was last selected. Which one applies follows the tab the
        // operator is actually installing from.
        const fromPicker =
            installTab === 'modpack' ? (modpackSelection?.mcVersion || '')
            : installTab === 'pack' ? (packSelection?.mcVersion || '')
            : effectiveMcVersion(selectedMajor, selectedBuild);
        if (!fromPicker) return;
        const rec = recommendJavaForVersion(fromPicker);
        if (rec) setJavaImage(rec);
    }, [selectedMajor, selectedBuild, formMode, installTab, modpackSelection?.mcVersion, packSelection?.mcVersion]);

    // Pre-populate version when entering edit mode (handles case where software didn't change)
    useEffect(() => {
        if (formMode !== 'edit' || allVersions.length === 0) return;
        const { mcVersion: mcVer, buildVersion: buildNum } = recordedVersions();
        if (!mcVer) return;
        if (allVersions.some(v => v.major === mcVer)) {
            setSelectedMajor(mcVer);
            const buildsForMajor = allVersions.filter(v => v.major === mcVer).map(v => v.build);
            if (buildNum && buildsForMajor.includes(buildNum)) {
                setSelectedBuild(buildNum);
            } else {
                setSelectedBuild(buildsForMajor[0] || '');
            }
        }
    }, [formMode]);

    // Library
    const [libraryFiles, setLibraryFiles] = useState<any[]>([]);
    const [libraryPath, setLibraryPath] = useState('');
    const [selectedLibraryFile, setSelectedLibraryFile] = useState('');

    // Upload
    const [uploadFile, setUploadFile] = useState<File | null>(null);
    const [uploadStructure, setUploadStructure] = useState<'direct' | 'subfolder'>('direct');
    const [uploadProgress, setUploadProgress] = useState(0);
    const [uploadStatus, setUploadStatus] = useState('');

    // File size check
    const [fileTooLarge, setFileTooLarge] = useState(false);

    // Submit
    const [submitting, setSubmitting] = useState(false);
    const [error, setError] = useState('');

    // Gateway route (optional, for setup wizard)
    const [gatewayRoute, setGatewayRoute] = useState<CreateRouteRequest>({ targetPort: 25565 });
    // Routes already attached to this server. Used by the picker to
    // distinguish "domain is yours" from "domain is taken by someone
    // else" (different colour + can submit unchanged), and to suppress
    // the createServerRoute call on submit when the user picked a
    // domain that's already routed to this same server.
    const [existingRoutes, setExistingRoutes] = useState<GatewayRoute[]>([]);
    // How each sub-server was installed. Absent for anything installed before
    // Core started recording it, which is why every read falls back to the
    // server row rather than treating "no record" as "installed with nothing".
    const [installs, setInstalls] = useState<SubServerInstall[]>([]);
    // Set while the cleanup dialog is open. handleSubmit runs again with the
    // chosen tokens once it is answered, so the whole submit path stays one
    // function rather than two that have to agree.
    const [pendingWipe, setPendingWipe] = useState<InstallChange | null>(null);
    // True when edit mode opened before the install records had arrived. The
    // records are fetched on mount and the Edit button is a click away, so a fast
    // operator got the servers-row fallback and a form that had quietly not
    // loaded their modpack - the exact complaint this feature answers.
    const [editPrefillPending, setEditPrefillPending] = useState(false);
    const [showRoutesModal, setShowRoutesModal] = useState(false);
    const loadRoutes = useCallback(async () => {
        try {
            const res: any = await getServerRoutes(server.id);
            if (Array.isArray(res)) setExistingRoutes(res);
            else if (res && Array.isArray(res.routes)) setExistingRoutes(res.routes);
            else setExistingRoutes([]);
        } catch { /* non-fatal: tooltip just won't know about own routes */ }
    }, [server.id]);
    useEffect(() => { loadRoutes(); }, [loadRoutes]);

    const loadInstalls = useCallback(async () => {
        const res = await getSubServerInstalls(server.id);
        if (res.success && res.installs) setInstalls(res.installs);
    }, [server.id]);
    useEffect(() => { loadInstalls(); }, [loadInstalls]);

    const installFor = useCallback(
        (name: string) => installs.find(i => i.subServerName === name),
        [installs],
    );

    /**
     * The versions to put back on the form, preferring the RECORDED install.
     *
     * The servers row carries the same two values, but only ever for whichever
     * sub-server is active - so with two sub-servers it is the wrong answer for
     * one of them, and editing that one silently offered the other's version.
     * The row stays the fallback for anything installed before Core recorded it.
     */
    const recordedVersions = useCallback(() => {
        const rec = installFor(server.activeSubServer || '');
        return {
            mcVersion: rec?.mcVersion || server.minecraftVersion || '',
            buildVersion: rec?.buildVersion || server.buildNumber || '',
        };
    }, [installFor, server.activeSubServer, server.minecraftVersion, server.buildNumber]);

    // Delete sub-server
    const [showDeleteConfirm, setShowDeleteConfirm] = useState(false);
    const [deleteCountdown, setDeleteCountdown] = useState(5);
    const [deleting, setDeleting] = useState(false);
    // Sub-server names whose backend deletion is still in flight. The
    // sidebar greys them out with a spinner so the user sees the row
    // hasn't been forgotten while the Node tears the container down +
    // removes the dir + Hub catches up. Removed once the polling loop
    // confirms the dir is gone server-side.
    const [pendingDelete, setPendingDelete] = useState<Set<string>>(new Set());

    // ---------- Data Loading ----------

    // Load software catalog + feature flags
    useEffect(() => {
        fetch(`${API_URL}/versions/software`)
            .then(r => r.json())
            .then(data => {
                if (data.success && Array.isArray(data.software)) {
                    setSoftwareCatalog(data.software);
                }
            })
            .catch(() => {});

        getServerSettings().then(res => {
            if (res.success && res.settings) setMaxSubServers(res.settings.maxSubServers);
        }).catch(() => {});
    }, []);

    useEffect(() => { loadSubServers(); }, [server.uuid]);

    const loadSubServers = async () => {
        const res = await getFiles('', server.uuid);
        if (res.success && res.files) {
            const dirs = (res.files as any[]).filter(f => f.is_dir).map(f => f.name);
            setSubServers(dirs);
            if (dirs.length === 0) {
                setActiveServerMissing(false);
                enterNewMode();
            } else {
                const activeExists = dirs.includes(server.activeSubServer || '');
                setActiveServerMissing(!activeExists && !!server.activeSubServer);
                // Don't auto-set switchTarget when the active is
                // missing -- with the new design that opens a switch
                // modal unbidden. The warning banner already prompts
                // the user; they click the Play icon to switch.
                enterViewMode();
            }
        }
    };

    useEffect(() => {
        if (installTab === 'library' && libraryEnabled) loadLibraryFiles('');
    }, [installTab, libraryEnabled]);

    const loadLibraryFiles = async (path: string) => {
        const res = await getLibraryFiles(path);
        if (res.success && res.files) { setLibraryFiles(res.files); setLibraryPath(path); }
    };

    useEffect(() => {
        if (installTab !== 'online') return;
        fetchVersions();
    }, [software, installTab]);

    const fetchVersions = async () => {
        setLoadingVersions(true);
        setAllVersions([]);
        setSelectedMajor('');
        setSelectedBuild('');
        try {
            const res = await fetch(`${API_URL}/versions?software=${software}`);
            // A non-2xx with a valid JSON body would otherwise fall through to
            // "no versions" and look identical to an upstream with nothing to
            // offer. Turn it into the error path so the message below fires.
            if (!res.ok) {
                throw new Error(`request failed (${res.status})`);
            }
            const data = await res.json();
            if (data.versions && data.versions.length > 0) {
                const sorted = [...data.versions].sort(
                    (a, b) => compareVersionsDesc(a.major, b.major) || compareVersionsDesc(a.build, b.build),
                );
                setAllVersions(sorted);
                setSelectedMajor(sorted[0].major);
                setSelectedBuild(sorted[0].build);
                prefillVersionFromServer(sorted);
            }
        } catch (e) {
            // Swallowing this left the user in the setup wizard staring at an
            // empty version dropdown with nothing to say why - the same failure
            // shape as a software that genuinely has no builds. The list is the
            // one thing this step cannot proceed without, so it has to say so.
            const detail = e instanceof Error ? `: ${e.message}` : '';
            setError(`Could not load ${software} versions${detail}. Check the connection and try again.`);
        }
        setLoadingVersions(false);
    };

    // ---------- Mode Transitions ----------

    const enterViewMode = () => {
        setFormMode('view');
        setSubName(server.activeSubServer || '');
        setJavaImage(server.image || JAVA_21);
        setExtraFlags(server.extraJvmFlags || '');
        setSwitchTarget(null);
    };

    /** Puts the form on the tab the sub-server was installed from. */
    const applyInstallTab = useCallback((sType: string) => {
        if (sType === 'library') {
            setInstallTab('library');
        } else if (sType === 'upload' || sType === 'upload-zip') {
            setInstallTab('upload');
        } else if (sType === 'modpack') {
            // Used to fall into the else below, which put a modpack server on the
            // ONLINE tab with "modpack" selected as its server software - a value
            // no software list contains.
            setInstallTab('modpack');
        } else if (sType === 'pack') {
            setInstallTab('pack');
        } else {
            setSoftware(sType);
            setInstallTab('online');
        }
    }, []);

    // Finish a prefill that opened before the records had loaded. Runs once: the
    // flag is cleared here, so an operator who then switches tabs by hand is not
    // moved back by a late response.
    useEffect(() => {
        if (!editPrefillPending || formMode !== 'edit') return;
        const rec = installFor(server.activeSubServer || '');
        if (!rec) return;
        setEditPrefillPending(false);
        applyInstallTab(rec.installerType);
    }, [editPrefillPending, formMode, installFor, server.activeSubServer, applyInstallTab]);

    const enterEditMode = () => {
        setFormMode('edit');
        setSubName(server.activeSubServer || '');
        setJavaImage(server.image || JAVA_21);
        setExtraFlags(server.extraJvmFlags || '');
        // The RECORDED install wins over the servers row: that row only ever
        // described whichever sub-server was active, so with two sub-servers it
        // was the wrong answer for one of them. Falls back to the row for
        // anything installed before Core recorded this.
        const rec = installFor(server.activeSubServer || '');
        setEditPrefillPending(!rec);
        applyInstallTab(rec?.installerType || server.installerType || defaultSoftware);
        setError('');
    };

    const enterNewMode = () => {
        setFormMode('new');
        setSubName('');
        setSubNameError('');
        // Java 21 for both game and proxy. Named, not JAVA_IMAGES[0]: the list
        // now leads with Java 25 and an index would have moved this default
        // silently onto a runtime that cannot run 1.8-1.16.
        setJavaImage(JAVA_21);
        setExtraFlags(isProxy ? PROXY_GC_FLAGS : DEFAULT_GC_FLAGS);
        setInstallTab('online');
        setSoftware(defaultSoftware);
        setSelectedLibraryFile('');
        setUploadFile(null);
        setUploadProgress(0);
        setUploadStatus('');
        setError('');
    };

    const prefillVersionFromServer = (versions: VersionEntry[]) => {
        if (formMode !== 'edit') return;
        const { mcVersion: mcVer, buildVersion: buildNum } = recordedVersions();
        if (mcVer && versions.some(v => v.major === mcVer)) {
            setSelectedMajor(mcVer);
            if (buildNum && versions.some(v => v.major === mcVer && v.build === buildNum)) {
                setSelectedBuild(buildNum);
            } else {
                const first = versions.find(v => v.major === mcVer);
                setSelectedBuild(first?.build || '');
            }
        }
    };

    // ---------- Handlers ----------

    const handleSubNameChange = (raw: string) => {
        setSubName(raw);
        const s = sanitizeName(raw);
        setSubNameError(raw && !isSubServerName(s) ? 'Use letters, numbers, -, _ or +, up to 50 characters.' : '');
    };

    const handleSwitchServer = async () => {
        if (!switchTarget) return;

        setSubmitting(true);
        setError('');
        const res = await switchSubServer(server.id, switchTarget);
        if (res.success) { setSwitchTarget(null); setActiveServerMissing(false); onSetupComplete(); }
        else setError(res.message || 'Switch failed');
        setSubmitting(false);
    };

    const handleSubmit = async (wipePaths?: WipeToken[]) => {
        const sanitized = sanitizeName(subName);
        if (!sanitized) { setSubNameError('Server name is required.'); return; }
        // The field shows this error while you type, and nothing used to act on
        // it: submitting anyway sent a name the rule rejects. Core sanitized it
        // through, so a 51-character name became a sub-server that no switch
        // would ever accept. Core refuses it now, so this is the message that
        // makes the refusal readable instead of a generic server error.
        if (!isSubServerName(sanitized)) {
            setSubNameError('Use letters, numbers, -, _ or +, up to 50 characters.');
            return;
        }

        // What is actually changing decides which of two very different things
        // this save is. Ask before destroying anything, and only when the INSTALL
        // is changing: a dialog on every save is one people learn to click
        // through.
        if (wipePaths === undefined && formMode === 'edit') {
            const change = classifyInstallChange(installFor(sanitized), {
                tab: installTab,
                backupFileSelected: !!backupFile,
                software,
                mcVersion: selectedMajor,
                buildVersion: selectedBuild,
                modrinthVersionId: modpackSelection?.versionId,
                packBuildId: packSelection?.buildId,
            });
            if (change !== 'runtime' && change !== 'none') {
                setPendingWipe(change);
                return;
            }
            // Nothing about the install changed, so the installer must not run.
            // It used to anyway - there was no other path that could rebuild a
            // start command - which made "change a GC flag" a reinstall over a
            // live server directory.
            setSubmitting(true);
            setError('');
            const res = await updateServerRuntime(server.id, { javaImage, extraJvmFlags: extraFlags });
            setSubmitting(false);
            if (!res.success) {
                setError(res.message || 'Could not apply the settings');
                return;
            }
            if (res.warning) setError(res.warning);
            onSetupComplete();
            // NOT enterViewMode(): that re-derives the fields from the `server`
            // prop, which is still the pre-save row - onSetupComplete refreshes it
            // asynchronously. Going through it showed the OLD Java version back to
            // an operator who had just changed it, until they left the tab and
            // came back. What was saved is what is on screen.
            setFormMode('view');
            setSwitchTarget(null);
            return;
        }

        setSubmitting(true);
        setError('');

        const installer: any = {};
        if (installTab === 'online') {
            installer.type = software;
            if (software === 'neoforge') {
                // NeoForge versions are self-contained — the version IS the loader,
                // the matching MC version is implicit.
                installer.loader = selectedBuild;
            } else {
                installer.version = selectedBuild;
                installer.mcVersion = selectedMajor;
            }
        } else if (installTab === 'library') {
            installer.type = 'library';
            installer.path = selectedLibraryFile;
        } else if (installTab === 'modpack' && modpackSelection) {
            installer.type = 'modpack';
            installer.url = modpackSelection.downloadUrl;
            installer.modrinthProjectId = modpackSelection.projectId;
            installer.modrinthVersionId = modpackSelection.versionId;
            installer.modrinthProjectSlug = modpackSelection.projectSlug;
            if (modpackSelection.loader) installer.loader = modpackSelection.loader;
            if (modpackSelection.mcVersion) installer.mcVersion = modpackSelection.mcVersion;
        } else if (installTab === 'pack' && packSelection) {
            installer.type = 'pack';
            installer.packId = packSelection.packId;
            installer.buildId = packSelection.buildId;
            if (packSelection.loader) installer.loader = packSelection.loader;
            if (packSelection.mcVersion) installer.mcVersion = packSelection.mcVersion;
        } else if (installTab === 'backup' && backupFile) {
            installer.type = 'backup';
            setUploadStatus('Uploading...');
            try {
                // The node looks for this exact name in the sub-server directory,
                // the same way an upload-zip install finds ".upload.zip". It is a
                // dotfile but NOT under the ".dylaris" prefix: that namespace is
                // platform-reserved and every write to it is refused, which is
                // what the first name for this file ran into.
                const renamed = new File([backupFile], '.upload-backup.tar.gz', { type: 'application/gzip' });
                const dt = new DataTransfer();
                dt.items.add(renamed);
                const uploadRes = await createBeamAdapter().uploadFiles(sanitized, dt.files, (p) => setUploadProgress(p), undefined, undefined, server.uuid);
                if (!uploadRes.success) {
                    setError(uploadRes.message || 'Upload failed');
                    setSubmitting(false);
                    setUploadStatus('');
                    return;
                }
            } catch {
                setError('Upload failed');
                setSubmitting(false);
                setUploadStatus('');
                return;
            }
            setUploadStatus('Restoring...');
        } else if (installTab === 'upload' && uploadFile) {
            installer.type = 'upload-zip';
            installer.structure = uploadStructure;
            setUploadStatus('Uploading...');
            try {
                const renamedFile = new File([uploadFile], '.upload.zip', { type: uploadFile.type });
                const dt = new DataTransfer();
                dt.items.add(renamedFile);
                // Inside the Beam desktop app this streams the archive straight to
                // the node over the beam tunnel; in a browser createBeamAdapter
                // resolves to the same HTTP-through-Core upload as before.
                const uploadRes = await createBeamAdapter().uploadFiles(sanitized, dt.files, (p) => setUploadProgress(p), undefined, undefined, server.uuid);
                if (!uploadRes.success) {
                    setError(uploadRes.message || 'Upload failed');
                    setSubmitting(false);
                    setUploadStatus('');
                    return;
                }
            } catch {
                setError('Upload failed');
                setSubmitting(false);
                setUploadStatus('');
                return;
            }
            setUploadStatus('Installing...');
        } else {
            installer.type = 'upload';
        }

        // Only what the operator ticked. Absent means "install on top", which is
        // what every install did before the dialog existed and is still right for
        // a jar swap.
        if (wipePaths && wipePaths.length > 0) installer.wipePaths = wipePaths;

        const res = await setupServer(server.id, {
            subServerName: sanitized,
            javaImage,
            extraJvmFlags: extraFlags,
            installer,
        });

        if (res.success) {
            // Create gateway route if any domain field was filled. We surface
            // failures inline so a typo'd domain or a race-loss against
            // another tab doesn't silently drop the route -- the server is
            // already installed at this point, but the user needs to know
            // the routing piece didn't go through so they can retry from
            // the Setup tab.
            const hasDomain = !!(gatewayRoute.subdomain || gatewayRoute.customDomain || gatewayRoute.domain);
            // Effective domain for own-route detection. Mirrors the
            // picker's preview computation so the comparison sees the
            // same string that ends up in createServerRoute.
            const effectiveDomain = (gatewayRoute.subdomain && gatewayRoute.hosterDomain)
                ? `${gatewayRoute.subdomain}.${gatewayRoute.hosterDomain}`.toLowerCase()
                : (gatewayRoute.customDomain || gatewayRoute.domain || '').toLowerCase();
            const alreadyOurs = !!effectiveDomain && existingRoutes.some(r => r.domain.toLowerCase() === effectiveDomain);
            let routeError = '';
            if (hasDomain && alreadyOurs) {
                // Domain is already routed to this server; nothing to do
                // on the API side, just clear the field so the wizard
                // resets cleanly for the next round.
                setGatewayRoute({ targetPort: 25565 });
            }
            // Whatever happens below, the local route list is refetched before
            // the form is shown again. Without it the picker keeps the list it
            // had BEFORE the route existed, decides the domain is somebody
            // else's, and shows "already taken" about the route just created -
            // until the operator opens the routes modal or reloads the page.
            let routeCreated = false;
            if (hasDomain && !alreadyOurs) {
                try {
                    const routeRes = await createServerRoute(server.id, gatewayRoute);
                    // fetchAPI now wraps text/plain errors as
                    // {success: false, error, message}. The success response
                    // from CreateServerRoute is {message, domain} with no
                    // explicit success flag, so we treat the absence of
                    // success:false / error as a pass.
                    if (routeRes && (routeRes.success === false || routeRes.error)) {
                        routeError = routeRes.error || routeRes.message || 'Could not create domain route';
                    } else {
                        setGatewayRoute({ targetPort: 25565 });
                        routeCreated = true;
                    }
                } catch (e: any) {
                    routeError = e?.message || 'Could not create domain route';
                }
            }
            await loadSubServers();
            if (routeCreated) await loadRoutes();
            await loadInstalls();
            if (routeError) {
                setError(`Server installed, but domain route failed: ${routeError}`);
            } else {
                onSetupComplete();
            }
        } else setError(res.message || 'Setup failed');
        setSubmitting(false);
        setUploadStatus('');
        setUploadProgress(0);
    };

    // Delete sub-server countdown
    useEffect(() => {
        if (!showDeleteConfirm) { setDeleteCountdown(5); return; }
        if (deleteCountdown <= 0) return;
        const timer = setTimeout(() => setDeleteCountdown(c => c - 1), 1000);
        return () => clearTimeout(timer);
    }, [showDeleteConfirm, deleteCountdown]);

    const handleDeleteSubServer = async () => {
        const target = subName;
        setDeleting(true);
        const res = await deleteSubServer(server.id, target);
        setDeleting(false);
        setShowDeleteConfirm(false);
        if (!res.success) {
            setError(res.message || 'Delete failed');
            return;
        }

        // Optimistic UI: mark the row as pending-delete so the sidebar
        // greys it out + shows a spinner; the row stays visible so the
        // user knows we're working on it. We do NOT call loadSubServers
        // yet -- that would re-fetch the still-present dir from the
        // Node and "un-delete" the row in the UI.
        setPendingDelete(prev => new Set(prev).add(target));

        // Background poll: hit the file listing every 500ms until the
        // target dir is gone. Capped at 30s so a wedged delete (FS
        // refuses to release, Node crashed, etc.) doesn't leave the
        // sidebar in pending-state forever -- we fall back to a normal
        // reload after the timeout. Important: this loop never calls
        // enterNewMode / enterViewMode while formMode === 'new'; the
        // user might be filling the new-server form right now and we
        // mustn't blow away their progress because an unrelated
        // sidebar row finished deleting.
        const startedAt = Date.now();
        const maxWait = 30_000;
        const wasInNewMode = formMode === 'new';
        let confirmed = false;
        while (Date.now() - startedAt < maxWait) {
            await new Promise(r => setTimeout(r, 500));
            try {
                const lst = await getFiles('', server.uuid);
                if (lst.success && Array.isArray(lst.files)) {
                    const dirs = (lst.files as any[]).filter(f => f.is_dir).map(f => f.name);
                    if (!dirs.includes(target)) {
                        setSubServers(dirs);
                        confirmed = true;
                        if (dirs.length === 0) {
                            setActiveServerMissing(false);
                            // Only auto-enter new mode if the user
                            // isn't already in it. They could be
                            // mid-form for a different sub-server
                            // creation and getting wiped to a fresh
                            // new-mode would lose their typing.
                            if (!wasInNewMode) enterNewMode();
                        } else {
                            const activeExists = dirs.includes(server.activeSubServer || '');
                            setActiveServerMissing(!activeExists && !!server.activeSubServer);
                        }
                        break;
                    }
                }
            } catch { /* swallow & keep polling */ }
        }

        setPendingDelete(prev => {
            const next = new Set(prev);
            next.delete(target);
            return next;
        });
        // Fallback if polling timed out without the dir disappearing.
        // The Node may have been slow but eventually catches up; reload
        // so the next view reflects whatever state actually exists.
        if (!confirmed) {
            await loadSubServers();
        }
        await refreshServers();
        // Only signal "setup complete" upstream when we're not in the
        // middle of a new-sub-server flow -- otherwise the parent
        // refresh could yank the user out.
        if (!wasInNewMode) onSetupComplete();
    };

    // ---------- Shared props for install sections ----------

    const installProps = {
        installTab,
        onInstallTabChange: setInstallTab,
        libraryEnabled,
        software,
        onSoftwareChange: setSoftware,
        softwareList: filteredSoftware.length > 0 ? filteredSoftware : undefined,
        serverType: (server.serverType || 'game') as 'game' | 'proxy',
        allVersions,
        selectedMajor,
        onMajorChange: setSelectedMajor,
        selectedBuild,
        onBuildChange: setSelectedBuild,
        loadingVersions,
        libraryFiles,
        libraryPath,
        selectedLibraryFile,
        onLibraryNavigate: loadLibraryFiles,
        onLibrarySelect: setSelectedLibraryFile,
        uploadFile,
        onUploadFileChange: (f: File | null) => { setUploadFile(f); if (!f) { setUploadProgress(0); setUploadStatus(''); } },
        uploadStructure,
        onUploadStructureChange: setUploadStructure,
        uploadProgress,
        uploadStatus,
        onUploadStatusChange: setUploadStatus,
        backupFile,
        onBackupFileChange: (f: File | null) => { setBackupFile(f); if (!f) { setUploadProgress(0); setUploadStatus(''); } },
        modpackSelection,
        onModpackSelect: setModpackSelection,
        packSelection,
        onPackSelect: setPackSelection,
        serverId: server.id,
        onFileTooLarge: setFileTooLarge,
    };

    // ---------- Render ----------

    return (
        <div className="flex gap-6 h-full min-h-0">
            {/* Sidebar -- always rendered, including during
                pending_setup of a fresh container so the user can
                Add Server right away. Empty-state hint inside the
                sidebar orients new servers; once any sub-server
                exists the list takes over. */}
            <SubServerSidebar
                subServers={subServers}
                activeSubServer={server.activeSubServer}
                pendingDelete={pendingDelete}
                onSwitch={(name) => {
                    if (formMode !== 'view') return;
                    setSwitchTarget(name);
                }}
                onAddNew={enterNewMode}
                onEditSubServer={(name) => {
                    // Switch to the right slot first, then jump
                    // straight into edit. enterEditMode reads
                    // subName from server.activeSubServer, so we
                    // need the panel state to match the picked
                    // entry before we transition.
                    setSubName(name);
                    enterEditMode();
                }}
                onDeleteSubServer={(name) => {
                    setSubName(name);
                    setShowDeleteConfirm(true);
                }}
                submitting={submitting}
                disabled={formMode !== 'view'}
                maxSubServers={maxSubServers}
            />

            {/* Right panel - mode dependent */}
            {formMode === 'view' && (
                <SetupViewMode
                    server={server}
                    activeServerMissing={activeServerMissing}
                    onEdit={enterEditMode}
                    onAddNew={enterNewMode}
                    hasSubServers={subServers.length > 0}
                />
            )}

            {formMode === 'new' && (
                <SetupNewWizard
                    subName={subName}
                    onSubNameChange={handleSubNameChange}
                    subNameError={subNameError}
                    javaImage={javaImage}
                    onJavaChange={setJavaImage}
                    extraFlags={extraFlags}
                    onFlagsChange={setExtraFlags}
                    ramMB={server.memory}
                    {...installProps}
                    onSubmit={handleSubmit}
                    onClose={enterViewMode}
                    submitting={submitting}
                    fileTooLarge={fileTooLarge}
                    error={error}
                    hasSubServers={subServers.length > 0}
                    isFirstSetup={subServers.length === 0}
                    gatewayRoute={gatewayRoute}
                    onGatewayRouteChange={setGatewayRoute}
                    existingRoutes={existingRoutes}
                    onOpenRoutesModal={() => setShowRoutesModal(true)}
                />
            )}

            {formMode === 'edit' && (
                <SetupEditMode
                    currentInstall={installFor(server.activeSubServer || '')}
                    subName={subName}
                    javaImage={javaImage}
                    onJavaChange={setJavaImage}
                    extraFlags={extraFlags}
                    onFlagsChange={setExtraFlags}
                    ramMB={server.memory}
                    {...installProps}
                    activeServerMissing={activeServerMissing}
                    activeSubServer={server.activeSubServer}
                    onSubmit={handleSubmit}
                    onClose={enterViewMode}
                    onDelete={() => setShowDeleteConfirm(true)}
                    submitting={submitting}
                    fileTooLarge={fileTooLarge}
                    error={error}
                />
            )}

            {/* Switch confirmation modal. Triggered by the Play icon
                on a non-active sub-server. Switching stops the
                currently-running container so this stays an explicit
                opt-in (not silent) -- destructive transition for any
                player connected to the live server. */}
            {switchTarget && switchTarget !== server.activeSubServer && (
                <div className="modal-overlay animate-fade-in" onClick={() => setSwitchTarget(null)}>
                    <div className="modal-panel max-w-md" onClick={e => e.stopPropagation()}>
                        <div className="modal-header">
                            <h3 className="modal-title flex items-center gap-2 text-(--warning-light)">
                                <RefreshCw size={20} /> Switch Sub-Server
                            </h3>
                        </div>
                        <div className="modal-body space-y-3">
                            <p className="text-sm text-(--base-08)">
                                Switch active sub-server to{' '}
                                <span className="font-mono font-semibold text-(--warning-light)">{switchTarget}</span>?
                            </p>
                            <p className="text-sm text-(--base-06)">
                                The currently-running container will be stopped and the new sub-server will be
                                started in its place. Any connected players will be disconnected.
                            </p>
                        </div>
                        <div className="modal-footer">
                            <button onClick={() => setSwitchTarget(null)} className="btn btn-secondary flex-1">
                                Cancel
                            </button>
                            <button
                                onClick={handleSwitchServer}
                                disabled={submitting}
                                className="btn btn-primary flex-1 bg-(--warning) border-(--warning) hover:bg-(--warning-light)"
                            >
                                {submitting
                                    ? <><RefreshCw size={14} className="animate-spin" /> Switching...</>
                                    : <><RefreshCw size={14} /> Switch</>
                                }
                            </button>
                        </div>
                    </div>
                </div>
            )}

            {/* Delete Confirmation Modal */}
            {showDeleteConfirm && (
                <div className="modal-overlay animate-fade-in" onClick={() => setShowDeleteConfirm(false)}>
                    <div className="modal-panel max-w-md" onClick={e => e.stopPropagation()}>
                        <div className="modal-header">
                            <h3 className="modal-title flex items-center gap-2 text-(--error-light)">
                                <AlertTriangle size={20} /> Delete Sub-Server
                            </h3>
                        </div>
                        <div className="modal-body space-y-4">
                            <p className="text-sm text-(--base-08)">
                                Are you sure you want to delete <span className="font-mono font-semibold text-(--error-light)">{subName}</span>?
                            </p>
                            <p className="text-sm text-(--base-06)">
                                All data will be permanently deleted. This action cannot be undone.
                            </p>
                            <div className="text-xs text-(--base-07) bg-(--base-02) border border-(--base-03) rounded-md p-2.5 leading-relaxed">
                                <span className="font-semibold text-(--accent-light)">Note:</span> Domain routes
                                belong to the server, not individual sub-servers, so they stay attached and keep
                                pointing at whichever sub-server you make active next. Manage them under{' '}
                                <span className="font-mono">Network → Routes</span>.
                            </div>
                        </div>
                        <div className="modal-footer">
                            <button onClick={() => setShowDeleteConfirm(false)} className="btn btn-secondary flex-1">
                                Cancel
                            </button>
                            <button
                                onClick={handleDeleteSubServer}
                                disabled={deleteCountdown > 0 || deleting}
                                className="btn btn-danger flex-1"
                            >
                                {deleting
                                    ? <><RefreshCw size={14} className="animate-spin" /> Deleting...</>
                                    : deleteCountdown > 0
                                    ? `Delete (${deleteCountdown}s)`
                                    : <><Trash2 size={14} /> Delete permanently</>
                                }
                            </button>
                        </div>
                    </div>
                </div>
            )}

            {/* Inline routes management. Same component the server-
                header globe icon opens; we mount it here too so the
                setup-tab globe shortcut works without round-tripping
                through the layout. Reload local existingRoutes when
                the user mutates anything so the picker's "own route"
                check stays in sync. */}
            {pendingWipe && (
                <WipeChoiceDialog
                    change={pendingWipe}
                    onCancel={() => setPendingWipe(null)}
                    onConfirm={(tokens) => { setPendingWipe(null); void handleSubmit(tokens); }}
                />
            )}

            {showRoutesModal && (
                <RoutesModal
                    serverId={server.id}
                    serverName={server.name}
                    onClose={() => { setShowRoutesModal(false); loadRoutes(); }}
                    onRoutesChanged={(rs) => setExistingRoutes(rs)}
                />
            )}
        </div>
    );
}
