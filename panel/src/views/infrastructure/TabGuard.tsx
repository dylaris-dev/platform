"use client";

import Link from 'next/link';
import { Info } from 'lucide-react';
import EmptyState from '@/components/ui/EmptyState';

/**
 * What a gated tab shows when its precondition is not met.
 *
 * This exists because the tabs became real URLs. Before, a tab that should not
 * exist simply had no button and there was nothing to guard - the check WAS the
 * rendering. Now every one of these pages is one typed address away, so each
 * gated page asks for itself.
 *
 * It says what is missing and offers the way back, rather than rendering an
 * empty panel that reads like a broken screen.
 */
export default function TabGuard({ reason }: { reason: string }) {
    return (
        <EmptyState
            icon={<Info size={20} />}
            title="Nothing to show here"
            description={reason}
            action={<Link href="/infrastructure/nodes" className="btn btn-secondary btn-sm">Back to nodes</Link>}
        />
    );
}
