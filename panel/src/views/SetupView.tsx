"use client";

import React, { useState, useEffect, useMemo, useCallback, useRef } from 'react';
import { Server, setupServer, updateServerRuntime, switchSubServer, getFiles, getLibraryFiles, deleteSubServer, createServerRoute, getServerSettings, getServerRoutes, GatewayRoute, CreateRouteRequest } from '@/lib/api';
import { createBeamAdapter } from '@/lib/adapters';
import { useAppData } from '@/lib/AppDataContext';
import { AlertTriangle, Trash2, RefreshCw } from 'lucide-react';
import { recommendJavaForVersion } from './setup/JavaVersionPicker';
import { JAVA_21 } from '@/lib/javaVersion';
import VersionPicker, { VersionEntry, compareVersionsDesc } from './setup/VersionPicker';
import UploadSoftwareChoice from './setup/UploadSoftwareChoice';
import { readZipEntryNames, detectUploadSoftware, singleTopFolder, readPackMcVersion, type UploadDetection } from '@/lib/uploadDetect';
import SubServerSidebar from './setup/SubServerSidebar';
import SetupNewWizard from './setup/SetupNewWizard';
import SetupEditMode from './setup/SetupEditMode';
import RoutesModal from '@/components/RoutesModal';
import { getSubServerInstalls, type SubServerInstall } from '@/lib/api/subServerInstalls';
import WipeChoiceDialog from '@/views/setup/WipeChoiceDialog';
import { classifyInstallChange, type InstallChange, type WipeToken } from '@/lib/installWipe';
import { API_URL } from '@/lib/api/core';
import { isSubServerName } from '@/lib/validation';
import { ROUTES_CHANGED_EVENT } from '@/lib/systemEvents';
import { technicInstaller, type TechnicSelection } from '@/views/setup/technic';
import ModalPanel from '@/components/ui/ModalPanel';
import { installTargetMcVersion } from '@/views/setup/installTarget';
import { createUploadStager, uploadStartStep, UPLOAD_ZIP_NAME } from '@/views/setup/uploadStaging';
import { setupPreviewModel } from '@/views/setup/setupPreview';

// What may be installed over an upload: Core's reinstallableInstallers.
const UPLOAD_SOFTWARE = ['paper', 'vanilla', 'fabric', 'forge', 'neoforge'];

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
    /** After a fresh install went through: the console is where it is followed. */
    onInstalled?: () => void;
    libraryEnabled?: boolean;
}

export default function SetupView({ server, onSetupComplete, onInstalled, libraryEnabled }: SetupViewProps) {
    const { refreshServers } = useAppData();
    const [subServers, setSubServers] = useState<string[]>([]);
    const [maxSubServers, setMaxSubServers] = useState<number>(3);
    const [formMode, setFormMode] = useState<FormMode>('view');
    const [switchTarget, setSwitchTarget] = useState<string | null>(null);
    // A sub-server picked in the sidebar to LOOK at. Only the explicit switch
    // makes it active: that restarts the server and drops its players.
    const [previewSub, setPreviewSub] = useState<string | null>(null);
    const [activeServerMissing, setActiveServerMissing] = useState(false);

    // Form fields
    const [subName, setSubName] = useState('');
    const [subNameError, setSubNameError] = useState('');
    const [javaImage, setJavaImage] = useState(JAVA_21);
    const [extraFlags, setExtraFlags] = useState('');
    const [installTab, setInstallTab] = useState<'online' | 'library' | 'upload' | 'backup' | 'modpack' | 'pack' | 'technic'>('online');
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
    // Selected Technic pack; Core resolves its download at install time.
    const [technicSelection, setTechnicSelection] = useState<TechnicSelection | null>(null);

    // Software list from API
    const [softwareCatalog, setSoftwareCatalog] = useState<{ name: string; type: string }[]>([]);
    const isProxy = server.serverType === 'proxy';

    // Upload
    const [uploadFile, setUploadFile] = useState<File | null>(null);
    const [uploadStructure, setUploadStructure] = useState<'direct' | 'subfolder'>('direct');
    const [uploadProgress, setUploadProgress] = useState(0);
    const [uploadStatus, setUploadStatus] = useState('');
    // What the archive holds (null while reading it) and whether to start its
    // own jar instead of installing the picked software over it.
    const [uploadDetection, setUploadDetection] = useState<UploadDetection | null | undefined>(undefined);
    const [uploadKeepJar, setUploadKeepJar] = useState(false);
    const uploadPrefillBuild = useRef<string | undefined>(undefined);
    // Set by a detected version that the list offers, or by the operator
    // touching the picker. A preselected newest version is not a choice: put
    // over a world made with an older one, its first start upgrades the world.
    const [uploadVersionChosen, setUploadVersionChosen] = useState(false);
    // The picked archive is uploaded as soon as it has a sub-server to go to,
    // straight to <sub>/.upload.zip where every node release looks for it
    // (uploadStaging.ts). uploadTarget is that sub-server, and locks the name;
    // uploadStaged is true once the archive is all there.
    const [uploadTarget, setUploadTarget] = useState<string | null>(null);
    const [uploadStaged, setUploadStaged] = useState(false);
    const stager = useMemo(() => createUploadStager({
        upload: (sub, file, onProgress) => {
            const dt = new DataTransfer();
            dt.items.add(new File([file], UPLOAD_ZIP_NAME, { type: file.type }));
            // Inside the Beam desktop app this streams straight to the node over
            // the beam tunnel; in a browser it is the HTTP upload through Core.
            return createBeamAdapter().uploadFiles(sub, dt.files, onProgress, undefined, undefined, server.uuid);
        },
        removeZip: sub => createBeamAdapter().deleteFile(`${sub}/${UPLOAD_ZIP_NAME}`, server.uuid),
        list: async sub => {
            const res: any = await createBeamAdapter().getFiles(sub, server.uuid);
            return res?.success && Array.isArray(res.files) ? res.files.map((f: any) => f.name) : null;
        },
        removeDir: sub => createBeamAdapter().deleteFile(sub, server.uuid),
    }), [server.uuid]);
    // Leaving the Setup tab drops a staged archive nobody installed.
    useEffect(() => () => stager.discard(), [stager]);
    // An upload installs server software over the files unless it brings a
    // jar the node can start. Not offered for proxies: there is no proxy
    // installer to put over an upload. In edit mode only with a new archive;
    // without one the save does not install anything.
    const uploadNeedsSoftware = installTab === 'upload' && !isProxy && (formMode === 'new' || !!uploadFile);
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
    // The Minecraft version the install will run, which picks the Java. A
    // modpack carries its own, and on the modpack tabs the online version
    // pickers are empty; an upload kept as it is says its own in its files.
    // Keyed on the pickers alone, the form recommended the Java of the server
    // being replaced ("Minecraft 26.3 needs Java 25" over a 1.20.1 pack).
    // The upload tab shares the pickers with the Online tab, which preselects
    // its newest version: "Paper 26.3" online, then an upload, recommended Java
    // 25 for the upload. Each tab answers for itself in installTargetMcVersion.
    const { version: targetMcVersion, unknown: javaVersionUnknown } = installTargetMcVersion({
        tab: installTab,
        isProxy,
        selectedMajor,
        selectedBuild,
        selectionMcVersion: installTab === 'modpack' ? modpackSelection?.mcVersion
            : installTab === 'pack' ? packSelection?.mcVersion
            : installTab === 'technic' ? technicSelection?.mcVersion
            : undefined,
        uploadFilePicked: !!uploadFile,
        uploadDetection,
        uploadInstallsSoftware: uploadNeedsSoftware && !uploadKeepJar,
        uploadVersionChosen,
    });

    useEffect(() => {
        if (formMode === 'view' || !targetMcVersion) return;
        const rec = recommendJavaForVersion(targetMcVersion);
        if (rec) setJavaImage(rec);
    }, [formMode, targetMcVersion]);

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

    const preview = useMemo(
        () => setupPreviewModel(server, installs, subServers, previewSub),
        [server, installs, subServers, previewSub],
    );

    // The read-only form follows the row when the row changes. Keyed on the row
    // alone, not on formMode: a runtime save returns to view while the row is
    // still the pre-save one, and re-reading it then showed the old values.
    useEffect(() => {
        if (formMode !== 'view') return;
        setJavaImage(server.image || JAVA_21);
        setExtraFlags(server.extraJvmFlags || '');
    }, [server.image, server.extraJvmFlags]);

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
        if (installTab !== 'online' && !uploadNeedsSoftware) return;
        fetchVersions();
    }, [software, installTab, uploadNeedsSoftware]);

    useEffect(() => {
        setUploadKeepJar(false);
        setUploadVersionChosen(false);
        uploadPrefillBuild.current = undefined;
        if (!uploadFile) { setUploadDetection(undefined); return; }
        setUploadDetection(null);
        let live = true;
        readZipEntryNames(uploadFile)
            .then(names => {
                // A picked zip holding one folder is extracted into that folder;
                // installed over, the server would start beside the files with a
                // fresh world. The node moves it up for "subfolder", and this
                // effect runs again for it.
                if (names && uploadStructure === 'direct' && singleTopFolder(names)) {
                    if (live) setUploadStructure('subfolder');
                    return undefined;
                }
                return names ? detectUploadSoftware(names, uploadStructure === 'subfolder') : { launchable: false };
            })
            .then(async (d: UploadDetection | undefined) => {
                if (!d) return d;
                // NeoForge's build is its own version, not Minecraft's.
                const mc = d.serverPack ? await readPackMcVersion(uploadFile)
                    : d.software && d.software !== 'neoforge' ? d.build : undefined;
                return { ...d, mcVersion: mc };
            })
            .catch(() => ({ launchable: false }))
            .then((d: UploadDetection | undefined) => {
                if (!live || !d) return;
                setUploadDetection(d);
                // Installing picked software over a server pack only works with
                // exactly the loader version it was made for; the pack names it.
                if (d.serverPack) setUploadKeepJar(true);
                if (!d.software || !UPLOAD_SOFTWARE.includes(d.software)) return;
                uploadPrefillBuild.current = d.build;
                setSoftware(d.software);
            });
        return () => { live = false; };
    }, [uploadFile, uploadStructure]);

    // Applied whenever the version list lands or the detection does, in
    // whichever order the two arrive.
    useEffect(() => {
        if (installTab !== 'upload') return;
        const hit = allVersions.find(v => v.build === uploadPrefillBuild.current);
        if (hit) { setSelectedMajor(hit.major); setSelectedBuild(hit.build); setUploadVersionChosen(true); }
    }, [allVersions, uploadDetection, installTab]);

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
        clearUpload();
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
        } else if (sType === 'technic') {
            setInstallTab('technic');
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
        setPreviewSub(null);
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
        clearUpload();
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

    // Also what the name field's "Change" does: the archive went to the old
    // name, so it goes, and the file is picked again under the new one.
    const clearUpload = () => {
        stager.discard();
        setUploadFile(null);
        setUploadTarget(null);
        setUploadStaged(false);
        setUploadProgress(0);
        setUploadStatus('');
    };

    const startUpload = (f: File, sub: string) => {
        setUploadTarget(sub);
        setUploadStaged(false);
        setUploadProgress(0);
        setUploadStatus('Uploading...');
        setError(e => e.startsWith('Upload failed') ? '' : e);
        // A new sub-server's directory is made by this upload, so a discard may
        // remove it again; an existing one's never.
        const ownsDir = formMode === 'new' && !subServers.includes(sub);
        // A newer pick or a clear supersedes this one; the stager answers that.
        void stager.stage(sub, ownsDir, f, setUploadProgress).then(r => {
            if (r.status === 'superseded') return;
            if (r.status === 'staged') {
                setUploadStaged(true);
                setUploadProgress(100);
                setUploadStatus('Uploaded');
                return;
            }
            clearUpload();
            setError(`Upload failed: ${r.message}. Pick the file again to retry.`);
        });
    };

    const handleUploadFileChange = (f: File | null) => {
        if (!f) { clearUpload(); return; }
        setUploadFile(f);
        // Already going to a sub-server: the new file replaces it there.
        if (uploadTarget) startUpload(f, uploadTarget);
    };

    // A new sub-server is usually named after the file is picked. Waits for the
    // typing to settle: the first letter is already a valid name, and the
    // upload locks the field.
    const uploadName = sanitizeName(subName);
    const uploadStep = uploadStartStep({
        onUploadTab: installTab === 'upload' && formMode !== 'view',
        filePicked: !!uploadFile,
        started: !!uploadTarget,
        nameValid: isSubServerName(uploadName),
    });
    useEffect(() => {
        if (uploadStep === 'need-name') { setUploadStatus('Enter a name to start the upload'); return; }
        if (uploadStep !== 'start' || !uploadFile) return;
        const t = setTimeout(() => startUpload(uploadFile, uploadName), formMode === 'new' ? 800 : 0);
        return () => clearTimeout(t);
    }, [uploadStep, uploadFile, uploadName, formMode]);

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
                uploadFileSelected: !!uploadFile,
                software,
                mcVersion: selectedMajor,
                buildVersion: selectedBuild,
                modrinthVersionId: modpackSelection?.versionId,
                packBuildId: packSelection?.buildId,
                technicPicked: !!technicSelection,
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
        } else if (installTab === 'technic' && technicSelection) {
            Object.assign(installer, technicInstaller(technicSelection));
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
            // Already uploaded to <sub>/.upload.zip when it was picked; the
            // request is the one the node has always taken.
            if (!uploadStaged || uploadTarget !== sanitized) { setSubmitting(false); return; }
            installer.type = 'upload-zip';
            installer.structure = uploadStructure;
            setUploadStatus('Installing...');
        } else {
            installer.type = 'upload';
        }
        if (uploadNeedsSoftware && !uploadKeepJar) {
            installer.software = software;
            if (software === 'neoforge') installer.loader = selectedBuild;
            else { installer.version = selectedBuild; installer.mcVersion = selectedMajor; }
        }

        // Only what the operator ticked. Absent means "install on top", which is
        // what every install did before the dialog existed and is still right for
        // a jar swap.
        if (wipePaths && wipePaths.length > 0) installer.wipePaths = wipePaths;

        // From here Core may queue the install, so leaving the tab mid-request
        // must not delete the archive (or a new sub-server's directory) under
        // it. Refused, it is the panel's again to retry or discard.
        // ponytail: installed from another tab, a staged archive stays in the
        // sub-server as a dotfile; the next upload install replaces it.
        const holdsUpload = uploadTarget === sanitized;
        if (holdsUpload) stager.beginInstall();
        let res: Awaited<ReturnType<typeof setupServer>>;
        try {
            res = await setupServer(server.id, {
                subServerName: sanitized,
                javaImage,
                extraJvmFlags: extraFlags,
                installer,
            });
        } catch (e) {
            if (holdsUpload) stager.endInstall(false);
            throw e;
        }
        if (holdsUpload) stager.endInstall(!!res.success);

        if (res.success) {
            if (holdsUpload) { setUploadTarget(null); setUploadStaged(false); }
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
            if (routeCreated) {
                await loadRoutes();
                window.dispatchEvent(new CustomEvent(ROUTES_CHANGED_EVENT, { detail: { serverId: server.id } }));
            }
            await loadInstalls();
            if (routeError) {
                setError(`Server installed, but domain route failed: ${routeError}`);
            } else {
                onSetupComplete();
                // An upload with no file only prepares the slot for SFTP: the
                // next step is the upload, not a console with nothing in it.
                if (installer.type !== 'upload') onInstalled?.();
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

    const uploadSoftwareList = filteredSoftware.filter(s => UPLOAD_SOFTWARE.includes(s));
    const uploadKeepAllowed = !uploadFile || !!uploadDetection?.launchable || !!uploadDetection?.serverPack;
    const uploadIncomplete = uploadNeedsSoftware && (
        uploadKeepJar
            ? !uploadKeepAllowed
            : uploadDetection === null || loadingVersions || !UPLOAD_SOFTWARE.includes(software) || !selectedBuild || !uploadVersionChosen
    );
    const uploadPending = installTab === 'upload' && !!uploadFile && !uploadStaged;
    const uploadSoftware = uploadNeedsSoftware ? (
        <UploadSoftwareChoice
            detection={uploadDetection}
            keepJar={uploadKeepJar}
            onKeepJarChange={setUploadKeepJar}
            keepAllowed={uploadKeepAllowed}
            versionChosen={uploadVersionChosen}
        >
            <VersionPicker
                software={software}
                onSoftwareChange={s => { setSoftware(s); setUploadVersionChosen(false); }}
                softwareList={uploadSoftwareList.length > 0 ? uploadSoftwareList : UPLOAD_SOFTWARE}
                allVersions={allVersions}
                selectedMajor={selectedMajor}
                onMajorChange={m => { setSelectedMajor(m); setUploadVersionChosen(true); }}
                selectedBuild={selectedBuild}
                onBuildChange={b => { setSelectedBuild(b); setUploadVersionChosen(true); }}
                loading={loadingVersions}
            />
        </UploadSoftwareChoice>
    ) : null;

    const installProps = {
        targetMcVersion,
        javaVersionUnknown,
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
        onUploadFileChange: handleUploadFileChange,
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
        technicSelection,
        onTechnicSelect: setTechnicSelection,
        serverId: server.id,
        onFileTooLarge: setFileTooLarge,
        uploadSoftware,
        submitBlocked: uploadIncomplete || uploadPending,
        subNameLocked: !!uploadTarget,
        onSubNameUnlock: clearUpload,
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
                previewSubServer={preview.isActive ? undefined : preview.subServer}
                onPreview={(name) => {
                    if (formMode !== 'view') return;
                    setPreviewSub(name === server.activeSubServer ? null : name);
                }}
                onSwitch={(name) => {
                    if (formMode !== 'view') return;
                    setSwitchTarget(name);
                }}
                onAddNew={enterNewMode}
                onEditSubServer={(name) => {
                    // Edit applies to the ACTIVE sub-server only: enterEditMode
                    // reads it from the server row, so editing another used to
                    // open the active one's form under the other's row. Another
                    // one is previewed; its switch is on the form.
                    if (name !== server.activeSubServer) { setPreviewSub(name); return; }
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
                <SetupEditMode
                    currentInstall={installFor(preview.subServer)}
                    subName={preview.subServer}
                    javaImage={javaImage}
                    onJavaChange={setJavaImage}
                    extraFlags={extraFlags}
                    onFlagsChange={setExtraFlags}
                    ramMB={server.memory}
                    {...installProps}
                    // The Java recommendation follows what is installed, not the
                    // pickers, which hold nothing meaningful while viewing.
                    targetMcVersion={preview.mcVersion}
                    javaVersionUnknown={false}
                    activeServerMissing={activeServerMissing}
                    activeSubServer={server.activeSubServer}
                    onSubmit={() => {}}
                    onClose={() => {}}
                    onDelete={() => {}}
                    submitting={submitting}
                    error={error}
                    view={{
                        model: preview,
                        canEdit: subServers.length > 0,
                        onEdit: enterEditMode,
                        onSwitch: () => setSwitchTarget(preview.subServer),
                    }}
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
                    <ModalPanel onClose={() => setSwitchTarget(null)} className="modal-panel max-w-md" onClick={e => e.stopPropagation()}>
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
                    </ModalPanel>
                </div>
            )}

            {/* Delete Confirmation Modal */}
            {showDeleteConfirm && (
                <div className="modal-overlay animate-fade-in" onClick={() => setShowDeleteConfirm(false)}>
                    <ModalPanel onClose={() => setShowDeleteConfirm(false)} className="modal-panel max-w-md" onClick={e => e.stopPropagation()}>
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
                    </ModalPanel>
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
                    onRoutesChanged={(rs) => {
                        setExistingRoutes(rs);
                        window.dispatchEvent(new CustomEvent(ROUTES_CHANGED_EVENT, { detail: { serverId: server.id } }));
                    }}
                />
            )}
        </div>
    );
}
