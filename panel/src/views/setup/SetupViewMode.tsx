"use client";

import React, { useState } from 'react';
import { Server } from '@/lib/api';
import { Pencil, PackageOpen, Cpu, HardDrive, ChevronDown, ChevronUp, AlertTriangle } from 'lucide-react';
import { JAVA_IMAGES } from './JavaVersionPicker';
import { jvmHeapMB } from '@/lib/jvmHeap';

interface SetupViewModeProps {
    server: Server;
    activeServerMissing: boolean;
    onEdit: () => void;
    // onAddNew kept for compatibility with parent wiring even though
    // the inline header button moved into the sub-server sidebar.
    onAddNew: () => void;
    hasSubServers: boolean;
}

const formatInstallerType = (type?: string) => {
    if (!type) return null;
    if (type === 'upload') return 'Manual Upload';
    if (type === 'library') return 'From Library';
    return type.charAt(0).toUpperCase() + type.slice(1);
};

export default function SetupViewMode({ server, activeServerMissing, onEdit, onAddNew: _onAddNew, hasSubServers }: SetupViewModeProps) {
    const [flagsOpen, setFlagsOpen] = useState(false);
    const javaLabel = JAVA_IMAGES.find(j => j.id === server.image)?.label || 'Unknown';
    const javaNote = JAVA_IMAGES.find(j => j.id === server.image)?.note || '';
    const hasFlags = !!server.extraJvmFlags;
    const flagCount = server.extraJvmFlags ? server.extraJvmFlags.split(' ').filter(Boolean).length : 0;

    return (
        <div className="flex-1 card flex flex-col overflow-hidden min-w-0">
            {/* Header */}
            <div className="modal-header flex items-center justify-between shrink-0">
                <div>
                    <h3 className="modal-title">Server Configuration</h3>
                    {server.activeSubServer && (
                        <p className="text-xs text-(--base-07) mt-1">
                            Active: <span className="font-mono text-(--primary-light)">{server.activeSubServer}</span>
                        </p>
                    )}
                </div>
                {hasSubServers && (
                    <div className="flex gap-2">
                        <button onClick={onEdit} className="btn btn-secondary btn-sm">
                            <Pencil size={14} /> Edit
                        </button>
                    </div>
                )}
            </div>

            {/* Warning banner */}
            {activeServerMissing && (
                <div className="alert alert-warning gap-3 rounded-xl mx-6 mt-4">
                    <AlertTriangle size={20} className="text-(--warning-light) shrink-0" />
                    <div className="text-sm">
                        <p className="font-medium text-(--warning-light)">Server folder not found</p>
                        <p className="text-(--base-07) mt-1">
                            The active server folder <code className="bg-black/20 px-1 rounded font-mono">{server.activeSubServer}</code> no longer exists.
                            Select an existing server from the sidebar to continue.
                        </p>
                    </div>
                </div>
            )}

            {/* Summary body */}
            <div className="flex-1 overflow-y-auto p-6 space-y-5">
                {/* Software hero badge */}
                {server.installerType && (
                    <div className="flex items-center gap-3 px-5 py-4 bg-(--base-03) rounded-lg border border-(--base-04)">
                        <div className="w-10 h-10 rounded-md bg-(--primary-ghost) border border-(--primary-border) flex items-center justify-center">
                            <PackageOpen size={20} className="text-(--primary-light)" />
                        </div>
                        <div>
                            <p className="text-base font-semibold text-(--base-09)">
                                {formatInstallerType(server.installerType)}
                            </p>
                            {server.minecraftVersion && (
                                <p className="text-sm text-(--base-07) font-mono">
                                    {server.minecraftVersion}
                                    {server.buildNumber && <span className="text-(--base-06)"> #{server.buildNumber}</span>}
                                </p>
                            )}
                        </div>
                    </div>
                )}

                {/* Stat chips */}
                <div className="flex flex-wrap gap-3">
                    <div className="flex items-center gap-2 px-3.5 py-2 bg-(--base-02) rounded-md border border-(--base-03)">
                        <Cpu size={14} className="text-(--accent-light)" />
                        <span className="text-sm text-(--base-08) font-medium">{javaLabel}</span>
                        <span className="text-xs text-(--base-06)">{javaNote}</span>
                    </div>
                    <div className="flex items-center gap-2 px-3.5 py-2 bg-(--base-02) rounded-md border border-(--base-03)">
                        <HardDrive size={14} className="text-(--primary-light)" />
                        <span className="text-sm text-(--base-08) font-medium">{server.memory} MB</span>
                        <span className="text-xs text-(--base-06)">RAM</span>
                    </div>
                    {hasFlags && (
                        <div className="flex items-center gap-2 px-3.5 py-2 bg-(--base-02) rounded-md border border-(--base-03)">
                            <span className="text-sm text-(--base-08) font-medium">{flagCount} JVM Flags</span>
                        </div>
                    )}
                </div>

                {/* Collapsible JVM flags */}
                {hasFlags && (
                    <div>
                        <button
                            type="button"
                            onClick={() => setFlagsOpen(o => !o)}
                            className="input-label flex items-center gap-1.5 w-fit cursor-pointer hover:text-(--base-08) transition-colors"
                        >
                            {flagsOpen ? <ChevronUp size={12} /> : <ChevronDown size={12} />}
                            <span>JVM Flags</span>
                        </button>
                        {flagsOpen && (
                            <div className="mt-2 p-3 bg-(--base-02) rounded-md border border-(--base-03) font-mono text-xs text-(--base-07) whitespace-pre-wrap animate-fade-in">
                                java -Xms{jvmHeapMB(server.memory)}M -Xmx{jvmHeapMB(server.memory)}M {server.extraJvmFlags}
                            </div>
                        )}
                    </div>
                )}

            </div>
        </div>
    );
}
