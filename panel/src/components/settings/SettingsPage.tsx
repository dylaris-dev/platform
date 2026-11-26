'use client';

import React from 'react';
import { SkeletonHeader, SkeletonCard } from '@/components/Skeleton';
import PageHeader from '@/components/ui/PageHeader';

/**
 * The frame every settings screen sits in.
 *
 * There were six different page headers across the settings tabs - three font
 * sizes, two weights, some with an icon, some with the description above the
 * heading - because each tab wrote its own. Same for the loading state and the
 * column width. None of it was a decision; it was just whatever the tab next to
 * it happened to do on the day it was written.
 */

/** Column width. Settings are read left to right, so the cap is deliberate:
 *  a 1600px-wide row of switches is unreadable. */
const WIDTHS = {
    '2xl': 'max-w-2xl',
    '3xl': 'max-w-3xl',
    '4xl': 'max-w-4xl',
    '5xl': 'max-w-5xl',
    full: '',
} as const;

export default function SettingsPage({
    title,
    description,
    icon: Icon,
    width = '3xl',
    loading = false,
    /** Number of card outlines to draw while loading. */
    skeletonCards = 1,
    actions,
    children,
}: {
    title: React.ReactNode;
    description?: React.ReactNode;
    icon?: React.ElementType;
    width?: keyof typeof WIDTHS;
    loading?: boolean;
    skeletonCards?: number;
    /** Page-level buttons, right of the heading. Not a save control: saving
     *  belongs to a card, because a card is what knows what it would write. */
    actions?: React.ReactNode;
    children: React.ReactNode;
}) {
    const frame = `space-y-6 ${WIDTHS[width]}`;

    if (loading) {
        return (
            <div className={frame}>
                <SkeletonHeader />
                {Array.from({ length: skeletonCards }).map((_, i) => (
                    <SkeletonCard key={i} height="h-56" />
                ))}
            </div>
        );
    }

    return (
        <div className={frame}>
            <PageHeader
                as="h2"
                title={title}
                description={description}
                icon={Icon ? <Icon size={18} /> : undefined}
                actions={actions}
            />
            {children}
        </div>
    );
}
