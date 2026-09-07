"use client";

import React, { useMemo } from 'react';
import { X, RotateCcw, Trash2, RefreshCw, AlertTriangle } from 'lucide-react';
import JavaVersionPicker, { recommendJavaForVersion, effectiveMcVersion } from './JavaVersionPicker';
import JvmFlagsSection from './JvmFlagsSection';
import VersionPicker, { VersionEntry } from './VersionPicker';
import LibraryPicker from './LibraryPicker';
import UploadSection from './UploadSection';
import BackupImportSection from './BackupImportSection';
import ModpackPicker from './ModpackPicker';
import PackPicker from './PackPicker';
import type { SubServerInstall } from '@/lib/api/subServerInstalls';

/**
 * What is installed right now, above the picker that would replace it.
 *
 * The picker shows what you are ABOUT to install and says nothing about what is
 * there, so an operator opening the tab to change a Java version saw an empty
 * search box and had to remember which pack they had chosen. Leaving the picker
 * untouched keeps this install; picking something replaces it.
 *
 * Absent for anything set up before Core recorded installs, and for a
 * sub-server whose record was written by an older Core. Silence is the honest
 * answer there - inventing "unknown" would look like a failure rather than a
 * gap.
 */
function InstalledNow({ install, kind }: { install?: SubServerInstall; kind: 'modpack' | 'pack' }) {
    if (!install) return null;
    const isModpack = kind === 'modpack' && !!install.modrinthProjectId;
    const isPack = kind === 'pack' && !!install.packId;
    if (!isModpack && !isPack) return null;

    const name = isModpack
        ? (install.modrinthProjectSlug || install.modrinthProjectId)
        : `Pack #${install.packId}`;
    const version = isModpack
        ? install.modrinthVersionId
        : `build ${install.packBuildId}`;

    return (
        <div className="mb-3 rounded-md border border-(--base-03) bg-(--base-02) px-3 py-2">
            <div className="text-xs text-(--base-06)">Installed now</div>
            <div className="text-sm text-(--base-09) mt-0.5 flex flex-wrap items-baseline gap-x-2">
                <span className="font-medium break-all">{name}</span>
                {version && <span className="font-mono text-xs text-(--base-07) break-all">{version}</span>}
                {install.mcVersion && <span className="text-xs text-(--base-06)">MC {install.mcVersion}</span>}
            </div>
            <div className="text-xs text-(--base-06) mt-1">
                Leave the search below untouched to keep it. Picking something replaces it.
            </div>
        </div>
    );
}

interface SetupEditModeProps {
    subName: string;
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
    /** What is on disk right now, when Core recorded it. */
    currentInstall?: SubServerInstall;
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
    // Warning
    activeServerMissing: boolean;
    activeSubServer?: string;
    // Actions
    onSubmit: () => void;
    onClose: () => void;
    onDelete: () => void;
    submitting: boolean;
    fileTooLarge?: boolean;
    error: string;
}

export default function SetupEditMode(props: SetupEditModeProps) {
    const effectiveVersion = useMemo(
        () => effectiveMcVersion(props.selectedMajor, props.selectedBuild),
        [props.selectedMajor, props.selectedBuild],
    );
    const recommendedJava = useMemo(() => recommendJavaForVersion(effectiveVersion), [effectiveVersion]);

    return (
        <div className="flex-1 card flex flex-col overflow-hidden min-w-0">
            {/* Header */}
            <div className="modal-header flex items-center justify-between shrink-0">
                <div>
                    <h3 className="modal-title">Edit Server Config</h3>
                    <p className="text-xs text-(--base-07) mt-1">
                        Editing: <span className="font-mono text-(--primary-light)">{props.subName}</span>
                    </p>
                </div>
                <button type="button" onClick={props.onClose} className="text-(--base-07) hover:text-(--error-light) transition-colors">
                    <X size={20} />
                </button>
            </div>

            {/* Body */}
            <div className="flex-1 overflow-y-auto p-6 space-y-4 min-h-0">

                {/* Warning banner */}
                {props.activeServerMissing && (
                    <div className="alert alert-warning gap-3 rounded-xl">
                        <AlertTriangle size={20} className="text-(--warning-light) shrink-0" />
                        <div className="text-sm">
                            <p className="font-medium text-(--warning-light)">Server folder not found</p>
                            <p className="text-(--base-07) mt-1">
                                The folder <code className="bg-black/20 px-1 rounded font-mono">{props.activeSubServer}</code> no longer exists.
                            </p>
                        </div>
                    </div>
                )}

                {/* Card 1: Runtime Settings */}
                <div className="card p-5 space-y-5">
                    <p className="text-sm font-semibold text-(--base-09)">Runtime Settings</p>

                    <JavaVersionPicker
                        value={props.javaImage}
                        onChange={props.onJavaChange}
                        serverType={props.serverType}
                        recommended={recommendedJava ?? undefined}
                        mcVersion={effectiveVersion || undefined}
                    />

                    <JvmFlagsSection
                        extraFlags={props.extraFlags}
                        onChange={props.onFlagsChange}
                        ramMB={props.ramMB}
                        defaultOpen
                    />
                </div>

                {/* Card 2: Reinstall Software */}
                <div className="card p-5 space-y-4">
                    <p className="text-sm font-semibold text-(--base-09)">Reinstall Software</p>

                    {/* Install tabs */}
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
                        <>
                            <InstalledNow install={props.currentInstall} kind="modpack" />
                            <ModpackPicker
                                selection={props.modpackSelection ?? null}
                                onSelect={(s) => props.onModpackSelect?.(s)}
                            />
                        </>
                    )}

                    {props.installTab === 'pack' && (
                        <>
                            <InstalledNow install={props.currentInstall} kind="pack" />
                            <PackPicker
                                selection={props.packSelection ?? null}
                                onSelect={(s) => props.onPackSelect?.(s)}
                            />
                        </>
                    )}
                </div>

                {props.error && <p className="text-(--error-light) text-sm font-medium">{props.error}</p>}
            </div>

            {/* Footer */}
            <div className="modal-footer flex gap-2">
                <button
                    type="button"
                    onClick={props.onSubmit}
                    disabled={props.submitting || props.fileTooLarge}
                    className="btn btn-primary btn-lg flex-1"
                >
                    {props.submitting
                        ? <><RefreshCw size={16} className="animate-spin" /> Working...</>
                        : <><RotateCcw size={16} /> Change & Restart Server</>
                    }
                </button>
                <button
                    type="button"
                    onClick={props.onDelete}
                    className="btn btn-danger btn-lg"
                    title="Delete this sub-server"
                >
                    <Trash2 size={16} />
                </button>
            </div>
        </div>
    );
}
