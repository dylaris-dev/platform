"use client";

import { useEffect, useRef, useState } from 'react';
import { AlertTriangle, MoreHorizontal, Play, RotateCcw, Square, Zap } from 'lucide-react';
import Spinner from '@/components/Spinner';
import { useNow } from '@/lib/useNow';
import { powerModel, type PendingPower, type PowerAction, type PowerGates } from '@/lib/powerControls';

// One primary button that becomes its own progress indicator, Restart beside
// it, and Force kill behind an overflow menu so it is never the button next
// to Stop. The state machine lives in lib/powerControls; this only draws it.
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

export default function PowerControls({
    status, pending, gates, onAction,
}: {
    status: string;
    pending: PendingPower | null;
    gates: PowerGates;
    onAction: (action: PowerAction) => void;
}) {
    // Ticks the elapsed seconds and the 90 s timeout. Only this component re-renders.
    const now = useNow(1000);
    const m = powerModel(status, pending, now, gates);
    const [menuOpen, setMenuOpen] = useState(false);
    const wrapRef = useRef<HTMLDivElement>(null);
    const triggerRef = useRef<HTMLButtonElement>(null);
    const itemRef = useRef<HTMLButtonElement>(null);

    const menuUsable = m.showSecondary && !m.menuDisabled;
    useEffect(() => { if (!menuUsable) setMenuOpen(false); }, [menuUsable]);

    useEffect(() => {
        if (!menuOpen) return;
        itemRef.current?.focus();
        const onDown = (e: MouseEvent) => {
            if (!wrapRef.current?.contains(e.target as Node)) setMenuOpen(false);
        };
        const onKey = (e: KeyboardEvent) => {
            if (e.key === 'Escape') {
                setMenuOpen(false);
                triggerRef.current?.focus();
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
                <>
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
                    <button
                        ref={triggerRef}
                        type="button"
                        onClick={() => setMenuOpen(v => !v)}
                        disabled={m.menuDisabled}
                        aria-haspopup="menu"
                        aria-expanded={menuOpen}
                        aria-label="More power actions"
                        title="More power actions"
                        className={`${BASE} w-8 px-0 justify-center ${GHOST} ${menuOpen ? 'bg-(--base-03) text-(--base-09)' : ''}`}
                    >
                        <MoreHorizontal size={16} />
                    </button>
                </>
            )}
            {menuOpen && (
                <div role="menu" aria-label="More power actions" className="dropdown-menu right-0 top-full mt-1.5 w-64 animate-fade-in motion-reduce:animate-none">
                    <button
                        ref={itemRef}
                        type="button"
                        role="menuitem"
                        disabled={m.killDisabled}
                        onClick={() => { setMenuOpen(false); onAction('kill'); }}
                        className="focus-ring w-full flex items-start gap-2.5 px-2.5 py-2 rounded-md text-left transition-colors hover:bg-(--error-ghost) active:bg-(--error-ghost) disabled:hover:bg-transparent disabled:cursor-not-allowed group"
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
                </div>
            )}
            <span className="sr-only" aria-live="polite">{m.announcement}</span>
        </div>
    );
}
