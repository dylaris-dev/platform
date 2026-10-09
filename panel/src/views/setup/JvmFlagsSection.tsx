"use client";

import React, { useState } from 'react';
import { AlertTriangle, Info, ChevronDown, ChevronUp, Zap } from 'lucide-react';
import { jvmHeapMB } from '@/lib/jvmHeap';

const FIXED_FLAGS = ['-Dterminal.ansi', '-Djline.terminal'];

const AIKARS_FLAGS = "-XX:+UseG1GC -XX:+ParallelRefProcEnabled -XX:MaxGCPauseMillis=200 " +
    "-XX:+UnlockExperimentalVMOptions -XX:+DisableExplicitGC -XX:+AlwaysPreTouch " +
    "-XX:G1NewSizePercent=30 -XX:G1MaxNewSizePercent=40 -XX:G1HeapRegionSize=8M " +
    "-XX:G1ReservePercent=20 -XX:G1HeapWastePercent=5 -XX:G1MixedGCCountTarget=4 " +
    "-XX:InitiatingHeapOccupancyPercent=15 -XX:G1MixedGCLiveThresholdPercent=90 " +
    "-XX:G1RSetUpdatingPauseTimePercent=5 -XX:SurvivorRatio=32 " +
    "-XX:+PerfDisableSharedMem -XX:MaxTenuringThreshold=1";

const AIKARS_HIGH_MEM_FLAGS = "-XX:+UseG1GC -XX:+ParallelRefProcEnabled -XX:MaxGCPauseMillis=200 " +
    "-XX:+UnlockExperimentalVMOptions -XX:+DisableExplicitGC -XX:+AlwaysPreTouch " +
    "-XX:G1NewSizePercent=40 -XX:G1MaxNewSizePercent=50 -XX:G1HeapRegionSize=16M " +
    "-XX:G1ReservePercent=20 -XX:G1HeapWastePercent=5 -XX:G1MixedGCCountTarget=4 " +
    "-XX:InitiatingHeapOccupancyPercent=15 -XX:G1MixedGCLiveThresholdPercent=90 " +
    "-XX:G1RSetUpdatingPauseTimePercent=5 -XX:SurvivorRatio=32 " +
    "-XX:+PerfDisableSharedMem -XX:MaxTenuringThreshold=1";

interface JvmFlagsSectionProps {
    extraFlags: string;
    onChange: (flags: string) => void;
    ramMB: number;
    disabled?: boolean;
    defaultOpen?: boolean;
}

export default function JvmFlagsSection({ extraFlags, onChange, ramMB, disabled, defaultOpen = false }: JvmFlagsSectionProps) {
    const [open, setOpen] = useState(defaultOpen);
    const hasFixedFlagConflict = FIXED_FLAGS.some(f => extraFlags.includes(f));

    return (
        <div className="flex flex-col gap-[5px]">
            <button
                type="button"
                onClick={() => setOpen(o => !o)}
                className="input-label flex items-center gap-1.5 w-fit cursor-pointer hover:text-(--base-08) transition-colors"
            >
                {open ? <ChevronUp size={12} /> : <ChevronDown size={12} />}
                <span>JVM Flags</span>
                {!open && extraFlags && (
                    <span className="text-[10px] text-(--base-06) font-normal ml-1 normal-case tracking-normal">
                        ({extraFlags.split(' ').filter(Boolean).length} flags configured)
                    </span>
                )}
            </button>

            {open && (
                <div className="space-y-2 animate-fade-in">
                    <div className="flex items-start gap-2">
                        <div className="shrink-0 px-2.5 py-2 rounded-md bg-(--base-03) border border-(--base-04) text-(--base-07) font-mono text-xs opacity-60 mt-px">
                            java -Xms{jvmHeapMB(ramMB)}M -Xmx{jvmHeapMB(ramMB)}M
                        </div>
                        <textarea
                            value={extraFlags}
                            onChange={e => onChange(e.target.value)}
                            placeholder="Leave empty for Aikar's optimized flags (recommended)"
                            disabled={disabled}
                            rows={3}
                            className={`input-mono flex-1 resize-none ${disabled ? 'opacity-40 cursor-not-allowed' : ''}`}
                        />
                    </div>

                    {!disabled && (
                        <div className="flex items-center gap-2">
                            <button
                                type="button"
                                onClick={() => onChange(ramMB >= 12288 ? AIKARS_HIGH_MEM_FLAGS : AIKARS_FLAGS)}
                                className="flex items-center gap-1.5 px-2.5 py-1 rounded-sm bg-(--accent-ghost) border border-(--accent-border) text-(--accent-light) text-xs font-medium hover:bg-(--accent-border) transition-colors"
                            >
                                <Zap size={11} />
                                Aikar&apos;s Flags{ramMB >= 12288 ? ' (High RAM)' : ''}
                            </button>
                            {extraFlags && (
                                <button
                                    type="button"
                                    onClick={() => onChange('')}
                                    className="px-2.5 py-1 rounded-sm bg-(--base-02) border border-(--base-04) text-(--base-06) text-xs hover:text-(--base-08) transition-colors"
                                >
                                    Clear
                                </button>
                            )}
                        </div>
                    )}

                    <p className="text-xs text-(--base-07)">-Xms/-Xmx are set from the allocated RAM, minus headroom the JVM needs outside the heap. Leave empty to use Aikar&apos;s optimized GC flags.</p>

                    {hasFixedFlagConflict && (
                        <p className="text-xs text-(--error-light) flex items-center gap-1.5">
                            <AlertTriangle size={12} />
                            This flag is already applied automatically for colored console output.
                        </p>
                    )}

                    {!disabled && (
                        <div className="relative group/info inline-flex items-center gap-1.5 w-fit">
                            <Info size={12} className="text-(--accent-light)" />
                            <span className="text-xs text-(--accent-light)">About Aikar&apos;s Flags</span>
                            <div className="absolute bottom-full left-0 mb-1.5 hidden group-hover/info:block z-10 w-80 p-3 rounded-md bg-(--base-02) border border-(--base-04) shadow-lg">
                                <p className="text-xs text-(--base-09) font-medium mb-1.5">Aikar&apos;s G1GC Tuning for Minecraft:</p>
                                <ul className="text-xs text-(--base-07) space-y-1">
                                    <li><code className="text-(--accent-light)">UseG1GC</code> — G1 Garbage Collector optimized for MC</li>
                                    <li><code className="text-(--accent-light)">MaxGCPauseMillis=200</code> — Limits GC pause to 200ms</li>
                                    <li><code className="text-(--accent-light)">AlwaysPreTouch</code> — Pre-allocates memory pages at startup</li>
                                    <li><code className="text-(--accent-light)">ParallelRefProcEnabled</code> — Multi-threaded reference processing</li>
                                    <li><code className="text-(--accent-light)">PerfDisableSharedMem</code> — Prevents GC stalls from disk I/O</li>
                                </ul>
                                <p className="text-xs text-(--base-06) mt-2">High RAM variant (12GB+) increases G1 region sizes for better throughput.</p>
                            </div>
                        </div>
                    )}
                </div>
            )}
        </div>
    );
}
