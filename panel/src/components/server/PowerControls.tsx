"use client";

import { useEffect, useRef, useState } from 'react';
import { AlertTriangle, MoreHorizontal, Play, RotateCcw, Square, Trash2, Zap } from 'lucide-react';
import Spinner from '@/components/Spinner';
import { useNow } from '@/lib/useNow';
import { powerModel, type PendingPower, type PowerAction, type PowerGates } from '@/lib/powerControls';

// One primary button that becomes its own progress indicator, Restart beside
// it, and Force kill behind an overflow menu so it is never the button next
// to Stop. Delete (admins) sits in the same menu under a divider: a red button
// in the header was one slip away from the power buttons, and its confirm
// popup is where the red belongs. The state machine lives in
// lib/powerControls; this only draws it.
//
// The primary uses aria-disabled rather than disabled while busy: a disabled
// button drops hover in some browsers, and its title is the one place that
// explains why a stop takes half a minute.

const BASE = 'focus-ring inline-flex items-center gap-1.5 h-8 px-3 rounded-md border text-xs font-semibold transition-colors';
const LOCKED = 'bg-(--base-02) border-(--base-03) text-(--base-05) cursor-not-allowed';

const PRIMARY: Record<'start' | 'stop' | 'busy', string> = {
    start: 'bg-(--success) border-(--success) text-white hover:bg-(--success-light) hover:border-(--success-light) active:bg-(--success)',
    stop: 'bg-(--base-03) border-(--base-05) text-(--base-09) hover:bg-(--base-04) active:bg-(--base-03)',
    busy: 'bg-(--base-03) border-(--base-04) text-(--base-08) cursor-progress',
};
const GHOST = 'bg-transparent border-(--base-04) text-(--base-08) hover:bg-(--base-03) hover:text-(--base-09) active:bg-(--base-04) disabled:bg-transparent disabled:border-(--base-03) disabled:text-(--base-05) disabled:cursor-not-allowed';
const ITEM = 'focus-ring w-full flex items-start gap-2.5 px-2.5 py-2 rounded-md text-left transition-colors disabled:hover:bg-transparent disabled:cursor-not-allowed group';

export default function PowerControls({
    status, pending, gates, onAction, onDelete,
}: {
    status: string;
    pending: PendingPower | null;
    gates: PowerGates;
    onAction: (action: PowerAction) => void;
    // Admin only; without it the menu has no Delete entry.
    onDelete?: () => void;
}) {
    // Ticks the elapsed seconds, the 90 s timeout and the post-action pause.
    // Only this component re-renders.
    const now = useNow(1000);
    const m = powerModel(status, pending, now, { ...gates, canDelete: !!onDelete });
    const [menuOpen, setMenuOpen] = useState(false);
    const wrapRef = useRef<HTMLDivElement>(null);
    const triggerRef = useRef<HTMLButtonElement>(null);
    const menuRef = useRef<HTMLDivElement>(null);

    const menuUsable = m.showMenu && !m.menuDisabled;
    useEffect(() => { if (!menuUsable) setMenuOpen(false); }, [menuUsable]);

    useEffect(() => {
        if (!menuOpen) return;
        // The first item that can be used: Force kill is disabled on a stopped
        // server, and a disabled button cannot take focus.
        const items = () => Array.from(menuRef.current?.querySelectorAll<HTMLButtonElement>('[role="menuitem"]:not(:disabled)') ?? []);
        items()[0]?.focus();
        const onDown = (e: MouseEvent) => {
            if (!wrapRef.current?.contains(e.target as Node)) setMenuOpen(false);
        };
        const onKey = (e: KeyboardEvent) => {
            if (e.key === 'Escape') {
                setMenuOpen(false);
                triggerRef.current?.focus();
            } else if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
                const list = items();
                if (list.length === 0) return;
                e.preventDefault();
                const at = list.indexOf(document.activeElement as HTMLButtonElement);
                const step = e.key === 'ArrowDown' ? 1 : -1;
                list[(at + step + list.length) % list.length].focus();
            }
        };
        document.addEventListener('mousedown', onDown);
        document.addEventListener('keydown', onKey);
        return () => {
            document.removeEventListener('mousedown', onDown);
            document.removeEventListener('keydown', onKey);
        };
    }, [menuOpen]);

    const p = m.primary;
    const primaryIcon = p.icon === 'start' ? <Play size={15} />
        : p.icon === 'stop' ? <Square size={14} />
        : p.icon === 'stale' ? <AlertTriangle size={15} className="text-(--warning)" />
        : <Spinner size={15} className="motion-reduce:animate-none" />;

    return (
        <div ref={wrapRef} className="relative flex items-center gap-1.5" aria-busy={m.busy}>
            <button
                type="button"
                aria-disabled={p.disabled}
                onClick={() => { if (!p.disabled && p.action) onAction(p.action); }}
                title={p.title}
                className={`${BASE} min-w-[5.5rem] justify-center tabular-nums ${
                    p.variant === 'busy' ? PRIMARY.busy : p.disabled ? LOCKED : PRIMARY[p.variant]
                }`}
            >
                {primaryIcon}
                <span>{p.label}</span>
            </button>
            {m.showSecondary && (
                <button
                    type="button"
                    onClick={() => onAction('restart')}
                    disabled={m.restartDisabled}
                    title={m.restartDisabled && !m.busy ? p.title : 'Restart server'}
                    className={`${BASE} ${GHOST}`}
                >
                    <RotateCcw size={14} />
                    <span>Restart</span>
                </button>
            )}
            {m.showMenu && (
                <button
                    ref={triggerRef}
                    type="button"
                    onClick={() => setMenuOpen(v => !v)}
                    disabled={m.menuDisabled}
                    aria-haspopup="menu"
                    aria-expanded={menuOpen}
                    aria-label="More actions"
                    title="More actions"
                    className={`${BASE} w-8 px-0 justify-center ${GHOST} ${menuOpen ? 'bg-(--base-03) text-(--base-09)' : ''}`}
                >
                    <MoreHorizontal size={16} />
                </button>
            )}
            {menuOpen && (
                <div ref={menuRef} role="menu" aria-label="More actions" className="dropdown-menu right-0 top-full mt-1.5 w-64 animate-fade-in motion-reduce:animate-none">
                    <button
                        type="button"
                        role="menuitem"
                        disabled={m.killDisabled}
                        onClick={() => { setMenuOpen(false); onAction('kill'); }}
                        className={`${ITEM} hover:bg-(--error-ghost) active:bg-(--error-ghost)`}
                    >
                        <Zap size={15} className="mt-0.5 shrink-0 text-(--error-light) group-disabled:text-(--base-05)" />
                        <span className="min-w-0">
                            <span className="block text-sm font-medium text-(--error-light) group-disabled:text-(--base-05)">Force kill</span>
                            <span className="block text-xs text-(--base-06) leading-snug">
                                {m.killDisabled && gates.killCooldown
                                    ? 'Available again a minute after the last kill'
                                    : 'Stops the server immediately without saving'}
                            </span>
                        </span>
                    </button>
                    {onDelete && (
                        <>
                            <div role="separator" className="my-1.5 h-px bg-(--base-03)" />
                            <button
                                type="button"
                                role="menuitem"
                                disabled={m.deleteDisabled}
                                title={m.deleteDisabled ? 'Upload in progress. Wait or cancel it first.' : undefined}
                                onClick={() => { setMenuOpen(false); onDelete(); }}
                                className={`${ITEM} hover:bg-(--base-03) active:bg-(--base-04)`}
                            >
                                <Trash2 size={15} className="mt-0.5 shrink-0 text-(--base-07) group-disabled:text-(--base-05)" />
                                <span className="min-w-0">
                                    <span className="block text-sm font-medium text-(--base-09) group-disabled:text-(--base-05)">Delete server</span>
                                    <span className="block text-xs text-(--base-06) leading-snug">Removes the server and all its data, after a confirmation</span>
                                </span>
                            </button>
                        </>
                    )}
                </div>
            )}
            <span className="sr-only" aria-live="polite">{m.announcement}</span>
        </div>
    );
}
