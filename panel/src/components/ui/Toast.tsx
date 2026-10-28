'use client';

import { useEffect, useState, useRef } from 'react';
import { CircleAlert, CircleCheck, Copy, X } from 'lucide-react';
import { advanceToastClock, toastDuration, toastOpacity } from './toastTiming';

/**
 * One toast host for the whole panel.
 *
 * There were 31 of these before, each a local `useState` plus its own copy of
 * the markup, with five different shapes of state and dismiss timeouts ranging
 * from 2800ms to 4500ms. The same action reported differently depending on
 * which screen you triggered it from, and six autosaving controls reported
 * nothing at all because their file had no toast to begin with.
 *
 * Deliberately a module-level singleton driven by one <ToastRoot/>, the same
 * shape as confirmDialog(): a call site changes by an import instead of also
 * having to own state and render a node. Fails SILENT rather than closed - a
 * missing host costs a message, and refusing the action the message describes
 * would be the worse trade.
 */

export interface ToastOptions {
    /** false renders the error treatment. Defaults to true. */
    ok?: boolean;
    /** Requested lifetime before the global 35% reduction; legacy minimums still apply. */
    durationMs?: number;
}

interface ToastItem extends Required<ToastOptions> {
    id: number;
    message: string;
}

let publish: ((t: ToastItem) => void) | null = null;
let nextId = 1;

/**
 * toast shows a message. The second argument is the ok flag rather than an
 * options object because that is the signature the ~31 call sites already used
 * (`showToast(msg, ok = true)`), so migrating them is a rename.
 */
export function toast(message: string, ok: boolean = true, opts: ToastOptions = {}): void {
    if (!publish) {
        // Not an error worth throwing: the message is the least important part
        // of whatever just succeeded.
        console.warn('toast() called with no <ToastRoot/> mounted:', message);
        return;
    }
    publish({
        id: nextId++,
        message,
        ok: opts.ok ?? ok,
        durationMs: toastDuration(opts.ok ?? ok, opts.durationMs),
    });
}

/** Mount once, above every screen that can report something. */
export function ToastRoot() {
    const [items, setItems] = useState<ToastItem[]>([]);

    useEffect(() => {
        publish = (t: ToastItem) => setItems((prev) => [...prev, t]);
        return () => {
            publish = null;
        };
    }, []);

    return (
        // Always mounted, even when empty: a live region has to exist in the DOM
        // BEFORE the mutation for assistive tech to announce it reliably. It
        // renders nothing and is pointer-events:none while empty.
        //
        // polite, not assertive: these announce a completed action, and
        // interrupting a screen reader mid-sentence for "Saved" is worse than
        // waiting for the pause.
        <div className="toast-container" role="status" aria-live="polite">
            {items.map((item) => (
                <ToastCard
                    key={item.id}
                    item={item}
                    onDismiss={() => setItems((prev) => prev.filter((i) => i.id !== item.id))}
                />
            ))}
        </div>
    );
}

function ToastCard({ item, onDismiss }: { item: ToastItem; onDismiss: () => void }) {
    const flags = useRef({ hovered: false, focused: false });
    const clock = useRef({ elapsed: 0, at: performance.now(), paused: false });
    const [elapsed, setElapsed] = useState(0);
    const [paused, setPaused] = useState(false);
    const [leaving, setLeaving] = useState(false);
    const [copied, setCopied] = useState(false);
    const [copyError, setCopyError] = useState(false);
    const dismiss = useRef(onDismiss);
    dismiss.current = onDismiss;
    const pause = (kind: 'hovered' | 'focused', value: boolean) => {
        flags.current[kind] = value;
        const next = flags.current.hovered || flags.current.focused;
        clock.current = advanceToastClock(clock.current, performance.now(), item.durationMs, next);
        setElapsed(clock.current.elapsed);
        setPaused(next);
    };
    useEffect(() => {
        const timer = setInterval(() => {
            clock.current = advanceToastClock(clock.current, performance.now(), item.durationMs);
            setElapsed(clock.current.elapsed);
        }, 40);
        return () => clearInterval(timer);
    }, [item.durationMs]);
    const expired = elapsed >= item.durationMs;
    useEffect(() => {
        if (!expired || paused) { setLeaving(false); return; }
        setLeaving(true);
        const timer = setTimeout(() => dismiss.current(), 200);
        return () => clearTimeout(timer);
    }, [expired, paused]);
    const copy = async () => {
        try {
            await navigator.clipboard.writeText(item.message);
            setCopied(true);
            setCopyError(false);
        } catch {
            setCopyError(true);
        }
    };

    return (
        <div
            className="toast toast-dismissable toast-copyable"
            data-duration-ms={item.durationMs}
            data-elapsed-ms={Math.round(elapsed)}
            style={{ opacity: paused ? 1 : leaving ? 0 : toastOpacity(elapsed, item.durationMs) }}
            onMouseEnter={() => pause('hovered', true)}
            onMouseLeave={() => pause('hovered', false)}
            onFocusCapture={(event) => {
                if ((event.target as HTMLElement).matches(':focus-visible')) pause('focused', true);
            }}
            onBlurCapture={(event) => {
                if (!event.currentTarget.contains(event.relatedTarget)) pause('focused', false);
            }}
        >
            <svg className="toast-timer" aria-hidden="true">
                <rect
                    x="1"
                    y="1"
                    width="calc(100% - 2px)"
                    height="calc(100% - 2px)"
                    rx="12"
                    pathLength="1"
                    style={{
                        strokeDashoffset: elapsed / item.durationMs,
                        stroke: item.ok ? 'var(--success-light)' : 'var(--error-light)',
                    }}
                />
            </svg>
            <button
                type="button"
                onClick={copy}
                className="flex items-center gap-2.5 min-w-0 flex-1 text-left"
                title="Click to copy message"
            >
                <div className={`toast-bar ${item.ok ? 'bg-(--success-light)' : 'bg-(--error-light)'}`} />
                {item.ok ? (
                    <CircleCheck size={14} className="text-(--success-light) shrink-0" />
                ) : (
                    <CircleAlert size={14} className="text-(--error-light) shrink-0" />
                )}
                <span className="min-w-0">
                    <span className="text-sm text-(--base-09) block break-words">{item.message}</span>
                    <span className="text-[10px] text-(--base-06) block mt-1">
                        {copyError
                            ? 'Copy unavailable. Select and copy the message manually.'
                            : copied
                              ? 'Copied to clipboard'
                              : 'Click to copy'}{' '}
                        <Copy size={10} className="inline" />
                    </span>
                </span>
            </button>
            <button
                type="button"
                onClick={onDismiss}
                className="btn btn-ghost btn-sm shrink-0"
                aria-label="Dismiss message"
            >
                <X size={13} />
            </button>
        </div>
    );
}
