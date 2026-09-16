"use client";

import { ShieldCheck, ShieldAlert, ShieldQuestion } from 'lucide-react';
import HelpTip from '@/components/ui/HelpTip';
import type { LinkState } from '@/lib/linkState';

const ICON = {
    ok: ShieldCheck,
    bad: ShieldAlert,
    unknown: ShieldQuestion,
} as const;

const TONE = {
    ok: 'text-(--success-light)',
    bad: 'text-(--error-light)',
    unknown: 'text-(--base-06)',
} as const;

/**
 * Whether a Link is carrying players right now, for one row.
 *
 * Icon plus a help tip rather than a badge with words: these rows are dense and
 * the sentence that matters ("the machine is online and its link is not, so
 * nobody can join") is a sentence, not a label. The label still reaches a screen
 * reader through the tip's own button text.
 */
export default function LinkBadge({ state, className = '' }: { state: LinkState; className?: string }) {
    const Icon = ICON[state.tone];
    return (
        <span className={`inline-flex items-center gap-1 shrink-0 ${className}`}>
            <Icon size={14} className={TONE[state.tone]} aria-hidden />
            <HelpTip label={state.label}>
                <strong>{state.label}.</strong> {state.hint}
            </HelpTip>
        </span>
    );
}
