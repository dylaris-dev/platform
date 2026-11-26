"use client";

import { AlertTriangle, RefreshCw } from 'lucide-react';
import EmptyState from '@/components/ui/EmptyState';

/**
 * Shown in place of a settings form whose load failed.
 *
 * A form that could not load renders its DEFAULTS, and defaults are
 * indistinguishable from a configuration: no limits, no domains, every toggle
 * off. An operator reading that screen concludes their settings are gone.
 *
 * It replaces the form rather than sitting above it, because the form is not
 * usable either way - dirty is measured against a snapshot that a failed load
 * never sets, so nothing typed into it can be saved and nothing would say so.
 * Forms built on useSettingsForm express the same thing through a blocked save
 * bar; this is for the hand-rolled ones, which have no save bar to block.
 */
export default function SettingsLoadError({ what, onRetry }: {
    /** What could not be loaded, lowercase: "the gateway settings". */
    what: string;
    /** Runs the same load again. The caller returns to its skeleton while it is
     *  out, so this card is never on screen during a retry and needs no busy
     *  state of its own. */
    onRetry: () => void;
}) {
    return (
        <EmptyState
            tone="error"
            icon={<AlertTriangle size={20} />}
            title={`Could not load ${what}`}
            description="Nothing has changed. The form is hidden on purpose: it would show its own defaults, which look exactly like a configuration that was never set."
            action={
                <button type="button" onClick={onRetry} className="btn btn-secondary btn-sm inline-flex items-center gap-1.5">
                    <RefreshCw size={13} /> Try again
                </button>
            }
        />
    );
}
