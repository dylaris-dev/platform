'use client';

import { Field, FieldLabel } from '@ui-sdk/react/field';
import { NumberInput, NumberInputControl, NumberInputGroup } from '@ui-sdk/react/number-input';

/**
 * A labelled number input on the UI SDK Number Input. An emptied field reports
 * 0 so callers keep a plain number; use `min` to say what an empty means.
 */
export default function NumberField({ label, value, onChange, min, max, step, largeStep, placeholder, disabled, hint, className = '' }: {
    label: string;
    value: number;
    onChange: (value: number) => void;
    min?: number;
    max?: number;
    /** Arrow step and snap grid; keep it fine, a coarse one rounds typed values. */
    step?: number;
    largeStep?: number;
    placeholder?: string;
    disabled?: boolean;
    hint?: string;
    className?: string;
}) {
    return (
        <Field disabled={disabled} className={`flex flex-col gap-1.5 ${className}`}>
            <FieldLabel className="srv-label">{label}</FieldLabel>
            <NumberInput value={value} onValueChange={v => onChange(v ?? 0)} min={min} max={max} step={step} largeStep={largeStep} format={{ useGrouping: false }}>
                <NumberInputGroup>
                    <NumberInputControl placeholder={placeholder} className="input-field w-full srv-num" />
                </NumberInputGroup>
            </NumberInput>
            {hint && <p className="text-xs text-(--base-06)">{hint}</p>}
        </Field>
    );
}
