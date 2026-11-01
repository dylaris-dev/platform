import { useRef, useState } from 'react';
import type { FileEntry } from './types';
import { toggleSelection } from './selection';

export type LoadDir = (relativePath: string) => Promise<{ success: boolean; files: FileEntry[]; message?: string }>;

// State behind a SelectiveTree: a lazily loaded folder tree and what is
// checked in it. open() starts over at a new root; the loader it is given
// takes paths relative to that root.
export function useSelectionTree() {
  const loader = useRef<LoadDir | null>(null);
  const [tree, setTree] = useState<Record<string, FileEntry[]>>({});
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [checked, setChecked] = useState<Set<string>>(new Set());
  const [selectAll, setSelectAll] = useState(true);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');

  const open = async (load: LoadDir) => {
    loader.current = load;
    setTree({});
    setExpanded(new Set());
    setChecked(new Set());
    setSelectAll(true);
    setError('');
    setLoading(true);
    const res = await load('');
    // A newer open() owns the state now.
    if (loader.current !== load) return;
    if (res.success) setTree({ '': res.files });
    else setError(res.message || 'Could not read the folder.');
    setLoading(false);
  };

  const toggleExpand = async (path: string) => {
    if (expanded.has(path)) {
      setExpanded(prev => { const n = new Set(prev); n.delete(path); return n; });
      return;
    }
    const load = loader.current;
    if (!tree[path] && load) {
      const res = await load(path);
      if (loader.current !== load) return;
      if (!res.success) { setError(res.message || 'Could not read the folder.'); return; }
      setTree(prev => ({ ...prev, [path]: res.files }));
      setError('');
    }
    setExpanded(prev => new Set(prev).add(path));
  };

  const toggleCheck = (path: string) => {
    setChecked(toggleSelection(tree, checked, selectAll, path));
    setSelectAll(false);
  };

  const toggleSelectAll = () => {
    setSelectAll(!selectAll);
    setChecked(new Set());
  };

  return { tree, expanded, checked, selectAll, loading, error, open, toggleExpand, toggleCheck, toggleSelectAll };
}
