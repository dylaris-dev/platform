"use client";

import React from 'react';
import { Info, AlertTriangle } from 'lucide-react';
import Callout from '@/components/server/Callout';
import '@/styles/server-setup.css';

// The pure part lives in src/lib/javaVersion so the vitest suite can cover it.
// Re-exported here because SetupNewWizard/SetupEditMode import
// them from this module.
export { JAVA_IMAGES, recommendJavaForVersion, recommendJavaForSetup, effectiveMcVersion } from '@/lib/javaVersion';
import { JAVA_IMAGES } from '@/lib/javaVersion';

interface JavaVersionPickerProps {
    value: string;
    onChange: (id: string) => void;
    disabled?: boolean;
    /** Shows the choice without offering it: legible, unlike disabled. */
    readOnly?: boolean;
    serverType?: 'game' | 'proxy';
    recommended?: string;
    /** The Minecraft major version string (e.g. "1.20.4") used in the mismatch warning. */
    mcVersion?: string;
    /** No version could be read: the choice is the operator's to check. */
    versionUnknown?: boolean;
}

export default function JavaVersionPicker({ value, onChange, disabled, readOnly, serverType, recommended, mcVersion, versionUnknown }: JavaVersionPickerProps) {
    const isProxy = serverType === 'proxy';

    const recommendedImage = recommended ? JAVA_IMAGES.find(j => j.id === recommended) : undefined;
    const showMismatchWarning = !!recommended && !!recommendedImage && value !== recommended;

    return (
        <div className="flex flex-col gap-2">
            <span className="srv-label" id="java-version-label">Java version</span>
            <div className="grid grid-cols-1 gap-2 sm:grid-cols-2 xl:grid-cols-4" role="group" aria-labelledby="java-version-label">
                {JAVA_IMAGES.map(j => {
                    const isRecommended = recommended === j.id;
                    if (readOnly) {
                        const selected = value === j.id;
                        return (
                            <span
                                key={j.id}
                                aria-current={selected ? 'true' : undefined}
                                className="srv-setup-option" data-selected={selected}
                            >
                                <span className="flex min-w-0 flex-col"><span className="font-medium">{j.label}</span><span className="srv-num text-xs text-(--base-06)">{isProxy ? j.proxyNote : j.note}</span></span>
                            </span>
                        );
                    }
                    return (
                        <button
                            key={j.id}
                            type="button"
                            onClick={() => !disabled && onChange(j.id)}
                            disabled={disabled}
                            className="srv-setup-option" data-selected={value === j.id} data-recommended={isRecommended && value !== j.id}
                            aria-pressed={value === j.id}
                        >
                            <span className="flex min-w-0 flex-col"><span className="font-medium">{j.label}</span><span className="srv-num text-xs text-(--base-06)">{isProxy ? j.proxyNote : j.note}</span></span>
                            {isRecommended && value !== j.id && (
                                <span className="ml-auto inline-flex items-center gap-1 text-xs text-(--success-light)"><Info size={14} /> Recommended</span>
                            )}
                        </button>
                    );
                })}
            </div>
            {versionUnknown && !showMismatchWarning && (
                <Callout tone="warning" icon={<AlertTriangle size={16} />}>
                    The upload does not say which Minecraft version it runs, so no Java version is recommended.
                    Check the one the pack needs; the wrong one can stop the server from starting.
                </Callout>
            )}
            {showMismatchWarning && (
                <Callout tone="error" icon={<AlertTriangle size={16} />}>
                    {mcVersion
                        ? <>Minecraft {mcVersion} needs {recommendedImage.label}. Running a different Java version can prevent the server from starting.</>
                        : <>{recommendedImage.label} is recommended. Running a different Java version can prevent the server from starting.</>
                    }
                </Callout>
            )}
        </div>
    );
}
