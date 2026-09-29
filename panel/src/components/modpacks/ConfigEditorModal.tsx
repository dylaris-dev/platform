"use client";

import React, { useEffect, useState } from 'react';
import { X, Loader2, FileText } from 'lucide-react';
import { getContentText, setContentText } from '@/lib/api/packsPublish';
import ModalPanel from '@/components/ui/ModalPanel';

// Loads a config entry's text, lets the user edit it, and saves it back.
// onSaved fires so the parent can refresh + toast.
export default function ConfigEditorModal({
    packId,
    buildId,
    modversionId,
    title,
    onClose,
    onSaved,
}: {
    packId: number;
    buildId: number;
    modversionId: number;
    title: string;
    onClose: () => void;
    onSaved: () => void;
}) {
    const [text, setText] = useState('');
    const [targetPath, setTargetPath] = useState('');
    const [loading, setLoading] = useState(true);
    const [saving, setSaving] = useState(false);
    const [error, setError] = useState<string | null>(null);
    const [loadFailed, setLoadFailed] = useState(false);

    useEffect(() => {
        let live = true;
        (async () => {
            const res = await getContentText(packId, buildId, modversionId);
            if (!live) return;
            setLoading(false);
            if (res.success && typeof res.text === 'string') {
                setText(res.text);
                setTargetPath(res.targetPath || '');
                setLoadFailed(false);
            } else {
                setError(res.message || 'This content cannot be edited as text.');
                setLoadFailed(true);
            }
        })();
        return () => { live = false; };
    }, [packId, buildId, modversionId]);

    const save = async () => {
        setError(null);
        setSaving(true);
        const res = await setContentText(packId, buildId, modversionId, text);
        setSaving(false);
        if (res.success) {
            onSaved();
        } else {
            setError(res.message || 'Save failed.');
        }
    };

    return (
        <div className="modal-overlay animate-fade-in" onClick={onClose}>
            <ModalPanel onClose={onClose} className="modal-panel max-w-2xl" onClick={e => e.stopPropagation()}>
                <div className="modal-header">
                    <h3 className="modal-title flex items-center gap-2">
                        <FileText size={16} />
                        Edit {title}
                    </h3>
                    <button onClick={onClose} className="text-(--base-06)"><X size={16} /></button>
                </div>
                <div className="modal-body space-y-3">
                    {targetPath && <div className="text-[10px] font-mono text-(--base-06)">{targetPath}</div>}
                    {loading ? (
                        <div className="flex items-center gap-2 text-sm text-(--base-06)">
                            <Loader2 size={14} className="animate-spin" /> Loading...
                        </div>
                    ) : loadFailed ? (
                        <p className="text-xs text-(--error-light)">{error}</p>
                    ) : (
                        <textarea
                            value={text}
                            onChange={e => setText(e.target.value)}
                            spellCheck={false}
                            className="input-field input-mono w-full h-80 resize-y"
                        />
                    )}
                    {error && !loadFailed && <p className="text-xs text-(--error-light)">{error}</p>}
                </div>
                <div className="modal-footer">
                    <button onClick={onClose} className="btn btn-secondary">Cancel</button>
                    <button
                        onClick={save}
                        className="btn btn-primary disabled:opacity-40 disabled:cursor-not-allowed"
                        disabled={loading || saving || loadFailed}
                    >
                        {saving ? <Loader2 size={14} className="animate-spin" /> : 'Save'}
                    </button>
                </div>
            </ModalPanel>
        </div>
    );
}
