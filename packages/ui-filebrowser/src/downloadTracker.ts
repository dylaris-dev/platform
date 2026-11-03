import { createContext } from 'react';

// onProgress for one download. savePath is where a native save dialog put the
// file; a browser download has none. note is a passing status to show instead
// of the speed (the app's "Connection lost, retrying"); the next report
// without one clears it.
export type DownloadProgressFn = (loaded: number, total: number, filename?: string, savePath?: string, note?: string) => void;

// A host that tracks downloads globally (the panel's header download manager)
// provides this, and the file browser then hands every download to it instead
// of showing its own card. track runs the download as its own entry, reports
// failure itself and never rejects; it resolves true when the file arrived.
export interface DownloadTracker {
  track(filename: string, total: number, run: (onProgress: DownloadProgressFn) => Promise<void> | void): Promise<boolean>;
}

export const DownloadTrackerContext = createContext<DownloadTracker | null>(null);
