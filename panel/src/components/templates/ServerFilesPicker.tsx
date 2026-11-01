"use client";

import { useEffect, useRef, useState } from 'react';
import { Loader2 } from 'lucide-react';
import { overPathLimit, SelectiveTree, selectedPaths, useSelectionTree } from '@dylaris/ui-filebrowser';
import { MAX_CAPTURE_PATHS } from '@/lib/templates';
import { getFiles } from '@/lib/api/files';

type Listing = { success: boolean; files?: { name: string; is_dir: boolean; size: number }[]; message?: string };

// Picks a sub-server and the files in it to capture, as the full checkbox
// tree the selective download uses. Reports the sub-server and the paths
// Core expects (relative to it, never "").
export default function ServerFilesPicker({ uuid, preferredSub, disabled, onChange }: {
    uuid: string;
    preferredSub: string;
    disabled?: boolean;
    onChange: (subServer: string, paths: string[]) => void;
}) {
    const [subServers, setSubServers] = useState<string[] | null>(null);
    const [subServer, setSubServer] = useState('');
    const [listError, setListError] = useState<string | null>(null);
    const sel = useSelectionTree();
    const onChangeRef = useRef(onChange);
    onChangeRef.current = onChange;

    useEffect(() => {
        let live = true;
        setSubServers(null);
        setListError(null);
        getFiles('', uuid).then((res: Listing) => {
            if (!live) return;
            if (!res.success) { setListError(res.message || 'Could not read the server folders.'); return; }
            const dirs = (res.files ?? []).filter((f) => f.is_dir).map((f) => f.name);
            setSubServers(dirs);
            setSubServer(dirs.includes(preferredSub) ? preferredSub : dirs[0] ?? '');
        });
        return () => { live = false; };
    }, [uuid, preferredSub]);

    // sel.open is a fresh closure every render; the root only changes with these.
    useEffect(() => {
        if (!subServer) return;
        void sel.open(async (rel) => {
            const res: Listing = await getFiles(rel ? `${subServer}/${rel}` : subServer, uuid);
            return { success: res.success, files: res.files ?? [], message: res.message };
        });
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [uuid, subServer]);

    const paths = subServer && !sel.loading ? selectedPaths(sel.tree, sel.checked, sel.selectAll) : [];
    // Keyed on the path list's content, so a new but equal array does not re-report.
    const pathsKey = JSON.stringify(paths);
    useEffect(() => {
        onChangeRef.current(subServer, JSON.parse(pathsKey));
    }, [subServer, pathsKey]);

    if (listError) return <p className="text-xs text-(--error-light)" role="alert">{listError}</p>;
    if (subServers === null) {
        return <div className="flex items-center gap-2 text-xs text-(--base-06)" role="status"><Loader2 size={12} className="animate-spin" /> Reading files...</div>;
    }
    if (subServers.length === 0) return <p className="text-xs text-(--base-06)">This server has no files yet.</p>;

    return (
        <div className="space-y-3">
            <div>
                <label htmlFor="tpl-sub" className="input-label">Sub-server</label>
                <select id="tpl-sub" className="input-field w-full sm:w-1/2" value={subServer} disabled={disabled} onChange={(e) => setSubServer(e.target.value)}>
                    {subServers.map((d) => <option key={d} value={d}>{d}</option>)}
                </select>
            </div>
            <fieldset className="rounded-md border border-(--base-04) p-2">
                <legend className="input-label px-1">Include</legend>
                <SelectiveTree
                    tree={sel.tree}
                    expanded={sel.expanded}
                    checked={sel.checked}
                    selectAll={sel.selectAll}
                    loading={sel.loading}
                    error={sel.error}
                    overLimit={overPathLimit(paths, MAX_CAPTURE_PATHS)}
                    disabled={disabled}
                    onToggleSelectAll={sel.toggleSelectAll}
                    onToggleExpand={sel.toggleExpand}
                    onToggleCheck={sel.toggleCheck}
                />
            </fieldset>
        </div>
    );
}
