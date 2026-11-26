'use client';

import { ModrinthLibraryPanel } from '@/components/mods/ModrinthLibrary';
import HelpTourButton from '@/components/help/HelpTourButton';
import PageHeader from '@/components/ui/PageHeader';

const STEPS = [
    { title: '1. Star a project', body: 'Save a mod, resource pack or shader from a content list or Modrinth search. A star is a bookmark; it does not install anything or subscribe you automatically.' },
    { title: '2. Choose a version alert', body: 'Use the bell to select Minecraft releases and a target loader. Checks run daily and report once per Minecraft version, not on every mod update.' },
    { title: '3. Read your inbox', body: 'Specific checks finish after notification. Future-release alerts stay active. Messages remain in the notification bell until you delete them.' },
];

export default function FavoritesPage() {
    return (
        <div className="space-y-6">
            <PageHeader
                title="Favorites & alerts"
                description="One library across Server Content and Modpacks. Your saved projects and alerts are private to your account."
                actions={<HelpTourButton topic="favorites" />}
            />
            <div className="grid gap-4 md:grid-cols-3">
                {STEPS.map(s => (
                    <section key={s.title} className="srv-surface p-5">
                        <h2 className="text-sm font-semibold text-(--base-09)">{s.title}</h2>
                        <p className="mt-2 text-[13px] leading-relaxed text-(--base-06)">{s.body}</p>
                    </section>
                ))}
            </div>
            <ModrinthLibraryPanel />
        </div>
    );
}
