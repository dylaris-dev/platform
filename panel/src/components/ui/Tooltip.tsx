'use client';

import React from 'react';
import { Tooltip as SdkTooltip, TooltipContent, TooltipTrigger } from '@ui-sdk/react/tooltip';

/**
 * A short plain-text hint on hover and keyboard focus; for an icon-only
 * control it also names it. Replaces the native `title` attribute.
 *
 * `children` is ONE element (a button, a span) that becomes the trigger and
 * keeps its own classes. Anything longer than a few words, or anything with a
 * link in it, belongs in HelpTip (a Popover) instead.
 */
export default function Tooltip({
    content,
    children,
    side = 'top',
    delay,
    className = '',
}: {
    content: string;
    children: React.ReactElement;
    side?: 'top' | 'right' | 'bottom' | 'left';
    /** Milliseconds the pointer rests before it opens. */
    delay?: number;
    className?: string;
}) {
    return (
        <SdkTooltip delay={delay}>
            <TooltipTrigger render={children} />
            <TooltipContent side={side} sideOffset={6} className={`ui-popup ui-tooltip ${className}`}>
                {content}
            </TooltipContent>
        </SdkTooltip>
    );
}
