'use client';

import React from 'react';
import { HelpCircle } from 'lucide-react';
import { Popover, PopoverContent, PopoverTrigger } from '@ui-sdk/react/popover';

/**
 * Field-level help.
 *
 * The panel had no help of its own: 28 files leaned on the native `title`
 * attribute, which is a browser tooltip with a delay, no keyboard route to it,
 * no styling, and no way to hold a second line or an example. Several of the
 * values it was used on - beam throttles, warp connection limits, an S3
 * endpoint - cannot be guessed from their label at all.
 *
 * A UI SDK Popover rather than a Tooltip: the text can hold a second line, code
 * or a link, so it has to stay open under the pointer and be reachable by keyboard.
 */
export default function HelpTip({
    children,
    label = 'Help',
    className = '',
}: {
    /** The help text. Rich content is fine: <code>, <strong>, a second line. */
    children: React.ReactNode;
    label?: string;
    className?: string;
}) {
    return (
        <Popover>
            <PopoverTrigger aria-label={label} className={`help-trigger ${className}`}>
                <HelpCircle size={14} />
            </PopoverTrigger>
            <PopoverContent aria-label={label} side="right" align="start" sideOffset={8} className="help-pop">
                {children}
            </PopoverContent>
        </Popover>
    );
}
