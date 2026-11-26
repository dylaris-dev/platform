'use client';

import React from 'react';
import { OTPInput, OTPInputGroup, OTPInputSlot } from '@ui-sdk/react/otp-input';

/**
 * Six-digit authenticator code entry (enrollment forms). Digits only, so it is
 * NOT for fields that also accept a backup code. One real input underneath, so
 * autofill, "one-time-code" suggestions, paste and form submit keep working.
 */
export default function OtpInput({
    value,
    onChange,
    id,
    ariaLabel = 'Authenticator code',
    disabled,
    maxLength = 6,
    className = '',
}: {
    value: string;
    onChange: (value: string) => void;
    id?: string;
    ariaLabel?: string;
    disabled?: boolean;
    maxLength?: number;
    className?: string;
}) {
    return (
        <OTPInput
            id={id}
            aria-label={ariaLabel}
            value={value}
            onValueChange={onChange}
            maxLength={maxLength}
            pattern="digits"
            inputMode="numeric"
            autoComplete="one-time-code"
            disabled={disabled}
            className={className}
        >
            <OTPInputGroup className="flex justify-center gap-2">
                {Array.from({ length: maxLength }, (_, i) => (
                    <OTPInputSlot
                        key={i}
                        index={i}
                        className="relative flex h-11 w-9 items-center justify-center rounded-md border border-(--base-05) bg-(--base-03) font-mono text-lg text-(--base-09) transition-colors data-active:border-(--accent) data-active:ring-2 data-active:ring-(--accent-ghost) data-disabled:opacity-40"
                    />
                ))}
            </OTPInputGroup>
        </OTPInput>
    );
}
