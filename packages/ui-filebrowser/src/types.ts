import type { ReactNode } from 'react';
import type { BeamConnectionMode } from './connectionMode';
import type { DownloadProgressFn } from './downloadTracker';

export interface FileEntry {
  name: string;
  is_dir: boolean;
  size: number;
  path?: string;
  // Set by a host for an entry that is listed but has no bytes to open, such as
  // a reference downloaded elsewhere. The label is shown as a badge; the entry
  // cannot be opened, renamed, copied or downloaded, only deleted.
  tag?: string;
}

export interface FileBrowserAdapter {
  getFiles(path: string, serverUuid?: string): Promise<{ success: boolean; files: FileEntry[]; message?: string }>;
  // readonly: Core marks content that must not be saved back (a decompressed .gz).
  getFileContent(path: string, serverUuid?: string): Promise<{ success: boolean; content: string; message?: string; readonly?: boolean }>;
  saveFile(path: string, content: string, serverUuid?: string): Promise<{ success: boolean; message?: string }>;
  createFile(path: string, isDir: boolean, serverUuid?: string): Promise<{ success: boolean; message?: string }>;
  deleteFile(path: string, serverUuid?: string): Promise<{ success: boolean; message?: string }>;
  renameFile(oldPath: string, newPath: string, serverUuid?: string): Promise<{ success: boolean; message?: string }>;
  copyFile(srcPath: string, dstPath: string, serverUuid?: string): Promise<{ success: boolean; message?: string }>;
  uploadFiles(
    path: string,
    files: FileList,
    onProgress: (progress: number) => void,
    strategy?: string,
    mergeConflict?: string,
    serverUuid?: string
  ): Promise<{ success: boolean; message?: string }>;
  // onProgress may name the file: a native save dialog lets the user pick one,
  // and may say where it was saved.
  downloadFile(path: string, serverUuid?: string, isDir?: boolean, onProgress?: DownloadProgressFn): Promise<void> | void;
  selectiveDownload(basePath: string, selected: string[], selectAll: boolean, serverUuid?: string, onProgress?: DownloadProgressFn): Promise<void> | void;
  getUserLimits(): Promise<{ success: boolean; uploadLimit: number; downloadLimit: number }>;
}

export interface FileBrowserProps {
  rootLabel?: string;
  allowReadOnlyDownloads?: boolean;
  notify?: (message: string, ok: boolean) => void;
  currentServerPath: string;
  serverUuid?: string;
  adapter: FileBrowserAdapter;
  // Read-only mode (e.g. the public demo server). All action buttons stay
  // VISIBLE so users see the full UI, but every mutating action is inert:
  // download/delete/rename/copy/create/upload show a toast instead, and the
  // editor opens for reading with no Save. The backend 403s these anyway
  // (default-deny); this is purely the client-side UX.
  readOnly?: boolean;
  // Active beam transport for the connection-mode badge. Only the Beam Desktop app
  // sets this (via GetConnectionMode); the web Panel uses Core REST and passes
  // nothing, so no badge renders there.
  connectionMode?: BeamConnectionMode | null;
  // Extra buttons at the end of the toolbar, from the host app.
  toolbarActions?: ReactNode;
}
