'use client';

import { SegmentedControl, SegmentedControlItem } from '@ui-sdk/react/segmented-control';

/**
 * A small inline choice between a handful of mutually exclusive options.
 *
 * Not Tabs: Tabs is page-level sub-navigation and swaps what is on screen.
 * This is a form control that changes a parameter of what is already on screen,
 * so it sits next to a label like a select does and is a radio group (the UI SDK
 * Segmented Control) rather than a tablist.
 */
export interface SegmentedOption<Id extends string = string> {
    id: Id;
    label: string;
    /** Shown as the option's title; use it for the "what does this mean" line. */
    hint?: string;
    disabled?: boolean;
}

export default function Segmented<Id extends string = string>({
    options,
    value,
    onChange,
    ariaLabel,
    className = '',
}: {
    options: SegmentedOption<Id>[];
    value: Id;
    onChange: (id: Id) => void;
    ariaLabel: string;
    className?: string;
}) {
    return (
        <SegmentedControl
            aria-label={ariaLabel}
            value={value}
            onValueChange={v => { if (v != null) onChange(v as Id); }}
            className={`inline-flex items-center gap-1 p-1 rounded-md bg-(--base-02) border border-(--base-04) ${className}`}
        >
            {options.map(({ id, label, hint, disabled }) => (
                <SegmentedControlItem
                    key={id}
                    value={id}
                    title={hint}
                    disabled={disabled}
                    className="cursor-pointer rounded px-2.5 py-1 text-xs transition-colors focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)] data-disabled:opacity-40 data-disabled:cursor-not-allowed data-checked:bg-(--accent) data-checked:text-(--base-00) data-unchecked:text-(--base-07) data-unchecked:not-data-disabled:hover:bg-(--base-03) data-unchecked:not-data-disabled:hover:text-(--base-09)"
                >
                    {label}
                </SegmentedControlItem>
            ))}
        </SegmentedControl>
    );
}
