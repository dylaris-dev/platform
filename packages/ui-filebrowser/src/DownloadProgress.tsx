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

// Fixed bottom-right download progress card. When `total` is unknown (0) it
// shows the loaded byte count and an indeterminate pulse bar instead.
const DownloadProgress: React.FC<DownloadProgressProps> = ({ progress }) => (
  <div className="fixed bottom-[calc(1rem+var(--beam-launcher-space,0px))] right-4 z-50 w-68 bg-(--base-01) border border-(--base-04) rounded-lg shadow-xl p-3 animate-fade-in">
    <div className="flex items-center gap-2 mb-2">
      <Download size={13} className="text-(--accent) shrink-0" />
      <span className="text-xs text-(--base-08) truncate font-mono flex-1">{progress.filename}</span>
      <span className="text-[10px] text-(--base-05) font-mono shrink-0">
        {progress.total > 0
          ? `${Math.round((progress.loaded / progress.total) * 100)}%`
          : formatBytes(progress.loaded)}
      </span>
    </div>
    <div className="w-full h-1 bg-(--base-03) rounded-full overflow-hidden">
      {progress.total > 0 ? (
        <div
          className="h-full bg-(--accent) rounded-full transition-all duration-100"
          style={{ width: `${Math.min(100, (progress.loaded / progress.total) * 100)}%` }}
        />
      ) : (
        <div className="h-full bg-(--accent) rounded-full w-1/3 animate-pulse" />
      )}
    </div>
    {progress.total > 0 && (
      <div className="flex justify-between text-[10px] text-(--base-05) font-mono mt-1.5">
        <span>{formatBytes(progress.loaded)}</span>
        <span>{formatBytes(progress.total)}</span>
      </div>
    )}
  </div>
);

export default DownloadProgress;
