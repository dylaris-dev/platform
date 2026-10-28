'use client';

import { ModrinthLibraryPanel } from '@/components/mods/ModrinthLibrary';
import HelpTourButton from '@/components/help/HelpTourButton';

export default function FavoritesPage() {
    return <main className="flex-1 min-w-0 overflow-auto p-6">
        <div className="max-w-5xl mx-auto space-y-5">
            <header className="flex justify-between items-start gap-4"><div><p className="mono-label text-(--accent-light)">Personal library</p><h1 className="font-display text-2xl mt-2">Favorites & alerts</h1><p className="text-sm text-(--base-06) mt-3">One library across Server Content and Modpacks. Your saved projects and alerts are private to your account.</p></div><HelpTourButton topic="favorites" /></header>
            <div className="grid md:grid-cols-3 gap-3">
                <section className="card p-4"><h2 className="font-medium text-sm">1. Star a project</h2><p className="text-xs text-(--base-06) mt-2">Save a mod, resource pack or shader from a content list or Modrinth search. A star is a bookmark; it does not install anything or subscribe you automatically.</p></section>
                <section className="card p-4"><h2 className="font-medium text-sm">2. Choose a version alert</h2><p className="text-xs text-(--base-06) mt-2">Use the bell to select Minecraft releases and a target loader. Checks run daily and report once per Minecraft version, not on every mod update.</p></section>
                <section className="card p-4"><h2 className="font-medium text-sm">3. Read your inbox</h2><p className="text-xs text-(--base-06) mt-2">Specific checks finish after notification. Future-release alerts stay active. Messages remain in the notification bell until you delete them.</p></section>
            </div>
            <ModrinthLibraryPanel />
        </div>
    </main>;
}
