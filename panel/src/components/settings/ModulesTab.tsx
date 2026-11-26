"use client";

import React, { useState, useEffect } from 'react';
import { createModule, deleteModule, toggleModule, updateModulePosition, setModuleAccessRole, AppModule } from '@/lib/api';
import { Plus, Trash2, GripVertical, ShieldCheck, Users, LayoutGrid, AlertTriangle } from 'lucide-react';
import { DynamicIcon } from '@/lib/icons';
import {
    DndContext,
    DragEndEvent,
    PointerSensor,
    useSensor,
    useSensors,
    closestCenter,
} from '@dnd-kit/core';
import {
    SortableContext,
    useSortable,
    verticalListSortingStrategy,
    arrayMove,
} from '@dnd-kit/sortable';
import { CSS } from '@dnd-kit/utilities';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import { toast } from '@/components/ui/Toast';
import HelpTip from '@/components/ui/HelpTip';
import ModalPanel from '@/components/ui/ModalPanel';
import Select from '@/components/ui/Select';
import Tooltip from '@/components/ui/Tooltip';
import Switch from '@/components/ui/Switch';
import Segmented from '@/components/ui/Segmented';
import EmptyState from '@/components/ui/EmptyState';
import SettingsPage from '@/components/settings/SettingsPage';
import Callout from '@/components/server/Callout';
import '@/styles/settings-people.css';

interface ModulesTabProps {
    modules: AppModule[];
    onModulesChange: () => void;
}

// Built-in modules cannot be deleted (kept in sync with Core's seed list +
// builtInModules check in handlers/modules.go). Gateway was retired and its
// content moved into Infrastructure's Routes tab. Tickets is included here
// even though it is_system=false server-side (it stays toggle-able) - only
// deletion is blocked, both here (hides the button) and in Core (rejects the
// request).
const BUILTIN_MODULES = new Set(['Servers', 'Admin', 'Infrastructure', 'Tickets', 'Modpacks', 'Custom Tabs', 'Templates']);

// Modules whose ENABLED state is derived from a feature flag rather than set
// here. Offering the control would be worse than hiding it: an edit would appear
// to work and then be silently undone the next time the owning flag is saved.
// Position stays editable - reordering is this screen's job.
//
// Tickets is the one this list was missing. It had two independent switches, so
// an operator could turn the feature on in Features, see no Tickets entry in the
// navbar because the row here was still off, and have no way to connect the two
// - meanwhile the whole ticket API was live and reachable by URL.
const DERIVED_MODULES = new Map([
    ['Modpacks', 'Settings -> Features -> Modpacks'],
    ['Custom Tabs', 'Settings -> Features -> Custom-tab reverse proxy'],
    ['Tickets', 'Settings -> Features -> Ticket system'],
    ['Templates', 'Settings -> Features -> Templates'],
]);

// Of those, the ones whose AUDIENCE is derived too, from a second flag that has
// no other home (authoring for Modpacks, the audience picker for Custom Tabs).
// For Tickets the audience stays an answer to a different question - whether
// the platform HAS the feature is not who should see it - so it keeps the
// All/Admin control.
const AUDIENCE_DERIVED = new Set(['Modpacks', 'Custom Tabs', 'Templates']);

interface SortableModuleCardProps {
    module: AppModule;
    onToggle: (id: number, current: boolean) => void;
    onDelete: (id: number) => void;
    onRoleChange: (id: number, role: 'all' | 'admin') => void;
}

function SortableModuleCard({ module: m, onToggle, onDelete, onRoleChange }: SortableModuleCardProps) {
    const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({ id: m.id });
    // Non-empty when this row's state comes from feature flags: names where the
    // admin can change it, so the lock reads as a pointer rather than a dead end.
    const derivedFrom = DERIVED_MODULES.get(m.name);

    const style = {
        transform: CSS.Transform.toString(transform),
        transition,
        opacity: isDragging ? 0.5 : 1,
        zIndex: isDragging ? 10 : undefined,
    };

    const fixedAudience = (label: string, hint: string, admin: boolean) => (
        <Tooltip content={hint}>
            <span className="set-ppl-audience" tabIndex={0}>
                {admin ? <ShieldCheck size={12} /> : <Users size={12} />} {label}
            </span>
        </Tooltip>
    );

    return (
        <div
            ref={setNodeRef}
            style={style}
            className={`app-row ${m.isEnabled ? '' : 'opacity-60'}`}
        >
            <button
                type="button"
                {...attributes}
                {...listeners}
                className="app-icon-btn shrink-0 cursor-grab touch-none active:cursor-grabbing"
                aria-label={`Reorder ${m.name}`}
            >
                <GripVertical size={16} />
            </button>
            <span className={`flex size-9 shrink-0 items-center justify-center rounded-lg ${m.isEnabled ? 'bg-(--accent-ghost) text-(--accent-light)' : 'bg-(--base-03) text-(--base-06)'}`}>
                <DynamicIcon name={m.icon} size={20} />
            </span>
            <div className="min-w-0 flex-1">
                <div className="flex items-center gap-2 text-sm font-medium text-(--base-09)">
                    <span className="truncate">{m.name}</span>
                    {m.isSystem && <span className="badge badge-neutral">System</span>}
                </div>
                <div className="mt-0.5 text-xs text-(--base-06)">{m.type}</div>
            </div>
            <div className="flex shrink-0 flex-wrap items-center justify-end gap-3">
                {/* Role toggle - Servers is hard-locked to "all", Admin and
                    Infrastructure to "admin" (Infrastructure's own page
                    hard-gates isAdmin, so the "all" option would never take
                    effect anyway - the toggle is locked to avoid a misleading
                    control) */}
                {derivedFrom && AUDIENCE_DERIVED.has(m.name) ? (
                    fixedAudience(m.accessRole === 'all' ? 'All' : 'Admin', `Audience follows ${derivedFrom}`, m.accessRole !== 'all')
                ) : m.name === 'Servers' ? (
                    fixedAudience('All', 'Always visible to all users', false)
                ) : m.name === 'Admin' || m.name === 'Infrastructure' ? (
                    fixedAudience('Admin', 'Always admin-only', true)
                ) : (
                    <Segmented
                        ariaLabel={`Who can see ${m.name}`}
                        value={m.accessRole === 'admin' ? 'admin' : 'all'}
                        onChange={role => onRoleChange(m.id, role)}
                        options={[{ id: 'all', label: 'All' }, { id: 'admin', label: 'Admin' }]}
                    />
                )}

                {derivedFrom ? (
                    <Tooltip content={`Enabled state follows ${derivedFrom}`}>
                        <span tabIndex={0} className="inline-flex">
                            <Switch checked={m.isEnabled} onChange={() => {}} disabled ariaLabel={`${m.name} enabled (set in ${derivedFrom})`} />
                        </span>
                    </Tooltip>
                ) : m.name === 'Servers' || m.name === 'Admin' || m.name === 'Infrastructure' ? (
                    <Tooltip content={`${m.name} module cannot be disabled`}>
                        <span tabIndex={0} className="inline-flex">
                            <Switch checked disabled onChange={() => {}} ariaLabel={`${m.name} enabled (cannot be disabled)`} />
                        </span>
                    </Tooltip>
                ) : (
                    <Switch checked={m.isEnabled} onChange={() => onToggle(m.id, m.isEnabled)} ariaLabel={`${m.name} enabled`} />
                )}

                {!m.isSystem && !BUILTIN_MODULES.has(m.name) ? (
                    <Tooltip content="Delete"><button type="button" onClick={() => onDelete(m.id)} className="app-icon-btn hover:!text-(--error-light)" aria-label={`Delete ${m.name}`}>
                        <Trash2 size={16} />
                    </button></Tooltip>
                ) : (
                    <span className="size-8" aria-hidden="true" />
                )}
            </div>
        </div>
    );
}

export default function ModulesTab({ modules, onModulesChange }: ModulesTabProps) {
    const [isModalOpen, setIsModalOpen] = useState(false);
    const [error, setError] = useState("");
    // Separate from `error`, which belongs to the add-module modal. These are
    // failures of the inline row actions, which used to go to window.alert.
    const [actionError, setActionError] = useState("");
    const [modForm, setModForm] = useState({ name: "", type: "iframe", icon: "link", url: "" });
    const [sortedModules, setSortedModules] = useState<AppModule[]>([]);

    useEffect(() => {
        setSortedModules([...modules].sort((a, b) => a.position - b.position));
    }, [modules]);

    const sensors = useSensors(useSensor(PointerSensor, { activationConstraint: { distance: 5 } }));

    const handleDragEnd = async (event: DragEndEvent) => {
        const { active, over } = event;
        if (!over || active.id === over.id) return;

        const oldIndex = sortedModules.findIndex(m => m.id === active.id);
        const newIndex = sortedModules.findIndex(m => m.id === over.id);
        const reordered = arrayMove(sortedModules, oldIndex, newIndex);

        // Optimistic update
        const previous = sortedModules;
        setSortedModules(reordered);

        // Compute new position: slot between neighbours (spaced by 10)
        const newPos = (newIndex + 1) * 10;
        const writes = [await updateModulePosition(Number(active.id), newPos)];

        // Persist all positions after reorder to keep them clean
        for (let i = 0; i < reordered.length; i++) {
            const m = reordered[i];
            const targetPos = (i + 1) * 10;
            if (m.position !== targetPos) {
                writes.push(await updateModulePosition(m.id, targetPos));
            }
        }

        // Every one of those results used to be discarded. A refused write left
        // the new order on screen and the old order in the database, so the drag
        // silently undid itself on the next load - and a partial failure left the
        // two disagreeing about only some rows. Put the list back instead.
        const failed = writes.find(r => r && r.success === false);
        if (failed) {
            setSortedModules(previous);
            setActionError(failed.message || failed.error || 'Could not save the new order.');
            return;
        }
        setActionError('');
        toast('Order saved');
        onModulesChange();
    };

    const handleCreateModule = async (e: React.FormEvent) => {
        e.preventDefault();
        const res = await createModule(modForm as any);
        if (res.success) {
            setIsModalOpen(false);
            toast('Module added');
            onModulesChange();
        } else setError(res.message);
    };

    const handleDeleteModule = async (id: number) => {
        if (!(await confirmDialog({ title: 'Delete module', message: "Do you really want to delete this module?" }))) return;
        // handleCreateModule and handleToggleModule both read res.success; this
        // one threw it away, so a refused delete just re-rendered the module
        // still sitting there with nothing said. setActionError, not setError:
        // the `error` alert only renders inside the create modal, so a card-level
        // failure written there would never be seen. handleRoleChange already
        // makes that distinction.
        const res = await deleteModule(id);
        if (!res.success) {
            setActionError(res.message || res.error || 'Could not delete the module.');
            return;
        }
        setActionError('');
        toast('Module deleted');
        onModulesChange();
    };

    const handleToggleModule = async (id: number, currentStatus: boolean) => {
        const res = await toggleModule(id, !currentStatus);
        if (res.success) {
            setActionError("");
            // These are row operations on a list, like deleting a user, so they
            // apply straight away rather than waiting for a Save. What
            // they did NOT do was say so: this screen had no toast at all, so a
            // toggle, a role change and a drag all succeeded in silence and the
            // only way to know was that something moved.
            toast(currentStatus ? 'Module disabled' : 'Module enabled');
            onModulesChange();
        } else {
            setActionError(res.message || "Could not toggle the module.");
        }
    };

    const handleRoleChange = async (id: number, role: 'all' | 'admin') => {
        const res = await setModuleAccessRole(id, role);
        if (res.success) {
            setActionError("");
            toast(role === 'admin' ? 'Module is now admin-only' : 'Module is now visible to everyone');
            onModulesChange();
        } else {
            setActionError(res.message || "Could not change the module role.");
        }
    };

    return (
        <SettingsPage
            title="Modules"
            icon={LayoutGrid}
            width="4xl"
            description={<span className="inline-flex items-center gap-1.5">
                Manage system features and external links. Drag to reorder. Disabled modules are hidden from the sidebar.
                <HelpTip label="About modules">
                    <p className="mb-2">
                        A module is an entry in the main navigation, nothing more. This page
                        decides what the sidebar shows and in which order.
                    </p>
                    <p className="mb-2">
                        <strong>All / Admin here is visibility, not permission.</strong> Hiding a
                        module removes the link; it does not close the page, and it does not
                        revoke anything. Who may actually do what is Roles and Access. Treat this
                        as tidying the sidebar, never as a way to lock something down.
                    </p>
                    <p>
                        A few modules cannot be changed because their audience is fixed in code -
                        Servers is always visible, Admin and Infrastructure always are not.
                    </p>
                </HelpTip>
            </span>}
            actions={
                <button onClick={() => { setModForm({ name: "", type: "iframe", icon: "link", url: "" }); setError(""); setIsModalOpen(true); }} className="btn btn-primary">
                    <Plus size={14} />
                    Add Module
                </button>
            }
        >
            {actionError && <Callout tone="error" icon={<AlertTriangle size={15} />} role="alert">{actionError}</Callout>}

            {sortedModules.length === 0 ? (
                <EmptyState icon={<LayoutGrid size={20} />} title="No modules" description="Add an external link as a module to show it in the sidebar." />
            ) : (
                <DndContext sensors={sensors} collisionDetection={closestCenter} onDragEnd={handleDragEnd}>
                    <SortableContext items={sortedModules.map(m => m.id)} strategy={verticalListSortingStrategy}>
                        <div className="srv-surface flex flex-col gap-0.5 p-1.5" role="list" aria-label="Modules">
                            {sortedModules.map(m => (
                                <SortableModuleCard
                                    key={m.id}
                                    module={m}
                                    onToggle={handleToggleModule}
                                    onDelete={handleDeleteModule}
                                    onRoleChange={handleRoleChange}
                                />
                            ))}
                        </div>
                    </SortableContext>
                </DndContext>
            )}

            {isModalOpen && (
                <div className="modal-overlay animate-fade-in">
                    <ModalPanel className="modal-panel w-full max-w-md">
                        <div className="modal-header">
                            <h3 className="modal-title">New Module</h3>
                        </div>
                        <div className="modal-body">
                            {error && <div className="alert alert-error mb-4 font-medium">{error}</div>}

                            <form onSubmit={handleCreateModule} className="space-y-4">
                                <div className="flex flex-col gap-[5px]">
                                    <label className="input-label">Name</label>
                                    <input required type="text" value={modForm.name} onChange={e => setModForm({ ...modForm, name: e.target.value })} className="input-field w-full" placeholder="e.g. Map" />
                                </div>
                                <div className="flex flex-col gap-[5px]">
                                    <label className="input-label flex items-center gap-1.5">
                                        Type
                                        <HelpTip label="About the module type">
                                            <p className="mb-2">
                                                <strong>IFrame</strong> embeds a page you host somewhere else,
                                                inside the panel&apos;s layout. It must allow being framed, or the
                                                tab opens blank with the reason only in the browser console.
                                            </p>
                                            <p>
                                                <strong>Internal</strong> links to a page of the panel itself, by
                                                path - but this form does not offer a path field for it, and a
                                                module cannot be edited after it is created. One made here would
                                                land on an empty page with no way to correct it, so use IFrame
                                                unless you know the row will be set up elsewhere.
                                            </p>
                                        </HelpTip>
                                    </label>
                                    <Select ariaLabel="Module type" value={modForm.type} onChange={v => setModForm({ ...modForm, type: v })}
                                        options={[{ value: 'iframe', label: 'IFrame (External URL)' }, { value: 'internal', label: 'Internal (Custom View)' }]} />
                                </div>
                                {modForm.type === 'iframe' && (
                                    <div className="flex flex-col gap-[5px]">
                                        <label className="input-label">URL</label>
                                        <input required type="url" value={modForm.url} onChange={e => setModForm({ ...modForm, url: e.target.value })} className="input-field w-full" placeholder="https://..." />
                                    </div>
                                )}
                                <div className="flex flex-col gap-[5px]">
                                    <label className="input-label">Icon (Lucide name)</label>
                                    <div className="flex gap-2">
                                        <input required type="text" value={modForm.icon} onChange={e => setModForm({ ...modForm, icon: e.target.value })} className="input-field w-full" placeholder="e.g. map" />
                                        <div className="w-10 h-10 flex items-center justify-center bg-(--base-03) rounded-md text-(--base-07) shrink-0"><DynamicIcon name={modForm.icon || 'circle-help'} size={24} /></div>
                                    </div>
                                </div>
                                <div className="modal-footer">
                                    <button type="button" onClick={() => setIsModalOpen(false)} className="btn btn-secondary">Cancel</button>
                                    <button type="submit" className="btn btn-primary">Save Module</button>
                                </div>
                            </form>
                        </div>
                    </ModalPanel>
                </div>
            )}
        </SettingsPage>
    );
}
