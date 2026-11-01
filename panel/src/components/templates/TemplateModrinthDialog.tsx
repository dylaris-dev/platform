"use client";

import { X } from 'lucide-react';
import ModalPanel from '@/components/ui/ModalPanel';
import ModrinthVersionBrowser from '@/components/modrinth/ModrinthVersionBrowser';
import { addTemplateModrinth } from '@/lib/api/templates';
import type { templateModrinthTarget } from '@/lib/templates';
import { toast } from '@/components/ui/Toast';

// Search Modrinth for the template's software and Minecraft version, open a
// project and pick a version (newest compatible first). Core keeps the file
// as a reference the node downloads on install; nothing is stored here.
export default function TemplateModrinthDialog({
    templateId,
    target,
    mcVersion,
    addedProjectIds,
    onAdded,
    onClose,
}: {
    templateId: number;
    target: NonNullable<ReturnType<typeof templateModrinthTarget>>;
    mcVersion: string;
    addedProjectIds: string[];
    onAdded: (sizeBytes?: number) => void;
    onClose: () => void;
}) {
    const pick = async (_projectId: string, versionId: string, hit: { title: string }) => {
        const res = await addTemplateModrinth(templateId, versionId);
        if (!res.success) throw new Error(res.message || `Could not add ${hit.title}.`);
        toast(`Added ${hit.title} as ${res.reference?.path ?? 'a Modrinth reference'}.`, true);
        if (res.warning) toast(res.warning, false);
        onAdded(res.sizeBytes);
    };
    return (
        <div className="modal-overlay animate-fade-in" onClick={onClose}>
            <ModalPanel onClose={onClose} className="modal-panel max-w-3xl w-full h-[80vh] flex flex-col" onClick={(e) => e.stopPropagation()}>
                <div className="modal-header flex items-center justify-between gap-3">
                    <h3 className="modal-title">Add from Modrinth</h3>
                    <button type="button" onClick={onClose} aria-label="Close" className="btn btn-ghost btn-sm"><X size={16} /></button>
                </div>
                <div className="modal-body flex flex-col flex-1 min-h-0 gap-3">
                    <p className="text-xs text-(--base-06)">
                        Showing {target.projectType}s for {target.loader}{mcVersion ? ` on Minecraft ${mcVersion}` : ''}. Open a
                        project and choose a version; the newest compatible one is listed first. It goes into {target.dir}/ and is
                        downloaded from Modrinth when a server is installed from this template.
                    </p>
                    <ModrinthVersionBrowser
                        loader={target.loader}
                        mcVersion={mcVersion || undefined}
                        projectType={target.projectType}
                        installedProjectIds={addedProjectIds}
                        onPick={pick}
                    />
                </div>
            </ModalPanel>
        </div>
    );
}
