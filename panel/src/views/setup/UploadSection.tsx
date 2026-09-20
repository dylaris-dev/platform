"use client";

import React, { useRef, useState, useEffect } from 'react';
import { FileArchive, Folder, X, AlertTriangle, Upload } from 'lucide-react';
import { getSftpCredentials, SftpCredentials, getUserLimits } from '@/lib/api';
import { SkeletonText } from '@/components/Skeleton';

declare const JSZip: any;

function formatSize(bytes: number): string {
    if (bytes >= 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024 * 1024)).toFixed(1)} GB`;
    return `${Math.round(bytes / (1024 * 1024))} MB`;
}

interface UploadSectionProps {
    uploadFile: File | null;
    onFileChange: (file: File | null) => void;
    uploadStructure: 'direct' | 'subfolder';
    onStructureChange: (s: 'direct' | 'subfolder') => void;
    uploadProgress: number;
    uploadStatus: string;
    onStatusChange: (s: string) => void;
    serverId: number;
    onFileTooLarge?: (tooLarge: boolean) => void;
}

export default function UploadSection({
    uploadFile, onFileChange,
    uploadStructure, onStructureChange,
    uploadProgress, uploadStatus, onStatusChange,
    serverId, onFileTooLarge,
}: UploadSectionProps) {
    const uploadInputRef = useRef<HTMLInputElement>(null);
    const folderInputRef = useRef<HTMLInputElement>(null);
    const dropZoneRef = useRef<HTMLDivElement>(null);

    const [uploadLimit, setUploadLimit] = useState<number>(0);
    const [showPicker, setShowPicker] = useState(false);
    const [dragging, setDragging] = useState(false);

    // SFTP
    // null while the answer is still on its way; afterwards either credentials
    // or the reason there are none, so the banner can say which.
    const [sftp, setSftp] = useState<{ creds: SftpCredentials | null; reason: string } | null>(null);

    // Load JSZip CDN
    useEffect(() => {
        if (typeof window !== 'undefined' && (window as any).JSZip) return;
        const script = document.createElement('script');
        script.src = 'https://cdnjs.cloudflare.com/ajax/libs/jszip/3.10.1/jszip.min.js';
        script.async = true;
        document.body.appendChild(script);
        return () => { document.body.removeChild(script); };
    }, []);

    // Load upload limit + SFTP creds on mount
    useEffect(() => {
        getUserLimits().then(res => {
            if (res.success && res.uploadLimit) setUploadLimit(res.uploadLimit);
        }).catch(() => {});
        // Core answers with the fields at the TOP level, not under `credentials`.
        // Reading a key that is never sent left this undefined, so the banner
        // below sat in its skeleton state forever - on every platform, not only
        // where SFTP is off.
        getSftpCredentials(serverId).then(res => {
            if (!res.success) { setSftp({ creds: null, reason: '' }); return; }
            if (res.host) {
                setSftp({ creds: { host: res.host, port: res.port, username: res.username, path: res.path }, reason: '' });
                return;
            }
            setSftp({ creds: null, reason: res.reason || '' });
        }).catch(() => setSftp({ creds: null, reason: '' }));
    }, [serverId]);

    const fileTooLarge = !!(uploadFile && uploadLimit > 0 && uploadFile.size > uploadLimit);

    useEffect(() => {
        onFileTooLarge?.(fileTooLarge);
    }, [fileTooLarge]);

    // Close picker on outside click
    useEffect(() => {
        if (!showPicker) return;
        const handler = (e: MouseEvent) => {
            if (dropZoneRef.current && !dropZoneRef.current.contains(e.target as Node)) {
                setShowPicker(false);
            }
        };
        document.addEventListener('mousedown', handler);
        return () => document.removeEventListener('mousedown', handler);
    }, [showPicker]);

    const handleZipSelect = (file: File) => {
        onStructureChange('direct');
        onFileChange(file);
    };

    const handleFolderSelect = async (files: FileList | null) => {
        if (!files || files.length === 0) return;
        onStatusChange('Zipping folder...');
        const zip = new JSZip();
        for (let i = 0; i < files.length; i++) {
            const f = files[i];
            const path = (f as any).webkitRelativePath || f.name;
            zip.file(path, f);
        }
        const blob = await zip.generateAsync({ type: 'blob' });
        const folderName = ((files[0] as any).webkitRelativePath || '').split('/')[0] || 'server';
        onFileChange(new File([blob], `${folderName}.zip`, { type: 'application/zip' }));
        onStructureChange('subfolder');
        onStatusChange('');
    };

    const handleDrop = (e: React.DragEvent) => {
        e.preventDefault();
        setDragging(false);
        const files = e.dataTransfer.files;
        if (!files || files.length === 0) return;
        const first = files[0];
        if (first.name.endsWith('.zip')) {
            handleZipSelect(first);
        } else {
            handleFolderSelect(files);
        }
    };

    return (
        <div className="space-y-3">
            {/* SFTP Banner */}
            <div className="flex items-center gap-3 px-3 py-2 bg-(--base-02) rounded-md border border-(--base-03)">
                <span className="mono-label shrink-0">SFTP</span>
                {sftp === null ? (
                    <SkeletonText width="w-40" className="h-3" />
                ) : sftp.creds ? (
                    <span className="text-xs font-mono text-(--base-07) truncate">
                        {sftp.creds.host}:{sftp.creds.port} &middot; {sftp.creds.username}
                    </span>
                ) : (
                    <span className="text-xs text-(--base-06) truncate">
                        {sftp.reason === 'external_node_beam_only'
                            ? 'Not available on your own machine. Upload here or use the file manager.'
                            : 'Off on this platform. Upload here or use the file manager.'}
                    </span>
                )}
                {uploadLimit > 0 && (
                    <span className="ml-auto text-[10px] font-mono text-(--base-06) shrink-0">
                        Limit: {formatSize(uploadLimit)}
                    </span>
                )}
            </div>

            {/* Two-column: Structures + Drop Zone */}
            <div className="flex gap-3">
                {/* Left: Structures */}
                <div className="w-[155px] shrink-0 space-y-2.5">
                    <p className="input-label">Structures</p>
                    <div className="space-y-2">
                        <div>
                            <p className="text-[10px] text-(--base-07) mb-0.5">.zip (root)</p>
                            <pre className="text-[10px] text-(--base-06) font-mono leading-[1.4]">
{`├─ server.jar
├─ eula.txt
└─ server.properties`}
                            </pre>
                        </div>
                        <div>
                            <p className="text-[10px] text-(--base-07) mb-0.5">.zip (subfolder)</p>
                            <pre className="text-[10px] text-(--base-06) font-mono leading-[1.4]">
{`└─ my-server/
   ├─ server.jar
   └─ eula.txt`}
                            </pre>
                        </div>
                        <div>
                            <p className="text-[10px] text-(--base-07) mb-0.5">folder (auto-zip)</p>
                            <pre className="text-[10px] text-(--base-06) font-mono leading-[1.4]">
{`├─ server.jar
├─ eula.txt
└─ server.properties`}
                            </pre>
                        </div>
                    </div>
                </div>

                {/* Right: Drop Zone / File Display */}
                <div className="flex-1 min-w-0 flex flex-col gap-2" ref={dropZoneRef}>
                    {!uploadFile ? (
                        <div className="relative flex-1">
                            <div
                                onClick={() => setShowPicker(p => !p)}
                                onDragOver={e => { e.preventDefault(); setDragging(true); }}
                                onDragLeave={() => setDragging(false)}
                                onDrop={handleDrop}
                                className={`flex-1 min-h-[140px] border-2 border-dashed rounded-lg flex flex-col items-center justify-center gap-2 cursor-pointer transition-all ${
                                    dragging
                                        ? 'border-(--accent) bg-(--accent-ghost)'
                                        : 'border-(--base-04) hover:border-(--accent) hover:bg-(--accent-ghost)/50'
                                }`}
                            >
                                <Upload size={22} className="text-(--base-06)" />
                                <p className="text-sm text-(--base-07)">Drop files or click to select</p>
                            </div>

                            {/* Picker Popup */}
                            {showPicker && (
                                <div className="absolute top-1/2 left-1/2 -translate-x-1/2 -translate-y-1/2 z-10 bg-(--base-02) border border-(--base-04) rounded-md shadow-lg overflow-hidden">
                                    <button
                                        type="button"
                                        onClick={() => { setShowPicker(false); uploadInputRef.current?.click(); }}
                                        className="w-full flex items-center gap-2.5 px-4 py-2.5 text-sm text-(--base-08) hover:bg-(--base-04) transition-colors"
                                    >
                                        <FileArchive size={15} className="text-(--accent-light)" />
                                        ZIP file
                                    </button>
                                    <button
                                        type="button"
                                        onClick={() => { setShowPicker(false); folderInputRef.current?.click(); }}
                                        className="w-full flex items-center gap-2.5 px-4 py-2.5 text-sm text-(--base-08) hover:bg-(--base-04) transition-colors border-t border-(--base-03)"
                                    >
                                        <Folder size={15} className="text-(--primary-light)" />
                                        Folder
                                    </button>
                                </div>
                            )}
                        </div>
                    ) : (
                        <div className="space-y-2">
                            <div className={`flex items-center gap-3 p-3 rounded-md border ${
                                fileTooLarge
                                    ? 'bg-(--error)/5 border-(--error)/30'
                                    : 'bg-(--base-02) border-(--base-04)'
                            }`}>
                                <FileArchive size={16} className={fileTooLarge ? 'text-(--error-light)' : 'text-(--accent-light)'} />
                                <div className="flex-1 min-w-0">
                                    <p className="text-sm font-mono text-(--base-09) truncate">{uploadFile.name}</p>
                                    <p className={`text-xs ${fileTooLarge ? 'text-(--error-light) font-medium' : 'text-(--base-06)'}`}>
                                        {formatSize(uploadFile.size)}
                                        {fileTooLarge && ` — exceeds ${formatSize(uploadLimit)} limit`}
                                    </p>
                                </div>
                                <button type="button" onClick={() => onFileChange(null)}
                                    className="text-(--base-06) hover:text-(--error-light) transition-colors">
                                    <X size={16} />
                                </button>
                            </div>

                            {fileTooLarge && (
                                <div className="alert alert-warning text-xs">
                                    <AlertTriangle size={13} className="text-(--warning-light) shrink-0 mt-0.5" />
                                    <p>
                                        File exceeds limit. Use SFTP to upload large files.
                                    </p>
                                </div>
                            )}

                            {uploadStatus && (
                                <div className="space-y-1">
                                    <p className="text-xs text-(--accent-light)">{uploadStatus}</p>
                                    {uploadProgress > 0 && (
                                        <div className="w-full bg-(--base-04) rounded-full h-1.5">
                                            <div className="bg-(--accent) h-1.5 rounded-full transition-all" style={{ width: `${uploadProgress}%` }} />
                                        </div>
                                    )}
                                </div>
                            )}
                        </div>
                    )}
                </div>
            </div>

            {/* Hidden inputs */}
            <input ref={uploadInputRef} type="file" accept=".zip" className="hidden"
                onChange={e => { if (e.target.files?.[0]) handleZipSelect(e.target.files[0]); }} />
            <input ref={folderInputRef} type="file" className="hidden"
                {...{ webkitdirectory: '', directory: '' } as any}
                onChange={e => handleFolderSelect(e.target.files)} />
        </div>
    );
}
