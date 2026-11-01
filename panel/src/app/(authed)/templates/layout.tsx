"use client";

import { LayoutTemplate } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import { templatesVisible } from '@/lib/templates';

// One gate for every Templates page, matching Core's audience gate: a page
// reached by URL while the feature is closed explains itself instead of
// rendering a list of 403s.
export default function TemplatesLayout({ children }: { children: React.ReactNode }) {
    const { featureFlags, user } = useAppData();
    if (templatesVisible(featureFlags.templates, !!user?.isAdmin)) return <>{children}</>;
    return (
        <main className="flex-1 min-h-0 overflow-y-auto p-5">
            <div className="card p-8 flex flex-col items-center text-center gap-2 max-w-lg mx-auto mt-10">
                <LayoutTemplate size={28} className="text-(--base-05)" />
                <h1 className="text-sm font-medium text-(--base-09)">Templates are not available</h1>
                <p className="text-xs text-(--base-06)">
                    Templates are not open to your account on this platform. Ask an administrator if you need them.
                </p>
            </div>
        </main>
    );
}
