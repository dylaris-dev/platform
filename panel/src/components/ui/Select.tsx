"use client";

import { useMemo } from 'react';
import { ChevronDown, Check } from 'lucide-react';
import {
    Select as SdkSelect,
    SelectContent,
    SelectEmpty,
    SelectInput,
    SelectItem,
    SelectItemIndicator,
    SelectItemText,
    SelectList,
    SelectTrigger,
    SelectValue,
} from '@ui-sdk/react/select';

export interface SelectOption {
    value: string;
    label: string;
    badge?: string;
}

export interface SelectProps {
    value: string;
    onChange: (value: string) => void;
    options: SelectOption[];
    placeholder?: string;
    disabled?: boolean;
    className?: string;
    ariaLabel?: string;
    /** A filter field at the top of the open list, for long lists (time zones). */
    searchable?: boolean;
    id?: string;
}

// The UI SDK select reads '' as "nothing selected", but callers use '' as a real
// option ("same as the owner's account"), so it travels as a sentinel.
const EMPTY = '__empty__';
const toSdk = (v: string) => (v === '' ? EMPTY : v);

// Reusable branded single-select dropdown on the UI SDK Select. The trigger
// reuses `.input-field` so it matches every other input (including the focus
// ring); the popup reuses `.ui-popup` / `.dropdown-item`.
export default function Select({
    value,
    onChange,
    options,
    placeholder = 'Select…',
    disabled = false,
    className = '',
    ariaLabel,
    searchable = false,
    id,
}: SelectProps) {
    const items = useMemo(
        () => options.map(o => ({ value: toSdk(o.value), label: o.label, keywords: [o.value] })),
        [options],
    );
    const byValue = useMemo(() => new Map(options.map(o => [toSdk(o.value), o])), [options]);
    const selected = options.some(o => o.value === value) ? toSdk(value) : null;

    return (
        <div className={`relative ${className}`}>
        <SdkSelect
            items={items}
            filter="contains"
            value={selected}
            onValueChange={v => { if (v != null) onChange(v === EMPTY ? '' : v); }}
            placeholder={placeholder}
            disabled={disabled}
        >
            <SelectTrigger
                id={id}
                aria-label={ariaLabel}
                className={`input-field w-full flex items-center justify-between gap-2 text-left group data-disabled:opacity-40`}
            >
                <SelectValue className="truncate text-(--base-09) data-placeholder:text-(--base-06)" />
                <ChevronDown
                    size={14}
                    className="shrink-0 text-(--base-06) transition-transform group-data-open:rotate-180"
                />
            </SelectTrigger>
            <SelectContent className="ui-popup flex flex-col w-(--anchor-width) max-h-[min(15rem,var(--available-height))] overflow-hidden">
                {searchable && (
                    <div className="p-1.5 border-b border-(--base-03)">
                        <SelectInput
                            placeholder="Search..."
                            aria-label={ariaLabel ? `Search ${ariaLabel}` : 'Search'}
                            className="input-field w-full text-xs py-1"
                        />
                    </div>
                )}
                <SelectEmpty className="hidden data-empty:block px-3 py-2 text-xs text-(--base-06)">Nothing matches.</SelectEmpty>
                <SelectList className="min-h-0 flex-1 overflow-y-auto p-1.5">
                    {state => (state.items as { value: string }[]).map(({ value: sdkValue }) => {
                        const o = byValue.get(sdkValue)!;
                        const isSelected = o.value === value;
                        return (
                            <SelectItem
                                key={sdkValue}
                                value={sdkValue}
                                label={o.label}
                                className={`dropdown-item justify-between gap-2 ${isSelected ? 'text-(--accent-light)' : ''}`}
                            >
                                <SelectItemText className="truncate">{o.label}</SelectItemText>
                                <span className="flex items-center gap-2 shrink-0">
                                    {o.badge && <span className="mono-label">{o.badge}</span>}
                                    <SelectItemIndicator>
                                        <Check size={13} className="text-(--accent-light)" />
                                    </SelectItemIndicator>
                                </span>
                            </SelectItem>
                        );
                    })}
                </SelectList>
            </SelectContent>
        </SdkSelect>
        </div>
    );
}
