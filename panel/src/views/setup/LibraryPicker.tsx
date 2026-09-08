"use client";

import React from 'react';
import { Folder, Archive } from 'lucide-react';

interface LibraryPickerProps {
    files: any[];
    path: string;
    selectedFile: string;
    onNavigate: (path: string) => void;
    onSelect: (file: string) => void;
}

export default function LibraryPicker({ files, path, selectedFile, onNavigate, onSelect }: LibraryPickerProps) {
    return (
        <div className="border border-(--base-03) rounded-xl p-3 min-h-36 flex-1">
            {path && (
                <button type="button" onClick={() => onNavigate('')} className="text-xs text-(--accent-light) mb-2 hover:underline font-mono">/ root</button>
            )}
            <div className="space-y-1">
                {files.length === 0 && (
                    <p className="text-(--base-07) text-sm text-center py-4">Library is empty. Upload files on the Library page.</p>
                )}
                {files.map((f: any) => {
                    const fullPath = path ? `${path}/${f.name}` : f.name;
                    return (
                        <button
                            key={f.name}
                            type="button"
                            onClick={() => {
                                if (f.is_dir) onNavigate(fullPath);
                                else onSelect(fullPath);
                            }}
                            className={`w-full text-left p-2 rounded-md text-sm flex items-center gap-2 transition-all ${
                                selectedFile === fullPath
                                    ? 'bg-(--accent-ghost) border border-(--accent-border)'
                                    : 'hover:bg-(--base-04) border border-transparent'
                            }`}
                        >
                            {f.is_dir ? <Folder size={14} /> : <Archive size={14} />}
                            <span className="font-mono">{f.name}</span>
                        </button>
                    );
                })}
            </div>
            {selectedFile && (
                <p className="text-xs mt-2 text-(--accent-light) font-mono">Selected: {selectedFile}</p>
            )}
        </div>
    );
}
