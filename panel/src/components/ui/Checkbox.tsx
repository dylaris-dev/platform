'use client';

import React from 'react';
import { Checkbox as SdkCheckbox } from '@ui-sdk/react/checkbox';

/**
 * The panel's checkbox.
 *
 * Before this there were 22 files with a raw, unstyled checkbox input and no
 * styling at all, so they rendered as OS chrome on a dark panel and the cursor
 * changed halfway across the row. The look lives in globals.css (.checkbox);
 * this component exists so the label, the disabled treatment and the
 * label-is-part-of-the-control wiring cannot drift between call sites.
 *
 * Use a Checkbox for a value inside a form or a multi-select. Use a Switch for
 * a single thing being on or off - that distinction is the whole reason both
 * exist, and mixing them is what made the settings pages look assembled from
 * parts.
 */
export interface CheckboxProps {
    checked: boolean;
    onChange: (checked: boolean) => void;
    /** Omit for a bare box, e.g. the leading column of a table row. */
    label?: React.ReactNode;
    /** Secondary line under the label. */
    hint?: React.ReactNode;
    disabled?: boolean;
    /** Needed when there is no visible label. */
    ariaLabel?: string;
    className?: string;
    id?: string;
}

export default function Checkbox({
    checked,
    onChange,
    label,
    hint,
    disabled = false,
    ariaLabel,
    className = '',
    id,
}: CheckboxProps) {
    const input = (
        <SdkCheckbox
            id={id}
            className="checkbox"
            checked={checked}
            onCheckedChange={c => onChange(c === true)}
            disabled={disabled}
            aria-label={label ? undefined : ariaLabel}
        />
    );

    if (!label) {
        return <span className={className}>{input}</span>;
    }

    return (
        <label className={`checkbox-row ${disabled ? 'is-disabled' : ''} ${className}`}>
            {input}
            <span className="min-w-0">
                <span className="text-sm text-(--base-09)">{label}</span>
                {hint && <span className="block text-xs text-(--base-06) mt-0.5">{hint}</span>}
            </span>
        </label>
    );
}
