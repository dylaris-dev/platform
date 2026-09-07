"use client";

import React, { useEffect, useRef, useState } from 'react';
import { AlertTriangle, FileArchive, Info, Upload, X } from 'lucide-react';
import { getUserLimits } from '@/lib/api';

function formatSize(bytes: number): string {
    if (bytes >= 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024 * 1024)).toFixed(1)} GB`;
    return `${Math.round(bytes / (1024 * 1024))} MB`;
}

// A Dylaris backup archive is a .tar.gz. The check is deliberately loose - it
// only decides which files the picker offers, never whether the import runs. The
// node reads the archive itself and reports what it is.
function looksLikeAnArchive(name: string): boolean {
    const n = name.toLowerCase();
    return n.endsWith('.tar.gz') || n.endsWith('.tgz');
}

interface BackupImportSectionProps {
    file: File | null;
    onFileChange: (file: File | null) => void;
    uploadProgress: number;
    uploadStatus: string;
    onFileTooLarge?: (tooLarge: boolean) => void;
}

export default function BackupImportSection({
    file, onFileChange, uploadProgress, uploadStatus, onFileTooLarge,
}: BackupImportSectionProps) {
    const inputRef = useRef<HTMLInputElement>(null);
    const [uploadLimit, setUploadLimit] = useState<number>(0);
    const [dragging, setDragging] = useState(false);

    useEffect(() => {
        getUserLimits().then(res => {
            if (res.success && res.uploadLimit) setUploadLimit(res.uploadLimit);
        }).catch(() => {});
    }, []);

    const fileTooLarge = !!(file && uploadLimit > 0 && file.size > uploadLimit);
    useEffect(() => { onFileTooLarge?.(fileTooLarge); }, [fileTooLarge]);

    const select = (files: FileList | null) => {
        if (!files || files.length === 0) return;
        onFileChange(files[0]);
    };

    return (
        <div className="space-y-3">
            <div className="alert alert-info text-xs">
                <Info size={13} className="text-(--accent-light) shrink-0 mt-0.5" />
                <p>
                    Restores a downloaded Dylaris backup into this sub-server. Archives made
                    from Dylaris 2026.09.07 onwards describe themselves, so the loader,
                    versions and mod list are set for you. An older archive, or one from
                    somewhere else, still restores its files - you then set the start
                    command and the loader yourself.
                </p>
            </div>

            {!file ? (
                <div
                    onClick={() => inputRef.current?.click()}
                    onDragOver={e => { e.preventDefault(); setDragging(true); }}
                    onDragLeave={() => setDragging(false)}
                    onDrop={e => { e.preventDefault(); setDragging(false); select(e.dataTransfer.files); }}
                    className={`min-h-[140px] border-2 border-dashed rounded-lg flex flex-col items-center justify-center gap-2 cursor-pointer transition-all ${
                        dragging
                            ? 'border-(--accent) bg-(--accent-ghost)'
                            : 'border-(--base-04) hover:border-(--accent) hover:bg-(--accent-ghost)/50'
                    }`}
                >
                    <Upload size={22} className="text-(--base-06)" />
                    <p className="text-sm text-(--base-07)">Drop a backup archive or click to select</p>
                    <p className="text-xs text-(--base-06)">.tar.gz</p>
                </div>
            ) : (
                <div className="space-y-2">
                    <div className={`flex items-center gap-3 p-3 rounded-md border ${
                        fileTooLarge ? 'bg-(--error)/5 border-(--error)/30' : 'bg-(--base-02) border-(--base-04)'
                    }`}>
                        <FileArchive size={16} className={fileTooLarge ? 'text-(--error-light)' : 'text-(--accent-light)'} />
                        <div className="flex-1 min-w-0">
                            <p className="text-sm font-mono text-(--base-09) truncate">{file.name}</p>
                            <p className={`text-xs ${fileTooLarge ? 'text-(--error-light) font-medium' : 'text-(--base-06)'}`}>
                                {formatSize(file.size)}
                                {fileTooLarge && ` — exceeds ${formatSize(uploadLimit)} limit`}
                            </p>
                        </div>
                        <button type="button" onClick={() => onFileChange(null)}
                            aria-label="Remove the selected archive"
                            className="text-(--base-06) hover:text-(--error-light) transition-colors">
                            <X size={16} />
                        </button>
                    </div>

                    {!looksLikeAnArchive(file.name) && !fileTooLarge && (
                        <div className="alert alert-warning text-xs">
                            <AlertTriangle size={13} className="text-(--warning-light) shrink-0 mt-0.5" />
                            <p>
                                This does not look like a .tar.gz archive. The import will fail if it
                                is not one.
                            </p>
                        </div>
                    )}

                    {fileTooLarge && (
                        <div className="alert alert-warning text-xs">
                            <AlertTriangle size={13} className="text-(--warning-light) shrink-0 mt-0.5" />
                            <p>Archive exceeds the upload limit. Use SFTP for large files.</p>
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

            <input ref={inputRef} type="file" accept=".gz,.tgz,.tar.gz" className="hidden"
                onChange={e => select(e.target.files)} />
        </div>
    );
}
