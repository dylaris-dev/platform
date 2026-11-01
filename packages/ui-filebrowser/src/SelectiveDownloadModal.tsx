import React from 'react';
import { Folder, File as FileIcon, Download, X, ChevronRight, ChevronDown } from 'lucide-react';
import type { FileEntry } from './types';
import { formatBytes } from './utils';
import { isPathChecked, MAX_DOWNLOAD_PATHS, overPathLimit, selectedPaths, TOO_MANY_PATHS_MESSAGE } from './selection';

export interface SelectiveTreeProps {
  tree: Record<string, FileEntry[]>;
  expanded: Set<string>;
  checked: Set<string>;
  selectAll: boolean;
  loading: boolean;
  error?: string;
  /** The selection is past what the caller can send; the caller also blocks submit. */
  overLimit?: boolean;
  disabled?: boolean;
  onToggleSelectAll: () => void;
  onToggleExpand: (relativePath: string) => void;
  onToggleCheck: (relativePath: string) => void;
}

// Controlled checkbox tree over a folder (state: useSelectionTree). Used by the
// selective download below and by the panel's "Save as template".
export const SelectiveTree: React.FC<SelectiveTreeProps> = ({
  tree,
  expanded,
  checked,
  selectAll,
  loading,
  error,
  overLimit,
  disabled,
  onToggleSelectAll,
  onToggleExpand,
  onToggleCheck,
}) => {
  const renderTree = (parentPath: string, depth: number): React.ReactNode => {
    const entries = tree[parentPath];
    if (!entries) return null;

    return entries.map(entry => {
      const entryPath = parentPath ? `${parentPath}/${entry.name}` : entry.name;
      const isExpanded = expanded.has(entryPath);

      return (
        <div key={entryPath}>
          <div className="flex items-center gap-1.5 py-1 px-2 hover:bg-(--base-02) rounded-sm" style={{ paddingLeft: `${depth * 20 + 8}px` }}>
            {entry.is_dir ? (
              <button
                type="button"
                onClick={() => onToggleExpand(entryPath)}
                aria-expanded={isExpanded}
                aria-label={`${isExpanded ? 'Collapse' : 'Expand'} ${entry.name}`}
                className="p-0.5 text-(--base-06) hover:text-(--base-09) rounded-sm focus-visible:outline focus-visible:outline-(--accent)"
              >
                {isExpanded ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
              </button>
            ) : (
              <span className="w-[18px]" />
            )}
            <input
              type="checkbox"
              checked={isPathChecked(checked, selectAll, entryPath)}
              onChange={() => onToggleCheck(entryPath)}
              disabled={disabled}
              aria-label={entryPath}
              className="accent-(--accent) w-3.5 h-3.5"
            />
            {entry.is_dir ? <Folder size={14} className="text-(--primary-light) shrink-0" /> : <FileIcon size={14} className="text-(--base-06) shrink-0" />}
            <span className="text-sm text-(--base-09) truncate">{entry.name}</span>
            <span className="text-xs text-(--base-06) ml-auto shrink-0">{formatBytes(entry.size)}</span>
          </div>
          {entry.is_dir && isExpanded && renderTree(entryPath, depth + 1)}
        </div>
      );
    });
  };

  return (
    <>
      <label className="flex items-center gap-2 mb-3 pb-2 border-b border-(--base-03) cursor-pointer">
        <input
          type="checkbox"
          checked={selectAll}
          onChange={onToggleSelectAll}
          disabled={disabled}
          className="accent-(--accent) w-3.5 h-3.5"
        />
        <span className="text-sm font-medium text-(--base-09)">Select All</span>
      </label>
      {error && <p className="text-xs text-(--error-light) mb-2" role="alert">{error}</p>}
      {overLimit && <p className="text-xs text-(--error-light) mb-2" role="alert">{TOO_MANY_PATHS_MESSAGE}</p>}
      <div className="max-h-80 overflow-y-auto">
        {loading ? (
          <div className="flex items-center justify-center py-8 text-(--base-06)" role="status">Loading...</div>
        ) : tree['']?.length === 0 ? (
          <div className="flex items-center justify-center py-8 text-sm text-(--base-06)">This folder is empty.</div>
        ) : (
          renderTree('', 0)
        )}
      </div>
    </>
  );
};

export interface SelectiveDownloadModalProps extends SelectiveTreeProps {
  target: FileEntry;
  downloading: boolean;
  onClose: () => void;
  onDownload: () => void;
}

// Tree-picker modal for downloading a subset of a folder as a zip.
const SelectiveDownloadModal: React.FC<SelectiveDownloadModalProps> = ({ target, downloading, onClose, onDownload, ...treeProps }) => {
  // Select-all goes as one flag, not as paths.
  const tooMany = !treeProps.selectAll && overPathLimit(selectedPaths(treeProps.tree, treeProps.checked, false), MAX_DOWNLOAD_PATHS);
  return (
  <div className="modal-overlay animate-fade-in">
    <div className="modal-panel w-full max-w-lg">
      <div className="modal-header">
        <h2 className="modal-title">Download: {target.name}/</h2>
        <button onClick={onClose} aria-label="Close" className="text-(--base-06) hover:text-(--base-09)"><X size={18} /></button>
      </div>
      <div className="modal-body">
        <SelectiveTree {...treeProps} overLimit={tooMany} />
      </div>
      <div className="flex justify-end gap-2 p-4 border-t border-(--base-03)">
        <button onClick={onClose} className="btn btn-secondary px-4 py-2 text-sm">Cancel</button>
        <button
          onClick={onDownload}
          disabled={downloading || tooMany || (!treeProps.selectAll && treeProps.checked.size === 0)}
          className="btn btn-primary px-4 py-2 text-sm disabled:opacity-50"
        >
          <Download size={14} />
          {downloading ? 'Downloading...' : 'Download .zip'}
        </button>
      </div>
    </div>
  </div>
  );
};

export default SelectiveDownloadModal;
