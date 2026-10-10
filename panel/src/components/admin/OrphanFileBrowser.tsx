"use client";

import React, { useState, useEffect, useCallback } from 'react';
import { Folder, FileText, ChevronRight, ArrowLeft } from 'lucide-react';
import { listOrphanFiles, getOrphanFileContent } from '@/lib/api/orphans';
import { SkeletonList, Skeleton } from '@/components/Skeleton';

interface FileEntry {
  name: string;
  is_dir: boolean;
  size: number;
}

interface OrphanFileBrowserProps {
  nodeId: number;
  uuid: string;
}

function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

function looksLikeBinary(content: string): boolean {
  // Heuristic: more than 1% non-printable chars (excluding common whitespace)
  let nonPrintable = 0;
  const limit = Math.min(content.length, 4096);
  for (let i = 0; i < limit; i++) {
    const code = content.charCodeAt(i);
    if (code === 0) return true; // NUL byte = definitely binary
    if (code < 9 || (code > 13 && code < 32) || code === 127) {
      nonPrintable++;
    }
  }
  return nonPrintable / limit > 0.01;
}

export function OrphanFileBrowser({ nodeId, uuid }: OrphanFileBrowserProps) {
  const [currentPath, setCurrentPath] = useState('');
  const [entries, setEntries] = useState<FileEntry[]>([]);
  const [listLoading, setListLoading] = useState(false);
  const [listError, setListError] = useState('');

  const [openFilePath, setOpenFilePath] = useState('');
  const [fileContent, setFileContent] = useState('');
  const [fileLoading, setFileLoading] = useState(false);
  const [fileError, setFileError] = useState('');

  const loadDirectory = useCallback(async (path: string) => {
    setListLoading(true);
    setListError('');
    setEntries([]);
    const res = await listOrphanFiles(nodeId, uuid, path);
    setListLoading(false);
    if (res.success && Array.isArray(res.files)) {
      // Sort: directories first, then files, both alphabetically
      const sorted = [...res.files].sort((a: FileEntry, b: FileEntry) => {
        if (a.is_dir !== b.is_dir) return a.is_dir ? -1 : 1;
        return a.name.localeCompare(b.name);
      });
      setEntries(sorted);
    } else {
      setListError(res.message ?? 'Failed to load directory.');
    }
  }, [nodeId, uuid]);

  // Load root on mount; reload when path changes
  useEffect(() => {
    loadDirectory(currentPath);
  }, [currentPath, loadDirectory]);

  const navigateInto = (folderName: string) => {
    const next = currentPath ? `${currentPath}/${folderName}` : folderName;
    setOpenFilePath('');
    setCurrentPath(next);
  };

  const navigateUp = () => {
    const idx = currentPath.lastIndexOf('/');
    const parent = idx > 0 ? currentPath.slice(0, idx) : '';
    setOpenFilePath('');
    setCurrentPath(parent);
  };

  const openFile = async (fileName: string) => {
    const filePath = currentPath ? `${currentPath}/${fileName}` : fileName;
    setOpenFilePath(filePath);
    setFileContent('');
    setFileError('');
    setFileLoading(true);
    const res = await getOrphanFileContent(nodeId, uuid, filePath);
    setFileLoading(false);
    if (res.success && typeof res.content === 'string') {
      setFileContent(res.content);
    } else {
      setFileError(res.message ?? 'Failed to load file.');
    }
  };

  const closeFile = () => {
    setOpenFilePath('');
    setFileContent('');
    setFileError('');
  };

  // Breadcrumb segments: ['folder', 'subfolder', ...]
  const pathSegments = currentPath ? currentPath.split('/') : [];

  // ── File viewer ──────────────────────────────────────────────────────────
  if (openFilePath) {
    return (
      <div className="flex flex-col h-full">
        {/* Viewer header */}
        <div className="flex items-center gap-2 px-1 pb-3 border-b border-(--base-03)">
          <button
            onClick={closeFile}
            className="p-1 rounded hover:bg-(--base-03) text-(--base-06) shrink-0"
            title="Back to directory list"
          >
            <ArrowLeft size={14} />
          </button>
          <FileText size={13} className="text-(--accent-light) shrink-0" />
          <span className="font-mono text-[11px] text-(--base-06) break-all leading-tight">
            {openFilePath}
          </span>
        </div>

        {/* File content */}
        <div className="mt-3 flex-1 overflow-hidden">
          {fileLoading ? (
            <Skeleton className="h-40 w-full rounded-(--radius-md)" />
          ) : fileError ? (
            <div className="alert alert-error text-sm">{fileError}</div>
          ) : looksLikeBinary(fileContent) ? (
            <div className="text-xs text-(--base-05) italic px-1">
              Binary file - content not shown.
            </div>
          ) : (
            <pre className="bg-(--base-01) border border-(--base-03) rounded-(--radius-md) p-3 text-[11px] font-mono text-(--base-07) overflow-auto max-h-72 whitespace-pre-wrap break-words leading-relaxed">
              {fileContent || <span className="text-(--base-05) italic">(empty file)</span>}
            </pre>
          )}
        </div>
      </div>
    );
  }

  // ── Directory listing ────────────────────────────────────────────────────
  return (
    <div className="flex flex-col">
      {/* Breadcrumb */}
      <div className="flex items-center gap-1 text-[11px] font-mono text-(--base-05) mb-3 flex-wrap">
        <button
          onClick={() => { setCurrentPath(''); }}
          className="hover:text-(--base-07) transition-colors"
        >
          /
        </button>
        {pathSegments.map((seg, i) => {
          const targetPath = pathSegments.slice(0, i + 1).join('/');
          const isLast = i === pathSegments.length - 1;
          return (
            <React.Fragment key={seg}>
              <ChevronRight size={10} className="text-(--base-05)" />
              {isLast ? (
                <span className="text-(--base-07)">{seg}</span>
              ) : (
                <button
                  onClick={() => setCurrentPath(targetPath)}
                  className="hover:text-(--base-07) transition-colors"
                >
                  {seg}
                </button>
              )}
            </React.Fragment>
          );
        })}
      </div>

      {/* Error */}
      {listError && (
        <div className="alert alert-error text-sm mb-3">{listError}</div>
      )}

      {/* File list */}
      {listLoading ? (
        <SkeletonList rows={5} />
      ) : (
        <div className="border border-(--base-03) rounded-(--radius-md) overflow-hidden">
          {/* Up button */}
          {currentPath && (
            <button
              onClick={navigateUp}
              className="w-full flex items-center gap-2 px-3 py-2 text-xs text-(--base-06) hover:bg-(--base-02) transition-colors border-b border-(--base-03)"
            >
              <ArrowLeft size={12} className="shrink-0" />
              <span className="font-mono">..</span>
            </button>
          )}

          {/* Entries */}
          {entries.length === 0 && !listError && !listLoading ? (
            <div className="px-3 py-4 text-xs text-(--base-05) italic text-center">
              Directory is empty.
            </div>
          ) : (
            entries.map((entry, idx) => (
              <button
                key={entry.name}
                onClick={() => entry.is_dir ? navigateInto(entry.name) : openFile(entry.name)}
                className={`w-full flex items-center gap-2.5 px-3 py-2 text-xs hover:bg-(--base-02) transition-colors text-left ${
                  idx < entries.length - 1 ? 'border-b border-(--base-03)' : ''
                }`}
              >
                {entry.is_dir ? (
                  <Folder size={13} className="text-(--accent-light) shrink-0" />
                ) : (
                  <FileText size={13} className="text-(--base-05) shrink-0" />
                )}
                <span className={`font-mono flex-1 truncate ${entry.is_dir ? 'text-(--base-08)' : 'text-(--base-07)'}`}>
                  {entry.name}
                </span>
                {!entry.is_dir && (
                  <span className="text-(--base-05) text-[10px] shrink-0 tabular-nums">
                    {formatSize(entry.size)}
                  </span>
                )}
                {entry.is_dir && (
                  <ChevronRight size={11} className="text-(--base-05) shrink-0" />
                )}
              </button>
            ))
          )}
        </div>
      )}
    </div>
  );
}
