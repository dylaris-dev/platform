'use client';

import { CheckCircle2, AlertTriangle, XCircle, Loader2, Cable } from 'lucide-react';
import type { ConnectionTest, ConnTestResult } from '@/lib/connectionTest';

/**
 * The button and the banner that every connection test in the panel uses.
 *
 * They are one file because they are one thing: a button whose result appears
 * somewhere. Splitting them let call sites take the button and forget the
 * banner, which is how three of these screens ended up with a spinner that
 * finished and told nobody what it found.
 *
 * The button is disabled while the request is in flight - not to prevent a
 * second request, which the hook already refuses, but because a control that
 * still looks pressable is a control that gets pressed, and the operator then
 * has no idea which answer belongs to which click.
 */

export interface TestConnectionButtonProps {
    test: ConnectionTest;
    /** Reason the test cannot run at all. Disables the button and explains itself. */
    blockedReason?: string | null;
    label?: string;
    runningLabel?: string;
    /** btn-sm in a card header, the default size in a form's action row. */
    small?: boolean;
    className?: string;
}

export function TestConnectionButton({
    test, blockedReason, label = 'Test connection', runningLabel = 'Testing…', small, className = '',
}: TestConnectionButtonProps) {
    const disabled = test.running || !!blockedReason;
    return (
        <button
            type="button"
            onClick={test.run}
            disabled={disabled}
            aria-busy={test.running}
            title={blockedReason ?? 'Open a connection and report what comes back'}
            className={`btn btn-secondary ${small ? 'btn-sm' : ''} inline-flex items-center gap-1.5 ` +
                `disabled:opacity-40 disabled:cursor-not-allowed ${className}`}
        >
            {test.running
                ? <Loader2 size={small ? 12 : 14} className="animate-spin" />
                : <Cable size={small ? 12 : 14} />}
            {test.running ? runningLabel : label}
        </button>
    );
}

/**
 * The last result.
 *
 * Three severities, not two. "Connected, but the extension is missing" is
 * neither a failure nor a clean pass: it works, and it will hurt later, which
 * is precisely the state a green tick would hide.
 *
 * The heading carries the finding - "Not reachable" against "Reached, but
 * refused" - because that is the part an operator reads before deciding whose
 * problem this is. The sentence below it says what to change.
 */
export function ConnectionTestNote({ result }: { result: ConnTestResult | null }) {
    if (!result) return null;
    const tone = {
        ok: { Icon: CheckCircle2, cls: 'border-(--success-border) bg-(--success-ghost) text-(--success-light)' },
        warning: { Icon: AlertTriangle, cls: 'border-(--warning-border) bg-(--warning-ghost) text-(--warning-light)' },
        error: { Icon: XCircle, cls: 'border-(--error-border) bg-(--error-ghost) text-(--error-light)' },
    }[result.severity];
    const { Icon, cls } = tone;
    return (
        <div role="status" aria-live="polite" className={`flex items-start gap-2 rounded-md border p-3 ${cls}`}>
            <Icon size={14} className="mt-0.5 shrink-0" />
            <div className="min-w-0">
                <p className="text-xs font-medium">{result.heading}</p>
                <p className="text-xs leading-relaxed opacity-90">{result.message}</p>
            </div>
        </div>
    );
}
