"use client";

import LibraryView from '@/views/LibraryView';
import { useAppData } from '@/lib/AppDataContext';
import { FolderOpen } from 'lucide-react';

export default function LibraryPage() {
    const { featureFlags } = useAppData();

    // The module row used to be the only gate, and a module row hides a NAVBAR
    // ENTRY. This page had no check of its own and Core's library routes were
    // capability-gated rather than feature-gated, so switching the library off
    // removed the link and left the page and its API answering to anyone who
    // knew the URL. Both ends are gated now; this one so the reader gets a
    // sentence instead of a wall of failed requests.
    if (!featureFlags.library) {
        return (
            <main className="flex-1 flex flex-col items-center justify-center overflow-hidden relative z-10 p-6">
                <FolderOpen size={28} className="text-(--base-05) mb-3" />
                <h1 className="h-section mb-1">The file library is off</h1>
                <p className="text-sm text-(--base-06) text-center max-w-sm">
                    An admin can turn it on under Settings, Features, Subsystems.
                </p>
            </main>
        );
    }

    return (
        <main className="flex-1 flex flex-col overflow-hidden relative z-10 p-6">
            <LibraryView />
        </main>
    );
}
