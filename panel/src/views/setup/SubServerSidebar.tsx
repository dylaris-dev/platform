"use client";

import React from 'react';
import { Menu, MenuContent, MenuContextTrigger, MenuItem } from '@ui-sdk/react/menu';
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
const SIDEBAR_WIDTH = 'w-full lg:w-[320px]';

export default function SubServerSidebar({
    subServers, activeSubServer,
    pendingDelete, previewSubServer, onPreview,
    onSwitch, onAddNew,
    onEditSubServer, onDeleteSubServer,
    submitting: _submitting, disabled, maxSubServers,
}: SubServerSidebarProps) {
    const limitReached = maxSubServers != null && maxSubServers > 0 && subServers.length >= maxSubServers;
    return (
        <Menu>
        <div className={`${SIDEBAR_WIDTH} shrink-0 srv-surface flex flex-col overflow-hidden self-start lg:max-h-full`}>
            <div className="flex items-center justify-between px-5 pt-5 pb-2">
                <h2 className="text-sm font-medium text-(--base-09)">Servers</h2>
                {maxSubServers != null && maxSubServers > 0 && <span className="srv-num text-xs text-(--base-06)">{subServers.length} / {maxSubServers}</span>}
            </div>

            <div className="flex-1 overflow-y-auto p-3 space-y-1">
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
                        <MenuContextTrigger
                            key={name}
                            payload={name}
                            disabled={isPendingDelete || (!onEditSubServer && !onDeleteSubServer)}
                            className={`group relative flex items-center gap-2 px-3 py-2 rounded-md text-sm srv-num transition-colors ${
                                isPreview
                                    ? 'bg-(--accent-ghost) text-(--accent-light)'
                                    : isActive
                                    ? 'bg-(--base-03) text-(--base-09)'
                                    : 'hover:bg-(--base-03) text-(--base-07) hover:text-(--base-09)'
                            } ${isPendingDelete ? 'opacity-50 pointer-events-none' : ''}`}
                        >
                            {isActive ? (
                                <span className="size-2 rounded-full bg-(--success-light) shrink-0" role="img" aria-label="Active" title="Active" />
                            ) : (
                                <ServerIcon size={14} className="shrink-0 text-(--base-05)" />
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
                                        className="p-1.5 rounded-md text-(--success-light) hover:bg-(--success-ghost) focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)] disabled:opacity-40 disabled:cursor-not-allowed"
                                    >
                                        <Play size={14} />
                                    </button>
                                )}
                                {onEditSubServer && !isPendingDelete && (
                                    <button
                                        type="button"
                                        onClick={() => !rowDisabled && onEditSubServer(name)}
                                        disabled={rowDisabled}
                                        title="Edit"
                                        aria-label={`Edit ${name}`}
                                        className="p-1.5 rounded-md text-(--base-07) hover:bg-(--base-04) hover:text-(--base-09) focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)] disabled:opacity-40 disabled:cursor-not-allowed"
                                    >
                                        <Pencil size={14} />
                                    </button>
                                )}
                                {onDeleteSubServer && !isPendingDelete && (
                                    <button
                                        type="button"
                                        onClick={() => !rowDisabled && onDeleteSubServer(name)}
                                        disabled={rowDisabled}
                                        title="Delete"
                                        aria-label={`Delete ${name}`}
                                        className="p-1.5 rounded-md text-(--error-light) hover:bg-(--error-ghost) focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)] disabled:opacity-40 disabled:cursor-not-allowed"
                                    >
                                        <Trash2 size={14} />
                                    </button>
                                )}
                            </div>

                            {/* Loading overlay while the backend delete
                                is in flight. Sits over the row's
                                action area so it's visually clear that
                                the row is being torn down, not just
                                disabled. */}
                            {isPendingDelete && (
                                <span className="absolute inset-y-0 right-2 flex items-center gap-1.5 text-xs srv-num text-(--base-06) pointer-events-none">
                                    <Loader2 size={12} className="animate-spin" />
                                    deleting…
                                </span>
                            )}
                        </MenuContextTrigger>
                    );
                })}

                {/* Add-server CTA at the bottom of the list. */}
                <button
                    type="button"
                    onClick={onAddNew}
                    disabled={disabled || limitReached}
                    className="btn btn-primary mt-2 w-full justify-center"
                    title={limitReached ? `Limit reached (${maxSubServers})` : 'Add sub-server'}
                >
                    <Plus size={16} />
                    <span>Add server</span>
                </button>
            </div>

            <MenuContent<string> className="ui-popup p-1.5 min-w-36">
                {state => (
                    <>
                        {onEditSubServer && (
                            <MenuItem onSelect={() => state.payload && onEditSubServer(state.payload)} className="dropdown-item gap-3">
                                <Pencil size={13} /> Edit
                            </MenuItem>
                        )}
                        {onDeleteSubServer && (
                            <MenuItem onSelect={() => state.payload && onDeleteSubServer(state.payload)} className="dropdown-item dropdown-item-danger gap-3">
                                <Trash2 size={13} /> Delete
                            </MenuItem>
                        )}
                    </>
                )}
            </MenuContent>
        </div>
        </Menu>
    );
}
