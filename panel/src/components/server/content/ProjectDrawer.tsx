"use client";

import { Download, ExternalLink, RefreshCw, X } from 'lucide-react';
import {
    Drawer, DrawerBackdrop, DrawerClose, DrawerContent, DrawerDescription, DrawerTitle,
} from '@ui-sdk/react/drawer';
import type { InstalledMod, ModrinthProject, ModrinthVersion } from '@/lib/api/modrinth';
import { installedState } from '@/lib/installedState';
import { isPending, jobLabel, type InstallJob } from '@/lib/modInstallJobs';
import { InstallBar } from '@/components/ModInstallWidget';
import { ModDescription } from '@/components/mods/ModDescription';
import { Skeleton, SkeletonText, SkeletonCard } from '@/components/Skeleton';
import Segmented from '@/components/ui/Segmented';

const DESC_OPTIONS = [{ id: 'short' as const, label: 'Short' }, { id: 'full' as const, label: 'Full' }];

/** The project detail: description with a short/full switch and the version list. */
export default function ProjectDrawer({
    open, onClose, loading, project, versions, highlightVersionId, mcVersion, descMode, onDescMode,
    installedMod, jobFor, planningVersion, onInstall,
}: {
    open: boolean;
    onClose: () => void;
    loading: boolean;
    project: ModrinthProject | null;
    versions: ModrinthVersion[];
    highlightVersionId: string | null;
    mcVersion: string;
    descMode: 'short' | 'full';
    onDescMode: (m: 'short' | 'full') => void;
    installedMod?: InstalledMod;
    jobFor: (projectId: string) => InstallJob | undefined;
    planningVersion: string | null;
    onInstall: (project: ModrinthProject, v: ModrinthVersion) => void;
}) {
    return (
        <Drawer open={open} onOpenChange={o => { if (!o) onClose(); }} side="right" modal>
            <DrawerBackdrop className="srv-drawer-backdrop" />
            <DrawerContent className="srv-drawer flex h-dvh w-[min(100vw,40rem)] flex-col">
                <div className="flex shrink-0 items-start gap-4 p-6">
                    {project?.icon_url && (
                        // eslint-disable-next-line @next/next/no-img-element
                        <img src={project.icon_url} alt="" className="size-14 shrink-0 rounded-(--radius-md)" />
                    )}
                    <div className="min-w-0 flex-1">
                        <DrawerTitle className="truncate text-lg font-semibold text-(--base-09)">
                            {project?.title ?? 'Loading project'}
                        </DrawerTitle>
                        <DrawerDescription className="srv-num mt-0.5 text-xs text-(--base-06)">
                            {project ? `${project.downloads.toLocaleString()} downloads` : 'Fetching details from Modrinth'}
                        </DrawerDescription>
                        {project && (
                            // The description is only what a description can be: no version
                            // history, no gallery, no comments. This is the way out to the rest.
                            <a
                                href={`https://modrinth.com/project/${project.slug}`}
                                target="_blank"
                                rel="noopener noreferrer"
                                className="btn btn-secondary btn-sm mt-3"
                            >
                                Open on Modrinth <ExternalLink size={14} />
                            </a>
                        )}
                    </div>
                    <DrawerClose aria-label="Close" className="btn btn-secondary btn-icon btn-sm shrink-0">
                        <X size={16} />
                    </DrawerClose>
                </div>

                <div className="min-h-0 flex-1 space-y-6 overflow-y-auto px-6 pb-6">
                    {loading || !project ? (
                        <div className="space-y-3" aria-busy="true" aria-label="Loading project">
                            <SkeletonText width="w-3/4" className="h-3" />
                            <SkeletonText width="w-1/2" className="h-3" />
                            <SkeletonCard height="h-40" />
                            <Skeleton className="h-14 w-full rounded-(--radius-md)" />
                            <Skeleton className="h-14 w-full rounded-(--radius-md)" />
                        </div>
                    ) : (
                        <>
                            <section>
                                <div className="mb-3 flex items-center justify-between">
                                    <h3 className="srv-content-section-title">Description</h3>
                                    <Segmented ariaLabel="Description length" options={DESC_OPTIONS} value={descMode} onChange={onDescMode} />
                                </div>
                                {descMode === 'short' ? (
                                    <p className="text-sm leading-relaxed text-(--base-07)">{project.description}</p>
                                ) : project.body ? (
                                    <ModDescription body={project.body} />
                                ) : (
                                    <p className="text-sm text-(--base-06)">No full description provided.</p>
                                )}
                            </section>

                            {/* Newest build for this MC version highlighted */}
                            <section>
                                <h3 className="srv-content-section-title mb-3">Versions</h3>
                                {versions.length === 0 ? (
                                    <p className="text-sm text-(--base-06)">No compatible versions found.</p>
                                ) : (
                                    <div className="space-y-2">
                                        {versions.slice(0, 30).map(v => {
                                            const highlight = v.id === highlightVersionId;
                                            // Which build is on the server: the one question you open
                                            // this with is what have I got.
                                            const state = installedState(installedMod, v.id);
                                            const vJob = jobFor(project.id);
                                            const job = vJob && vJob.versionId === v.id ? vJob : undefined;
                                            const planning = planningVersion === v.id;
                                            const jobBusy = (!!job && isPending(job)) || planning;
                                            return (
                                                <div
                                                    key={v.id}
                                                    className="srv-content-version-row"
                                                    data-state={state === 'installed' ? 'installed' : state === 'failed' ? 'failed' : highlight ? 'newest' : undefined}
                                                >
                                                    <div className="min-w-0 flex-1">
                                                        <div className="flex flex-wrap items-center gap-2">
                                                            <span className="srv-num truncate text-sm font-medium text-(--base-09)">{v.version_number}</span>
                                                            {state === 'installed' && <span className="text-xs text-(--success-light)">On this server</span>}
                                                            {state === 'installing' && <span className="text-xs text-(--base-06)">Installing…</span>}
                                                            {state === 'failed' && <span className="text-xs text-(--warning-light)">Install failed</span>}
                                                            {highlight && state === null && <span className="text-xs text-(--accent-light)">Newest for {mcVersion}</span>}
                                                        </div>
                                                        <div className="srv-num mt-0.5 truncate text-xs text-(--base-06)">
                                                            {v.version_type} · {v.loaders.join(', ')} · MC {v.game_versions.join(', ')}
                                                        </div>
                                                        {job && isPending(job) && <InstallBar job={job} />}
                                                        {job && (job.state === 'failed' || job.state === 'unknown') && job.message && (
                                                            <p className="mt-1 text-xs text-(--warning-light)">{job.message}</p>
                                                        )}
                                                    </div>
                                                    <button
                                                        onClick={() => onInstall(project, v)}
                                                        disabled={state === 'installing' || jobBusy}
                                                        aria-busy={jobBusy}
                                                        className="btn btn-secondary btn-sm shrink-0"
                                                    >
                                                        {jobBusy ? <RefreshCw size={14} className="animate-spin motion-reduce:animate-none" /> : <Download size={14} />}
                                                        {planning ? 'Checking…' : job && jobBusy ? jobLabel(job) : state === 'installed' ? 'Reinstall' : state === 'failed' ? 'Try again' : 'Install'}
                                                    </button>
                                                </div>
                                            );
                                        })}
                                    </div>
                                )}
                            </section>
                        </>
                    )}
                </div>
            </DrawerContent>
        </Drawer>
    );
}
