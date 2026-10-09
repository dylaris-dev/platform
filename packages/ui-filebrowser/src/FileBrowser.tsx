"use client";

import React, { useState, useEffect, useMemo, useRef, useCallback, lazy, Suspense } from 'react';
import JSZip from 'jszip';
import { Folder, FileText, File as FileIcon, Search, Upload, Plus, CornerDownLeft, ExternalLink, FilePen, Pencil, Copy, Download, Trash2, Check, X, ArrowUp, ArrowDown } from 'lucide-react';
import type { FileEntry, FileBrowserProps } from './types';
import { formatBytes, validFilenameRegex, getCopyName, getTruncatedPath, isOpenableFile, isGzipName, findMatches, OPEN_MAX_BYTES } from './utils';
import { useDelayedFlag } from './useDelayedFlag';
import { beamConnectionModeMeta } from './connectionMode';
import Toast from './Toast';
import Breadcrumbs from './Breadcrumbs';
import DownloadProgress from './DownloadProgress';
import SelectiveDownloadModal from './SelectiveDownloadModal';

// Lazy-load the CodeMirror bundle — only pulled in when an edit modal opens.
const CodeMirrorEditor = lazy(() => import('./CodeMirrorEditor'));

type PopupMode = 'create' | 'copy' | 'rename' | null;
type UploadPopupView = 'select' | 'progress' | 'conflict';

const FileBrowser: React.FC<FileBrowserProps> = ({ currentServerPath, serverUuid, adapter, readOnly = false, connectionMode }) => {
  const [files, setFiles] = useState<FileEntry[]>([]);
  const [currentPath, setCurrentPath] = useState(currentServerPath);
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);
  // Only surface "Loading…" if the op runs longer than this — fast
  // folder switches stay silent instead of flashing the indicator.
  const showLoading = useDelayedFlag(loading, 350);
  const [toastMessage, setToastMessage] = useState<{ message: string; type: 'error' | 'info' } | null>(null);
  
  const [globalSearchTerm, setGlobalSearchTerm] = useState('');
  const [isSearching, setIsSearching] = useState(false);
  const [searchResults, setSearchResults] = useState<FileEntry[]>([]);

  const [popupMode, setPopupMode] = useState<PopupMode>(null);
  const [actionTarget, setActionTarget] = useState<FileEntry | null>(null);
  const [newName, setNewName] = useState('');
  const [newFileType, setNewFileType] = useState<'file' | 'folder'>('file');
  const [popupError, setPopupError] = useState('');

  const [showDeleteConfirm, setShowDeleteConfirm] = useState(false);
  const [fileToDelete, setFileToDelete] = useState<FileEntry | null>(null);

  const [showEditPopup, setShowEditPopup] = useState(false);
  const [editingFile, setEditingFile] = useState('');
  const [fileContent, setFileContent] = useState('');
  const [originalFileContent, setOriginalFileContent] = useState('');
  const [editorPopupStyle] = useState({ width: '80vw', maxWidth: '1400px', minWidth: '700px' });
  const [isEditorLoading, setIsEditorLoading] = useState(false);
  const [isEditingEnabled, setIsEditingEnabled] = useState(false);
  // Opened read-only regardless of permissions: a .gz is shown decompressed,
  // and saving that text back would replace the archive with plain text.
  const [fileIsReadOnly, setFileIsReadOnly] = useState(false);
  const [isRenaming, setIsRenaming] = useState(false);
  const [tempFileName, setTempFileName] = useState('');
  const renameInputRef = useRef<HTMLInputElement>(null);
  const [searchTerm, setSearchTerm] = useState('');
  const [searchMatches, setSearchMatches] = useState<number[]>([]);
  const [currentMatchIndex, setCurrentMatchIndex] = useState(-1);
  const [searchJump, setSearchJump] = useState<{ from: number; to: number } | null>(null);
  const lastSearchTermRef = useRef('');


  const [showUploadPopup, setShowUploadPopup] = useState(false);
  const [filesToUpload, setFilesToUpload] = useState<any[]>([]);
  const [isDragging, setIsDragging] = useState(false);
  const [isWindowDragging, setIsWindowDragging] = useState(false);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const folderInputRef = useRef<HTMLInputElement>(null);
  const [uploadProgress, setUploadProgress] = useState<number | null>(null);
  const [uploadStatus, setUploadStatus] = useState('');

  // States for the upload flow
  const [uploadPopupView, setUploadPopupView] = useState<UploadPopupView>('select');
  const [uploadItems, setUploadItems] = useState<{ items: any[], isFolder: boolean, name: string } | null>(null);
  const [conflictFile, setConflictFile] = useState<{file: File, newName: string, isFolderConflict: boolean} | null>(null);

  // Transfer limits
  const [uploadLimit, setUploadLimit] = useState<number>(0);
  const [downloadLimit, setDownloadLimit] = useState<number>(0);

  // Download progress
  const [downloadProgress, setDownloadProgress] = useState<{ filename: string; loaded: number; total: number } | null>(null);

  // Selective download popup
  const [selectiveDownloadTarget, setSelectiveDownloadTarget] = useState<FileEntry | null>(null);
  const [selectiveTree, setSelectiveTree] = useState<Record<string, FileEntry[]>>({});
  const [selectiveExpanded, setSelectiveExpanded] = useState<Set<string>>(new Set());
  const [selectiveChecked, setSelectiveChecked] = useState<Set<string>>(new Set());
  const [selectiveAll, setSelectiveAll] = useState(true);
  const [selectiveLoading, setSelectiveLoading] = useState(false);
  const [selectiveDownloading, setSelectiveDownloading] = useState(false);

  // Inline multi-select for the current folder: checked entry names (files or
  // folders) that a single "Download as ZIP" action bundles via the same
  // selective-download endpoint. Names are bare and folder-scoped, so this is
  // cleared on folder change (see the currentPath effect below) and whenever
  // select mode is left (toggleSelectMode) - opening a file does not clear it.
  const [selected, setSelected] = useState<Set<string>>(new Set());
  // Explicit select mode: checkboxes (their own leading column) only render
  // while this is on, so a normal row click can never mis-click a checkbox.
  const [selectMode, setSelectMode] = useState(false);

  // JSZip is now a bundled dependency (no CDN/SRI/StrictMode concerns); just
  // fetch the transfer limits up front.
  useEffect(() => {
    adapter.getUserLimits().then(res => {
      if (res.success) {
        setUploadLimit(res.uploadLimit);
        setDownloadLimit(res.downloadLimit);
      }
    });
  }, []);

  // Selected names are bare filenames scoped to the current folder, so an
  // actual folder change (subfolder click, breadcrumb, go-up) invalidates
  // them - clear here. Opening a file does not change currentPath, so the
  // selection survives that (intended).
  useEffect(() => {
    setSelected(new Set());
  }, [currentPath]);

  // Checkboxes and the bulk bar are hidden during a global search, so an
  // active select mode would otherwise sit there empty and inert.
  useEffect(() => {
    if (globalSearchTerm) {
      setSelectMode(false);
      setSelected(new Set());
    }
  }, [globalSearchTerm]);


  const fetchFiles = async (path: string): Promise<boolean> => {
    setLoading(true);
    setError('');
    const result = await adapter.getFiles(path, serverUuid);
    if (result.success) {
      setFiles(result.files);
      setCurrentPath(path);
    } else {
      setError(result.message || 'Unknown error');
    }
    setLoading(false);
    return result.success;
  };
  
  // --- Selective Download Helpers ---
  const openSelectiveDownload = async (folder: FileEntry) => {
    setSelectiveDownloadTarget(folder);
    setSelectiveAll(true);
    setSelectiveChecked(new Set());
    setSelectiveExpanded(new Set());
    setSelectiveTree({});
    setSelectiveDownloading(false);
    // Load first level
    const folderPath = currentPath ? `${currentPath}/${folder.name}` : folder.name;
    setSelectiveLoading(true);
    const result = await adapter.getFiles(folderPath, serverUuid);
    if (result.success) {
      setSelectiveTree({ '': result.files });
    }
    setSelectiveLoading(false);
  };

  const toggleSelectiveExpand = async (relativePath: string) => {
    const newExpanded = new Set(selectiveExpanded);
    if (newExpanded.has(relativePath)) {
      newExpanded.delete(relativePath);
    } else {
      newExpanded.add(relativePath);
      // Lazy load if not yet loaded
      if (!selectiveTree[relativePath]) {
        const folderName = selectiveDownloadTarget?.name || '';
        const basePath = currentPath ? `${currentPath}/${folderName}` : folderName;
        const fullPath = relativePath ? `${basePath}/${relativePath}` : basePath;
        const result = await adapter.getFiles(fullPath, serverUuid);
        if (result.success) {
          setSelectiveTree(prev => ({ ...prev, [relativePath]: result.files }));
        }
      }
    }
    setSelectiveExpanded(newExpanded);
  };

  const toggleSelectiveCheck = (relativePath: string) => {
    setSelectiveAll(false);
    const newChecked = new Set(selectiveChecked);
    if (newChecked.has(relativePath)) {
      newChecked.delete(relativePath);
    } else {
      newChecked.add(relativePath);
    }
    setSelectiveChecked(newChecked);
  };

  const handleSelectiveDownload = async () => {
    if (!selectiveDownloadTarget) return;
    setSelectiveDownloading(true);
    setSelectiveDownloadTarget(null);
    const folderName = selectiveDownloadTarget.name;
    const basePath = currentPath ? `${currentPath}/${folderName}` : folderName;
    const filename = `${folderName}.zip`;
    setDownloadProgress({ filename, loaded: 0, total: 0 });
    try {
      const onProgress = (loaded: number, total: number) =>
        setDownloadProgress(prev => prev ? { ...prev, loaded, total: total || prev.total } : null);
      if (selectiveAll) {
        await adapter.selectiveDownload(basePath, [], true, serverUuid, onProgress);
      } else {
        await adapter.selectiveDownload(basePath, Array.from(selectiveChecked), false, serverUuid, onProgress);
      }
    } catch (err) {
      console.error('Selective download failed:', err);
      setToastMessage({ message: 'Download failed.', type: 'error' });
    }
    setDownloadProgress(null);
    setSelectiveDownloading(false);
  };

  const toggleSelected = (name: string) => {
    setSelected(prev => {
      const next = new Set(prev);
      if (next.has(name)) next.delete(name); else next.add(name);
      return next;
    });
  };

  // `picking` is select mode ACTUALLY active: read-only has no bulk download to
  // offer and global search spans folders, so a same-folder selection would be
  // meaningless there. Every select-mode branch below tests this one value, so
  // the row behaviour and the disabled per-row actions can never disagree - a
  // row that selects while its Delete button still deletes is the failure this
  // prevents.
  const picking = selectMode && !readOnly && !globalSearchTerm;

  // Select mode owns the selection lifecycle: turning it on starts empty
  // (already guaranteed since turning it off just cleared it below), turning
  // it off clears whatever was picked so no hidden selection lingers and the
  // bulk bar disappears.
  const toggleSelectMode = () => {
    setSelectMode(prev => {
      const next = !prev;
      if (!next) setSelected(new Set());
      return next;
    });
  };

  // Bundle the checked current-folder entries into one zip via the selective
  // endpoint (base_path = current folder, selected = checked names). Selection
  // is only cleared on success so a failed transfer can be retried.
  const downloadSelectedAsZip = async () => {
    if (selected.size === 0 || downloadProgress) return;
    const names = Array.from(selected);
    setDownloadProgress({ filename: 'selection.zip', loaded: 0, total: 0 });
    try {
      const onProgress = (loaded: number, total: number) =>
        setDownloadProgress(prev => prev ? { ...prev, loaded, total: total || prev.total } : null);
      await adapter.selectiveDownload(currentPath, names, false, serverUuid, onProgress);
      setSelected(new Set());
    } catch (err) {
      console.error('Zip download failed:', err);
      setToastMessage({ message: 'Download failed.', type: 'error' });
    }
    setDownloadProgress(null);
  };

  useEffect(() => {
    fetchFiles(currentServerPath);
  }, [currentServerPath]);

  useEffect(() => {
    if (!showEditPopup || !editingFile) return;
    // A response for a file the user already left must not land in the editor
    // of the one they opened next (and could then be saved over it).
    let stale = false;
    const failLoad = (message: string) => {
      setShowEditPopup(false);
      setIsEditingEnabled(false);
      showToast(message, 'error');
    };
    const loadContent = async () => {
      setIsEditorLoading(true);
      setFileContent('');
      const fullPath = currentPath ? `${currentPath}/${editingFile}` : editingFile;
      try {
        const result = await adapter.getFileContent(fullPath, serverUuid);
        if (stale) return;
        if (!result.success) {
          failLoad(result.message || 'Failed to load file content.');
          return;
        }
        // The Beam relay reads straight from the node, which does not
        // decompress: the gzip magic byte means we got the archive itself.
        if (isGzipName(editingFile) && result.content.charCodeAt(0) === 0x1f) {
          failLoad('This compressed file cannot be shown here. Download it instead.');
          return;
        }
        setFileContent(result.content);
        setOriginalFileContent(result.content);
        setFileIsReadOnly(isGzipName(editingFile) || result.readonly === true);
      } catch {
        if (!stale) failLoad('Failed to load file content.');
      } finally {
        if (!stale) setIsEditorLoading(false);
      }
    };
    loadContent();
    return () => { stale = true; };
  }, [showEditPopup, editingFile, currentPath]);

  useEffect(() => {
    if (!showEditPopup) return;
    const previous = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    return () => {
      document.body.style.overflow = previous;
    };
  }, [showEditPopup]);
  
   useEffect(() => {
    const handleDragEnter = (e: DragEvent) => {
        e.preventDefault();
        setIsWindowDragging(true);
    };

    const handleDragLeave = (e: DragEvent) => {
        e.preventDefault();
        if (e.relatedTarget === null) {
            setIsWindowDragging(false);
        }
    };

    const handleDrop = (e: DragEvent) => {
        e.preventDefault();
        setIsWindowDragging(false);
    };

    const handleDragOver = (e: DragEvent) => e.preventDefault();
    window.addEventListener('dragenter', handleDragEnter);
    window.addEventListener('dragleave', handleDragLeave);
    window.addEventListener('drop', handleDrop);
    window.addEventListener('dragover', handleDragOver);

    return () => {
        window.removeEventListener('dragenter', handleDragEnter);
        window.removeEventListener('dragleave', handleDragLeave);
        window.removeEventListener('drop', handleDrop);
        window.removeEventListener('dragover', handleDragOver);
    };
  }, []);
  
  const closeAllPopups = useCallback(() => {
    setShowEditPopup(false);
    // Same reset as closeEditPopup, or the next file opened in edit mode.
    setIsEditingEnabled(false);
    setSearchTerm('');
    setIsRenaming(false);
    setShowUploadPopup(false);
    setShowDeleteConfirm(false);
    setPopupMode(null);
  }, []);

  useEffect(() => {
    const handleKeyDown = (e: KeyboardEvent) => {
        if (e.key === 'Escape') {
            closeAllPopups();
        }
    };

    window.addEventListener('keydown', handleKeyDown);
    return () => window.removeEventListener('keydown', handleKeyDown);
  }, [closeAllPopups]);
  
    // Cap depth + result count so a broad global search can't fan out into an
    // unbounded client-driven crawl of the whole tree.
    const handleRecursiveSearch = useCallback(async (path: string, term: string, depth = 0): Promise<FileEntry[]> => {
        let results: FileEntry[] = [];
        if (depth > 6) return results;
        const response = await adapter.getFiles(path, serverUuid);

        if (response.success) {
            for (const file of response.files) {
                if (results.length >= 500) break;
                const fullPath = path ? `${path}/${file.name}` : file.name;
                if (file.name.toLowerCase().includes(term.toLowerCase())) {
                    results.push({ ...file, path: fullPath });
                }
                if (file.is_dir) {
                    const subResults = await handleRecursiveSearch(fullPath, term, depth + 1);
                    results = results.concat(subResults);
                }
            }
        }
        return results;
    }, [adapter, serverUuid]);

    const searchGenRef = useRef(0);
    useEffect(() => {
        // Generation guard: a slow recursive walk must not overwrite the
        // results of a newer search started after the term or path changed.
        const gen = ++searchGenRef.current;
        const search = async () => {
            // Require >=2 chars so a single keystroke can't trigger a full-tree crawl.
            if (globalSearchTerm.length >= 2) {
                setIsSearching(true);
                const results = await handleRecursiveSearch(currentPath, globalSearchTerm);
                if (gen !== searchGenRef.current) return;
                setSearchResults(results);
                setIsSearching(false);
            } else {
                setSearchResults([]);
            }
        };
        const timer = setTimeout(() => {
          search();
        }, 300); // Debounce search

        return () => clearTimeout(timer);
    }, [globalSearchTerm, currentPath, handleRecursiveSearch]);

  const handleRenameClick = () => {
    if (blockReadOnly()) return;
    setTempFileName(editingFile);
    setIsRenaming(true);
    setTimeout(() => renameInputRef.current?.focus(), 0);
  };

  const handleRenameSubmit = async () => {
    if (!validFilenameRegex.test(tempFileName)) {
      showToast("Invalid filename.", 'error');
      return;
    }
    const oldPath = currentPath ? `${currentPath}/${editingFile}` : editingFile;
    const newPath = currentPath ? `${currentPath}/${tempFileName}` : tempFileName;
    
    const result = await adapter.renameFile(oldPath, newPath, serverUuid);
    if(result.success){
        setEditingFile(tempFileName);
        setIsRenaming(false);
        fetchFiles(currentPath);
    } else {
        showToast(result.message || 'Operation failed', 'error');
    }
  };


  const showToast = (message: string, type: 'error' | 'info' = 'info') => {
    setToastMessage({ message, type });
    setTimeout(() => {
      setToastMessage(null);
    }, 3000);
  };

  // Read-only guard: in demo mode every mutating button stays visible but does
  // nothing except surface this toast. Returns true when the action was blocked.
  const blockReadOnly = (): boolean => {
    if (readOnly) {
      showToast('Read-only demo. Changes are disabled here.', 'info');
      return true;
    }
    return false;
  };

  // `file` is the clicked entry itself, so its size is known even for a
  // global-search hit that is not in the current folder's list.
  const openFile = (file: FileEntry) => {
    if (!isOpenableFile(file.name)) {
      showToast("This file type cannot be opened.", 'error');
      return;
    }
    if (file.size > OPEN_MAX_BYTES) {
      showToast("File is too large to open here (over 10 MB). Download it instead.", 'error');
      return;
    }
    setIsEditorLoading(true);
    setIsEditingEnabled(false);
    setFileIsReadOnly(isGzipName(file.name));
    setEditingFile(file.name);
    setShowEditPopup(true);
  };

  // goToFolder: the "Go to folder" button on a search hit, which lands in the
  // folder holding it instead of opening it.
  const handleFileClick = async (file: FileEntry, goToFolder = false) => {
    if (file.path) {
      const parentPath = file.path.substring(0, file.path.lastIndexOf('/'));
      setGlobalSearchTerm('');
      if (goToFolder) {
        fetchFiles(parentPath);
      } else if (file.is_dir) {
        fetchFiles(file.path);
      } else if (await fetchFiles(parentPath) && isOpenableFile(file.name)) {
        openFile(file);
      }
      return;
    }
    if (file.is_dir) {
      fetchFiles(currentPath ? `${currentPath}/${file.name}` : file.name);
    } else {
      openFile(file);
    }
  };

  const handleGoUp = () => {
    if (currentPath) {
      const pathParts = currentPath.split('/');
      pathParts.pop();
      const parentPath = pathParts.join('/');
      fetchFiles(parentPath);
    }
  };
  
  const closeEditPopup = () => {
      setShowEditPopup(false);
      setFileIsReadOnly(false);
      setIsEditingEnabled(false);
      setSearchTerm('');
      setIsRenaming(false);
  }

  const handleSaveFile = async (e: React.FormEvent) => {
    e.preventDefault();
    if (fileIsReadOnly || loading) return;
    setLoading(true);
    const fullPath = currentPath ? `${currentPath}/${editingFile}` : editingFile;
    try {
      const result = await adapter.saveFile(fullPath, fileContent, serverUuid);
      if (result.success) {
        setOriginalFileContent(fileContent);
        closeEditPopup();
        setError('');
      } else {
        showToast(result.message || 'Operation failed', 'error');
      }
    } catch {
      showToast('Failed to save the file. Your changes are still in the editor.', 'error');
    } finally {
      setLoading(false);
    }
  };
  
  const handleSearchChange = (e: React.ChangeEvent<HTMLInputElement>) => {
      setSearchTerm(e.target.value);
  }

  // Debounced: a full rescan of a 10 MB file on every keystroke stalls typing.
  // Only a new term jumps to the first match; an edit to the content keeps the
  // position and must not pull the cursor away from where the user types.
  useEffect(() => {
      const timer = setTimeout(() => {
          const matches = findMatches(fileContent, searchTerm);
          const termChanged = lastSearchTermRef.current !== searchTerm;
          lastSearchTermRef.current = searchTerm;
          setSearchMatches(matches);
          if (termChanged) {
              setCurrentMatchIndex(matches.length > 0 ? 0 : -1);
              if (matches.length > 0) setSearchJump({ from: matches[0], to: matches[0] + searchTerm.length });
          } else {
              setCurrentMatchIndex(prev => Math.min(prev, matches.length - 1));
          }
      }, 200);
      return () => clearTimeout(timer);
  }, [searchTerm, fileContent]);

  const goToMatch = (delta: number) => {
    const n = searchMatches.length;
    if (n === 0) return;
    const next = currentMatchIndex < 0 ? 0 : (currentMatchIndex + delta + n) % n;
    setCurrentMatchIndex(next);
    setSearchJump({ from: searchMatches[next], to: searchMatches[next] + searchTerm.length });
  };

  const handleDeleteClick = (e: React.MouseEvent, file: FileEntry) => {
    e.stopPropagation();
    if (blockReadOnly()) return;
    setFileToDelete(file);
    setShowDeleteConfirm(true);
  };

  const handleConfirmDelete = async () => {
    if (!fileToDelete) return;
    setLoading(true);
    const fullPath = currentPath ? `${currentPath}/${fileToDelete.name}` : fileToDelete.name;
    const result = await adapter.deleteFile(fullPath, serverUuid);
    setShowDeleteConfirm(false);
    setFileToDelete(null);
    if (result.success) {
      fetchFiles(currentPath);
      setError('');
    } else {
      setError(result.message || 'Unknown error');
    }
    setLoading(false);
  };

  const closePopup = () => {
    setPopupMode(null);
    setActionTarget(null);
    setNewName('');
    setPopupError('');
  };

  const handlePopupSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!validFilenameRegex.test(newName)) {
      setPopupError("Invalid name. Use letters, numbers, dot, underscore, hyphen.");
      return;
    }
    setLoading(true);
    setPopupError('');
    let result;
    const oldPath = actionTarget ? (actionTarget.path || (currentPath ? `${currentPath}/${actionTarget.name}` : actionTarget.name)) : '';
    const newPath = oldPath.substring(0, oldPath.lastIndexOf('/') + 1) + newName;

    switch (popupMode) {
      case 'create': result = await adapter.createFile(newPath, newFileType === 'folder', serverUuid); break;
      case 'copy': result = await adapter.copyFile(oldPath, newPath, serverUuid); break;
      case 'rename': result = await adapter.renameFile(oldPath, newPath, serverUuid); break;
    }
    if (result && result.success) {
      closePopup();
      fetchFiles(currentPath);
    } else if (result) {
      setPopupError(result.message || 'Operation failed');
    }
    setLoading(false);
  };

  const handleDragOver = (e: React.DragEvent<HTMLDivElement>) => { e.preventDefault(); setIsDragging(true); };
  const handleDragLeave = (e: React.DragEvent<HTMLDivElement>) => { e.preventDefault(); setIsDragging(false); };
  const handleDrop = (e: React.DragEvent<HTMLDivElement>) => {
    e.preventDefault();
    setIsDragging(false);
    if(e.dataTransfer.items) {
      setFilesToUpload(Array.from(e.dataTransfer.items)); 
    }
  };
  const handleFileSelect = (e: React.ChangeEvent<HTMLInputElement>) => {
    if(e.target.files) {
      setFilesToUpload(Array.from(e.target.files));
    }
  };
  
  const startUploadProcess = async () => {
    if (!filesToUpload || filesToUpload.length === 0) {
      setPopupError("Please select files or a folder.");
      return;
    }
    setPopupError('');
    const firstItem = filesToUpload[0];
    const isFolder = firstItem.webkitGetAsEntry?.()?.isDirectory || (firstItem.webkitRelativePath && firstItem.webkitRelativePath.includes('/'));
    
    // For folders we must keep the original entries (webkitGetAsEntry /
    // createReader only work on the live drop items). For single files we
    // materialize the File NOW: getAsFile() on a DataTransferItem is only
    // reliable during the drop event, so deferring it to executeUpload (after
    // React re-renders) can return null and yield a broken `new File([null])`.
    let originalName = '';
    let itemsForUpload: any[] = filesToUpload;
    if (isFolder) {
      originalName = firstItem.webkitRelativePath ? firstItem.webkitRelativePath.split('/')[0] : firstItem.webkitGetAsEntry().name;
    } else {
      const fileObjects = await Promise.all(
        Array.from(filesToUpload).map(async (item: any) => item.kind === 'file' ? item.getAsFile() : item)
      );
      if (fileObjects.length > 1) {
        setPopupError("For simplicity, please upload multiple files as a single zip archive.");
        return;
      }
      if (!fileObjects[0]) {
        setPopupError("Could not read the dropped file. Please pick it again.");
        return;
      }
      originalName = fileObjects[0].name;
      itemsForUpload = fileObjects;
    }

    const finalName = originalName.replace(/[^a-zA-Z0-9._-]/g, '_');

    // Check upload size limit
    if (uploadLimit > 0) {
      const totalSize = Array.from(itemsForUpload).reduce((sum: number, f: any) => sum + (f.size || 0), 0);
      if (totalSize > uploadLimit) {
        const limitStr = uploadLimit >= 1024 * 1024 * 1024
          ? `${(uploadLimit / (1024 * 1024 * 1024)).toFixed(1)} GB`
          : `${Math.round(uploadLimit / (1024 * 1024))} MB`;
        setPopupError(`File size exceeds upload limit of ${limitStr}.`);
        return;
      }
    }

    // Save items for potential retries from conflict resolution
    setUploadItems({ items: itemsForUpload, isFolder, name: finalName });

    executeUpload(finalName, itemsForUpload, isFolder);
  };

  const executeUpload = async (finalName: string, items: any[], isFolder: boolean, strategy: 'check' | 'replace' | 'merge' = 'check', mergeConflictStrategy? : 'replace' | 'ignore') => {
    if (!validFilenameRegex.test(finalName)) {
      setPopupError("Invalid filename. Only letters, numbers, dot, underscore, and hyphen are allowed.");
      setUploadPopupView('select');
      return;
    }

    if (strategy === 'check') {
        const conflictObject = isFolder ? files.find(f => f.name === finalName && f.is_dir) : files.find(f => f.name === finalName && !f.is_dir);
        if (conflictObject) {
            setConflictFile({ file: new File([], finalName), newName: `new_${finalName}`, isFolderConflict: isFolder });
            setUploadPopupView('conflict');
            return;
        }
    }

    setLoading(true);
    setPopupError('');
    setUploadPopupView('progress');
    setUploadStatus('Preparing upload...');
    setUploadProgress(0);
    
    const filesToFileList = (files: File[]): FileList => {
      const dataTransfer = new DataTransfer();
      files.forEach(file => dataTransfer.items.add(file));
      return dataTransfer.files;
    };
    
    try {
      let filesToActuallyUpload: File[];

      if (isFolder) {
        setUploadStatus(`Zipping ${finalName}...`);
        const zip = new JSZip();
        
        const traverseFileTree = async (entry: any, currentZipFolder: any): Promise<void> => {
          if (entry.isFile) {
            const file = await new Promise<File>(resolve => entry.file(resolve));
            if (file.name === ".DS_Store") return;
            currentZipFolder.file(file.name, file);
          } else if (entry.isDirectory) {
            const newFolder = currentZipFolder.folder(entry.name);
            const dirReader = entry.createReader();
            let directoryEntries: any[] = [];
            let readEntries: any[] = await new Promise(resolve => dirReader.readEntries(resolve));
            while (readEntries.length > 0) {
              directoryEntries.push(...readEntries);
              readEntries = await new Promise(resolve => dirReader.readEntries(resolve));
            }
            await Promise.all(directoryEntries.map((dirEntry: any) => traverseFileTree(dirEntry, newFolder)));
          }
        };
        
        if (items[0].webkitRelativePath) { 
            const topLevelFolder = items[0].webkitRelativePath.split('/')[0];
            const rootZipFolder = (strategy === 'merge') ? zip : zip.folder(finalName);

            for (const file of items) {
                if (file.name === ".DS_Store") continue;
                const pathInZip = (strategy === 'merge')
                    ? file.webkitRelativePath.substring(topLevelFolder.length + 1)
                    : file.webkitRelativePath.substring(topLevelFolder.length + 1); 
                
                if (pathInZip) {
                    rootZipFolder!.file(pathInZip, file);
                }
            }
        } else {
            const entry = items[0].webkitGetAsEntry();
            const rootZipFolder = (strategy === 'merge') ? zip : zip.folder(finalName);
            
            const dirReader = entry.createReader();
            let allEntries: any[] = [];
            let readResults: any[] = await new Promise(res => dirReader.readEntries(res));
            while(readResults.length > 0){
                allEntries.push(...readResults);
                readResults = await new Promise(res => dirReader.readEntries(res));
            }
            await Promise.all(allEntries.map((dirEntry: any) => traverseFileTree(dirEntry, rootZipFolder)));
        }
        
        const content = await zip.generateAsync({ type: "blob" }, (metadata: { percent: number }) => {
          setUploadProgress(Math.round(metadata.percent));
        });
        filesToActuallyUpload = [new File([content], `${finalName}.zip`)];
      } else {
        // items are already materialized File objects (startUploadProcess
        // calls getAsFile during the drop event), so just re-wrap with the
        // sanitized name.
        filesToActuallyUpload = items.map((file: any) => new File([file], finalName, { type: file.type }));
      }

      setUploadStatus('Uploading...');
      const apiStrategy = strategy === 'check' ? undefined : strategy;
      const result = await adapter.uploadFiles(currentPath, filesToFileList(filesToActuallyUpload), setUploadProgress, apiStrategy, mergeConflictStrategy, serverUuid);
      if (result.success) {
        closeUploadPopup();
        fetchFiles(currentPath);
      } else {
        setPopupError(result.message || 'Operation failed');
        setUploadPopupView('select');
      }
    } catch (err: any) {
      setPopupError(err.message || 'An unexpected error occurred.');
      setUploadPopupView('select');
    } finally {
      setLoading(false);
      setUploadProgress(null);
    }
  };


  const closeUploadPopup = () => {
    setShowUploadPopup(false);
    setFilesToUpload([]);
    setPopupError('');
    setUploadProgress(null);
    setUploadStatus('');
    setUploadItems(null);
    setConflictFile(null);
    setUploadPopupView('select');
  };

  const handleNameChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    const sanitized = e.target.value.replace(/[^a-zA-Z0-9._-]/g, '');
    setNewName(sanitized);
  };


  const getPopupTitle = () => {
    switch (popupMode) {
      case 'create': return 'Create New';
      case 'copy': return `Copy ${actionTarget?.name}`;
      case 'rename': return `Rename ${actionTarget?.name}`;
      default: return '';
    }
  };
  
  const sortedFiles = useMemo(() => {
    const filesToDisplay = globalSearchTerm ? searchResults : files;
    
    return [...filesToDisplay].sort((a, b) => {
        if (a.is_dir && !b.is_dir) return -1;
        if (!a.is_dir && b.is_dir) return 1;
        return a.name.localeCompare(b.name);
    });
  }, [files, globalSearchTerm, searchResults]);
  
  const renderFileRepresentation = (file: FileEntry) => {
    if (file.is_dir) {
      return <Folder size={36} className="mr-3 text-(--primary-light)" />;
    }

    if (isOpenableFile(file.name)) {
        return <FileText size={36} className="mr-3 text-(--primary-light)" />;
    }

    return <FileIcon size={36} className="mr-3 text-(--primary-light)" />;
  }
  

  return (
    <div className="p-6 card">
      <style>{`
        @keyframes slide-in-and-fade-out {
            0% { transform: translate(-50%, 100%); opacity: 0; }
            10%, 90% { transform: translate(-50%, 0); opacity: 1; }
            100% { transform: translate(-50%, 100%); opacity: 0; }
        }
        .animate-toast { animation: slide-in-and-fade-out 3s ease-in-out forwards; }
      `}</style>
      <div className="flex items-center gap-3 mb-4">
        <div className="flex-1 min-w-0"><Breadcrumbs currentPath={currentPath} onNavigate={fetchFiles} /></div>
        {connectionMode && (
          <span
            title={beamConnectionModeMeta(connectionMode).description}
            className="shrink-0 inline-flex items-center gap-1.5 rounded-md border border-(--base-04) bg-(--base-03) px-2.5 h-[37px] font-mono text-[10px] uppercase tracking-[0.08em] text-(--base-07) cursor-help"
          >
            <span className="w-1.5 h-1.5 rounded-full bg-(--accent)" aria-hidden="true" />
            {beamConnectionModeMeta(connectionMode).label}
          </span>
        )}
        <label className="flex items-center gap-2 bg-(--base-03) border border-(--base-04) rounded-md px-3 w-56 h-[37px] shrink-0 cursor-text transition-[border-color,box-shadow] focus-within:border-(--accent) focus-within:shadow-[0_0_0_3px_rgba(112,72,200,0.15)]">
            <Search size={16} className="text-(--base-07) shrink-0" />
            <input
                type="text"
                placeholder="Search all files..."
                value={globalSearchTerm}
                onChange={(e) => setGlobalSearchTerm(e.target.value)}
                className="flex-1 min-w-0 bg-transparent outline-none text-sm text-(--base-09) placeholder:text-(--base-05)"
            />
        </label>
        <div className="flex gap-1.5 shrink-0">
          <button title="Upload" onClick={() => { if (blockReadOnly()) return; setShowUploadPopup(true); }} className="btn btn-secondary p-2">
            <Upload size={20} />
          </button>
          <button title="New File/Folder" onClick={() => { if (blockReadOnly()) return; setPopupMode('create'); setNewName(''); setPopupError(''); }} className="btn btn-secondary p-2">
            <Plus size={20} />
          </button>
          {!readOnly && !globalSearchTerm && (
            <button
              title="Toggle select mode"
              onClick={toggleSelectMode}
              className={`btn ${selectMode ? 'btn-primary' : 'btn-secondary'} px-3 h-[37px] text-sm`}
            >
              {selectMode ? 'Done' : 'Select'}
            </button>
          )}
        </div>
      </div>
      
      {isSearching && <p className="text-center text-xl text-(--warning)">Searching...</p>}
      {showLoading && !showUploadPopup && <p className="text-center text-xl text-(--warning)">Loading...</p>}
      {error && <p className="text-(--error) text-center text-xl mb-4">{error}</p>}
      {/* Rows carry no checkbox any more, so the mode has to say what a click
          does now - otherwise a user in select mode just sees folders that
          stopped opening. */}
      {picking && selected.size === 0 && (
        <div className="mb-3 px-3 py-2 rounded-md bg-(--base-03) border border-(--base-04) text-sm text-(--base-07)">
          Click entries to select them. Row actions are paused until you leave select mode.
        </div>
      )}
      {picking && selected.size > 0 && (
        <div className="flex items-center justify-between gap-3 mb-3 px-3 py-2 rounded-md bg-(--accent-ghost) border border-(--accent-border)">
          <span className="text-sm font-medium text-(--accent-light)">{selected.size} selected</span>
          <div className="flex items-center gap-2">
            <button onClick={() => setSelected(new Set())} className="btn btn-secondary btn-sm">Clear</button>
            <button
              onClick={downloadSelectedAsZip}
              disabled={!!downloadProgress}
              className="btn btn-primary btn-sm disabled:opacity-40 disabled:cursor-not-allowed"
            >
              <Download size={14} /> Download as ZIP
            </button>
          </div>
        </div>
      )}
      <ul className="space-y-2">
        {currentPath && !globalSearchTerm && !picking && (
          <li onClick={handleGoUp} className="server-row justify-between">
            <span className="flex flex-1 items-center min-w-0 truncate pr-4 text-(--base-09)">
              <CornerDownLeft size={36} className="mr-3 text-(--primary-light)" />
               <span className="text-lg">..</span>
            </span>
          </li>
        )}
        {sortedFiles.map((file) => {
          const isPicked = picking && selected.has(file.name);
          return (
          <li
            key={file.path || file.name}
            onClick={() => picking
              ? toggleSelected(file.name)
              : handleFileClick(file)}
            aria-selected={picking ? selected.has(file.name) : undefined}
            className={`server-row justify-between ${isPicked ? 'border-(--accent) bg-(--accent-ghost)' : ''}`}
          >
            <span className={`flex items-center flex-1 min-w-0 truncate pr-4 ${isPicked ? 'text-(--accent-light)' : 'text-(--base-09)'}`}>
              {renderFileRepresentation(file)}
              <div>
                  <span className="text-lg">{file.name}</span>
                  {file.path && file.path !== file.name && <span className="text-xs text-(--base-07) block">{getTruncatedPath(file.path)}</span>}
              </div>
            </span>
            <div className="flex items-center space-x-1 text-sm shrink-0">
              <span className="text-(--base-09) hidden sm:inline mr-2">{formatBytes(file.size)}</span>
              {file.path && file.path !== file.name && (
                <button title="Go to folder" disabled={picking} onClick={(e) => { e.stopPropagation(); handleFileClick(file, true) }} className="disabled:opacity-30 disabled:cursor-not-allowed disabled:hover:bg-transparent disabled:hover:text-(--primary-light) p-2 flex items-center justify-center text-(--primary-light) rounded-md transition-colors hover:bg-(--accent) hover:text-white">
                    <ExternalLink size={18} />
                </button>
              )}
              <button title="Rename" disabled={picking} onClick={(e) => { e.stopPropagation(); if (blockReadOnly()) return; setActionTarget(file); setNewName(file.name); setPopupMode('rename'); }} className="p-2 flex items-center justify-center text-(--primary-light) rounded-md transition-colors hover:bg-(--accent) hover:text-white disabled:opacity-30 disabled:cursor-not-allowed disabled:hover:bg-transparent disabled:hover:text-(--primary-light)">
                  <FilePen size={18} />
              </button>
              <button title="Copy" disabled={picking} onClick={(e) => { e.stopPropagation(); if (blockReadOnly()) return; setActionTarget(file); setNewName(getCopyName(file.name, file.is_dir, files)); setPopupMode('copy'); setPopupError(''); }} className="p-2 flex items-center justify-center text-(--primary-light) rounded-md transition-colors hover:bg-(--accent) hover:text-white disabled:opacity-30 disabled:cursor-not-allowed disabled:hover:bg-transparent disabled:hover:text-(--primary-light)">
                <Copy size={18} />
              </button>
              <button title="Download" disabled={picking || !!downloadProgress} onClick={async (e) => {
                e.stopPropagation();
                if (blockReadOnly()) return;
                // Single-flight: ignore clicks while a download is already
                // running so a double-click can't launch overlapping downloads.
                if (downloadProgress) return;
                if (downloadLimit > 0 && file.size > downloadLimit) {
                  const limitStr = downloadLimit >= 1024 * 1024 * 1024 ? `${(downloadLimit / (1024 * 1024 * 1024)).toFixed(1)} GB` : `${Math.round(downloadLimit / (1024 * 1024))} MB`;
                  setToastMessage({ message: `File size exceeds download limit of ${limitStr}.`, type: 'error' }); return;
                }
                if (file.is_dir) {
                  openSelectiveDownload(file);
                } else {
                  const filePath = `${currentPath ? `${currentPath}/` : ''}${file.name}`;
                  setDownloadProgress({ filename: file.name, loaded: 0, total: file.size });
                  try {
                    await adapter.downloadFile(filePath, serverUuid, false, (loaded, total) =>
                      setDownloadProgress(prev => prev ? { ...prev, loaded, total: total || prev.total } : null)
                    );
                  } catch (err) {
                    console.error('Download failed:', err);
                    setToastMessage({ message: 'Download failed.', type: 'error' });
                  }
                  setDownloadProgress(null);
                }
              }} className="p-2 flex items-center justify-center text-(--primary-light) rounded-md transition-colors hover:bg-(--accent) hover:text-white disabled:opacity-40 disabled:cursor-not-allowed disabled:hover:bg-transparent disabled:hover:text-(--primary-light)">
                <Download size={18} />
              </button>
              <button title="Delete" disabled={picking} onClick={(e) => handleDeleteClick(e, file)} className="p-2 flex items-center justify-center text-(--error) rounded-md transition-colors hover:bg-(--error) hover:text-white disabled:opacity-30 disabled:cursor-not-allowed disabled:hover:bg-transparent disabled:hover:text-(--error)">
                <Trash2 size={18} />
              </button>
            </div>
          </li>
          );
        })}
      </ul>

      {popupMode && (
        <div className="modal-overlay animate-fade-in">
          <div className="modal-panel w-full max-w-sm">
            <div className="modal-header">
              <h2 className="modal-title">{getPopupTitle()}</h2>
            </div>
            <div className="modal-body">
              {popupError && <p className="text-(--error-light) text-sm mb-3">{popupError}</p>}
              <form onSubmit={handlePopupSubmit}>
                <div className="flex flex-col gap-[5px] mb-4">
                  <label className="input-label">Name</label>
                  <input type="text" value={newName} onChange={handleNameChange} className="input-field w-full" />
                </div>
                {popupMode === 'create' && (
                  <div className="flex flex-col gap-[5px] mb-4">
                    <label className="input-label">Type</label>
                    <select value={newFileType} onChange={e => setNewFileType(e.target.value as 'file' | 'folder')} className="input-field w-full">
                      <option value="file">File</option>
                      <option value="folder">Folder</option>
                    </select>
                  </div>
                )}
                <div className="flex justify-end gap-2 mt-4">
                  <button type="button" onClick={closePopup} className="btn btn-secondary px-4 py-2 text-sm">Cancel</button>
                  <button type="submit" className="btn btn-primary px-4 py-2 text-sm">Confirm</button>
                </div>
              </form>
            </div>
          </div>
        </div>
      )}

      {showDeleteConfirm && (
        <div className="modal-overlay animate-fade-in">
          <div className="modal-panel w-full max-w-md">
            <div className="modal-header">
              <h2 className="modal-title text-(--error-light)">Confirm Deletion</h2>
            </div>
            <div className="modal-body">
              <p className="text-sm text-(--base-08)">Are you sure you want to delete <span className="font-medium text-(--warning-light)">{fileToDelete?.name}</span>? This action cannot be undone.</p>
            </div>
            <div className="modal-footer">
              <button onClick={() => setShowDeleteConfirm(false)} className="btn btn-secondary px-5 py-2 text-sm">Cancel</button>
              <button onClick={handleConfirmDelete} className="btn btn-danger px-5 py-2 text-sm">Delete</button>
            </div>
          </div>
        </div>
      )}

      {showUploadPopup && (
        <div className="modal-overlay animate-fade-in">
          <div className="modal-panel w-full max-w-2xl">
            <div className="modal-header">
              <h2 className="modal-title">Upload</h2>
            </div>
            <div className="modal-body">
            
            {uploadPopupView === 'select' && (
              <>
                <p className="text-sm mb-4 text-(--base-08)">Upload to: <span className="font-mono text-(--accent-light)">/{currentPath || 'servers'}</span></p>
                <div 
                  onDragOver={handleDragOver} 
                  onDragLeave={handleDragLeave} 
                  onDrop={handleDrop} 
                  className={`p-10 border-2 border-dashed rounded-xl transition-all duration-300 bg-(--base-03) ${isDragging ? 'border-(--accent)' : 'border-(--base-05)'} ${isWindowDragging ? 'scale-105 border-(--accent)' : ''}`}
                >
                    <input type="file" multiple ref={fileInputRef} onChange={handleFileSelect} className="hidden" />
                    {/* @ts-ignore */}
                    <input type="file" ref={folderInputRef} onChange={handleFileSelect} className="hidden" multiple webkitdirectory="" directory="" />
                    
                    <div className="text-center">
                        <p className="text-sm text-(--base-07) mb-4">Drag & Drop files or a folder here</p>
                        <div className="flex justify-center gap-3">
                            <button onClick={() => fileInputRef.current?.click()} className="btn btn-secondary px-5 py-2 text-sm">Select Files</button>
                            <button onClick={() => folderInputRef.current?.click()} className="btn btn-secondary px-5 py-2 text-sm">Select Folder</button>
                        </div>
                    </div>
                </div>

                {filesToUpload && filesToUpload.length > 0 && (
                  <div className="mt-4">
                    <h3 className="input-label">Selected:</h3>
                     <div className="max-h-32 overflow-y-auto bg-(--base-03)/50 p-2 rounded-md">
                        {filesToUpload[0].webkitGetAsEntry?.().isDirectory || (filesToUpload[0].webkitRelativePath && filesToUpload[0].webkitRelativePath.includes('/')) ?
                            <span>Folder: {filesToUpload[0].webkitRelativePath ? filesToUpload[0].webkitRelativePath.split('/')[0] : filesToUpload[0].name} ({filesToUpload.length} items)</span> :
                             <ul className="list-disc list-inside">{
                                Array.from(filesToUpload).map((item: any, index) => {
                                    const fileName = item.kind === 'file' ? item.getAsFile()?.name : item.name;
                                    return <li key={index} className="truncate">{fileName}</li>
                                })
                            }</ul>
                        }
                    </div>
                  </div>
                )}
                
                {popupError && <p className="text-(--error-light) text-sm mt-3">{popupError}</p>}

                <div className="flex justify-end gap-2 mt-6">
                  <button onClick={closeUploadPopup} className="btn btn-secondary px-5 py-2 text-sm">Cancel</button>
                  <button onClick={startUploadProcess} disabled={!filesToUpload || filesToUpload.length === 0} className="btn btn-primary px-5 py-2 text-sm">
                    Upload
                  </button>
                </div>
              </>
            )}
            
            {uploadPopupView === 'progress' && (
              <div className="mt-4">
                 <p className="text-center text-sm text-(--base-08) mb-2">{uploadStatus}</p>
                 <div className="w-full bg-(--base-04) rounded-full h-2.5 overflow-hidden">
                    <div className="bg-(--accent) h-full rounded-full transition-all duration-300" style={{ width: `${uploadProgress || 0}%` }} />
                </div>
                <p className="text-center text-xs text-(--base-06) mt-1">{uploadProgress || 0}%</p>
              </div>
            )}

            {uploadPopupView === 'conflict' && conflictFile && uploadItems && (
                 <div>
                    <p className="text-sm text-(--base-08) text-center mb-4">The file or folder <span className="font-medium text-(--warning-light)">{conflictFile.file.name}</span> already exists.</p>

                    <div className="mb-5 space-y-2">
                        <label className="input-label block">1. Rename (Recommended)</label>
                        <div className="flex items-center gap-2">
                            <input type="text" value={conflictFile.newName} onChange={(e) => setConflictFile({...conflictFile, newName: e.target.value.replace(/[^a-zA-Z0-9._-]/g, '')})} className="input-field w-full" />
                            <button onClick={() => {
                                executeUpload(conflictFile.newName, uploadItems.items, uploadItems.isFolder, 'replace');
                            }} className="btn btn-primary px-4 py-2 text-sm whitespace-nowrap">Upload with New Name</button>
                        </div>
                    </div>

                    <div className="mb-5 space-y-2">
                      {conflictFile.isFolderConflict ? (
                        <>
                          <label className="input-label block">2. Merge Options</label>
                          <div className="flex gap-2">
                              <button onClick={() => executeUpload(conflictFile.file.name, uploadItems.items, true, 'merge', 'replace')} className="btn btn-secondary w-full py-2 text-sm">Merge & Overwrite Files</button>
                              <button onClick={() => executeUpload(conflictFile.file.name, uploadItems.items, true, 'merge', 'ignore')} className="btn btn-secondary w-full py-2 text-sm">Merge & Keep Files</button>
                          </div>
                        </>
                      ) : (
                         <label className="input-label block">2. Overwrite</label>
                      )}
                    </div>

                    <div className="flex justify-between items-center mt-6 pt-4 border-t border-(--base-03)">
                       <button onClick={() => executeUpload(conflictFile.file.name, uploadItems.items, uploadItems.isFolder, 'replace')} className="btn btn-danger px-5 py-2 text-sm">
                         {conflictFile.isFolderConflict ? 'Replace Entire Folder' : 'Overwrite File'}
                       </button>
                       <button onClick={() => {setUploadPopupView('select'); setConflictFile(null);}} className="btn btn-secondary px-5 py-2 text-sm">Cancel</button>
                    </div>
                </div>
            )}
            
            </div>
          </div>
        </div>
      )}

      {showEditPopup && (
        <div className="modal-overlay animate-fade-in">
          <div style={editorPopupStyle} className="modal-panel relative h-[85vh] flex flex-col p-0 overflow-hidden">
            {/* Header */}
            <div className="modal-header flex items-center justify-between shrink-0">
              <div className="flex items-center gap-2 flex-1 min-w-0">
                 {isRenaming ? (
                   <>
                    <input
                        ref={renameInputRef}
                        type="text"
                        value={tempFileName}
                        onChange={(e) => setTempFileName(e.target.value)}
                        onKeyDown={(e) => e.key === 'Enter' && handleRenameSubmit()}
                        className="input-field text-base"
                    />
                    <button onClick={handleRenameSubmit} className="p-2 flex items-center justify-center rounded-md text-(--success) hover:bg-(--base-04)"><Check size={20} /></button>
                    <button onClick={() => setIsRenaming(false)} className="p-2 flex items-center justify-center rounded-md text-(--error) hover:bg-(--base-04)"><X size={20} /></button>
                   </>
                 ) : (
                    <h2 className="modal-title truncate">{editingFile}</h2>
                 )}
                {!isRenaming && <button onClick={handleRenameClick} className="p-1 rounded-md hover:bg-(--base-04) text-(--base-06)"><Pencil size={18} /></button>}
              </div>
              <div className="flex items-center gap-1.5 shrink-0">
                   <div className="flex items-center gap-1 bg-(--base-03) p-1 rounded-md">
                       <input
                          type="text"
                          placeholder="Search..."
                          value={searchTerm}
                          onChange={handleSearchChange}
                          className="input-field h-7 px-2 text-xs"
                       />
                       <button onClick={() => goToMatch(-1)} disabled={searchMatches.length === 0} className="w-7 h-7 flex items-center justify-center rounded-md hover:bg-(--base-04) disabled:opacity-40 text-(--base-07)"><ArrowUp size={16} /></button>
                       <button onClick={() => goToMatch(1)} disabled={searchMatches.length === 0} className="w-7 h-7 flex items-center justify-center rounded-md hover:bg-(--base-04) disabled:opacity-40 text-(--base-07)"><ArrowDown size={16} /></button>
                       <span className="text-xs text-(--base-06) px-1.5 font-mono">{searchMatches.length > 0 ? `${currentMatchIndex + 1}/${searchMatches.length}` : '0/0'}</span>
                   </div>
                   <button onClick={closeEditPopup} className="w-8 h-8 flex items-center justify-center bg-(--base-03) hover:bg-(--base-04) transition-colors rounded-md text-(--base-07)">
                     <X size={18} />
                   </button>
              </div>
            </div>
            {isEditorLoading ? (
              <div className="grow flex items-center justify-center text-sm text-(--base-07)">
                Loading content...
              </div>
            ) : (
                <div className="relative grow mx-4 mb-2 rounded-md bg-(--base-01) border border-(--base-03) focus-within:border-(--accent) overflow-hidden">
                    <Suspense fallback={<div className="h-full flex items-center justify-center text-sm text-(--base-07)">Loading editor...</div>}>
                        <CodeMirrorEditor
                            value={fileContent}
                            onChange={setFileContent}
                            filename={editingFile || ''}
                            readOnly={!isEditingEnabled || fileIsReadOnly}
                            jumpTo={searchJump}
                            className="h-full"
                        />
                    </Suspense>
                </div>
            )}
            <div className="modal-footer">
              {fileIsReadOnly ? (
                 <span className="text-sm text-(--base-07) mr-auto self-center">Compressed file, read-only.</span>
              ) : !isEditingEnabled ? (
                 <button onClick={() => { if (blockReadOnly()) return; setIsEditingEnabled(true); }} className="btn btn-secondary px-4 py-2 text-sm">Edit</button>
              ) : (
                <>
                  <button onClick={() => {
                      setIsEditingEnabled(false);
                      setFileContent(originalFileContent);
                  }} className="btn btn-secondary px-4 py-2 text-sm">Cancel</button>
                  <button onClick={handleSaveFile} disabled={loading} className="btn btn-primary px-4 py-2 text-sm disabled:opacity-40 disabled:cursor-not-allowed">Save</button>
                </>
              )}
            </div>
          </div>
        </div>
      )}
      
      {/* Selective Download Popup */}
      {selectiveDownloadTarget && (
        <SelectiveDownloadModal
          target={selectiveDownloadTarget}
          tree={selectiveTree}
          expanded={selectiveExpanded}
          checked={selectiveChecked}
          selectAll={selectiveAll}
          loading={selectiveLoading}
          downloading={selectiveDownloading}
          onClose={() => setSelectiveDownloadTarget(null)}
          onToggleSelectAll={() => {
            setSelectiveAll(!selectiveAll);
            if (!selectiveAll) setSelectiveChecked(new Set());
          }}
          onToggleExpand={toggleSelectiveExpand}
          onToggleCheck={toggleSelectiveCheck}
          onDownload={handleSelectiveDownload}
        />
      )}

      {/* Download Progress */}
      {downloadProgress && <DownloadProgress progress={downloadProgress} />}

      {toastMessage && <Toast message={toastMessage.message} type={toastMessage.type} />}
    </div>
  );
};

export default FileBrowser;
