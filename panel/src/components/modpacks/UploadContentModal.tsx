"use client";

import { useRef, useState } from 'react';
import { FolderOpen, Loader2, Upload, X } from 'lucide-react';
import ModalPanel from '@/components/ui/ModalPanel';
import Select from '@/components/ui/Select';
import Checkbox from '@/components/ui/Checkbox';
import { uploadContent } from '@/lib/api/packs';
import { packTargetPathError, packUploadTarget } from '@/lib/packUpload';

export default function UploadContentModal({ packId, buildId, onClose, onUploaded }: {
    packId: number; buildId: number; onClose: () => void; onUploaded: (count: number) => void;
}) {
    const fileRef = useRef<HTMLInputElement>(null);
    const folderRef = useRef<HTMLInputElement>(null);
    const [files, setFiles] = useState<File[]>([]);
    const [contentType, setContentType] = useState('mod');
    const [side, setSide] = useState('both');
    const [prefix, setPrefix] = useState('config');
    const [singleTarget, setSingleTarget] = useState('');
    const [archive, setArchive] = useState(false);
    const [completed, setCompleted] = useState(0);
    const [uploading, setUploading] = useState(false);
    const [error, setError] = useState('');

    const choose = (selected: FileList | null) => {
        setFiles(Array.from(selected || []));
        setCompleted(0); setError(''); setSingleTarget('');
    };
    const isConfig = contentType === 'config';
    const plans = files.map(file => {
        const asArchive = isConfig && archive && file.name.toLowerCase().endsWith('.zip');
        const targetPath = isConfig && !asArchive
            ? (files.length === 1 && singleTarget ? singleTarget : packUploadTarget(file, prefix)) : undefined;
        return { file, archive: asArchive, targetPath };
    });
    const invalid = plans.find(plan => plan.targetPath && packTargetPathError(plan.targetPath));
    const duplicate = plans.some((plan, i) => plan.targetPath && plans.slice(0, i).some(previous => previous.targetPath === plan.targetPath));
    const validationError = invalid ? packTargetPathError(invalid.targetPath!) : duplicate ? 'Two files have the same target path. Select distinct paths or upload them separately.' : null;

    const upload = async () => {
        if (uploading || validationError || !files.length) return;
        setUploading(true); setError('');
        let count = completed;
        for (let i = completed; i < plans.length; i++) {
            const plan = plans[i];
            const result = await uploadContent(packId, buildId, plan.file, side, contentType, {
                targetPath: plan.targetPath, archive: plan.archive,
            });
            if (!result.success) {
                setError(`${plan.file.name}: ${result.message || 'Upload failed'}. ${count} of ${plans.length} files uploaded. Retry resumes with this file.`);
                setUploading(false); return;
            }
            count++; setCompleted(count);
        }
        setUploading(false);
        onUploaded(count);
    };

    return (
        <div className="modal-overlay animate-fade-in" onClick={() => { if (!uploading) onClose(); }}>
            <ModalPanel onClose={() => { if (!uploading) onClose(); }} className="modal-panel max-w-2xl" onClick={e => e.stopPropagation()}>
                <div className="modal-header">
                    <h3 className="modal-title flex items-center gap-2"><Upload size={16} /> Upload content</h3>
                    <button onClick={onClose} disabled={uploading} aria-label="Close" className="text-(--base-06)"><X size={16} /></button>
                </div>
                <div className="modal-body space-y-4">
                    <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
                        <div><label htmlFor="pack-upload-type" className="input-label">Content type</label><Select id="pack-upload-type" value={contentType} onChange={value => { setContentType(value); setCompleted(0); }} disabled={uploading || completed > 0} options={[{ value: 'mod', label: 'Mod' }, { value: 'resourcepack', label: 'Resource pack' }, { value: 'shaderpack', label: 'Shader pack' }, { value: 'config', label: 'Config / scripts' }]} /></div>
                        <div><label htmlFor="pack-upload-side" className="input-label">Install on</label><Select id="pack-upload-side" value={side} onChange={setSide} disabled={uploading || completed > 0} options={[{ value: 'both', label: 'Client and server' }, { value: 'client', label: 'Client only' }, { value: 'server', label: 'Server only' }]} /></div>
                    </div>
                    {isConfig && <>
                        <p className="text-xs text-(--base-06)">Paths are relative to the Minecraft folder. Select a config, defaultconfigs or kubejs folder to preserve its structure, or choose individual files and set their destination.</p>
                        <div><label htmlFor="pack-upload-prefix" className="input-label">Destination folder for individual files</label><input id="pack-upload-prefix" value={prefix} onChange={e => setPrefix(e.target.value)} disabled={uploading || completed > 0} className="input-field input-mono w-full" placeholder="config or kubejs/server_scripts" /></div>
                        <Checkbox checked={archive} onChange={setArchive} disabled={uploading || completed > 0} label="Extract config ZIP bundles using their Minecraft-relative paths" hint="A bundle should contain paths such as config/example.toml or kubejs/server_scripts/recipes.js. Leave off to keep a ZIP as one config file." />
                    </>}
                    <div className="flex items-center gap-2 flex-wrap">
                        <button onClick={() => fileRef.current?.click()} disabled={uploading || completed > 0} className="btn btn-secondary btn-sm"><Upload size={13} /> Choose files</button>
                        {isConfig && <button onClick={() => folderRef.current?.click()} disabled={uploading || completed > 0} className="btn btn-secondary btn-sm"><FolderOpen size={13} /> Choose folder</button>}
                        <span className="text-xs text-(--base-06)">{files.length} file{files.length === 1 ? '' : 's'} selected</span>
                    </div>
                    <input type="file" multiple ref={fileRef} className="hidden" onChange={e => choose(e.target.files)} />
                    <input type="file" multiple ref={folderRef} className="hidden" {...{ webkitdirectory: '', directory: '' } as any} onChange={e => choose(e.target.files)} />
                    {isConfig && files.length === 1 && !plans[0]?.archive && <div><label htmlFor="pack-upload-target" className="input-label">Target path</label><input id="pack-upload-target" value={singleTarget || plans[0].targetPath || ''} onChange={e => setSingleTarget(e.target.value)} disabled={uploading || completed > 0} className="input-field input-mono w-full" /></div>}
                    {plans.length > 0 && <ul className="max-h-48 overflow-y-auto space-y-1 text-xs font-mono text-(--base-07)">{plans.map((plan, i) => <li key={`${plan.file.name}:${i}`} className="break-all">{i < completed ? 'Uploaded: ' : ''}{plan.targetPath || (plan.archive ? `${plan.file.name} (extract bundle)` : plan.file.name)}</li>)}</ul>}
                    {uploading && <p role="status" className="text-xs text-(--base-06) flex items-center gap-2"><Loader2 size={13} className="animate-spin" /> Uploading file {Math.min(completed + 1, files.length)} of {files.length}...</p>}
                    {(error || validationError) && <p role="alert" className="text-xs text-(--error-light)">{error || validationError}</p>}
                    {completed > 0 && !uploading && <p className="text-xs text-(--base-06)">Uploaded files stay in the build if you close this dialog.</p>}
                </div>
                <div className="modal-footer"><button onClick={onClose} disabled={uploading} className="btn btn-secondary">Close</button><button onClick={upload} disabled={uploading || !files.length || !!validationError} className="btn btn-primary disabled:opacity-40">{uploading ? 'Uploading...' : completed ? 'Retry remaining files' : 'Upload'}</button></div>
            </ModalPanel>
        </div>
    );
}
