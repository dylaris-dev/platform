"use client";

import React, { useMemo, useState } from 'react';
import { X, Rocket, RefreshCw, Globe } from 'lucide-react';
import { CreateRouteRequest, GatewayRoute } from '@/lib/api';
import { useAppData } from '@/lib/AppDataContext';
import JavaVersionPicker, { recommendJavaForVersion, effectiveMcVersion } from './JavaVersionPicker';
import JvmFlagsSection from './JvmFlagsSection';
import VersionPicker, { VersionEntry } from './VersionPicker';
import LibraryPicker from './LibraryPicker';
import UploadSection from './UploadSection';
import BackupImportSection from './BackupImportSection';
import ModpackPicker from './ModpackPicker';
import PackPicker from './PackPicker';
import RouteDomainPicker, { DomainAvailability } from '@/components/RouteDomainPicker';

function sanitizeName(raw: string): string {
    return raw.replace(/ /g, '_').replace(/[^a-zA-Z0-9\-_+]/g, '');
}

interface SetupNewWizardProps {
    // Name
    subName: string;
    onSubNameChange: (name: string) => void;
    subNameError: string;
    // Java
    javaImage: string;
    onJavaChange: (id: string) => void;
    // Flags
    extraFlags: string;
    onFlagsChange: (flags: string) => void;
    ramMB: number;
    // Install tab
    installTab: 'online' | 'library' | 'upload' | 'backup' | 'modpack' | 'pack';
    onInstallTabChange: (tab: 'online' | 'library' | 'upload' | 'backup' | 'modpack' | 'pack') => void;
    modpackSelection?: import('@/views/setup/ModpackPicker').ModpackSelection | null;
    onModpackSelect?: (s: import('@/views/setup/ModpackPicker').ModpackSelection | null) => void;
    packSelection?: import('@/views/setup/PackPicker').PackSelection | null;
    onPackSelect?: (s: import('@/views/setup/PackPicker').PackSelection | null) => void;
    libraryEnabled?: boolean;
    // Server type
    serverType?: 'game' | 'proxy';
    // Version picker
    software: string;
    onSoftwareChange: (s: string) => void;
    softwareList?: string[];
    allVersions: VersionEntry[];
    selectedMajor: string;
    onMajorChange: (m: string) => void;
    selectedBuild: string;
    onBuildChange: (b: string) => void;
    loadingVersions: boolean;
    // Library
    libraryFiles: any[];
    libraryPath: string;
    selectedLibraryFile: string;
    onLibraryNavigate: (path: string) => void;
    onLibrarySelect: (file: string) => void;
    // Upload
    uploadFile: File | null;
    onUploadFileChange: (file: File | null) => void;
    uploadStructure: 'direct' | 'subfolder';
    onUploadStructureChange: (s: 'direct' | 'subfolder') => void;
    uploadProgress: number;
    uploadStatus: string;
    onUploadStatusChange: (s: string) => void;
    serverId: number;
    onFileTooLarge?: (tooLarge: boolean) => void;
    backupFile: File | null;
    onBackupFileChange: (f: File | null) => void;
    // Gateway route (optional)
    gatewayRoute?: CreateRouteRequest;
    onGatewayRouteChange?: (next: CreateRouteRequest) => void;
    // Routes already attached to this server. Lets the picker show a
    // friendly "you already have this one" tag (instead of "taken")
    // when the user types one of their own domains, and lets the
    // submit path skip the create call for that case.
    existingRoutes?: GatewayRoute[];
    // Opens the full routes-management modal (the same one available
    // from the server header globe icon).
    onOpenRoutesModal?: () => void;
    // Actions
    onSubmit: () => void;
    onClose: () => void;
    submitting: boolean;
    fileTooLarge?: boolean;
    error: string;
    hasSubServers: boolean;
    isFirstSetup: boolean;
}

export default function SetupNewWizard(props: SetupNewWizardProps) {
    const sanitized = sanitizeName(props.subName);
    const effectiveVersion = useMemo(
        () => effectiveMcVersion(props.selectedMajor, props.selectedBuild),
        [props.selectedMajor, props.selectedBuild],
    );
    const recommendedJava = useMemo(() => recommendJavaForVersion(effectiveVersion), [effectiveVersion]);

    // Domain row only matters when the gateway actually handles traffic.
    // In ip_port mode players connect via Node IP + port, so showing a
    // domain input here is confusing — hide it.
    const { routingMode } = useAppData();
    const showDomainField = routingMode !== 'ip_port';

    // Block submit while we're mid-check or sitting on a taken domain.
    // Idle / available are both fine (idle covers "empty input — skip route
    // creation entirely", which is the no-domain case).
    const [domainAvailability, setDomainAvailability] = useState<DomainAvailability>('idle');
    const domainBlocksSubmit = showDomainField && (domainAvailability === 'checking' || domainAvailability === 'taken');

    return (
        <div className="flex-1 card flex flex-col overflow-hidden min-w-0">
            {/* Header */}
            <div className="modal-header flex items-center justify-between shrink-0">
                <h3 className="modal-title">
                    {props.isFirstSetup ? 'Server Setup' : 'Add New Server'}
                </h3>
                {props.hasSubServers && (
                    <button type="button" onClick={props.onClose} className="text-(--base-07) hover:text-(--error-light) transition-colors">
                        <X size={20} />
                    </button>
                )}
            </div>

            {/* Body */}
            <div className="flex-1 overflow-y-auto p-6 space-y-5 min-h-0">
                {/* Row 1: Name */}
                <div className="flex flex-col gap-[5px]">
                    <label className="input-label">Server Slot Name</label>
                    <input
                        type="text"
                        value={props.subName}
                        onChange={e => props.onSubNameChange(e.target.value)}
                        placeholder="e.g. Survival"
                        className={`input-mono w-full md:w-1/2 ${props.subNameError ? 'input-field-error' : ''}`}
                        autoFocus
                    />
                    {props.subNameError && <p className="text-xs text-(--error-light)">{props.subNameError}</p>}
                    {props.subName && !props.subNameError && (
                        <p className="text-xs text-(--base-07) font-mono">
                            Stored as: <span className="text-(--primary-light)">/{sanitized}/</span>
                        </p>
                    )}
                </div>

                {/* Row 2: Optional gateway route (only when gateway routes traffic) */}
                {showDomainField && (
                    <div className="flex flex-col gap-[5px]">
                        <div className="flex items-center gap-2">
                            <label className="input-label flex items-center gap-1.5 mb-0">
                                Domain Route
                                <span className="text-[9px] font-mono uppercase tracking-[0.08em] text-(--base-06) bg-(--base-03) px-1 py-0.5 rounded-sm ml-1">Optional</span>
                            </label>
                            {props.onOpenRoutesModal && (
                                <button
                                    type="button"
                                    onClick={props.onOpenRoutesModal}
                                    title="Manage all routes for this server"
                                    aria-label="Manage routes"
                                    className="inline-flex items-center justify-center w-6 h-6 rounded-md bg-(--accent-ghost) border border-(--accent-border) text-(--accent-light) hover:bg-(--accent)/15 transition-colors"
                                >
                                    <Globe size={12} />
                                </button>
                            )}
                        </div>
                        <RouteDomainPicker
                            value={props.gatewayRoute || { targetPort: 25565 }}
                            onChange={next => props.onGatewayRouteChange?.(next)}
                            onAvailabilityChange={setDomainAvailability}
                            existingRoutes={props.existingRoutes}
                            portChildren={
                                <select
                                    value={props.gatewayRoute?.targetPort || 25565}
                                    onChange={e => props.onGatewayRouteChange?.({ ...(props.gatewayRoute || {}), targetPort: Number(e.target.value) })}
                                    className="input-field text-sm w-28"
                                >
                                    <option value={25565}>MC (25565)</option>
                                </select>
                            }
                        />
                        <p className="text-xs text-(--base-06)">Routes are managed via the globe button above or the Routes button next to the power actions.</p>
                    </div>
                )}

                {/* Row 2: Java Version (full width) */}
                <JavaVersionPicker
                    value={props.javaImage}
                    onChange={props.onJavaChange}
                    serverType={props.serverType}
                    recommended={recommendedJava ?? undefined}
                    mcVersion={effectiveVersion || undefined}
                />

                {/* Install tabs */}
                <div>
                    <label className="input-label mb-2 block">Installation Method</label>
                    <div className="flex bg-(--base-03) p-1 rounded-md max-w-md">
                        <button type="button" onClick={() => props.onInstallTabChange('online')}
                            className={`btn flex-1 py-2 text-sm border-0 rounded-md ${props.installTab === 'online' ? 'bg-(--accent) text-white' : 'bg-transparent text-(--base-07) hover:text-(--base-09)'}`}>
                            Online
                        </button>
                        {props.libraryEnabled && (
                            <button type="button" onClick={() => props.onInstallTabChange('library')}
                                className={`btn flex-1 py-2 text-sm border-0 rounded-md ${props.installTab === 'library' ? 'bg-(--accent) text-white' : 'bg-transparent text-(--base-07) hover:text-(--base-09)'}`}>
                                Library
                            </button>
                        )}
                        <button type="button" onClick={() => props.onInstallTabChange('upload')}
                            className={`btn flex-1 py-2 text-sm border-0 rounded-md ${props.installTab === 'upload' ? 'bg-(--accent) text-white' : 'bg-transparent text-(--base-07) hover:text-(--base-09)'}`}>
                            Upload / SFTP
                        </button>
                        <button type="button" onClick={() => props.onInstallTabChange('backup')}
                            className={`btn flex-1 py-2 text-sm border-0 rounded-md ${props.installTab === 'backup' ? 'bg-(--accent) text-white' : 'bg-transparent text-(--base-07) hover:text-(--base-09)'}`}>
                            Backup
                        </button>
                        <button type="button" onClick={() => props.onInstallTabChange('modpack')}
                            className={`btn flex-1 py-2 text-sm border-0 rounded-md ${props.installTab === 'modpack' ? 'bg-(--accent) text-white' : 'bg-transparent text-(--base-07) hover:text-(--base-09)'}`}>
                            Modrinth modpacks
                        </button>
                        <button type="button" onClick={() => props.onInstallTabChange('pack')}
                            className={`btn flex-1 py-2 text-sm border-0 rounded-md ${props.installTab === 'pack' ? 'bg-(--accent) text-white' : 'bg-transparent text-(--base-07) hover:text-(--base-09)'}`}>
                            My modpacks
                        </button>
                    </div>
                </div>

                {/* Tab content */}
                {props.installTab === 'online' && (
                    <div className="min-h-[220px]">
                        <VersionPicker
                            software={props.software}
                            onSoftwareChange={props.onSoftwareChange}
                            softwareList={props.softwareList}
                            allVersions={props.allVersions}
                            selectedMajor={props.selectedMajor}
                            onMajorChange={props.onMajorChange}
                            selectedBuild={props.selectedBuild}
                            onBuildChange={props.onBuildChange}
                            loading={props.loadingVersions}
                        />
                    </div>
                )}

                {props.installTab === 'library' && props.libraryEnabled && (
                    <LibraryPicker
                        files={props.libraryFiles}
                        path={props.libraryPath}
                        selectedFile={props.selectedLibraryFile}
                        onNavigate={props.onLibraryNavigate}
                        onSelect={props.onLibrarySelect}
                    />
                )}

                {props.installTab === 'upload' && (
                    <UploadSection
                        uploadFile={props.uploadFile}
                        onFileChange={props.onUploadFileChange}
                        uploadStructure={props.uploadStructure}
                        onStructureChange={props.onUploadStructureChange}
                        uploadProgress={props.uploadProgress}
                        uploadStatus={props.uploadStatus}
                        onStatusChange={props.onUploadStatusChange}
                        serverId={props.serverId}
                        onFileTooLarge={props.onFileTooLarge}
                    />
                )}

                {props.installTab === 'backup' && (
                    <BackupImportSection
                        file={props.backupFile}
                        onFileChange={props.onBackupFileChange}
                        uploadProgress={props.uploadProgress}
                        uploadStatus={props.uploadStatus}
                        onFileTooLarge={props.onFileTooLarge}
                    />
                )}

                {props.installTab === 'modpack' && (
                    <ModpackPicker
                        selection={props.modpackSelection ?? null}
                        onSelect={(s) => props.onModpackSelect?.(s)}
                    />
                )}

                {props.installTab === 'pack' && (
                    <PackPicker
                        selection={props.packSelection ?? null}
                        onSelect={(s) => props.onPackSelect?.(s)}
                    />
                )}

                {/* JVM Flags (collapsed by default) */}
                <JvmFlagsSection
                    extraFlags={props.extraFlags}
                    onChange={props.onFlagsChange}
                    ramMB={props.ramMB}
                />

                {props.error && <p className="text-(--error-light) text-sm font-medium">{props.error}</p>}
            </div>

            {/* Footer */}
            <div className="modal-footer flex gap-2">
                <button
                    type="button"
                    onClick={props.onSubmit}
                    disabled={props.submitting || !sanitized || props.fileTooLarge || domainBlocksSubmit}
                    title={domainBlocksSubmit
                        ? (domainAvailability === 'checking' ? 'Checking domain availability…' : 'Pick an available domain or clear the field.')
                        : undefined}
                    className="btn btn-primary btn-lg flex-1"
                >
                    {props.submitting
                        ? <><RefreshCw size={16} className="animate-spin" /> Working...</>
                        : <><Rocket size={16} /> Install & Start Server</>
                    }
                </button>
            </div>
        </div>
    );
}
