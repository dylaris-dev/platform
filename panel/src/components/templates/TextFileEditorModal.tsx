"use client";

import React, { useEffect, useState } from 'react';
import { X, Loader2, FileText } from 'lucide-react';
import ModalPanel from '@/components/ui/ModalPanel';

// A text editor in a modal over any load/save pair. Both reject with an Error
// whose message is shown verbatim; save resolving closes nothing, the caller
// decides (usually onClose plus a toast).
export default function TextFileEditorModal({
    title,
    load,
    save,
    onClose,
}: {
    title: string;
    load: () => Promise<string>;
    save: (text: string) => Promise<void>;
    onClose: () => void;
}) {
    const [text, setText] = useState('');
    const [loading, setLoading] = useState(true);
    const [saving, setSaving] = useState(false);
    const [error, setError] = useState<string | null>(null);
    const [loadFailed, setLoadFailed] = useState(false);

    useEffect(() => {
        let live = true;
        load().then(
            (t) => { if (live) { setText(t); setLoading(false); } },
            (e) => { if (live) { setError(e instanceof Error ? e.message : 'Could not open the file.'); setLoadFailed(true); setLoading(false); } },
        );
        return () => { live = false; };
        // load is the caller's closure; reloading on every render would discard edits.
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, []);

    const onSave = async () => {
        setError(null);
        setSaving(true);
        try {
            await save(text);
        } catch (e) {
            setError(e instanceof Error ? e.message : 'Save failed.');
        } finally {
            setSaving(false);
        }
    };

    return (
        <div className="modal-overlay animate-fade-in" onClick={saving ? undefined : onClose}>
            <ModalPanel onClose={onClose} dismissible={!saving} className="modal-panel max-w-2xl" onClick={e => e.stopPropagation()}>
                <div className="modal-header flex items-center justify-between gap-3">
                    <h3 className="modal-title flex items-center gap-2 min-w-0">
                        <FileText size={16} className="shrink-0" />
                        <span className="truncate">Edit {title}</span>
                    </h3>
                    <button onClick={onClose} disabled={saving} aria-label="Close" className="text-(--base-06) hover:text-(--base-09) disabled:opacity-40"><X size={16} /></button>
                </div>
                <div className="modal-body space-y-3">
                    {loading ? (
                        <div className="flex items-center gap-2 text-sm text-(--base-06)" role="status">
                            <Loader2 size={14} className="animate-spin" /> Loading...
                        </div>
                    ) : loadFailed ? (
                        <p className="text-xs text-(--error-light)" role="alert">{error}</p>
                    ) : (
                        <textarea
                            aria-label={`Contents of ${title}`}
                            value={text}
                            onChange={e => setText(e.target.value)}
                            spellCheck={false}
                            className="input-field input-mono w-full h-80 resize-y"
                        />
                    )}
                    {error && !loadFailed && <p className="text-xs text-(--error-light)" role="alert">{error}</p>}
                </div>
                <div className="modal-footer">
                    <button onClick={onClose} disabled={saving} className="btn btn-secondary disabled:opacity-40">Cancel</button>
                    <button
                        onClick={onSave}
                        className="btn btn-primary disabled:opacity-40 disabled:cursor-not-allowed"
                        disabled={loading || saving || loadFailed}
                    >
                        {saving ? <Loader2 size={14} className="animate-spin" aria-label="Saving" /> : 'Save'}
                    </button>
                </div>
            </ModalPanel>
        </div>
    );
}
