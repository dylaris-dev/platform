// Beam Desktop settings page: the one piece of UI the app ships itself.
// Reachable at /__beam/ (linked from the Panel-unreachable error page). Everything else
// the window shows is the remote Dylaris Panel, reverse-proxied through
// the Wails asset server - see proxy.go. This page lets the user point
// the app at a different Panel (dev / staging / self-hosted), is the
// fallback shown when the configured Panel can't be reached, and hosts the
// Phase 3 self-update flow (mandatory + optional).

import { useEffect, useRef, useState } from 'react';

type UpdateInfo = {
  current: string;
  latest: string;
  updateAvailable: boolean;
  downloadUrl: string;
};

type UpdateGate = {
  blocked: boolean;
  current: string;
  minVersion: string;
};

type WailsBindings = {
  GetPanelURL?: () => Promise<string>;
  GetDefaultPanelURL?: () => Promise<string>;
  SavePanelURL?: (token: string, url: string) => Promise<void>;
  GetAPIURL?: () => Promise<string>;
  GetDefaultAPIURL?: () => Promise<string>;
  SaveAPIURL?: (token: string, url: string) => Promise<void>;
  GetUpdateInfo?: () => Promise<UpdateInfo>;
  GetUpdateGate?: () => Promise<UpdateGate>;
  GetUpdateChannel?: () => Promise<string>;
  OpenUpdateDownload?: (token: string) => void;
  ApplyUpdate?: (token: string) => Promise<void>;
  ClearLocalData?: (token: string) => Promise<void>;
  ListPanels?: () => Promise<{ panels: SavedPanel[]; active: string }>;
  SavePanels?: (token: string, panels: SavedPanel[], active: string) => Promise<void>;
  SwitchPanel?: (token: string, url: string) => Promise<void>;
};

// One saved panel. name is the resolved label (the host when nothing was typed);
// rawName is what the user actually entered, so editing does not turn a blank
// name into the host permanently.
type SavedPanel = {
  name: string;
  rawName?: string;
  url: string;
  apiUrl?: string;
  /**
   * The built-in entry for the panel this build is for. Reported by the Go side
   * rather than guessed from the name, because the name is only a label and the
   * URL moves with DYLARIS_PANEL_URL. It has no Edit and no Remove.
   */
  official?: boolean;
};


// Must match panels.go's OfficialPanelName. Only used to stop someone naming
// their own entry the same thing, which would make the list unreadable.
const OFFICIAL_NAME = 'Dylaris Official';

declare global {
  interface Window {
    go?: { main?: { App?: WailsBindings } };
    // Per-run shell capability token, spliced into this page's HTML by the Go
    // proxy (serveBeamIndex). Required as the first arg of the side-effecting
    // bound methods; the proxied Panel never receives it.
    __beamShellToken?: string;
    // Wails injects window.runtime at load; EventsOn returns an unsubscribe fn.
    runtime?: {
      EventsOn?: (eventName: string, callback: (data: any) => void) => (() => void);
    };
  }
}

function getBindings(): WailsBindings | undefined {
  return window.go?.main?.App;
}

// The Beam shell publishes a per-run capability token on window (spliced into
// this page's HTML by the Go proxy). It is read once and passed as the first
// arg of the side-effecting bound methods; a proxied Panel never holds it.
const shellToken = window.__beamShellToken ?? '';

// Human-readable label per self-update phase (matches the Go update:status states).
const UPDATE_LABELS: Record<string, string> = {
  downloading: 'Downloading update...',
  verifying: 'Verifying update...',
  applying: 'Applying update...',
  relaunching: 'Restarting...',
};

export default function App() {
  // Defaults come from the build (GetDefaultPanelURL / GetDefaultAPIURL) - the
  // official binary ships the Dylaris hosts, a fork bakes its own; both fill on
  // mount. The API URL is optional (empty = same-origin /api).
  const [defaultUrl, setDefaultUrl] = useState('');
  const [inputUrl, setInputUrl] = useState('');
  const [apiInputUrl, setApiInputUrl] = useState('');
  const [loaded, setLoaded] = useState(false);
  const [savingError, setSavingError] = useState<string | null>(null);
  const [cleared, setCleared] = useState(false);
  const [panels, setPanels] = useState<SavedPanel[]>([]);
  const [activePanel, setActivePanel] = useState('');
  // The form at the top is BOTH "add" and "edit". editingUrl names the entry it
  // will replace, or '' for a new one - so Edit fills the same fields the user
  // already knows rather than opening a second dialogue somewhere else.
  const [inputName, setInputName] = useState('');
  const [editingUrl, setEditingUrl] = useState('');

  const [update, setUpdate] = useState<UpdateInfo | null>(null);
  // The launcher's update prompt lands here with ?update=1. The click on
  // "Update now" still has to happen on this page: only the shell may start the
  // self-update, so the prompt can bring the user here but not press the button.
  const updateButtonRef = useRef<HTMLButtonElement>(null);
  const cameForUpdate = new URLSearchParams(window.location.search).has('update');
  useEffect(() => {
    if (cameForUpdate && update?.updateAvailable) {
      updateButtonRef.current?.scrollIntoView({ block: 'center' });
      updateButtonRef.current?.focus();
    }
  }, [cameForUpdate, update?.updateAvailable]);
  const [gate, setGate] = useState<UpdateGate | null>(null);

  // Self-update flow state (Phase 3). updateState is one of the update:status
  // states while a self-apply runs, 'error' on failure, or null when idle.
  const [updateState, setUpdateState] = useState<string | null>(null);
  const [updateError, setUpdateError] = useState<string | null>(null);
  const [updateProgress, setUpdateProgress] = useState<{ loaded: number; total: number } | null>(null);
  const [devChannel, setDevChannel] = useState(false);

  // Reflect the effective update channel (set by Core after login) as a DEV badge.
  // GetUpdateChannel reads a local cached value, so a light poll cheaply picks up
  // the channel once the user authenticates without a manual refresh.
  useEffect(() => {
    const bindings = getBindings();
    if (!bindings?.GetUpdateChannel) return;
    let active = true;
    const check = () => bindings.GetUpdateChannel!().then(ch => { if (active) setDevChannel(ch === 'dev'); }).catch(() => {});
    check();
    const id = setInterval(check, 10000);
    return () => { active = false; clearInterval(id); };
  }, []);

  // Pull the current + default Panel and API URLs on mount.
  useEffect(() => {
    const load = async () => {
      try {
        const bindings = getBindings();
        const fallback = (await bindings?.GetDefaultPanelURL?.()) || '';
        setDefaultUrl(fallback);
        const url = (await bindings?.GetPanelURL?.()) || fallback;
        setInputUrl(url);
        // GetAPIURL already falls back to the build default; "" only when none
        // is compiled in, which means same-origin /api.
        setApiInputUrl((await bindings?.GetAPIURL?.()) ?? '');
        const list = await bindings?.ListPanels?.();
        if (list) { setPanels(list.panels ?? []); setActivePanel(list.active ?? ''); }
        bindings?.GetUpdateInfo?.().then(u => setUpdate(u)).catch(() => {});
        bindings?.GetUpdateGate?.().then(g => setGate(g)).catch(() => {});
      } catch (err) {
        console.warn('Panel/API URL resolve failed:', err);
        setInputUrl('');
      } finally {
        setLoaded(true);
      }
    };
    load();
  }, []);

  // Subscribe to the Go self-update events. EventsOn returns an unsubscribe fn.
  useEffect(() => {
    const on = window.runtime?.EventsOn;
    if (!on) return;
    const offProgress = on('update:progress', (data: { loaded: number; total: number }) => {
      setUpdateProgress(data);
    });
    const offStatus = on('update:status', (data: { state: string; message: string }) => {
      setUpdateState(data.state);
      if (data.state === 'error') {
        setUpdateError(data.message || 'Update failed.');
      }
    });
    return () => {
      offProgress?.();
      offStatus?.();
    };
  }, []);

  // Kick off the self-apply flow. Both the mandatory screen and the optional nag
  // call this; the Go side re-verifies fail-closed before applying.
  const startUpdate = () => {
    setUpdateError(null);
    setUpdateProgress(null);
    setUpdateState('downloading');
    getBindings()?.ApplyUpdate?.(shellToken);
  };

  const updating = updateState !== null && updateState !== 'error';
  const pct =
    updateProgress && updateProgress.total > 0
      ? Math.min(100, Math.round((updateProgress.loaded / updateProgress.total) * 100))
      : null;

  // Shared progress/status + error-fallback block rendered inside both update surfaces.
  const updateFlow = (
    <>
      {updating && (
        <div style={{ marginTop: '0.6rem' }}>
          <div style={{ fontSize: '0.8em', opacity: 0.85, marginBottom: '0.4rem' }}>
            {UPDATE_LABELS[updateState as string] || 'Working...'}
          </div>
          <div style={{ height: '6px', background: '#1C1F24', borderRadius: '3px', overflow: 'hidden' }}>
            <div
              style={{
                height: '100%',
                width: pct !== null ? `${pct}%` : '100%',
                background: '#7048C8',
                transition: 'width 0.2s ease',
                opacity: pct !== null ? 1 : 0.5,
              }}
            />
          </div>
        </div>
      )}
      {updateError && (
        <>
          <div className="error" style={{ marginTop: '0.6rem' }}>{updateError}</div>
          <div className="settings-actions">
            <button
              type="button"
              className="btn btn-secondary"
              onClick={() => getBindings()?.OpenUpdateDownload?.(shellToken)}
            >
              Download in browser instead
            </button>
          </div>
        </>
      )}
    </>
  );

  // The panel list. Saved as a WHOLE rather than per entry: the settings page
  // edits it as a list, and add/remove/rename as separate calls would let the
  // stored order and the active choice disagree halfway through an edit.
  const persistPanels = async (next: SavedPanel[], active: string) => {
    setSavingError(null);
    try {
      await getBindings()?.SavePanels?.(shellToken, next, active);
    } catch (err) {
      setSavingError(err instanceof Error ? err.message : String(err));
      return false;
    }
    setPanels(next);
    setActivePanel(active);
    return true;
  };

  // Switching does NOT sign you out. The shell keeps one cookie jar per host, so
  // the panel you are leaving stays signed in and coming back is instant - which
  // is the whole reason a list beats an edit box.
  const switchTo = async (url: string) => {
    setSavingError(null);
    try {
      await getBindings()?.SwitchPanel?.(shellToken, url);
    } catch (err) {
      setSavingError(err instanceof Error ? err.message : String(err));
      return;
    }
    window.location.href = '/';
  };


  // Fills the form at the top from an existing entry. The official one is not
  // editable: its URL is what this build is for, and letting it be repointed is
  // the one edit that can leave the app unable to find the service.
  const editPanel = (p: SavedPanel) => {
    if (p.official) return;
    setEditingUrl(p.url);
    setInputName(p.rawName ?? '');
    setInputUrl(p.url);
    setApiInputUrl(p.apiUrl ?? '');
    setSavingError(null);
  };

  const clearForm = () => {
    setEditingUrl('');
    setInputName('');
    setInputUrl('');
    setApiInputUrl('');
    setSavingError(null);
  };

  const removePanel = async (url: string) => {
    const target = panels.find(p => p.url === url);
    if (target?.official) return;
    const next = panels.filter(p => p.url !== url && !p.official);
    if (editingUrl === url) clearForm();
    // Removing the ACTIVE one has to move it, or the window would go on
    // proxying a panel the list no longer contains. The official entry is
    // always in the list, so there is always somewhere to land.
    const nextActive = url === activePanel ? (next[0]?.url ?? defaultUrl) : activePanel;
    await persistPanels(next, nextActive);
  };

  // The session lives in the app shell, not in the webview, so clearing site
  // data inside the Panel would reach nothing. This is the only way to drop a
  // half-broken session without reinstalling - which is exactly when someone
  // comes looking for a "clear cache" button.
  const handleClearLocalData = async () => {
    setSavingError(null);
    try {
      await getBindings()?.ClearLocalData?.(shellToken);
    } catch (err) {
      setSavingError(err instanceof Error ? err.message : String(err));
      return;
    }
    setCleared(true);
    setTimeout(() => setCleared(false), 2000);
  };

  // Adds the entry, or replaces one, then hands the window back to the Panel.
  //
  // Replacing happens on the NAME, not the URL: re-using a name is how someone
  // corrects the address of a panel they already listed, and creating a second
  // row called "Test" pointing somewhere else is never what they meant. Editing
  // an entry also replaces the row it came from, whatever its name became.
  const handleSave = async () => {
    setSavingError(null);
    let candidate = inputUrl.trim();
    if (!candidate) {
      setSavingError('Panel URL is required');
      return;
    }
    if (!/^https?:\/\//i.test(candidate)) {
      candidate = 'https://' + candidate;
    }
    // The API URL is optional: empty clears the override so the Panel talks to
    // its API same-origin (/api). Normalize the scheme when one is given.
    let apiCandidate = apiInputUrl.trim();
    if (apiCandidate && !/^https?:\/\//i.test(apiCandidate)) {
      apiCandidate = 'https://' + apiCandidate;
    }
    const name = inputName.trim();
    if (name.toLowerCase() === OFFICIAL_NAME.toLowerCase()) {
      setSavingError(`"${OFFICIAL_NAME}" is the built-in entry and cannot be reused as a name.`);
      return;
    }

    // The official entry is not stored by us - the app re-adds it - so it is
    // filtered out of everything written back.
    const others = panels.filter(p => !p.official);
    const replaces = (p: SavedPanel) =>
      (editingUrl && p.url === editingUrl) ||
      (!!name && (p.rawName ?? '').trim().toLowerCase() === name.toLowerCase()) ||
      p.url === candidate;

    const entry: SavedPanel = { name: name || candidate, rawName: name, url: candidate, apiUrl: apiCandidate };
    const next = others.some(replaces)
      ? others.map(p => (replaces(p) ? entry : p))
      : [...others, entry];

    if (!(await persistPanels(next, candidate))) return;
    try {
      await getBindings()?.SwitchPanel?.(shellToken, candidate);
    } catch (err) {
      setSavingError(err instanceof Error ? err.message : String(err));
      return;
    }
    window.location.href = '/';
  };

  // MANDATORY (blocking) tier: the running build is below the server's floor.
  // Full-screen, non-dismissible - no settings form, no cancel. The proxy
  // middleware keeps redirecting Panel requests here while blocked. The button
  // now runs the Phase 3 self-apply; the browser download stays as the fallback
  // shown on error.
  if (gate?.blocked) {
    return (
      <div className="loader">
        <div className="logo">
          <span className="brand-d">D</span>ylaris <span className="brand-beam">Beam</span>
          {devChannel && <span className="dev-badge">DEV</span>}
        </div>
        <div className="settings-card" style={{ borderColor: 'var(--accent, #7048C8)' }}>
          <div className="settings-title">Update required</div>
          <div style={{ fontSize: '0.85em', opacity: 0.85, marginTop: '0.4rem' }}>
            This Beam version ({gate.current}) can no longer connect. The server requires
            at least {gate.minVersion}. Update to continue.
          </div>
          {!updating && (
            <div className="settings-actions">
              <button type="button" className="btn btn-primary" onClick={startUpdate}>
                Update now
              </button>
            </div>
          )}
          {updateFlow}
        </div>
      </div>
    );
  }

  return (
    <div className="loader">
      <div className="logo">
        <span className="brand-d">D</span>ylaris <span className="brand-beam">Beam</span>
        {devChannel && <span className="dev-badge">DEV</span>}
      </div>

      {update?.updateAvailable && (
        <div
          className="settings-card"
          style={{ marginBottom: '1rem', borderColor: 'var(--accent, #7048C8)' }}
        >
          <div className="settings-title">Update available</div>
          <div style={{ fontSize: '0.85em', opacity: 0.85, marginTop: '0.4rem' }}>
            A newer Beam ({update.latest}) is available. You have {update.current}.
          </div>
          {!updating && (
            <div className="settings-actions">
              <button type="button" className="btn btn-primary" ref={updateButtonRef} onClick={startUpdate}>
                Update now
              </button>
            </div>
          )}
          {updateFlow}
        </div>
      )}

      {panels.length > 0 && (
        <div className="settings-card" style={{ marginBottom: '1rem' }}>
          <div className="settings-title">Panels</div>
          <div style={{ fontSize: '0.8em', opacity: 0.7, marginTop: '0.3rem' }}>
            Each keeps its own sign-in, so switching does not log you out.
          </div>
          <ul style={{ listStyle: 'none', padding: 0, margin: '0.75rem 0 0' }}>
            {panels.map(p => {
              const active = p.url === activePanel;
              return (
                <li
                  key={p.url}
                  style={{
                    display: 'flex', alignItems: 'center', gap: '0.5rem',
                    padding: '0.45rem 0.55rem', borderRadius: '6px',
                    background: active ? 'rgba(112,72,200,0.15)' : 'transparent',
                    border: active ? '1px solid rgba(112,72,200,0.45)' : '1px solid transparent',
                  }}
                >
                  <span style={{ flex: 1, minWidth: 0 }}>
                    <span style={{ display: 'block', fontSize: '0.9em' }}>{p.name || p.url}</span>
                    <span style={{ display: 'block', fontSize: '0.75em', opacity: 0.6, wordBreak: 'break-all' }}>
                      {p.url}
                    </span>
                  </span>
                  {active ? (
                    <span style={{ fontSize: '0.75em', opacity: 0.8, whiteSpace: 'nowrap' }}>in use</span>
                  ) : (
                    <button type="button" className="btn btn-secondary" style={{ padding: '0.3rem 0.6rem', fontSize: '0.8em' }} onClick={() => switchTo(p.url)}>
                      Switch
                    </button>
                  )}
                  {/* The official entry has no Edit or Remove. It is the address
                      this build exists for, so repointing or dropping it is the
                      one change that can leave the app unable to find the
                      service - and the app re-adds it anyway. */}
                  {!p.official && (
                    <>
                      <button
                        type="button"
                        className="btn btn-secondary"
                        style={{ padding: '0.3rem 0.55rem', fontSize: '0.8em' }}
                        onClick={() => editPanel(p)}
                        aria-label={`Edit ${p.name || p.url}`}
                      >
                        Edit
                      </button>
                      <button
                        type="button"
                        className="btn btn-secondary"
                        style={{ padding: '0.3rem 0.55rem', fontSize: '0.8em' }}
                        onClick={() => removePanel(p.url)}
                        aria-label={`Remove ${p.name || p.url}`}
                      >
                        Remove
                      </button>
                    </>
                  )}
                </li>
              );
            })}
          </ul>
        </div>
      )}

      <div className="settings-card">
        <div className="settings-title">
          {editingUrl ? 'Edit panel' : 'Add a panel'}
        </div>

        <div className="settings-title" style={{ marginTop: '0.75rem' }}>
          Name{' '}
          <span style={{ fontWeight: 400, opacity: 0.6 }}>(optional - saving under an existing name replaces it)</span>
        </div>
        <input
          type="text"
          className="url-input"
          value={inputName}
          onChange={e => setInputName(e.target.value)}
          placeholder="Production"
          disabled={!loaded}
          onKeyDown={e => {
            if (e.key === 'Enter') handleSave();
            if (e.key === 'Escape') window.location.href = '/';
          }}
        />

        <div className="settings-title" style={{ marginTop: '1rem' }}>Panel URL</div>
        <input
          type="text"
          className="url-input"
          value={inputUrl}
          onChange={e => setInputUrl(e.target.value)}
          placeholder="https://panel.example.com"
          disabled={!loaded}
          onKeyDown={e => {
            if (e.key === 'Enter') handleSave();
            if (e.key === 'Escape') window.location.href = '/';
          }}
        />

        <div className="settings-title" style={{ marginTop: '1rem' }}>
          Backend / API URL{' '}
          <span style={{ fontWeight: 400, opacity: 0.6 }}>(optional - blank = the Panel&apos;s own /api)</span>
        </div>
        <input
          type="text"
          className="url-input"
          value={apiInputUrl}
          onChange={e => setApiInputUrl(e.target.value)}
          placeholder="https://api.example.com"
          disabled={!loaded}
          onKeyDown={e => {
            if (e.key === 'Enter') handleSave();
            if (e.key === 'Escape') window.location.href = '/';
          }}
        />
        {savingError && <div className="error">{savingError}</div>}

        <div className="settings-actions">
          {editingUrl && (
            <button type="button" className="btn btn-secondary" onClick={clearForm}>
              Cancel edit
            </button>
          )}
          <button
            type="button"
            className="btn btn-secondary"
            onClick={handleClearLocalData}
            title="Signs out and forgets the session this app is holding"
          >
            {cleared ? 'Cleared' : 'Clear local data'}
          </button>
          <button
            type="button"
            className="btn btn-secondary"
            onClick={() => { window.location.href = '/'; }}
          >
            Cancel
          </button>
          <button type="button" className="btn btn-primary" onClick={handleSave}>
            {editingUrl ? 'Save & connect' : 'Add & connect'}
          </button>
        </div>

      </div>
    </div>
  );
}
