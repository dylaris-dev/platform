"use client";

import { useState } from 'react';
import { Server, linkServerToProxy, unlinkServerFromProxy } from '@/lib/api';
import { backendAddress } from '@/lib/proxyConfig';
import { Network, Link, Unlink, Info, Server as ServerIcon, AlertTriangle } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import Select from '@/components/ui/Select';
import CopyText from '@/components/ui/CopyText';
import Tooltip from '@/components/ui/Tooltip';
import Callout from '@/components/server/Callout';
import { stateDot, stateWord } from '@/components/server/ServerHeader';

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

  const hasDirect = !!server.nodeAddress && (server.hostPort ?? 0) > 0;
  const directAddr = hasDirect ? `${server.nodeAddress}:${server.hostPort}` : '';

  return (
    <div className="flex flex-col gap-6">
      {/* Connection: what is already known about how this server is reached. */}
      <section className="srv-surface p-5 md:p-6" aria-labelledby="net-conn">
        <h2 id="net-conn" className="text-base font-medium text-(--base-09)">Connection</h2>
        <dl className="mt-4 grid grid-cols-1 gap-x-8 gap-y-4 sm:grid-cols-2 lg:grid-cols-4">
          <div className="min-w-0">
            <dt className="srv-label">Role</dt>
            <dd className="mt-1 text-sm text-(--base-09)">{isProxy ? 'Proxy' : 'Game server'}</dd>
          </div>
          {hasDirect && (
            <div className="min-w-0">
              <dt className="srv-label">Address</dt>
              <dd className="mt-1">
                <CopyText value={directAddr} bubble className="srv-num cursor-copy text-sm text-(--base-09) hover:text-(--accent-light)" />
              </dd>
            </div>
          )}
          {hasDirect && (
            <div className="min-w-0">
              <dt className="srv-label">Host port</dt>
              <dd className="srv-num mt-1 text-sm text-(--base-09)">{server.hostPort}</dd>
            </div>
          )}
          {(server.containerPort ?? 0) > 0 && (
            <div className="min-w-0">
              <dt className="srv-label">Container port</dt>
              <dd className="srv-num mt-1 text-sm text-(--base-09)">{server.containerPort}</dd>
            </div>
          )}
        </dl>
      </section>

      {/* Proxy linking */}
      <section className="srv-surface p-5 md:p-6" aria-labelledby="net-link">
        <h2 id="net-link" className="flex items-center gap-2 text-base font-medium text-(--base-09)">
          <Network size={16} className="text-(--base-06)" aria-hidden="true" />
          {isProxy ? 'Linked servers' : 'Proxy linking'}
        </h2>
        <p className="mt-1 text-sm text-(--base-06)">
          {isProxy
            ? 'Manage game servers linked to this proxy. Linked servers appear as sub-containers in the sidebar.'
            : 'Link this server to a proxy to join a network. Linked servers are managed through the proxy.'}
        </p>

        {/* Sticky rather than a toast: the list below still shows the OLD wiring
            after a refused link, so the reason has to stay on screen next to it. */}
        {linkError && (
          <div className="mt-4">
            <Callout tone="error" icon={<AlertTriangle size={16} />} role="alert">{linkError}</Callout>
          </div>
        )}

        {/* Game Server: linked proxy or link dropdown */}
        {isGameServer && (
          <div className="mt-5 space-y-3">
            {linkedProxy ? (
              <div className="flex items-center justify-between gap-3 rounded-lg bg-(--base-03) px-4 py-3">
                <div className="flex min-w-0 items-center gap-3">
                  <span className={`badge-dot ${stateDot(linkedProxy.status)}`} aria-hidden="true" />
                  <div className="min-w-0">
                    <button
                      onClick={() => onServerSelect?.(linkedProxy.id)}
                      className="truncate rounded text-sm font-medium text-(--base-09) transition-colors hover:text-(--accent-light) focus-visible:shadow-(--focus-ring) focus-visible:outline-none"
                    >
                      {linkedProxy.name}
                    </button>
                    <div className="srv-label">Linked proxy · {stateWord(linkedProxy.status)}</div>
                  </div>
                </div>
                <button
                  onClick={() => handleUnlink(server.id)}
                  disabled={linkLoading}
                  className="btn btn-ghost btn-sm text-(--error-light) hover:bg-(--error-ghost)"
                >
                  <Unlink size={14} />
                  Unlink
                </button>
              </div>
            ) : (
              <>
                <div className="flex items-center gap-2">
                  <Select
                    ariaLabel="Proxy"
                    className="flex-1"
                    value={selectedId}
                    onChange={setSelectedId}
                    placeholder="Select a proxy..."
                    options={availableProxies.map(p => ({ value: String(p.id), label: p.name }))}
                  />
                  <button
                    onClick={() => selectedId && handleLink(server.id, Number(selectedId))}
                    disabled={!selectedId || linkLoading}
                    className="btn btn-primary btn-sm"
                  >
                    <Link size={14} />
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
          <div className="mt-5 space-y-3">
            {linkedChildren.length > 0 ? (
              <ul className="-mx-2">
                {linkedChildren.map(child => (
                  <li key={child.id} className="flex items-center justify-between gap-3 rounded-lg px-2 py-2 transition-colors hover:bg-(--base-03)">
                    <div className="flex min-w-0 items-center gap-3">
                      <span className={`badge-dot ${stateDot(child.status)}`} aria-hidden="true" />
                      <button
                        onClick={() => onServerSelect?.(child.id)}
                        className="truncate rounded text-sm font-medium text-(--base-09) transition-colors hover:text-(--accent-light) focus-visible:shadow-(--focus-ring) focus-visible:outline-none"
                      >
                        {child.name}
                      </button>
                      <span className="text-xs text-(--base-06)">{stateWord(child.status)}</span>
                      {child.activeSubServer && (
                        <span className="srv-chip srv-num h-5 bg-(--base-03) px-2 text-(--base-07)">{child.activeSubServer}</span>
                      )}
                    </div>
                    <Tooltip content="Unlink server">
                      <button
                        onClick={() => handleUnlink(child.id)}
                        disabled={linkLoading}
                        aria-label={`Unlink ${child.name}`}
                        className="btn btn-ghost btn-icon btn-sm text-(--error-light) hover:bg-(--error-ghost)"
                      >
                        <Unlink size={14} />
                      </button>
                    </Tooltip>
                  </li>
                ))}
              </ul>
            ) : (
              <p className="text-sm text-(--base-06)">No servers linked to this proxy yet.</p>
            )}

            {availableGameServers.length > 0 && (
              <div className="flex items-center gap-2 pt-2">
                <Select
                  ariaLabel="Server to add"
                  className="flex-1"
                  value={selectedId}
                  onChange={setSelectedId}
                  placeholder="Add a server..."
                  options={availableGameServers.map(s => ({ value: String(s.id), label: s.name }))}
                />
                <button
                  onClick={() => selectedId && handleLink(Number(selectedId), server.id)}
                  disabled={!selectedId || linkLoading}
                  className="btn btn-primary btn-sm"
                >
                  <Link size={14} />
                  Add
                </button>
              </div>
            )}
          </div>
        )}
      </section>

      {/* Backend addresses: stable in-network hostnames for the proxy config. */}
      {backendList.length > 0 && (
        <section className="srv-surface p-5 md:p-6" aria-labelledby="net-backend">
          <h2 id="net-backend" className="flex items-center gap-2 text-base font-medium text-(--base-09)">
            <ServerIcon size={16} className="text-(--base-06)" aria-hidden="true" />
            {isProxy ? 'Backend addresses' : 'Internal address'}
          </h2>
          <p className="mt-1 text-sm text-(--base-06)">
            {isProxy
              ? 'Stable in-network hostnames for the linked servers. Paste these into your proxy config; they survive restarts and sub-server switches.'
              : 'Paste this stable hostname into the proxy config so it can reach this server.'}
          </p>
          <ul className="-mx-2 mt-4">
            {backendList.map(b => {
              const addr = backendAddress(b.uuid, b.containerPort);
              return (
                <li key={b.id} className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1 rounded-lg px-2 py-2 transition-colors hover:bg-(--base-03)">
                  <div className="flex min-w-0 items-center gap-2">
                    <span className="truncate text-sm font-medium text-(--base-09)">{b.name}</span>
                    {b.activeSubServer && (
                      <span className="srv-chip srv-num h-5 bg-(--base-03) px-2 text-(--base-07)">{b.activeSubServer}</span>
                    )}
                  </div>
                  <CopyText
                    value={addr}
                    bubble
                    title="Copy address"
                    className="srv-num cursor-copy break-all text-left text-xs text-(--base-08) hover:text-(--accent-light)"
                  />
                </li>
              );
            })}
          </ul>
        </section>
      )}

      <Callout tone="info" icon={<Info size={16} />}>
        {isProxy ? (
          <div className="space-y-1">
            <p>Linked servers appear nested under this proxy in the sidebar.</p>
            <p>Use the <span className="font-medium text-(--base-09)">Members</span> tab to manage permissions. Enable <span className="font-medium text-(--base-09)">Inherit</span> to automatically grant access to all linked servers.</p>
            {gatewayEnabled && <p>Domain routes are managed in the <span className="font-medium text-(--base-09)">Setup</span> tab.</p>}
          </div>
        ) : (
          <div className="space-y-1">
            <p>Linking to a proxy groups this server in the sidebar and enables network features.</p>
            <p>If the proxy owner grants you <span className="font-medium text-(--base-09)">Inherit</span> permissions, you&apos;ll automatically get access to all linked servers.</p>
            {/* Domain routes only exist with gateway routing; pointing at a tab
                that has no such field is worse than saying nothing. */}
            {gatewayEnabled && <p>Domain routes are managed in the <span className="font-medium text-(--base-09)">Setup</span> tab.</p>}
          </div>
        )}
      </Callout>
    </div>
  );
}
