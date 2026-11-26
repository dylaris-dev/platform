'use client';

import HelpTourButton from '@/components/help/HelpTourButton';
import { tours } from '@/components/help/tours';
import GuardedLink from '@/components/GuardedLink';
import PageHeader from '@/components/ui/PageHeader';
import '@/styles/tickets.css';

export default function HelpCenterPage() {
    return (
        <div className="space-y-6">
            <PageHeader
                title="Help center"
                description="Use guided examples to explore screens, including features your account may not have. Next switches the example area and tab. The real panel stays where you left it, with your actual permissions."
                actions={<>
                    <GuardedLink href="/modpacks/guide" className="btn btn-secondary btn-sm">Full modpack authoring guide</GuardedLink>
                    <GuardedLink href="/favorites" className="btn btn-secondary btn-sm">Favorites &amp; alerts</GuardedLink>
                </>}
            />
            <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
                {tours.map(tour => (
                    <article key={tour.id} className="srv-surface flex flex-col gap-3 p-5">
                        <h2 className="text-[15px] font-semibold text-(--base-09)">{tour.title}</h2>
                        <p className="text-sm text-(--base-07)">{tour.summary}</p>
                        <p className="flex-1 text-xs text-(--base-06)">{tour.access}</p>
                        <div className="flex items-center justify-between">
                            <span className="text-xs text-(--base-06)">{tour.steps.length} guided steps</span>
                            <HelpTourButton topic={tour.id} />
                        </div>
                    </article>
                ))}
            </div>
        </div>
    );
}
