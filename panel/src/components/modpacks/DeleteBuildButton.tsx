'use client';

import { useState } from 'react';
import { Trash2 } from 'lucide-react';
import { deleteBuild, type PackBuild } from '@/lib/api/packs';
import ModalPanel from '@/components/ui/ModalPanel';
import { toast } from '@/components/ui/Toast';

export default function DeleteBuildButton({ packId, build, onDeleted }: {
    packId: number;
    build: PackBuild;
    onDeleted: () => void;
}) {
    const [open, setOpen] = useState(false);
    const [confirmation, setConfirmation] = useState('');
    const [busy, setBusy] = useState(false);
    const protectedBuild = build.frozen || build.modrinthPublished;
    return <>
        <button className="btn btn-secondary btn-sm" onClick={() => { setConfirmation(''); setOpen(true); }} aria-label={`Delete build ${build.versionString}`}><Trash2 size={13} />Delete build</button>
        {open && <div className="modal-overlay" onClick={() => { if (!busy) setOpen(false); }}>
            <ModalPanel onClose={() => setOpen(false)} dismissible={!busy} className="modal-panel max-w-lg" onClick={event => event.stopPropagation()}>
                <div className="modal-header"><h2 className="modal-title">Delete build {build.versionString}?</h2></div>
                <div className="modal-body space-y-4">
                    <p className="text-sm text-(--base-07)">This permanently removes this local build, its downloads and share links. Other builds, already installed server files and versions published on Modrinth remain.</p>
                    <p className="text-sm text-(--base-07)">If it is Recommended, the pack returns to the newest remaining release. You cannot install or download this deleted build from the panel again.</p>
                    {protectedBuild && <label className="input-label">Type the build version to confirm: {build.versionString}<input className="input-field w-full mt-2" value={confirmation} onChange={event => setConfirmation(event.target.value)} autoComplete="off" spellCheck={false} disabled={busy} /></label>}
                </div>
                <div className="modal-footer"><button className="btn btn-secondary" disabled={busy} onClick={() => setOpen(false)}>Cancel</button><button className="btn btn-danger" disabled={busy || (protectedBuild && confirmation !== build.versionString)} onClick={async () => {
                    setBusy(true);
                    try {
                        const result = await deleteBuild(packId, build.id, protectedBuild ? confirmation : undefined);
                        if (!result.success) throw new Error(result.message || 'Could not delete build.');
                        toast('Build deleted.');
                        setOpen(false);
                        onDeleted();
                    } catch (error) {
                        toast(error instanceof Error ? error.message : 'Could not delete build.', false);
                    } finally { setBusy(false); }
                }}>{busy ? 'Deleting...' : 'Delete permanently'}</button></div>
            </ModalPanel>
        </div>}
    </>;
}
