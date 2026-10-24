"use client";

import React, { useEffect, useRef, useState } from 'react';
import { AlertTriangle, Check, Copy } from 'lucide-react';

/**
 * Text that copies itself when clicked, and says so.
 *
 * The confirmation is inline rather than a toast on purpose: a toast for "copied"
 * appears in a different corner from the thing that was clicked, so on a list of
 * several copyable values it does not say WHICH one was taken. The check mark
 * sits on the row the operator pressed.
 *
 * A button, not a div with onClick, so it is reachable by keyboard and announced
 * as an action rather than read out as decoration.
 */
export default function CopyText({
    value,
    label,
    className = '',
    title,
    bubble = false,
}: {
    value: string;
    /** What is rendered. Defaults to the value itself. */
    label?: React.ReactNode;
    className?: string;
    title?: string;
    /**
     * For a chip: no trailing icon (whose reserved space read as an empty gap
     * in a bordered chip); "Click to copy" and "Copied" appear in a small
     * bubble beside it instead.
     */
    bubble?: boolean;
}) {
    const [state, setState] = useState<'idle' | 'copied' | 'failed'>('idle');
    const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

    // Cleared on unmount: the row can disappear while the confirmation is still
    // running (deleting the route you just copied), and a timer firing into a
    // gone component is a warning in the console for no benefit.
    useEffect(() => () => { if (timer.current) clearTimeout(timer.current); }, []);

    const copy = async () => {
        // A denied permission or an insecure origin leaves the clipboard API
        // missing or throwing. Reported, not swallowed: silence here looks
        // exactly like success, and the operator pastes whatever was in the
        // clipboard before - which for a domain field is a value that looks
        // plausible and is wrong.
        let ok = false;
        try {
            await navigator.clipboard.writeText(value);
            ok = true;
        } catch {
            ok = false;
        }
        setState(ok ? 'copied' : 'failed');
        if (timer.current) clearTimeout(timer.current);
        timer.current = setTimeout(() => setState('idle'), ok ? 1600 : 3000);
    };

    if (bubble) {
        const tip = state === 'copied' ? 'Copied' : state === 'failed' ? 'Copy blocked, select it by hand' : (title ?? 'Click to copy');
        return (
            <button
                type="button"
                onClick={copy}
                aria-label={`Copy ${value}`}
                className={`group relative inline-flex items-center text-left rounded transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring) ${className}`}
            >
                <span className="truncate">{label ?? value}</span>
                <span
                    role={state === 'idle' ? undefined : 'status'}
                    aria-live="polite"
                    className={`pointer-events-none absolute left-full ml-2 top-1/2 -translate-y-1/2 z-10 whitespace-nowrap rounded-md border px-2 py-0.5 text-[11px] font-sans shadow-md transition-opacity duration-150 motion-reduce:transition-none ${
                        state === 'copied' ? 'opacity-100 bg-(--base-02) border-(--success)/40 text-(--success-light)'
                            : state === 'failed' ? 'opacity-100 bg-(--base-02) border-(--error-border) text-(--error-light)'
                            : 'opacity-0 group-hover:opacity-100 group-focus-visible:opacity-100 bg-(--base-02) border-(--base-04) text-(--base-08)'
                    }`}
                >
                    {state === 'copied' && <Check size={11} aria-hidden="true" className="inline mr-1 -mt-px" />}
                    {tip}
                </span>
            </button>
        );
    }

    return (
        <button
            type="button"
            onClick={copy}
            title={title ?? `Copy ${value}`}
            aria-label={`Copy ${value}`}
            className={`group inline-flex items-center gap-1.5 text-left rounded transition-colors hover:text-(--accent-light) focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring) ${className}`}
        >
            <span className="truncate">{label ?? value}</span>
            {/* Announced, not just shown: the only feedback this control gives is
                a word that appears beside it. */}
            {state === 'copied' ? (
                <span role="status" aria-live="polite" className="inline-flex items-center gap-1 text-[11px] text-(--success-light) shrink-0">
                    <Check size={12} aria-hidden="true" /> copied
                </span>
            ) : state === 'failed' ? (
                <span role="status" aria-live="polite" className="inline-flex items-center gap-1 text-[11px] text-(--error-light) shrink-0">
                    <AlertTriangle size={12} aria-hidden="true" /> copy blocked, select it by hand
                </span>
            ) : (
                <Copy
                    size={12}
                    aria-hidden="true"
                    className="shrink-0 text-(--base-06) opacity-0 group-hover:opacity-100 group-focus-visible:opacity-100 transition-opacity"
                />
            )}
        </button>
    );
}
