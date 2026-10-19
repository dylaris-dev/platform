"use client";

import { useState } from 'react';
import { AlertTriangle } from 'lucide-react';
import {
    WIPE_TOKENS, WIPE_LABELS, WIPE_HINTS, recommendedWipe, changeSummary,
    type InstallChange, type WipeToken,
} from '@/lib/installWipe';

/**
 * What to clear before reinstalling.
 *
 * Shown only when the INSTALL actually changes. A dialog that opens on every
 * save is one people learn to click through, and the whole point is that the
 * destructive half is a decision rather than a default.
 *
 * The recommendations are ticked; every one can be unticked. The world is not on
 * the list at all - a destructive default has to be impossible to reach by
 * mis-clicking, not merely unchecked.
 */
export default function WipeChoiceDialog({
    change,
    loaderChanged = false,
    onCancel,
    onConfirm,
}: {
    change: InstallChange;
    loaderChanged?: boolean;
    onCancel: () => void;
    onConfirm: (tokens: WipeToken[]) => void;
}) {
    const [picked, setPicked] = useState<Set<WipeToken>>(() => new Set(recommendedWipe(change, loaderChanged)));
    const recommended = new Set(recommendedWipe(change, loaderChanged));

    const toggle = (t: WipeToken) =>
        setPicked(prev => {
            const next = new Set(prev);
            if (next.has(t)) next.delete(t);
            else next.add(t);
            return next;
        });

    return (
        <div className="modal-overlay animate-fade-in" onClick={onCancel}>
            <div
                className="modal-panel max-w-lg"
                role="dialog"
                aria-modal="true"
                aria-labelledby="wipe-title"
                onClick={e => e.stopPropagation()}
            >
                <h3 id="wipe-title" className="modal-title flex items-center gap-2">
                    <AlertTriangle size={18} className="text-(--warning-light)" aria-hidden="true" />
                    Clear anything first?
                </h3>

                <p className="text-sm text-(--base-07) mt-2">
                    {changeSummary(change)} Files that are not cleared stay where they are and the new
                    install is written on top of them.
                </p>
                <p className="text-xs text-(--base-06) mt-1">
                    Your world is never touched by this, whatever you pick.
                </p>

                <div className="mt-4 space-y-2">
                    {WIPE_TOKENS.map(t => (
                        <label
                            key={t}
                            className="flex items-start gap-3 rounded-md border border-(--base-03) p-3 cursor-pointer hover:border-(--accent)/40 transition-colors"
                        >
                            <input
                                type="checkbox"
                                className="checkbox mt-0.5"
                                checked={picked.has(t)}
                                onChange={() => toggle(t)}
                            />
                            <span className="min-w-0">
                                <span className="text-sm text-(--base-09) flex items-center gap-2">
                                    {WIPE_LABELS[t]}
                                    {recommended.has(t) && (
                                        <span className="badge badge-neutral text-[10px]">recommended</span>
                                    )}
                                </span>
                                <span className="block text-xs text-(--base-06) mt-0.5">{WIPE_HINTS[t]}</span>
                            </span>
                        </label>
                    ))}
                </div>

                <div className="flex gap-2 mt-5">
                    <button type="button" className="btn btn-secondary flex-1" onClick={onCancel}>
                        Cancel
                    </button>
                    <button
                        type="button"
                        className="btn btn-primary flex-1"
                        onClick={() => onConfirm([...picked])}
                    >
                        {picked.size === 0
                            ? 'Install without clearing'
                            : `Clear ${picked.size} and install`}
                    </button>
                </div>
            </div>
        </div>
    );
}
