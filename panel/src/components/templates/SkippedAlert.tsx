"use client";

import { CircleAlert, X } from 'lucide-react';
import type { SkippedFile } from '@/lib/api/templates';
import Callout from '@/components/server/Callout';

// Files Core left out of a template. An alert rather than a toast: the list is
// the thing to read, and a toast is gone before the third line.
export default function SkippedAlert({ skipped, onDismiss, children }: {
    skipped: SkippedFile[];
    onDismiss: () => void;
    children?: React.ReactNode;
}) {
    return (
        <Callout
            tone="warning"
            role="alert"
            icon={<CircleAlert size={16} />}
            title={`${skipped.length} ${skipped.length === 1 ? 'file was' : 'files were'} left out of the template:`}
            actions={<button onClick={onDismiss} aria-label="Dismiss" className="app-icon-btn"><X size={14} /></button>}
        >
            <div className="min-w-0 text-xs">
                <ul className="space-y-0.5 max-h-40 overflow-y-auto">
                    {skipped.map((s, i) => (
                        <li key={`${s.path}-${i}`}><span className="font-mono">{s.path}</span>: {s.reason}</li>
                    ))}
                </ul>
                {children}
            </div>
        </Callout>
    );
}
