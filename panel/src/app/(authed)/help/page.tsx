'use client';

import HelpTourButton from '@/components/help/HelpTourButton';
import { tours } from '@/components/help/tours';
import GuardedLink from '@/components/GuardedLink';

export default function HelpCenterPage() {
    return <main className="flex-1 min-w-0 overflow-auto p-6"><div className="max-w-5xl mx-auto space-y-6">
        <header><p className="mono-label text-(--accent-light)">Panel help</p><h1 className="text-2xl font-display mt-2">Learn the workflow before changing anything</h1><p className="text-sm text-(--base-06) mt-3 max-w-3xl">Use guided examples to explore screens, including features your account may not have. Next switches the example area and tab. The real panel stays where you left it, with your actual permissions.</p></header>
        <div className="grid md:grid-cols-2 gap-4">{tours.map(tour => <article key={tour.id} className="card p-5 flex flex-col gap-3"><h2 className="font-medium">{tour.title}</h2><p className="text-sm text-(--base-06)">{tour.summary}</p><p className="text-xs text-(--base-06) flex-1">{tour.access}</p><div className="flex justify-between items-center"><span className="text-xs text-(--base-06)">{tour.steps.length} guided steps</span><HelpTourButton topic={tour.id} /></div></article>)}</div>
        <div className="flex gap-3 flex-wrap"><GuardedLink href="/modpacks/guide" className="btn btn-secondary btn-sm">Full modpack authoring guide</GuardedLink><GuardedLink href="/favorites" className="btn btn-secondary btn-sm">Favorites & alerts</GuardedLink></div>
    </div></main>;
}
