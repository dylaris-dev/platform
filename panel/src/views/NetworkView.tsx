"use client";

import { useState } from 'react';
import { Server, linkServerToProxy, unlinkServerFromProxy } from '@/lib/api';
import { backendAddress } from '@/lib/proxyConfig';
import { Network, Link, Unlink, Info, Copy, Server as ServerIcon } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';

function getStatusDot(status: string) {
  switch (status) {
    case 'online': return 'bg-(--success-light)';
    case 'stopped': case 'offline': return 'bg-(--error)';
    default: return 'bg-(--warning) animate-pulse';
  }
}

interface NetworkViewProps {
  server: Server;
  allServers: Server[];
  onServerSelect?: (id: number) => void;
  onRefreshServers?: () => void;
}

export default function NetworkView({ server, allServers, onServerSelect, onRefreshServers }: NetworkViewProps) {
  const { gatewayEnabled } = useAppData();
  const [selectedId, setSelectedId] = useState('');
  const [linkLoading, setLinkLoading] = useState(false);
  const [linkError, setLinkError] = useState<string | null>(null);
  const [copiedKey, setCopiedKey] = useState<string | null>(null);

  const copyToClipboard = (key: string, text: string) => {
    navigator.clipboard.writeText(text);
    setCopiedKey(key);
    setTimeout(() => setCopiedKey(null), 1500);
  };

  const isProxy = server.serverType === 'proxy';
  const isGameServer = !isProxy;

  const linkedProxy = isGameServer && server.proxyId
    ? allServers.find(s => s.id === server.proxyId)
    : null;

  const linkedChildren = isProxy
    ? allServers.filter(s => s.proxyId === server.id && s.serverType !== 'proxy')
    : [];

  const availableProxies = isGameServer
    ? allServers.filter(s => s.serverType === 'proxy' && s.id !== server.id)
    : [];

  const availableGameServers = isProxy
    ? allServers.filter(s => s.serverType !== 'proxy' && !s.proxyId && s.id !== server.id)
    : [];

  // Stable in-network addresses to paste into a proxy config: on a proxy, the
  // linked backends; on a linked game server, itself (so its address can be
  // added to the proxy). Derived client-side from the loaded server list -
  // there is no per-server tenant IP to fetch, and the mc_<uuid> hostname is
  // the reliable address on the shared network.
  const backendList = isProxy ? linkedChildren : (server.proxyId ? [server] : []);

  // fetchAPI does not throw on an HTTP error - it returns {success:false,error}.
  // So the try/catch these two used to sit in caught nothing, and the result was
  // discarded: every refusal Core has here (proxy feature disabled, target is not
  // a proxy, proxy not found, DB write failed) ended with the dropdown clearing
  // and no word to the user, which reads exactly like it worked.
  const runLink = async (fn: () => Promise<any>) => {
    setLinkLoading(true);
    setLinkError(null);
    try {
      const res = await fn();
      if (res?.success === false) {
        setLinkError(res.message || res.error || 'The request was refused.');
      } else {
        onRefreshServers?.();
      }
    } catch (e) {
      setLinkError(e instanceof Error ? e.message : 'The request could not be sent.');
    }
    setLinkLoading(false);
  };

  const handleLink = async (serverId: number, proxyId: number) => {
    await runLink(() => linkServerToProxy(serverId, proxyId));
    setSelectedId('');
  };

  const handleUnlink = (serverId: number) => runLink(() => unlinkServerFromProxy(serverId));

  return (
    <div className="max-w-3xl mx-auto space-y-6">
      {/* Proxy Linking Section */}
      <div className="card p-6">
        <h2 className="modal-title mb-1 flex items-center gap-2">
          <Network size={20} className="text-(--accent-light)" />
          {isProxy ? 'Linked Servers' : 'Proxy Linking'}
        </h2>
        <p className="text-sm text-(--base-06) mb-5">
          {isProxy
            ? 'Manage game servers linked to this proxy. Linked servers appear as sub-containers in the sidebar.'
            : 'Link this server to a proxy to join a network. Linked servers are managed through the proxy.'}
        </p>

        {/* Sticky rather than a toast: the list below still shows the OLD wiring
            after a refused link, so the reason has to stay on screen next to it. */}
        {linkError && (
          <div className="mb-5 rounded-md border border-(--error) bg-(--error-ghost) px-4 py-3 text-sm text-(--error-light)">
            {linkError}
          </div>
        )}

        {/* Game Server: linked proxy or link dropdown */}
        {isGameServer && (
          <div className="space-y-3">
            {linkedProxy ? (
              <div className="flex items-center justify-between bg-(--base-02) rounded-md px-4 py-3 border border-(--base-03)">
                <div className="flex items-center gap-3">
                  <Network size={16} className="text-(--accent-light)" />
                  <div>
                    <button
                      onClick={() => onServerSelect?.(linkedProxy.id)}
                      className="text-sm font-medium text-(--base-09) hover:text-(--accent-light) transition-colors"
                    >
                      {linkedProxy.name}
                    </button>
                    <div className="mono-label">Linked Proxy</div>
                  </div>
                  <div className={`badge-dot ${getStatusDot(linkedProxy.status)}`} title={linkedProxy.status}></div>
                </div>
                <button
                  onClick={() => handleUnlink(server.id)}
                  disabled={linkLoading}
                  className="btn btn-ghost btn-sm text-(--error-light) hover:bg-(--error-ghost)"
                >
                  <Unlink size={12} />
                  Unlink
                </button>
              </div>
            ) : (
              <>
                <div className="flex items-center gap-2">
                  <select
                    value={selectedId}
                    onChange={e => setSelectedId(e.target.value)}
                    className="input-field flex-1 text-sm"
                  >
                    <option value="">Select a proxy...</option>
                    {availableProxies.map(p => (
                      <option key={p.id} value={String(p.id)}>{p.name}</option>
                    ))}
                  </select>
                  <button
                    onClick={() => selectedId && handleLink(server.id, Number(selectedId))}
                    disabled={!selectedId || linkLoading}
                    className="btn btn-primary btn-sm"
                  >
                    <Link size={12} />
                    Link
                  </button>
                </div>
                {availableProxies.length === 0 && (
                  <p className="text-xs text-(--base-06)">No proxy servers available. Create a proxy server first.</p>
                )}
              </>
            )}
          </div>
        )}

        {/* Proxy Server: linked children */}
        {isProxy && (
          <div className="space-y-3">
            {linkedChildren.length > 0 ? (
              <div className="space-y-2">
                {linkedChildren.map(child => (
                  <div key={child.id} className="flex items-center justify-between bg-(--base-02) rounded-md px-4 py-2.5 border border-(--base-03)">
                    <div className="flex items-center gap-3">
                      <div className={`badge-dot ${getStatusDot(child.status)}`} title={child.status}></div>
                      <button
                        onClick={() => onServerSelect?.(child.id)}
                        className="text-sm font-medium text-(--base-09) hover:text-(--accent-light) transition-colors"
                      >
                        {child.name}
                      </button>
                      {child.activeSubServer && (
                        <span className="text-[10px] bg-(--base-03) px-1.5 py-0.5 rounded text-(--base-06) font-mono">
                          {child.activeSubServer}
                        </span>
                      )}
                    </div>
                    <button
                      onClick={() => handleUnlink(child.id)}
                      disabled={linkLoading}
                      className="btn btn-ghost btn-sm text-(--error-light) hover:bg-(--error-ghost)"
                    >
                      <Unlink size={12} />
                    </button>
                  </div>
                ))}
              </div>
            ) : (
              <p className="text-sm text-(--base-06) italic">No servers linked to this proxy yet.</p>
            )}

            {availableGameServers.length > 0 && (
              <div className="flex items-center gap-2 pt-2 border-t border-(--base-03)">
                <select
                  value={selectedId}
                  onChange={e => setSelectedId(e.target.value)}
                  className="input-field flex-1 text-sm"
                >
                  <option value="">Add a server...</option>
                  {availableGameServers.map(s => (
                    <option key={s.id} value={String(s.id)}>{s.name}</option>
                  ))}
                </select>
                <button
                  onClick={() => selectedId && handleLink(Number(selectedId), server.id)}
                  disabled={!selectedId || linkLoading}
                  className="btn btn-primary btn-sm"
                >
                  <Link size={12} />
                  Add
                </button>
              </div>
            )}
          </div>
        )}
      </div>

      {/* Backend addresses — stable in-network hostnames for the proxy config. */}
      {backendList.length > 0 && (
        <div className="card p-6">
          <h2 className="modal-title mb-1 flex items-center gap-2">
            <ServerIcon size={18} className="text-(--accent-light)" />
            {isProxy ? 'Backend Addresses' : 'Internal Address'}
          </h2>
          <p className="text-sm text-(--base-06) mb-5">
            {isProxy
              ? 'Stable in-network hostnames for the linked servers. Paste these into your proxy config; they survive restarts and sub-server switches.'
              : 'Paste this stable hostname into the proxy config so it can reach this server.'}
          </p>
          <div className="space-y-2">
            {backendList.map(b => {
              const addr = backendAddress(b.uuid, b.containerPort);
              return (
                <div key={b.id} className="flex items-center justify-between gap-2 flex-wrap bg-(--base-02) rounded-md px-3 py-2 border border-(--base-03)">
                  <div className="flex items-center gap-2 min-w-0">
                    <span className="text-sm font-medium text-(--base-09) truncate">{b.name}</span>
                    {b.activeSubServer && (
                      <span className="mono-label bg-(--base-03) px-1.5 py-0.5 rounded-sm text-(--base-06) shrink-0">{b.activeSubServer}</span>
                    )}
                  </div>
                  <div className="flex items-center gap-1.5">
                    <code className="font-mono text-xs text-(--base-08) bg-(--base-03) px-1.5 py-0.5 rounded">{addr}</code>
                    <button onClick={() => copyToClipboard(`addr-${b.id}`, addr)} className="text-(--base-06) hover:text-(--base-09) transition-colors" title="Copy address">
                      <Copy size={11} />
                    </button>
                    {copiedKey === `addr-${b.id}` && <span className="mono-label text-(--success-light)">copied</span>}
                  </div>
                </div>
              );
            })}
          </div>
        </div>
      )}

      {/* Info Card */}
      <div className="flex items-start gap-3 bg-(--base-03) border border-(--base-04) rounded-xl px-4 py-3">
        <Info size={16} className="text-(--base-06) mt-0.5 shrink-0" />
        <div className="text-sm text-(--base-07) space-y-1">
          {isProxy ? (
            <>
              <p>Linked servers appear nested under this proxy in the sidebar.</p>
              <p>Use the <span className="font-medium text-(--base-09)">Members</span> tab to manage permissions. Enable <span className="font-medium text-(--base-09)">Inherit</span> to automatically grant access to all linked servers.</p>
            </>
          ) : (
            <>
              <p>Linking to a proxy groups this server in the sidebar and enables network features.</p>
              <p>If the proxy owner grants you <span className="font-medium text-(--base-09)">Inherit</span> permissions, you'll automatically get access to all linked servers.</p>
            </>
          )}
          {/* Domain routes only exist with gateway routing; pointing at a tab
              that has no such field is worse than saying nothing. */}
          {gatewayEnabled && (
            <p>Domain routes are managed in the <span className="font-medium text-(--base-09)">Setup</span> tab.</p>
          )}
        </div>
      </div>
    </div>
  );
}
