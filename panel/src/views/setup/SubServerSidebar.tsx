"use client";

import React, { useEffect, useState } from 'react';
import { Server as ServerIcon, Plus, Pencil, Trash2, Play, Loader2 } from 'lucide-react';

interface SubServerSidebarProps {
    subServers: string[];
    activeSubServer?: string;
    // Names that are mid-delete on the backend. The row stays
    // rendered but greyed out + spinner-overlaid so the user gets
    // immediate feedback that the delete is in flight without the
    // row disappearing prematurely (the Node tear-down takes a
    // couple of seconds and an early reload would re-fetch the
    // still-present dir and "un-delete" the row visually).
    pendingDelete?: Set<string>;
    /** The non-active sub-server the form is previewing. */
    previewSubServer?: string;
    /** Clicking a row shows it read-only; it never switches. */
    onPreview?: (name: string) => void;
    onSwitch?: (name: string) => void;
    onAddNew: () => void;
    onEditSubServer?: (name: string) => void;
    onDeleteSubServer?: (name: string) => void;
    submitting: boolean;
    disabled?: boolean;
    maxSubServers?: number;
}

// Width: ~50% wider than the previous w-60 (240px) per user request --
// each row now has three inline action buttons (switch / edit / delete)
// next to the name, and the old layout's name column had no room left.
const SIDEBAR_WIDTH = 'w-[360px]';

export default function SubServerSidebar({
    subServers, activeSubServer,
    pendingDelete, previewSubServer, onPreview,
    onSwitch, onAddNew,
    onEditSubServer, onDeleteSubServer,
    submitting: _submitting, disabled, maxSubServers,
}: SubServerSidebarProps) {
    const limitReached = maxSubServers != null && maxSubServers > 0 && subServers.length >= maxSubServers;
    // Right-click context menu, retained from the prior design as a
    // power-user shortcut. Inline action buttons are the primary path
    // now, but right-click still works for keyboard / touch-pad users
    // who prefer the gesture. Fixed-position so it pierces overflow.
    const [ctxMenu, setCtxMenu] = useState<{ name: string; x: number; y: number } | null>(null);

    useEffect(() => {
        if (!ctxMenu) return;
        const close = () => setCtxMenu(null);
        const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') close(); };
        window.addEventListener('click', close);
        window.addEventListener('contextmenu', close);
        window.addEventListener('keydown', onKey);
        return () => {
            window.removeEventListener('click', close);
            window.removeEventListener('contextmenu', close);
            window.removeEventListener('keydown', onKey);
        };
    }, [ctxMenu]);

    return (
        <div className={`${SIDEBAR_WIDTH} shrink-0 card flex flex-col overflow-hidden`}>
            <div className="px-3 py-3 border-b border-(--base-03) flex items-center justify-between">
                <p className="input-label mb-0">Servers</p>
            </div>

            <div className="flex-1 overflow-y-auto p-2 space-y-1">
                {/* Empty-state hint when there are no sub-servers yet
                    (fresh container, pre-setup). The Add Server button
                    below is still the action; this just orients the
                    user so the empty sidebar isn't confusing. */}
                {subServers.length === 0 && (
                    <p className="px-2 py-3 text-xs text-(--base-06) text-center">
                        No sub-servers yet. Add one to get started.
                    </p>
                )}

                {subServers.map(name => {
                    const isActive = name === activeSubServer;
                    const isPreview = !isActive && name === previewSubServer;
                    const isPendingDelete = !!pendingDelete?.has(name);
                    const rowDisabled = !!disabled || isPendingDelete;
                    return (
                        <div
                            key={name}
                            onContextMenu={e => {
                                if (isPendingDelete) return;
                                if (!onEditSubServer && !onDeleteSubServer) return;
                                e.preventDefault();
                                setCtxMenu({ name, x: e.clientX, y: e.clientY });
                            }}
                            className={`group relative flex items-center gap-2 px-2.5 py-2 rounded-md text-sm font-mono border transition-opacity ${
                                isActive
                                    ? 'border-(--success-border) bg-(--success-ghost) text-(--success-light)'
                                    : isPreview
                                    ? 'border-(--accent-border) bg-(--accent-ghost) text-(--base-09)'
                                    : 'border-transparent hover:bg-(--base-04) text-(--base-07) hover:text-(--base-09)'
                            } ${isPendingDelete ? 'opacity-50 pointer-events-none' : ''}`}
                        >
                            {isActive ? (
                                <span className="w-2 h-2 rounded-full bg-(--success-light) shrink-0" title="Active" />
                            ) : (
                                <ServerIcon size={14} className="shrink-0" />
                            )}
                            {onPreview ? (
                                <button
                                    type="button"
                                    onClick={() => !rowDisabled && onPreview(name)}
                                    disabled={rowDisabled}
                                    aria-current={isActive || isPreview ? 'true' : undefined}
                                    title={isActive ? 'Active sub-server' : 'Preview (does not switch)'}
                                    className="truncate flex-1 text-left rounded-sm cursor-pointer disabled:cursor-not-allowed"
                                >
                                    {name}
                                </button>
                            ) : (
                                <span className="truncate flex-1">{name}</span>
                            )}

                            {/* Inline action buttons. Switch only for
                                non-active rows; edit/delete on all rows
                                (delete-active is allowed and falls
                                through to the existing confirm flow). */}
                            <div className="flex items-center gap-0.5 shrink-0">
                                {!isActive && onSwitch && !isPendingDelete && (
                                    <button
                                        type="button"
                                        onClick={() => !rowDisabled && onSwitch(name)}
                                        disabled={rowDisabled}
                                        title="Switch active sub-server to this"
                                        aria-label={`Switch to ${name}`}
                                        className="p-1.5 rounded text-(--success-light) hover:bg-(--success-ghost) disabled:opacity-40 disabled:cursor-not-allowed"
                                    >
                                        <Play size={12} />
                                    </button>
                                )}
                                {onEditSubServer && !isPendingDelete && (
                                    <button
                                        type="button"
                                        onClick={() => !rowDisabled && onEditSubServer(name)}
                                        disabled={rowDisabled}
                                        title="Edit"
                                        aria-label={`Edit ${name}`}
                                        className="p-1.5 rounded text-(--base-07) hover:bg-(--base-04) hover:text-(--base-09) disabled:opacity-40 disabled:cursor-not-allowed"
                                    >
                                        <Pencil size={12} />
                                    </button>
                                )}
                                {onDeleteSubServer && !isPendingDelete && (
                                    <button
                                        type="button"
                                        onClick={() => !rowDisabled && onDeleteSubServer(name)}
                                        disabled={rowDisabled}
                                        title="Delete"
                                        aria-label={`Delete ${name}`}
                                        className="p-1.5 rounded text-(--error-light) hover:bg-(--error-ghost) disabled:opacity-40 disabled:cursor-not-allowed"
                                    >
                                        <Trash2 size={12} />
                                    </button>
                                )}
                            </div>

                            {/* Loading overlay while the backend delete
                                is in flight. Sits over the row's
                                action area so it's visually clear that
                                the row is being torn down, not just
                                disabled. */}
                            {isPendingDelete && (
                                <span className="absolute inset-y-0 right-2 flex items-center gap-1.5 text-xs font-mono text-(--base-06) pointer-events-none">
                                    <Loader2 size={12} className="animate-spin" />
                                    deleting…
                                </span>
                            )}
                        </div>
                    );
                })}

                {/* Add-server CTA at the bottom of the list. */}
                <button
                    type="button"
                    onClick={onAddNew}
                    disabled={disabled || limitReached}
                    className={`w-full mt-2 px-3 py-2 rounded-md text-sm font-medium flex items-center justify-center gap-2 transition-all border ${
                        disabled || limitReached
                            ? 'border-(--base-04) text-(--base-05) bg-(--base-02) cursor-not-allowed'
                            : 'border-(--accent-border) bg-(--accent) text-white hover:bg-(--accent-light)'
                    }`}
                    title={limitReached ? `Limit reached (${maxSubServers})` : 'Add sub-server'}
                >
                    <Plus size={14} />
                    <span>Add Server</span>
                </button>
            </div>

            {/* Right-click context menu. Position-fixed so it pierces
                the sidebar's overflow-y-auto clipping; click-outside
                closes via the global listener registered in useEffect. */}
            {ctxMenu && (
                <div
                    className="fixed z-50 min-w-[140px] rounded-md bg-(--base-02) border border-(--base-03) shadow-lg py-1 text-sm"
                    style={{ left: ctxMenu.x, top: ctxMenu.y }}
                    onClick={e => e.stopPropagation()}
                    onContextMenu={e => e.preventDefault()}
                >
                    {onEditSubServer && (
                        <button
                            type="button"
                            onClick={() => { onEditSubServer(ctxMenu.name); setCtxMenu(null); }}
                            className="w-full px-3 py-1.5 flex items-center gap-2 text-(--base-08) hover:bg-(--base-04) hover:text-(--base-09)"
                        >
                            <Pencil size={13} /> Edit
                        </button>
                    )}
                    {onDeleteSubServer && (
                        <button
                            type="button"
                            onClick={() => { onDeleteSubServer(ctxMenu.name); setCtxMenu(null); }}
                            className="w-full px-3 py-1.5 flex items-center gap-2 text-(--error-light) hover:bg-(--error-ghost)"
                        >
                            <Trash2 size={13} /> Delete
                        </button>
                    )}
                </div>
            )}
        </div>
    );
}
