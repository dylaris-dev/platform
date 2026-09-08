"use client";

import { useCallback, useState, useEffect, useReducer, useRef } from 'react';
import { getCoreStorage, saveCoreStorage, testCoreStorage } from '@/lib/api/coreStorage';
import { canSaveCoreStorage, type CoreStorageConfig } from '@/lib/coreStorage';
import { listStorageConnections, type StorageConnection } from '@/lib/api';
import { CircleCheck, CircleAlert, HardDrive, Cloud, AlertTriangle, Loader2, Info } from 'lucide-react';
import { SkeletonHeader, SkeletonCard, SkeletonFormRow } from '@/components/Skeleton';
import { useUnsavedChanges } from '@/components/settings/UnsavedChanges';
import { Badge } from '@/components/ui/Badge';
import { systemEvents } from '@/lib/systemEvents';
import { reachReducer, initialReachState, statusLabel, statusRemedy, parseStorageReachEvent } from '@/lib/storageReach';
import StorageHealthCard from '@/components/settings/StorageHealthCard';
import SettingsPage from '@/components/settings/SettingsPage';
import SettingsCard from '@/components/settings/SettingsCard';
import { toast } from '@/components/ui/Toast';
import HelpTip from '@/components/ui/HelpTip';
import { useConnectionTest, readConnTest } from '@/lib/connectionTest';
import { TestConnectionButton, ConnectionTestNote } from '@/components/ui/ConnectionTest';

const BACKENDS = [
  { id: 'path', label: 'Filesystem Path', description: 'A local disk or an OS-level mount (NFS/SMB/WebDAV). Must be reachable by every Core.', icon: HardDrive },
  { id: 's3', label: 'S3-compatible', description: 'Any S3 API: AWS, Cloudflare R2, Backblaze B2, Hetzner, MinIO, Wasabi.', icon: Cloud },
] as const;

const EMPTY: CoreStorageConfig = {
  backend: 'path', path: '', pathConfirmed: false,
  s3Endpoint: '', s3Bucket: '', s3Region: '', s3AccessKey: '', s3SecretKey: '',
  s3PathStyle: false, s3Prefix: '', s3SecretSet: false, connectionId: 0,
};

export default function CoreStorageTab() {
  const [settings, setSettings] = useState<CoreStorageConfig>(EMPTY);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);

  // Kept out of the toast on purpose: a durability warning that scrolls away
  // after three seconds is a warning nobody acts on. It stays on screen until
  // the next test replaces or clears it.
  const [testWarning, setTestWarning] = useState<string | null>(null);

  // onlineCores is the expected participant count shown as the counter's
  // total while a save round is running. The old count-based refusal
  // (blocking the filesystem backend outright above 1 Core) is gone: the
  // server now proves reachability with a real cross-Core round instead of
  // guessing from a count, so a genuinely shared path across many Cores is
  // no longer refused before it is even tried.
  const [onlineCores, setOnlineCores] = useState(0);

  // Saved storage connections for the "use a saved connection" selector.
  const [connections, setConnections] = useState<StorageConnection[]>([]);

  // Snapshot of the last-saved config, used for dirty detection.
  const snapshotRef = useRef<CoreStorageConfig | null>(null);

  const [reach, dispatchReach] = useReducer(reachReducer, initialReachState);
  const reachActive = reach.phase === 'verifying' || reach.phase === 'slow';

  // Bumped at the start of every handleSave call (Save or Retry) and
  // compared after the await: a Retry can start a fresh attempt while an
  // older, stale one is still in flight (that is the whole point of letting
  // Retry fire immediately instead of waiting for the stale request), so the
  // stale attempt's eventual result must be recognized as superseded and
  // discarded rather than clobbering the newer, still-active round's state.
  const saveGenerationRef = useRef(0);

  const showToast = (msg: string, ok = true) => toast(msg, ok);

  useEffect(() => {
    getCoreStorage().then(res => {
      if (res.success && res.settings) {
        // The secret is never returned by GET; keep the local field blank so
        // "unchanged" always means "the admin didn't type anything new".
        const s: CoreStorageConfig = { ...EMPTY, ...res.settings, s3SecretKey: '' };
        setSettings(s);
        snapshotRef.current = s;
      }
      setOnlineCores(res.onlineCores ?? 0);
      setLoading(false);
    });
  }, []);

  useEffect(() => {
    listStorageConnections().then(res => {
      if (res.success && res.connections) setConnections(res.connections);
    });
  }, []);

  const canSave = canSaveCoreStorage(settings, snapshotRef.current);
  // A saved connection supplies the credentials; the inline s3 fields and the
  // inline Test are then hidden (the connection has its own test on the Storage
  // Connections page).
  const usingConnection = settings.backend === 's3' && (settings.connectionId ?? 0) > 0;
  // Legacy: configured before storage connections existed, so the credentials
  // still live on this row. Read-only now — see the notice below.
  const hasInlineS3 = settings.backend === 's3' && !usingConnection && !!settings.s3Bucket.trim();
  // Concrete path used in the copyable docker-stack example below. Falls back to
  // the placeholder so the snippet is still valid before anything is typed.
  const exampleStoragePath = settings.path.trim() || '/mnt/dylaris-shared';

  const handleSave = async (): Promise<boolean> => {
    if (!canSave) {
      const reason =
        settings.backend !== 'path'
          ? 'Fill in the bucket, access key and secret before saving.'
          : 'Enter an absolute path and tick the confirmation checkbox before saving.';
      showToast(reason, false);
      return false;
    }
    const generation = ++saveGenerationRef.current;
    setSaving(true);
    // onlineCores is only the EXPECTED participant count for the counter; the
    // server decides the real participant set from live heartbeats. Captured
    // once so the synthetic success event below uses this SAME total instead
    // of `reach.total` read from the closure captured before this dispatch
    // takes effect (which would still hold the previous round's total).
    const total = Math.max(onlineCores, 1);
    dispatchReach({ type: 'start', total });
    const res = await saveCoreStorage(settings);
    // A newer Retry can have started (and even finished) while this request
    // was in flight; a stale result must not touch state that belongs to
    // that newer, still-active round. See saveGenerationRef above.
    // A newer round owns the UI now; this attempt did not store anything,
    // so it must not report success to a navigation guard either.
    if (saveGenerationRef.current !== generation) return false;
    if (res.success) {
      dispatchReach({
        type: 'progress',
        progress: { confirmed: total, total, done: true, ok: true, results: [] },
      });
      showToast('Core file storage saved.');
      const saved: CoreStorageConfig = { ...settings, s3SecretKey: '', s3SecretSet: settings.s3SecretSet || settings.s3SecretKey !== '' };
      setSettings(saved);
      snapshotRef.current = saved;
      setSaving(false);
      return true;
    } else {
      dispatchReach({
        type: 'failure',
        message: res.message || 'Save failed.',
        progress: res.round ?? null,
        // The real server round id, when the refusal carried one: lets the
        // reducer's own stale-round guard reject this if a newer round's
        // progress has since taken over roundId (see reachReducer 'failure').
        roundId: res.round?.roundId,
      });
      if (!res.round) showToast(res.message || 'Save failed.', false);
    }
    setSaving(false);
    return false;
  };

  // Live X/N counter. The save request stays open for the length of the
  // round, so the progress has to arrive out-of-band.
  useEffect(() => {
    if (!reachActive) return;
    return systemEvents.on('storagereach.changed', (event) => {
      const parsed = parseStorageReachEvent(event.payload);
      if (parsed) dispatchReach(parsed);
    });
  }, [reachActive]);

  useEffect(() => {
    if (!reachActive) return;
    const startedAt = Date.now();
    const id = setInterval(() => dispatchReach({ type: 'tick', elapsedMs: Date.now() - startedAt }), 250);
    return () => clearInterval(id);
  }, [reachActive]);

  const handleDiscard = () => { if (snapshotRef.current) setSettings(snapshotRef.current); };

  const dirty = snapshotRef.current !== null && JSON.stringify(settings) !== JSON.stringify(snapshotRef.current);
  useUnsavedChanges({ dirty, save: handleSave, discard: handleDiscard, saving });

  // The shared lifecycle: one at a time, disabled while it runs, and a
  // deadline. The verdict now says whether the endpoint answered at all - a
  // dead endpoint and a rejected access key were the same red toast before.
  const test = useConnectionTest(useCallback(async (signal: AbortSignal) => {
    const res = await testCoreStorage(settings, signal);
    // Cleared on every test, so a warning never outlives the config that
    // produced it: fixing the mount and re-testing makes it disappear.
    setTestWarning(res.warning || null);
    return readConnTest(res, 'Connection successful: write, read and delete all succeeded.');
  }, [settings]));

  const set = <K extends keyof CoreStorageConfig>(key: K, value: CoreStorageConfig[K]) =>
    setSettings(prev => ({ ...prev, [key]: value }));

  if (loading) return (
    <div className="max-w-2xl space-y-6">
      <SkeletonHeader />
      <div className="grid grid-cols-1 sm:grid-cols-2 gap-3"><SkeletonCard height="h-24" /><SkeletonCard height="h-24" /></div>
      <SkeletonFormRow />
    </div>
  );

  // The card owns the save control, but not this save: the fleet reachability
  // round below has a generation guard and a Retry that has to fire while the
  // previous attempt is still in flight, so the existing state machine stays
  // and only hands the card the four things it needs.
  const savable = { dirty, saving, save: handleSave, discard: handleDiscard };
  const blockedReason = canSave
    ? undefined
    : settings.backend !== 'path'
      ? 'Select a storage connection before saving'
      : 'Enter an absolute path and tick the confirmation before saving';

  return (
    <SettingsPage
      title="Core file storage"
      icon={HardDrive}
      width="2xl"
      description="Where Core keeps Library files, ticket attachments and ticket backups. Required before running more than one Core, and before enabling the ticket system."
    >
    <SettingsCard
      title="Backend"
      form={savable}
      saveBlockedReason={blockedReason}
      actions={!usingConnection && <TestConnectionButton test={test} small />}
    >
      {/* Backend selector */}
      <div>
        <label className="input-label mb-2 flex items-center gap-1.5">
          Backend
          <HelpTip label="About the storage backend">
            <p className="mb-2">
              <strong>Local</strong> writes to the disk inside the Core container. It is fine
              for one Core and nothing else: a second replica cannot see the first one&apos;s
              files, so uploads land on whichever container answered and disappear from the
              other.
            </p>
            <p className="mb-2">
              <strong>S3</strong> is what makes Core stateless. Any number of replicas share
              the same bucket, and the container can be replaced without losing anything.
            </p>
            <p>
              Switching backends does not move what is already stored. Use Settings, Storage
              Migration for that.
            </p>
          </HelpTip>
        </label>
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
          {BACKENDS.map(b => {
            const Icon = b.icon;
            const active = settings.backend === b.id;
            return (
              <button
                key={b.id}
                type="button"
                onClick={() => set('backend', b.id)}
                className={`card p-4 text-left transition-all relative focus:outline-none focus-visible:ring-2 focus-visible:ring-(--accent) ${
                  active
                    ? 'border-(--accent) ring-1 ring-(--accent)/40 bg-(--accent-ghost)'
                    : 'border-(--base-03) hover:border-(--base-05)'
                }`}
              >
                <div className="flex items-start gap-3">
                  <div className={`w-9 h-9 rounded-md flex items-center justify-center shrink-0 ${active ? 'bg-(--accent)/20 text-(--accent-light)' : 'bg-(--base-03) text-(--base-06)'}`}>
                    <Icon size={18} />
                  </div>
                  <div className="min-w-0">
                    <div className={`font-medium text-sm flex items-center gap-1.5 ${active ? 'text-(--accent-light)' : 'text-(--base-09)'}`}>
                      {b.label}
                      {active && <CircleCheck size={13} className="text-(--accent-light)" />}
                    </div>
                    <div className="text-xs text-(--base-06) mt-1">{b.description}</div>
                  </div>
                </div>
              </button>
            );
          })}
        </div>
      </div>

      {/* Path backend */}
      {settings.backend === 'path' && (
        <div className="space-y-4">
          <div className="flex flex-col gap-[5px]">
            <label className="input-label" htmlFor="core-storage-path">Absolute Path</label>
            <input
              id="core-storage-path"
              type="text"
              value={settings.path}
              onChange={e => set('path', e.target.value)}
              placeholder="/mnt/dylaris-shared"
              className="input-mono w-full"
            />
          </div>

          <div className="alert alert-warning text-xs">
            <AlertTriangle size={14} className="shrink-0 mt-0.5" />
            <div className="min-w-0 space-y-2.5">
              <p>
                Must be reachable by <strong>every</strong> Core, and must be a <strong>mounted volume</strong> - a plain
                directory lives inside the container and is erased when it is recreated. A host-local directory therefore
                only works with a single Core. For multiple Cores, mount a shared filesystem (NFS/SMB) at this path on
                every host and bind-mount it into the Core container, or use S3. The commented recipe in{' '}
                <code className="font-mono text-[11px]">docker-stack.yml</code> covers the mount options and the
                bind-propagation setting that a share mounted after container start requires.
              </p>
              <div>
                <p className="mb-1 text-(--base-07)">
                  Example <code className="font-mono text-[11px]">docker-stack.yml</code> volume mapping for this path:
                </p>
                <pre className="p-2.5 rounded-md bg-(--base-02) border border-(--base-04) font-mono text-[11px] leading-relaxed overflow-x-auto text-(--base-08)">{`services:
  core:
    volumes:
      - ${exampleStoragePath}:${exampleStoragePath}`}</pre>
                <p className="mt-1.5 text-(--base-06)">
                  Bind-mount the same host path identically into <strong>every</strong> Core replica, matching the path
                  entered above.
                </p>
              </div>
            </div>
          </div>

          <label className="flex items-start gap-2 cursor-pointer select-none group">
            <input
              type="checkbox"
              checked={settings.pathConfirmed}
              onChange={e => set('pathConfirmed', e.target.checked)}
              className="checkbox mt-0.5"
            />
            <span className="text-xs text-(--base-08) group-hover:text-(--base-09)">
              I confirm this path is shared across all Cores, or I run a single Core.
            </span>
          </label>
          {!settings.pathConfirmed && (
            <p className="text-xs text-(--error-light)">Required: Save is disabled until this is confirmed.</p>
          )}
        </div>
      )}

      {/* S3 backend */}
      {settings.backend === 's3' && (
        <div className="space-y-4">
          <div className="flex flex-col gap-[5px]">
            <label className="input-label" htmlFor="core-storage-connection">Storage connection</label>
            <select
              id="core-storage-connection"
              value={settings.connectionId ?? 0}
              onChange={e => set('connectionId', Number(e.target.value))}
              className="input-field w-full"
            >
              <option value={0}>&mdash; select a connection &mdash;</option>
              {connections.map(c => <option key={c.id} value={c.id}>{c.name}</option>)}
            </select>
            <p className="text-xs text-(--base-06)">
              Buckets and credentials are defined once under Settings &rarr; Storage Connections and
              referenced here, so a rotation happens in one place instead of once per screen.
            </p>
          </div>

          {usingConnection ? (
            <div className="alert alert-info text-xs flex items-start gap-2">
              <Info size={14} className="shrink-0 mt-0.5" />
              <span>Credentials come from the selected connection. Manage or test it on the Storage Connections page.</span>
            </div>
          ) : hasInlineS3 ? (
            /* An install configured before connections existed still has its own
               endpoint/bucket/key stored here. It keeps working untouched - the
               fields are just no longer editable from this screen, because two
               editable copies of one credential is exactly the state that made a
               rotation miss one of them. Recreate it as a connection to move it. */
            <div className="alert alert-warning text-xs flex items-start gap-2">
              <AlertTriangle size={14} className="shrink-0 mt-0.5" />
              <span>
                This backend still carries its own inline credentials
                (<span className="font-mono">{settings.s3Bucket || 'bucket'}</span>
                {settings.s3Endpoint ? <> at <span className="font-mono">{settings.s3Endpoint}</span></> : null}).
                It keeps working, but credentials are no longer edited here. Recreate it under
                Settings &rarr; Storage Connections and select it above.
              </span>
            </div>
          ) : (
            <div className="alert alert-warning text-xs flex items-start gap-2">
              <AlertTriangle size={14} className="shrink-0 mt-0.5" />
              <span>
                {connections.length === 0
                  ? 'No storage connections exist yet. Create one under Settings → Storage Connections, then select it above.'
                  : 'Pick a connection above — S3 storage is not configured until you do.'}
              </span>
            </div>
          )}
        </div>
      )}

      {/* Live fleet-wide reachability round, run on every save: the settings
          are persisted only once every online Core proves it can read and
          write the same shared storage. */}
      {reachActive && (
        <div className="alert alert-info text-xs flex flex-col gap-2" role="status" aria-live="polite">
          <div className="flex items-center gap-2">
            <Loader2 size={14} className="animate-spin" />
            <span className="font-mono text-[11px] tracking-[0.04em] text-(--base-09)">
              {reach.confirmed}/{reach.total} cores can read and write this storage
            </span>
          </div>
          {reach.phase === 'slow' && (
            <span className="text-(--base-06)">
              Taking longer than usual (first setup or cross-region mounts can be slower).
            </span>
          )}
        </div>
      )}

      {reach.phase === 'failed' && (
        <div className="alert alert-error text-xs flex flex-col gap-3" role="alert">
          <span className="text-(--base-09)">{reach.message}</span>
          {reach.results.length > 0 && (
            <ul className="flex flex-col gap-1.5">
              {reach.results.map(r => (
                <li key={r.coreId} className="flex flex-col gap-0.5">
                  <div className="flex items-center gap-2">
                    <Badge variant={r.status === 'ok' ? 'success' : 'warning'}>{statusLabel(r.status)}</Badge>
                    <span className="font-mono text-[11px] text-(--base-07)">{r.coreId}</span>
                  </div>
                  {r.status !== 'ok' && (
                    <span className="text-(--base-06)">
                      {statusRemedy(r.status)}
                      {r.missingPeers?.length ? ` Cannot see: ${r.missingPeers.join(', ')}.` : ''}
                      {r.deniedPeers?.length ? ` Cannot write into: ${r.deniedPeers.join(', ')}.` : ''}
                    </span>
                  )}
                </li>
              ))}
            </ul>
          )}
          <button
            type="button"
            className="btn btn-secondary btn-sm self-start"
            onClick={handleSave}
            // Driven by the round's own phase, not `saving`: `saving` stays
            // true for as long as the ORIGINAL, possibly-stale request is
            // still in flight, which is exactly the state this failure panel
            // is shown in on a genuine timeout. Retry must fire immediately,
            // not wait for that stale request to resolve.
            disabled={reachActive}
          >
            Retry
          </button>
        </div>
      )}

      <ConnectionTestNote result={test.result} />

      {testWarning && (
        <div className="alert alert-warning text-xs flex items-start gap-2">
          <CircleAlert size={14} className="shrink-0 mt-0.5" />
          <span>{testWarning}</span>
        </div>
      )}
    </SettingsCard>

    <StorageHealthCard />
    </SettingsPage>
  );
}
