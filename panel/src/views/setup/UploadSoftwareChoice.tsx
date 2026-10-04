"use client";

import React from 'react';
import type { UploadDetection } from '@/lib/uploadDetect';

interface UploadSoftwareChoiceProps {
    /** null while the archive is being read; undefined when there is no archive (files already on the server). */
    detection: UploadDetection | null | undefined;
    keepJar: boolean;
    onKeepJarChange: (keep: boolean) => void;
    keepAllowed: boolean;
    /** A version was detected or picked, rather than merely preselected. */
    versionChosen: boolean;
    /** The version picker, shown while installing. */
    children: React.ReactNode;
}

/**
 * What an upload starts with. Installing the chosen software is the default:
 * an upload whose jar is not in its top folder (Paper keeps it under versions/)
 * has nothing the node can start.
 */
export default function UploadSoftwareChoice({ detection, keepJar, onKeepJarChange, keepAllowed, versionChosen, children }: UploadSoftwareChoiceProps) {
    const hint = detection === null
        ? 'Reading the archive...'
        : detection?.software
            ? `Detected ${detection.software}${detection.build ? ' ' + detection.build : ''} in your upload.`
            : detection
                ? 'No server software recognised in your upload. Choose the one it was made with.'
                : 'Choose the server software your files were made with.';

    const tab = (active: boolean) =>
        `btn flex-1 py-2 text-sm border-0 rounded-md disabled:opacity-50 disabled:cursor-not-allowed ${active ? 'bg-(--accent) text-white' : 'bg-transparent text-(--base-07) hover:text-(--base-09)'}`;

    return (
        <div className="space-y-3">
            <div>
                <label className="input-label mb-2 block">Server software</label>
                <div role="radiogroup" aria-label="Server software" className="flex bg-(--base-03) p-1 rounded-md max-w-md">
                    <button type="button" role="radio" aria-checked={!keepJar} onClick={() => onKeepJarChange(false)} className={tab(!keepJar)}>
                        Install software
                    </button>
                    <button
                        type="button"
                        role="radio"
                        aria-checked={keepJar}
                        disabled={!keepAllowed}
                        title={keepAllowed ? undefined : 'Your upload has no server jar in its top folder.'}
                        onClick={() => onKeepJarChange(true)}
                        className={tab(keepJar)}
                    >
                        Keep my server jar
                    </button>
                </div>
                <p className="text-xs text-(--base-06) mt-2">
                    {keepJar
                        ? 'Starts the server jar in the top folder of your upload, unchanged.'
                        : `${hint} It is installed over your files: worlds, plugins, mods and configs stay, server jars in the top folder and versions/ are replaced.`}
                </p>
                {!keepJar && detection !== null && !versionChosen && (
                    <p className="text-xs text-(--warning-light) mt-1">
                        Pick the version your files were made with. A newer one upgrades the world on first start, and that cannot be undone.
                    </p>
                )}
            </div>
            {!keepJar && <div className="min-h-[220px]">{children}</div>}
        </div>
    );
}
