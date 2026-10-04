"use client";

import React, { useEffect, useMemo, useState } from 'react';
import { FileBrowser } from '@dylaris/ui-filebrowser';
import type { BeamConnectionMode } from '@dylaris/ui-filebrowser';
import { createBeamAdapter, isWails, syncSessionWithWails, connectWailsToServer, getWailsConnectionMode } from '@/lib/adapters';
import { useDevMode, devLog } from '@/lib/devLog';
import DebugLogPanel from '@/components/DebugLogPanel';

interface FileBrowserViewProps {
  currentServerPath: string;
  serverUuid?: string;
  // Read-only demo: buttons stay visible but every mutating action is inert.
  readOnly?: boolean;
}

const FileBrowserView: React.FC<FileBrowserViewProps> = ({ currentServerPath, serverUuid, readOnly }) => {
  // The adapter is fixed per environment — browser stays on HTTP, Wails
  // stays on gRPC. We don't memoize on isWails() because window.go is
  // available before render in Wails (no race).
  const adapter = useMemo(() => createBeamAdapter(), []);
  const devMode = useDevMode();

  // Active beam transport for the toolbar badge. Only set inside Beam Desktop; the
  // browser leaves it null so FileBrowser renders no badge.
  const [connectionMode, setConnectionMode] = useState<BeamConnectionMode | null>(null);

  // Inside Beam Desktop, point the native relay tunnel at whatever server
  // the user is browsing. Re-runs whenever the URL changes server.
  // syncSessionWithWails MUST finish before connecting — ConnectToServer
  // needs the session token the sync pushes to the Wails side.
  useEffect(() => {
    if (!isWails()) {
      devLog('beam.view', 'info', 'FileBrowserView mounted in browser mode (Wails not detected)');
      return;
    }
    devLog('beam.view', 'info', `FileBrowserView mounted in Wails mode, serverUuid=${serverUuid}`);
    let cancelled = false;
    (async () => {
      await syncSessionWithWails();
      if (cancelled) return;
      if (serverUuid) {
        await connectWailsToServer(serverUuid);
        if (cancelled) return;
        const mode = await getWailsConnectionMode();
        if (cancelled) return;
        setConnectionMode(mode ? (mode as BeamConnectionMode) : null);
      }
    })();
    return () => { cancelled = true; };
  }, [serverUuid]);

  return (
    <div className="flex flex-col gap-3 h-full overflow-hidden">
      {/* The scroll lives here: the outer box clips (so the debug log below
          stays pinned), and nothing inside FileBrowser scrolls, so a folder
          taller than the window was cut off with no way to reach the rest. */}
      <div className="flex-1 min-h-0 overflow-y-auto">
        <FileBrowser
          currentServerPath={currentServerPath}
          serverUuid={serverUuid}
          adapter={adapter}
          readOnly={readOnly}
          connectionMode={connectionMode}
        />
      </div>
      {devMode && (
        <div className="shrink-0">
          <DebugLogPanel
            filter="beam."
            title="Beam Debug Log"
            defaultOpen={true}
          />
        </div>
      )}
    </div>
  );
};

export default FileBrowserView;
