"use client";

import { useState } from 'react';
import { ExternalLink, FileText } from 'lucide-react';
import ModalPanel from '@/components/ui/ModalPanel';

export const MINECRAFT_EULA_URL = 'https://www.minecraft.net/en-us/eula';

// Asked once per account, before the first install. Every server the node
// installs gets eula=true written into it, and that line is the declaration of
// the person who runs the server - so they make it here, and Core records when.
// Unticked by default: a pre-ticked box is not a declaration anyone made.
export default function MinecraftEulaDialog({ onAccept, onCancel }: { onAccept: () => void; onCancel: () => void }) {
    const [agreed, setAgreed] = useState(false);
    return (
        <div className="modal-overlay animate-fade-in" onClick={onCancel}>
            <ModalPanel onClose={onCancel} className="modal-panel max-w-lg" onClick={e => e.stopPropagation()}>
                <h3 className="modal-title flex items-center gap-2">
                    <FileText size={18} className="text-(--accent-light)" aria-hidden="true" />
                    Minecraft End User License Agreement
                </h3>
                <p className="text-sm text-(--base-07) mt-2">
                    Running a Minecraft server requires accepting Mojang&apos;s EULA. The panel writes your acceptance
                    into the servers it installs for you, so we ask you once, here.
                </p>
                <a
                    href={MINECRAFT_EULA_URL}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="inline-flex items-center gap-1.5 mt-3 text-sm text-(--accent-light) hover:underline"
                >
                    Read the Minecraft EULA <ExternalLink size={13} aria-hidden="true" />
                </a>
                <label className="flex items-start gap-3 mt-4 rounded-md border border-(--base-03) p-3 cursor-pointer hover:border-(--accent)/40 transition-colors">
                    <input type="checkbox" className="checkbox mt-0.5" checked={agreed} onChange={e => setAgreed(e.target.checked)} />
                    <span className="text-sm text-(--base-09)">
                        I have read and accept the Minecraft End User License Agreement.
                    </span>
                </label>
                <div className="flex justify-end gap-2 mt-5">
                    <button type="button" className="btn btn-secondary btn-sm" onClick={onCancel}>Cancel</button>
                    <button type="button" className="btn btn-primary btn-sm" disabled={!agreed} onClick={onAccept}>
                        Accept and install
                    </button>
                </div>
            </ModalPanel>
        </div>
    );
}
