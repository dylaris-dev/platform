"use client";

import { useEffect, useState } from 'react';
import { AlertTriangle, MoreHorizontal, Play, RotateCcw, Square, Trash2, Zap } from 'lucide-react';
import { Menu, MenuContent, MenuItem, MenuSeparator, MenuTrigger } from '@ui-sdk/react/menu';
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

const BASE = 'focus-ring inline-flex items-center gap-2 h-9 rounded-md border text-[13px] font-medium transition-colors duration-150 ease-out';
const LOCKED = 'bg-(--base-02) border-(--base-03) text-(--base-05) cursor-not-allowed';

const PRIMARY: Record<'start' | 'stop' | 'busy', string> = {
    start: 'bg-(--accent) border-(--accent) text-white hover:bg-(--accent-light) hover:border-(--accent-light) active:bg-(--accent)',
    stop: 'bg-(--base-03) border-(--base-03) text-(--base-09) hover:bg-(--base-04) hover:border-(--base-04) active:bg-(--base-03)',
    busy: 'bg-(--base-03) border-(--base-03) text-(--base-08) cursor-progress',
};
const GHOST = 'bg-transparent border-(--base-03) text-(--base-08) hover:bg-(--base-03) hover:text-(--base-09) active:bg-(--base-04) disabled:bg-transparent disabled:border-(--base-03) disabled:text-(--base-05) disabled:cursor-not-allowed';
const ITEM = 'focus-ring w-full flex items-start gap-2.5 px-2.5 py-2 rounded-md text-left transition-colors data-disabled:hover:bg-transparent data-disabled:cursor-not-allowed group';

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

    const menuUsable = m.showMenu && !m.menuDisabled;
    useEffect(() => { if (!menuUsable) setMenuOpen(false); }, [menuUsable]);

    const p = m.primary;
    const primaryIcon = p.icon === 'start' ? <Play size={15} />
        : p.icon === 'stop' ? <Square size={14} />
        : p.icon === 'stale' ? <AlertTriangle size={15} className="text-(--warning)" />
        : <Spinner size={15} className="motion-reduce:animate-none" />;

    return (
        <div className="relative flex items-center gap-1.5" aria-busy={m.busy}>
            <button
                type="button"
                aria-disabled={p.disabled}
                onClick={() => { if (!p.disabled && p.action) onAction(p.action); }}
                title={p.title}
                className={`${BASE} px-4 min-w-[6rem] justify-center tabular-nums ${
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
                    title={m.restartTitle}
                    className={`${BASE} px-4 ${GHOST}`}
                >
                    <RotateCcw size={14} />
                    <span>Restart</span>
                </button>
            )}
            {m.showMenu && (
                <Menu open={menuOpen} onOpenChange={setMenuOpen}>
                    <MenuTrigger
                        disabled={m.menuDisabled}
                        aria-label="More actions"
                        title="More actions"
                        className={`${BASE} w-9 justify-center ${GHOST} ${menuOpen ? 'bg-(--base-03) text-(--base-09)' : ''}`}
                    >
                        <MoreHorizontal size={16} />
                    </MenuTrigger>
                    <MenuContent side="bottom" align="end" sideOffset={6} className="ui-popup p-1.5 w-64">
                        {m.showKill && (
                            <MenuItem
                                disabled={m.killDisabled}
                                onSelect={() => onAction('kill')}
                                className={`${ITEM} hover:bg-(--error-ghost) data-highlighted:bg-(--error-ghost) active:bg-(--error-ghost)`}
                            >
                                <Zap size={15} className="mt-0.5 shrink-0 text-(--error-light) group-data-disabled:text-(--base-05)" />
                                <span className="min-w-0">
                                    <span className="block text-sm font-medium text-(--error-light) group-data-disabled:text-(--base-05)">Force kill</span>
                                    <span className="block text-xs text-(--base-06) leading-snug">
                                        {m.killDisabled && gates.killCooldown
                                            ? 'Available again a minute after the last kill'
                                            : 'Stops the server immediately without saving'}
                                    </span>
                                </span>
                            </MenuItem>
                        )}
                        {onDelete && (
                            <>
                                {m.showKill && <MenuSeparator className="my-1.5 h-px bg-(--base-03)" />}
                                <MenuItem
                                    disabled={m.deleteDisabled}
                                    title={m.deleteDisabled ? 'Upload in progress. Wait or cancel it first.' : undefined}
                                    onSelect={() => onDelete()}
                                    className={`${ITEM} hover:bg-(--base-03) data-highlighted:bg-(--base-03) active:bg-(--base-04)`}
                                >
                                    <Trash2 size={15} className="mt-0.5 shrink-0 text-(--base-07) group-data-disabled:text-(--base-05)" />
                                    <span className="min-w-0">
                                        <span className="block text-sm font-medium text-(--base-09) group-data-disabled:text-(--base-05)">Delete server</span>
                                        <span className="block text-xs text-(--base-06) leading-snug">Removes the server and all its data, after a confirmation</span>
                                    </span>
                                </MenuItem>
                            </>
                        )}
                    </MenuContent>
                </Menu>
            )}
            <span className="sr-only" aria-live="polite">{m.announcement}</span>
        </div>
    );
}
