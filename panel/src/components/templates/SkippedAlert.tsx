"use client";

import { CircleAlert, X } from 'lucide-react';
import type { SkippedFile } from '@/lib/api/templates';

// Files Core left out of a template. An alert rather than a toast: the list is
// the thing to read, and a toast is gone before the third line.
export default function SkippedAlert({ skipped, onDismiss, children }: {
    skipped: SkippedFile[];
    onDismiss: () => void;
    children?: React.ReactNode;
}) {
    return (
        <div className="card p-3 border border-(--warning) bg-(--warning)/10 mb-4 flex items-start gap-2" role="alert">
            <CircleAlert size={16} className="text-(--warning) mt-0.5 shrink-0" />
            <div className="text-xs text-(--base-09) flex-1 min-w-0">
                <p className="font-medium">
                    {skipped.length} {skipped.length === 1 ? 'file was' : 'files were'} left out of the template:
                </p>
                <ul className="mt-1 space-y-0.5 max-h-40 overflow-y-auto">
                    {skipped.map((s, i) => (
                        <li key={`${s.path}-${i}`}><span className="font-mono">{s.path}</span>: {s.reason}</li>
                    ))}
                </ul>
                {children}
            </div>
            <button onClick={onDismiss} aria-label="Dismiss" className="text-(--base-06) hover:text-(--base-09) shrink-0"><X size={14} /></button>
        </div>
    );
}
