"use client";

import type { ReactNode } from 'react';
import { Radio, RadioGroup, RadioIndicator } from '@ui-sdk/react/radio';

export interface RadioCardOption<V extends string> {
    value: V;
    label: ReactNode;
    description?: ReactNode;
    /** `error` for the destructive choice; its dot and border use the error colour. */
    tone?: 'accent' | 'error';
    /** Rendered at the card's right edge, e.g. an input shown for the chosen option. */
    trailing?: ReactNode;
}

// A radio group whose options are whole-card click targets. The card chrome is
// the caller's (`cardClassName`) because the two settings screens that use it
// frame the chosen card differently.
export default function RadioCards<V extends string>({
    value,
    onChange,
    options,
    ariaLabel,
    cardClassName,
    className = '',
}: {
    value: V;
    onChange: (v: V) => void;
    options: RadioCardOption<V>[];
    ariaLabel: string;
    cardClassName: (checked: boolean, option: RadioCardOption<V>) => string;
    className?: string;
}) {
    return (
        <RadioGroup
            aria-label={ariaLabel}
            value={value}
            onValueChange={v => { if (v != null) onChange(v as V); }}
            className={className}
        >
            {options.map(o => {
                const checked = o.value === value;
                const tone = o.tone === 'error' ? 'error' : 'accent';
                return (
                    <label key={o.value} className={cardClassName(checked, o)}>
                        <Radio
                            value={o.value}
                            className={`mt-0.5 flex size-4 shrink-0 items-center justify-center rounded-full border border-(--base-05) bg-(--base-01) cursor-pointer focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)] ${tone === 'error' ? 'data-checked:border-(--error)' : 'data-checked:border-(--accent)'}`}
                        >
                            <RadioIndicator className={`size-2 rounded-full ${tone === 'error' ? 'bg-(--error)' : 'bg-(--accent)'}`} />
                        </Radio>
                        <div className="min-w-0 flex-1">
                            <p className="text-sm font-medium text-(--base-09)">{o.label}</p>
                            {o.description && <p className="text-xs text-(--base-06)">{o.description}</p>}
                        </div>
                        {o.trailing}
                    </label>
                );
            })}
        </RadioGroup>
    );
}
