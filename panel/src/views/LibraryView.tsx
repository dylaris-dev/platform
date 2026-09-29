"use client";

import React, { useState, useEffect, useRef } from 'react';
import { getLibraryFiles, deleteLibraryPath, createLibraryDir, uploadLibraryFiles, getLibraryDownloadUrl, toggleLibraryPath } from '@/lib/api';
import { useAppData } from '@/lib/AppDataContext';
import { FolderPlus, Upload, ArrowUp, FolderOpen, Folder, Archive, Trash2, Eye, EyeOff } from 'lucide-react';
import { SkeletonTable } from '@/components/Skeleton';
import { useBusy } from '@/lib/useBusy';
import { toast } from '@/components/ui/Toast';
import ModalPanel from '@/components/ui/ModalPanel';

interface FileEntry {
    name: string;
    is_dir: boolean;
    size: number;
    enabled?: boolean;
}

const formatBytes = (bytes: number): string => {
    if (bytes === 0) return '0 B';
    const k = 1024;
    const sizes = ['B', 'KB', 'MB', 'GB'];
    const i = Math.floor(Math.log(bytes) / Math.log(k));
    return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + ' ' + sizes[i];
};

export default function LibraryView() {
    const { user } = useAppData();
    const isAdmin = !!user?.isAdmin;
    const [files, setFiles] = useState<FileEntry[]>([]);
    const [currentPath, setCurrentPath] = useState('');
    const [loading, setLoading] = useState(false);
    const [creatingDir, runCreateDir] = useBusy();
    const [deletingEntry, runDelete] = useBusy();
    const [error, setError] = useState('');

    const [showCreateDir, setShowCreateDir] = useState(false);
    const [newDirName, setNewDirName] = useState('');
    const [deleteTarget, setDeleteTarget] = useState<FileEntry | null>(null);

    const [uploadProgress, setUploadProgress] = useState<number | null>(null);
    const fileInputRef = useRef<HTMLInputElement>(null);

    // The old local toast had no ok/fail concept - one neutral bar, no icon - so
    // every call site passed just a message. Delegating with the shared
    // default (ok = true) rendered "Delete failed" with a green tick.
    const showToast = (msg: string, ok = true) => {
        toast(msg, ok);
    };

    const fetchFiles = async (path: string) => {
        setLoading(true);
        setError('');
        const res = await getLibraryFiles(path || undefined);
        if (res.success && res.files) {
            setFiles(res.files);
            setCurrentPath(path);
        } else {
            setError(res.message || 'Failed to load library');
        }
        setLoading(false);
    };

    useEffect(() => { fetchFiles(''); }, []);

    const navigate = (name: string, isDir: boolean) => {
        if (isDir) {
            fetchFiles(currentPath ? `${currentPath}/${name}` : name);
        } else {
            // window.open is a plain navigation and cannot send the Authorization
            // header, so carry the token in the querystring the same way downloadFile
            // does for getDownloadUrl. Without it the endpoint 401s and the tab shows
            // the JSON error instead of downloading.
            // A new tab on the same origin carries the session cookie, so the
            // URL needs no credential in it.
            const url = getLibraryDownloadUrl(currentPath ? `${currentPath}/${name}` : name);
            window.open(url, '_blank');
        }
    };

    const goUp = () => {
        const parts = currentPath.split('/');
        parts.pop();
        fetchFiles(parts.join('/'));
    };

    const handleCreateDir = async () => {
        if (!newDirName.trim()) return;
        const path = currentPath ? `${currentPath}/${newDirName.trim()}` : newDirName.trim();
        const res = await createLibraryDir(path);
        if (res.success) {
            setNewDirName('');
            setShowCreateDir(false);
            fetchFiles(currentPath);
        } else {
            showToast(res.message || 'Failed to create folder', false);
        }
    };

    const handleDelete = async () => {
        if (!deleteTarget) return;
        const path = currentPath ? `${currentPath}/${deleteTarget.name}` : deleteTarget.name;
        const res = await deleteLibraryPath(path);
        if (res.success) {
            setDeleteTarget(null);
            fetchFiles(currentPath);
        } else {
            showToast(res.message || 'Delete failed', false);
        }
    };

    const handleToggleEnabled = async (entry: FileEntry, e: React.MouseEvent) => {
        e.stopPropagation();
        const path = currentPath ? `${currentPath}/${entry.name}` : entry.name;
        const next = entry.enabled === false; // disabled → flip to enabled
        // Optimistic update
        setFiles(curr => curr.map(f => f.name === entry.name ? { ...f, enabled: next } : f));
        const res = await toggleLibraryPath(path, next);
        if (!res.success) {
            // Revert
            setFiles(curr => curr.map(f => f.name === entry.name ? { ...f, enabled: !next } : f));
            showToast(res.message || 'Failed to toggle', false);
        }
    };

    const handleUpload = async (e: React.ChangeEvent<HTMLInputElement>) => {
        if (!e.target.files || e.target.files.length === 0) return;
        setUploadProgress(0);
        const res = await uploadLibraryFiles(currentPath, e.target.files, setUploadProgress);
        setUploadProgress(null);
        if (res.success) {
            fetchFiles(currentPath);
            showToast('Upload complete');
        } else {
            showToast(res.message || 'Upload failed', false);
        }
        e.target.value = '';
    };

    const pathParts = currentPath ? currentPath.split('/') : [];

    return (
        <div className="h-full flex flex-col">
            {/* Header */}
            <div className="flex items-center justify-between mb-4 shrink-0">
                <div>
                    <h2 className="h-page">Server Library</h2>
                    <p className="text-sm text-(--base-07)">Manage shared files available for server installation.</p>
                </div>
                <div className="flex gap-2">
                    <button onClick={() => setShowCreateDir(true)} className="btn btn-secondary btn-sm">
                        <FolderPlus size={14} /> New Folder
                    </button>
                    <button onClick={() => fileInputRef.current?.click()} className="btn btn-primary btn-sm">
                        <Upload size={14} /> Upload
                    </button>
                    <input ref={fileInputRef} type="file" multiple className="hidden" onChange={handleUpload} />
                </div>
            </div>

            {/* Breadcrumb */}
            <div className="flex items-center gap-1 text-sm mb-3 shrink-0">
                <button onClick={() => fetchFiles('')} className="text-(--accent-light) hover:underline font-mono">library</button>
                {pathParts.map((part, i) => (
                    <React.Fragment key={i}>
                        <span className="text-(--base-06)">/</span>
                        <button
                            onClick={() => fetchFiles(pathParts.slice(0, i + 1).join('/'))}
                            className="text-(--accent-light) hover:underline font-mono"
                        >{part}</button>
                    </React.Fragment>
                ))}
            </div>

            {/* Upload progress */}
            {uploadProgress !== null && (
                <div className="mb-3 shrink-0">
                    <div className="h-2 bg-(--base-04) rounded-full overflow-hidden">
                        <div className="h-full bg-(--accent) transition-all duration-200 rounded-full" style={{ width: `${uploadProgress}%` }} />
                    </div>
                    <p className="text-xs text-(--base-06) mt-1">Uploading... {uploadProgress}%</p>
                </div>
            )}

            {/* File list */}
            <div className="flex-1 overflow-y-auto table-wrapper">
                {loading ? (
                    <SkeletonTable rows={8} cols={3} />
                ) : error ? (
                    <div className="p-6 text-center text-(--error-light)">{error}</div>
                ) : (
                    <table className="w-full">
                        <thead>
                            <tr>
                                <th className="table-th text-left">Name</th>
                                <th className="table-th text-right">Size</th>
                                <th className="table-th w-20"></th>
                            </tr>
                        </thead>
                        <tbody>
                            {currentPath && (
                                <tr className="table-tr table-tr-hover cursor-pointer" onClick={goUp}>
                                    <td className="table-td flex items-center gap-2">
                                        <ArrowUp size={14} className="text-(--base-06)" />
                                        <span className="text-(--base-06) text-sm">..</span>
                                    </td>
                                    <td className="table-td"></td><td className="table-td"></td>
                                </tr>
                            )}
                            {files.length === 0 && !currentPath && (
                                <tr><td colSpan={3} className="text-center py-16 text-(--base-07)">
                                    <FolderOpen size={36} className="block mb-2 opacity-40 mx-auto" />
                                    Library is empty. Upload files to get started.
                                </td></tr>
                            )}
                            {files.map(f => {
                                const dimmed = isAdmin && f.enabled === false;
                                return (
                                <tr key={f.name} className="table-tr table-tr-hover cursor-pointer" onClick={() => navigate(f.name, f.is_dir)}>
                                    <td className={`table-td ${dimmed ? 'opacity-50' : ''}`}>
                                        <div className="flex items-center gap-2">
                                            {f.is_dir ? <Folder size={14} className="text-(--warning)" /> : <Archive size={14} className="text-(--base-06)" />}
                                            <span className="font-mono text-sm">{f.name}</span>
                                            {dimmed && (
                                                <span className="font-mono text-[9px] uppercase tracking-[0.08em] text-(--base-06) bg-(--base-02) border border-(--base-04) rounded px-1.5 py-0.5">
                                                    hidden
                                                </span>
                                            )}
                                        </div>
                                    </td>
                                    <td className={`table-td text-right text-sm text-(--base-07) ${dimmed ? 'opacity-50' : ''}`}>
                                        {f.is_dir ? '—' : formatBytes(f.size)}
                                    </td>
                                    <td className="table-td text-right">
                                        <div className="flex items-center justify-end gap-3">
                                            {isAdmin && (
                                                <button
                                                    onClick={e => handleToggleEnabled(f, e)}
                                                    className={`transition-colors ${f.enabled === false ? 'text-(--base-06) hover:text-(--accent-light)' : 'text-(--accent-light) hover:text-(--base-06)'}`}
                                                    title={f.enabled === false ? 'Enable for users' : 'Hide from users'}
                                                >
                                                    {f.enabled === false ? <EyeOff size={14} /> : <Eye size={14} />}
                                                </button>
                                            )}
                                            <button
                                                onClick={e => { e.stopPropagation(); setDeleteTarget(f); }}
                                                className="text-(--base-06) hover:text-(--error-light) transition-colors"
                                            >
                                                <Trash2 size={14} />
                                            </button>
                                        </div>
                                    </td>
                                </tr>
                                );
                            })}
                        </tbody>
                    </table>
                )}
            </div>

            {/* Create folder popup */}
            {showCreateDir && (
                <div className="modal-overlay animate-fade-in">
                    <ModalPanel className="modal-panel w-80">
                        <div className="modal-header">
                            <h3 className="modal-title">Create Folder</h3>
                        </div>
                        <div className="modal-body">
                            <input
                                autoFocus
                                type="text"
                                value={newDirName}
                                onChange={e => setNewDirName(e.target.value)}
                                onKeyDown={e => e.key === 'Enter' && handleCreateDir()}
                                placeholder="folder-name"
                                className="input-mono w-full"
                            />
                        </div>
                        <div className="modal-footer">
                            <button onClick={() => setShowCreateDir(false)} className="btn btn-secondary">Cancel</button>
                            <button onClick={() => runCreateDir(handleCreateDir)} disabled={creatingDir} className="btn btn-primary disabled:opacity-40">Create</button>
                        </div>
                    </ModalPanel>
                </div>
            )}

            {/* Delete confirm popup */}
            {deleteTarget && (
                <div className="modal-overlay animate-fade-in">
                    <ModalPanel className="modal-panel w-80">
                        <div className="modal-header">
                            <h3 className="modal-title text-(--error-light)">Delete {deleteTarget.is_dir ? 'Folder' : 'File'}?</h3>
                        </div>
                        <div className="modal-body">
                            <p className="text-sm text-(--base-07)">
                                <span className="font-mono text-(--base-09)">{deleteTarget.name}</span> will be permanently deleted.
                            </p>
                        </div>
                        <div className="modal-footer">
                            <button onClick={() => setDeleteTarget(null)} className="btn btn-secondary">Cancel</button>
                            <button onClick={() => runDelete(handleDelete)} disabled={deletingEntry} className="btn btn-danger disabled:opacity-40">Delete</button>
                        </div>
                    </ModalPanel>
                </div>
            )}

            {/* Toast */}
        </div>
    );
}
