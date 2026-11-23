'use client';

import React from 'react';
import { Switch as SdkSwitch, SwitchThumb } from '@ui-sdk/react/switch';
import HelpTip from './HelpTip';

/**
 * The panel's on/off switch.
 *
 * The markup below was copied by hand into 19 files - the same four lines, with
 * the on/off class pair spelled out at each site. None of them had a focus
 * ring, a hover state or a disabled treatment; one of them had a disabled
 * cursor, in a Tailwind class, at a single call site.
 *
 * Use a Switch for one thing being on or off. Use a Checkbox for a value inside
 * a form or a multi-select.
 */
export interface SwitchProps {
    checked: boolean;
    onChange: (checked: boolean) => void;
    disabled?: boolean;
    /** Required: the switch renders no text of its own. */
    ariaLabel: string;
    className?: string;
}

export default function Switch({
    checked,
    onChange,
    disabled = false,
    ariaLabel,
    className = '',
}: SwitchProps) {
    return (
        <SdkSwitch
            checked={checked}
            onCheckedChange={onChange}
            aria-label={ariaLabel}
            disabled={disabled}
            className={`toggle-track ${checked ? 'toggle-track-on' : 'toggle-track-off'} ${className}`}
        >
            <SwitchThumb className={`toggle-knob ${checked ? 'toggle-knob-on' : 'toggle-knob-off'}`} />
        </SdkSwitch>
    );
}

/**
 * SwitchRow is the layout every settings page reimplements around a Switch:
 * label and description on the left, the control hard right, the whole row
 * separated from its neighbour.
 */
export function SwitchRow({
    label,
    description,
    help,
    checked,
    onChange,
    disabled = false,
    className = '',
    children,
}: {
    label: React.ReactNode;
    description?: React.ReactNode;
    /**
     * The long answer, behind a help icon beside the label.
     *
     * Separate from `description` because the two carry different weight: the
     * description is one line every reader passes over, and this is the
     * paragraph only the reader who is unsure opens. Folding the second into
     * the first is what turned settings pages into walls of grey text.
     */
    help?: React.ReactNode;
    checked: boolean;
    onChange: (checked: boolean) => void;
    disabled?: boolean;
    className?: string;
    /** Rendered under the row: a warning, a dependent field, an explanation. */
    children?: React.ReactNode;
}) {
    return (
        <div className={className}>
            <div className="flex items-center justify-between gap-4">
                <div className="min-w-0">
                    <div className="text-sm font-medium text-(--base-09) flex items-center gap-1.5">
                        {label}
                        {help && (
                            <HelpTip label={typeof label === 'string' ? `About ${label}` : 'Help'}>
                                {help}
                            </HelpTip>
                        )}
                    </div>
                    {description && (
                        <div className="text-xs text-(--base-06) mt-0.5">{description}</div>
                    )}
                </div>
                <Switch
                    checked={checked}
                    onChange={onChange}
                    disabled={disabled}
                    ariaLabel={typeof label === 'string' ? label : 'Toggle'}
                />
            </div>
            {children}
        </div>
    );
}
