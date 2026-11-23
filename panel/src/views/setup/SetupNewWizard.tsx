"use client";

import Select from '@/components/ui/Select';
import React, { useMemo, useState } from 'react';
import { X, Rocket, RefreshCw, Globe, ArrowLeft, ArrowRight, AlertTriangle } from 'lucide-react';
import Callout from '@/components/server/Callout';
import SetupStepper from './SetupStepper';
import InstallMethodTabs from './InstallMethodTabs';
import { JAVA_IMAGES } from '@/lib/javaVersion';
import { CreateRouteRequest, GatewayRoute } from '@/lib/api';
import { useAppData } from '@/lib/AppDataContext';
import JavaVersionPicker, { recommendJavaForSetup } from './JavaVersionPicker';
import JvmFlagsSection from './JvmFlagsSection';
import VersionPicker, { VersionEntry } from './VersionPicker';
import UploadSection from './UploadSection';
import BackupImportSection from './BackupImportSection';
import ModpackPicker from './ModpackPicker';
import PackPicker from './PackPicker';
import TechnicPicker from './TechnicPicker';
import TemplatePicker, { type TemplateSelection } from './TemplatePicker';
import type { InstallTab } from './installTarget';
import RouteDomainPicker, { DomainAvailability } from '@/components/RouteDomainPicker';

const STEPS = [
    { id: 'name', label: 'Name' },
    { id: 'software', label: 'Software' },
    { id: 'runtime', label: 'Runtime' },
    { id: 'review', label: 'Review' },
];

function sanitizeName(raw: string): string {
    return raw.replace(/ /g, '_').replace(/[^a-zA-Z0-9\-_+]/g, '');
}

interface SetupNewWizardProps {
    // Name
    subName: string;
    onSubNameChange: (name: string) => void;
    subNameError: string;
    /** An upload is going to this name; Change discards it. */
    subNameLocked?: boolean;
    onSubNameUnlock?: () => void;
    // Java
    javaImage: string;
    onJavaChange: (id: string) => void;
    // Flags
    extraFlags: string;
    onFlagsChange: (flags: string) => void;
    ramMB: number;
    // Install tab
    installTab: InstallTab;
    onInstallTabChange: (tab: InstallTab) => void;
    modpackSelection?: import('@/views/setup/ModpackPicker').ModpackSelection | null;
    onModpackSelect?: (s: import('@/views/setup/ModpackPicker').ModpackSelection | null) => void;
    packSelection?: import('@/views/setup/PackPicker').PackSelection | null;
    onPackSelect?: (s: import('@/views/setup/PackPicker').PackSelection | null) => void;
    technicSelection?: import('@/views/setup/technic').TechnicSelection | null;
    onTechnicSelect?: (s: import('@/views/setup/technic').TechnicSelection | null) => void;
    /** The Template tab is offered only when the caller may use templates. */
    templatesEnabled?: boolean;
    templateSelection?: TemplateSelection | null;
    onTemplateSelect?: (s: TemplateSelection | null) => void;
    // Server type
    serverType?: 'game' | 'proxy';
    // Version picker
    software: string;
    onSoftwareChange: (s: string) => void;
    softwareList?: string[];
    allVersions: VersionEntry[];
    /** The Minecraft version the install will run, whatever tab it comes from. */
    targetMcVersion: string;
    /** An upload kept as it is whose files name no Minecraft version. */
    javaVersionUnknown?: boolean;
    selectedMajor: string;
    onMajorChange: (m: string) => void;
    selectedBuild: string;
    onBuildChange: (b: string) => void;
    loadingVersions: boolean;
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
    /** The server software choice under the upload tab. */
    uploadSoftware?: React.ReactNode;
    /** Something on the form still needs an answer. */
    submitBlocked?: boolean;
    error: string;
    hasSubServers: boolean;
    isFirstSetup: boolean;
}

export default function SetupNewWizard(props: SetupNewWizardProps) {
    const sanitized = sanitizeName(props.subName);
    const effectiveVersion = useMemo(
        () => props.targetMcVersion,
        [props.targetMcVersion],
    );
    const isProxy = props.serverType === 'proxy';
    const [step, setStep] = useState(0);
    const [reached, setReached] = useState(0);
    const recommendedJava = useMemo(
        () => recommendJavaForSetup({ isProxy, tab: props.installTab, software: props.software, build: props.selectedBuild, mcVersion: effectiveVersion }),
        [isProxy, props.installTab, props.software, props.selectedBuild, effectiveVersion],
    );

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

    const javaLabel = JAVA_IMAGES.find(j => j.id === props.javaImage)?.label ?? props.javaImage;

    // One line per install source, for the review step.
    const methodLabel: Record<InstallTab, string> = {
        online: 'Online', template: 'Template', upload: 'Upload / SFTP', backup: 'Backup',
        modpack: 'Modrinth modpack', pack: 'My modpack', technic: 'Technic',
    };
    const sourceLine = (() => {
        switch (props.installTab) {
            case 'online': return [props.software, props.selectedMajor, props.selectedBuild].filter(Boolean).join(' ') || 'Nothing selected';
            case 'template': return props.templateSelection?.name ?? 'Nothing selected';
            case 'upload': return props.uploadFile?.name ?? 'No file chosen';
            case 'backup': return props.backupFile?.name ?? 'No file chosen';
            case 'modpack': return props.modpackSelection?.title ?? 'Nothing selected';
            case 'pack': return props.packSelection ? `${props.packSelection.packName} ${props.packSelection.versionString}` : 'Nothing selected';
            case 'technic': return props.technicSelection?.displayName ?? 'Nothing selected';
        }
    })();

    const nameOk = !!sanitized && !props.subNameError && !domainBlocksSubmit;
    const installOk = !props.fileTooLarge && !props.submitBlocked;
    const last = STEPS.length - 1;
    const nextBlocked = (step === 0 && !nameOk) || (step === 1 && !installOk);
    const go = (i: number) => { setStep(i); setReached(r => Math.max(r, i)); };
    const submitBlocked = props.submitting || !sanitized || props.fileTooLarge || domainBlocksSubmit || props.submitBlocked;

    return (
        <div className="srv-surface flex min-w-0 flex-1 flex-col overflow-hidden">
            {/* Header */}
            <div className="flex shrink-0 items-start justify-between gap-3 px-6 pt-6">
                <div className="min-w-0">
                    <h2 className="h-page">{props.isFirstSetup ? 'Server setup' : 'Add a new server'}</h2>
                    <p className="mt-1 text-sm text-(--base-06)">Name it, choose what to install, then check the runtime.</p>
                </div>
                {props.hasSubServers && (
                    <button type="button" onClick={props.onClose} aria-label="Cancel and close" className="btn btn-ghost btn-icon">
                        <X size={16} />
                    </button>
                )}
            </div>
            <div className="shrink-0 px-6 pb-2 pt-4">
                <SetupStepper steps={STEPS} current={step} reached={reached} onSelect={go} />
            </div>

            {/* Body: every step stays mounted so a picker keeps its search and state. */}
            <div className="min-h-0 flex-1 overflow-y-auto px-6 py-5">
                <div className="space-y-6" hidden={step !== 0}>
                    <div className="flex flex-col gap-1.5">
                        <label className="srv-label" htmlFor="setup-sub-name">Server slot name</label>
                        <div className="flex items-center gap-2">
                            <input
                                id="setup-sub-name"
                                type="text"
                                value={props.subName}
                                onChange={e => props.onSubNameChange(e.target.value)}
                                placeholder="e.g. Survival"
                                className={`input-mono w-full md:w-1/2 ${props.subNameError ? 'input-field-error' : ''}`}
                                disabled={props.subNameLocked}
                                autoFocus
                            />
                            {props.subNameLocked && (
                                <button type="button" onClick={props.onSubNameUnlock} disabled={props.submitting}
                                    className="btn btn-secondary btn-sm" title="Discards the uploaded file">
                                    Change
                                </button>
                            )}
                        </div>
                        {props.subNameLocked && (
                            <p className="text-xs text-(--base-06)">The upload goes to this name. Change it to discard the upload.</p>
                        )}
                        {props.subNameError && <p className="text-xs text-(--error-light)">{props.subNameError}</p>}
                        {props.subName && !props.subNameError && (
                            <p className="srv-num text-xs text-(--base-07)">
                                Stored as: <span className="text-(--primary-light)">/{sanitized}/</span>
                            </p>
                        )}
                    </div>

                    {/* Optional gateway route (only when gateway routes traffic) */}
                    {showDomainField && (
                        <div className="flex flex-col gap-1.5">
                            <div className="flex items-center gap-2">
                                <span className="srv-label">Domain route</span>
                                <span className="srv-chip">Optional</span>
                                {props.onOpenRoutesModal && (
                                    <button
                                        type="button"
                                        onClick={props.onOpenRoutesModal}
                                        title="Manage all routes for this server"
                                        aria-label="Manage routes"
                                        className="btn btn-ghost btn-icon btn-sm"
                                    >
                                        <Globe size={14} />
                                    </button>
                                )}
                            </div>
                            <RouteDomainPicker
                                value={props.gatewayRoute || { targetPort: 25565 }}
                                onChange={next => props.onGatewayRouteChange?.(next)}
                                onAvailabilityChange={setDomainAvailability}
                                existingRoutes={props.existingRoutes}
                                portChildren={
                                    <Select
                                        ariaLabel="Target port"
                                        className="w-36"
                                        value={String(props.gatewayRoute?.targetPort || 25565)}
                                        onChange={v => props.onGatewayRouteChange?.({ ...(props.gatewayRoute || {}), targetPort: Number(v) })}
                                        options={[{ value: '25565', label: 'MC (25565)' }]}
                                    />
                                }
                            />
                            <p className="text-xs text-(--base-06)">Routes are managed via the globe button above or the Routes button next to the power actions.</p>
                        </div>
                    )}
                </div>

                <div className="space-y-5" hidden={step !== 1}>
                    <div>
                        <h3 className="text-sm font-medium text-(--base-09)">Installation method</h3>
                        <p className="mt-0.5 text-xs text-(--base-06)">Where the server software and its files come from.</p>
                    </div>
                    <InstallMethodTabs value={props.installTab} onChange={props.onInstallTabChange} templatesEnabled={props.templatesEnabled} />

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

                    {props.installTab === 'upload' && (
                        <>
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
                            {props.uploadSoftware}
                        </>
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

                    {props.installTab === 'template' && props.templatesEnabled && (
                        <TemplatePicker
                            selection={props.templateSelection ?? null}
                            onSelect={(s) => props.onTemplateSelect?.(s)}
                            serverType={props.serverType}
                        />
                    )}

                    {props.installTab === 'technic' && (
                        <TechnicPicker
                            selection={props.technicSelection ?? null}
                            onSelect={(s) => props.onTechnicSelect?.(s)}
                        />
                    )}
                </div>

                <div className="space-y-6" hidden={step !== 2}>
                    <JavaVersionPicker
                        value={props.javaImage}
                        onChange={props.onJavaChange}
                        serverType={props.serverType}
                        recommended={recommendedJava ?? undefined}
                        mcVersion={isProxy ? undefined : effectiveVersion || undefined}
                        versionUnknown={props.javaVersionUnknown}
                    />
                    {/* JVM Flags (collapsed by default) */}
                    <JvmFlagsSection
                        extraFlags={props.extraFlags}
                        onChange={props.onFlagsChange}
                        ramMB={props.ramMB}
                    />
                </div>

                <div className="space-y-5" hidden={step !== last}>
                    <div>
                        <h3 className="text-sm font-medium text-(--base-09)">Review</h3>
                        <p className="mt-0.5 text-xs text-(--base-06)">Check the choices below, then install.</p>
                    </div>
                    <dl className="grid grid-cols-[minmax(7rem,auto)_1fr] gap-x-6 gap-y-3 text-sm">
                        <dt className="text-(--base-06)">Slot name</dt>
                        <dd className="srv-num break-all text-(--base-09)">{sanitized ? `/${sanitized}/` : 'Not set'}</dd>
                        <dt className="text-(--base-06)">Install method</dt>
                        <dd className="text-(--base-09)">{methodLabel[props.installTab]}</dd>
                        <dt className="text-(--base-06)">Source</dt>
                        <dd className="srv-num break-all text-(--base-09)">{sourceLine}</dd>
                        <dt className="text-(--base-06)">Java</dt>
                        <dd className="text-(--base-09)">{javaLabel}</dd>
                        <dt className="text-(--base-06)">Memory</dt>
                        <dd className="srv-num text-(--base-09)">{props.ramMB} MB</dd>
                    </dl>
                    {props.subNameError && (
                        <Callout tone="error" icon={<AlertTriangle size={16} />} role="alert">{props.subNameError}</Callout>
                    )}
                    {props.error && (
                        <Callout tone="error" icon={<AlertTriangle size={16} />} role="alert">{props.error}</Callout>
                    )}
                </div>
            </div>

            {/* An error from a failed submit also has to be seen while the review is not open. */}
            {props.error && step !== last && (
                <div className="shrink-0 px-6 pb-4">
                    <Callout tone="error" icon={<AlertTriangle size={16} />} role="alert">{props.error}</Callout>
                </div>
            )}

            {/* Footer */}
            <div className="srv-setup-footer shrink-0">
                <button type="button" onClick={() => go(step - 1)} disabled={step === 0 || props.submitting} className="btn btn-secondary">
                    <ArrowLeft size={16} /> Back
                </button>
                <span className="srv-num ml-2 text-xs text-(--base-06)">Step {step + 1} of {STEPS.length}</span>
                <div className="ml-auto">
                    {step < last ? (
                        <button
                            type="button"
                            onClick={() => go(step + 1)}
                            disabled={nextBlocked}
                            title={step === 0 && domainBlocksSubmit
                                ? (domainAvailability === 'checking' ? 'Checking domain availability…' : 'Pick an available domain or clear the field.')
                                : undefined}
                            className="btn btn-primary"
                        >
                            Next <ArrowRight size={16} />
                        </button>
                    ) : (
                        <button
                            type="button"
                            onClick={props.onSubmit}
                            disabled={submitBlocked}
                            title={domainBlocksSubmit
                                ? (domainAvailability === 'checking' ? 'Checking domain availability…' : 'Pick an available domain or clear the field.')
                                : undefined}
                            className="btn btn-primary"
                        >
                            {props.submitting
                                ? <><RefreshCw size={16} className="animate-spin" /> Working...</>
                                : <><Rocket size={16} /> Install & Start Server</>
                            }
                        </button>
                    )}
                </div>
            </div>
        </div>
    );
}
