"use client";

import { useState } from 'react';
import { Download, Loader2 } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import { downloadBeamApp } from '@/lib/beamDownload';
import { isWails } from '@/lib/adapters';
import { toast } from '@/components/ui/Toast';

/**
 * Gets the Beam desktop app, from wherever the user happens to be.
 *
 * It used to live only on a server's Files page, which is the one screen you
 * cannot reach usefully without it: Beam is what makes that page work when file
 * access is set to Beam, so the download sat behind the thing it unlocks.
 *
 * Hidden entirely when Beam is not a file-access mode on this install, rather
 * than shown and failing: the endpoint would answer "no download configured",
 * and a button that only ever reports that is worse than no button.
 *
 * Hidden inside Beam itself for the same reason. The app proxies this panel, so
 * the button was on screen offering the user a download of the program they were
 * already looking at it through. Read at render rather than in an effect because
 * window.go is injected before the first render in Wails - the same assumption
 * FileBrowserView documents and relies on.
 */
export default function BeamDownloadButton() {
    const { fileAccessMode, beamSettings } = useAppData();
    const [downloading, setDownloading] = useState(false);

    const beamEnabled =
        (fileAccessMode === 'beam' || fileAccessMode === 'both') && beamSettings?.enabled !== false;
    if (!beamEnabled || isWails()) return null;

    const onClick = async () => {
        if (downloading) return;
        setDownloading(true);
        const err = await downloadBeamApp();
        toast(err ?? 'Beam app downloaded.', err === null);
        setDownloading(false);
    };

    return (
        <button
            type="button"
            onClick={onClick}
            disabled={downloading}
            title="Download the Beam desktop app"
            aria-label="Download the Beam desktop app"
            className="flex items-center gap-2 px-3 py-1.5 rounded-md border border-transparent font-medium
                       text-(--base-07) transition-colors hover:bg-(--base-04)/50 hover:text-(--base-09)
                       disabled:opacity-50 disabled:cursor-not-allowed"
        >
            {downloading ? <Loader2 size={20} className="animate-spin" /> : <Download size={20} />}
            <span className="text-sm hidden xl:block">Beam app</span>
        </button>
    );
}
