import React from 'react';
import { Download } from 'lucide-react';
import { formatBytes } from './utils';

export interface DownloadProgressState {
  filename: string;
  loaded: number;
  total: number;
}

export interface DownloadProgressProps {
  progress: DownloadProgressState;
}

// Fixed bottom-right download progress card, used when the host has no global
// download manager. A folder zip's total is an estimate, so the percent stops
// at 99 until the card goes away with the finished download. When `total` is
// unknown (0) it shows the loaded byte count and a full-width faint pulse: a
// partial bar read as "already at 30 %".
const DownloadProgress: React.FC<DownloadProgressProps> = ({ progress }) => {
  const pct = progress.total > 0 ? Math.min(99, Math.floor((progress.loaded / progress.total) * 100)) : null;
  return (
    <div className="fixed bottom-[calc(1rem+var(--beam-launcher-space,0px))] right-4 z-50 w-68 bg-(--base-01) border border-(--base-04) rounded-lg shadow-xl p-3 animate-fade-in">
      <div className="flex items-center gap-2 mb-2">
        <Download size={13} className="text-(--accent) shrink-0" />
        <span className="text-xs text-(--base-08) truncate font-mono flex-1">{progress.filename}</span>
        <span className="text-[10px] text-(--base-05) font-mono shrink-0">
          {pct !== null ? `${pct}%` : formatBytes(progress.loaded)}
        </span>
      </div>
      <div className="w-full h-1 bg-(--base-03) rounded-full overflow-hidden">
        {pct !== null ? (
          <div
            className="h-full bg-(--accent) rounded-full transition-all duration-100"
            style={{ width: `${pct}%` }}
          />
        ) : (
          <div className="h-full w-full bg-(--accent) opacity-40 rounded-full animate-pulse" />
        )}
      </div>
      {pct !== null && (
        <div className="flex justify-between text-[10px] text-(--base-05) font-mono mt-1.5">
          <span>{formatBytes(progress.loaded)}</span>
          <span>{formatBytes(progress.total)}</span>
        </div>
      )}
    </div>
  );
};

export default DownloadProgress;
