"use client";

import { useState } from 'react';
import Link from 'next/link';
import { CheckCircle2, CircleAlert, X } from 'lucide-react';
import ModalPanel from '@/components/ui/ModalPanel';
import type { SkippedFile, Template } from '@/lib/api/templates';
import NewTemplateDialog from './NewTemplateDialog';

// "Save as template" from a server's Files view: the new-template form locked
// to that server, then what was saved and what Core left out.
export default function CaptureTemplateDialog({ serverId, onClose }: { serverId: number; onClose: () => void }) {
    const [done, setDone] = useState<{ template: Template; skipped: SkippedFile[] } | null>(null);

    if (!done) {
        return <NewTemplateDialog fromServerId={serverId} onClose={onClose} onCreated={(template, skipped) => setDone({ template, skipped })} />;
    }

    const { template, skipped } = done;
    return (
        <div className="modal-overlay animate-fade-in" onClick={onClose}>
            <ModalPanel onClose={onClose} className="modal-panel max-w-lg" onClick={(e) => e.stopPropagation()}>
                <div className="modal-header flex items-center justify-between gap-3">
                    <h3 className="modal-title flex items-center gap-2 min-w-0">
                        <CheckCircle2 size={16} className="text-(--success-light) shrink-0" />
                        <span className="truncate">Saved as {template.name}</span>
                    </h3>
                    <button type="button" onClick={onClose} aria-label="Close" className="text-(--base-06) hover:text-(--base-09)"><X size={16} /></button>
                </div>
                <div className="modal-body">
                    {skipped.length === 0 ? (
                        <p className="text-sm text-(--base-08)">Every selected file is in the template.</p>
                    ) : (
                        <div className="flex items-start gap-2 text-xs text-(--base-09)" role="alert">
                            <CircleAlert size={16} className="text-(--warning) mt-0.5 shrink-0" />
                            <div className="min-w-0">
                                <p className="font-medium">
                                    {skipped.length} {skipped.length === 1 ? 'file was' : 'files were'} left out of the template:
                                </p>
                                <ul className="mt-1 space-y-0.5 max-h-60 overflow-y-auto">
                                    {skipped.map((s, i) => (
                                        <li key={`${s.path}-${i}`}><span className="font-mono">{s.path}</span>: {s.reason}</li>
                                    ))}
                                </ul>
                            </div>
                        </div>
                    )}
                </div>
                <div className="modal-footer">
                    <button type="button" onClick={onClose} className="btn btn-secondary">Close</button>
                    <Link href={`/templates/${template.id}`} className="btn btn-primary">Open template</Link>
                </div>
            </ModalPanel>
        </div>
    );
}
