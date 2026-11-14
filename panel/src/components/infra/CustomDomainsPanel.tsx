"use client";

import { useCallback, useEffect, useState } from 'react';
import { Globe, ShieldCheck, Clock, AlertTriangle, Check, Copy, Trash2 } from 'lucide-react';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import {
  listCustomDomains,
  issueCustomDomainToken,
  verifyCustomDomainTXT,
  addCustomDomain,
  removeCustomDomain,
  type CustomDomain,
  type CustomDomainList,
} from '@/lib/api/customDomains';
import { getGatewayRouteOptions } from '@/lib/api/types';
import { useAppData } from '@/lib/AppDataContext';
import { providerErrorText } from '@/lib/gatewayProvider';
import { cnameTargetsFor } from '@/lib/cnameTargets';
import { fmtDateTime } from '@/lib/time';

// Plain language per state. The customer needs to know what to DO, not what the
// state machine calls itself.
//
// cnameTargets are FULL names (route.eu.example.com), one per region. This used
// to render the raw operator setting, which is only the label ("route"): the
// instruction read "Add a CNAME to route", and a customer who followed it
// created a record pointing nowhere - then watched the claim expire and the
// route disappear on the deadline.
export function describeClaim(
  d: CustomDomain,
  cnameTargets: string[],
  cnameLookupFailed = false,
): { tone: string; title: string; body: string } {
  switch (d.state) {
    case 'verified':
      // Re-checked daily: a domain that expired or changed hands must stop
      // reaching this account, so the record has to stay where it is.
      if (d.lapsesAt) {
        return {
          tone: 'text-(--warning)',
          title: 'TXT record missing',
          body: `We cannot find the TXT record below. Publish it before ${formatDate(d.lapsesAt)}, or the routes on this domain are removed.`,
        };
      }
      return {
        tone: 'text-(--success-light)',
        title: 'Verified',
        body: 'You have proven you own this domain. Keep the TXT record below published: we check it daily, and a domain without it for 7 days loses its routes.',
      };
    case 'pending': {
      // Two records, two jobs: the TXT record below proves the domain is yours,
      // the CNAME brings players to us. Pointing at us alone proves nothing
      // about WHICH account the domain belongs to.
      const proof = 'Publish the TXT record below to prove you own this domain. We check it on our own every 30 minutes, or press check now; a domain is never blocked for not having it yet.';
      let body = `${proof} Players reach it once the domain points at us.`;
      // An empty target list has two causes and they need different sentences.
      // The operator may genuinely have configured no CNAME label - then the
      // generic line above is correct. Or the lookup that produces the targets
      // failed, and the customer is being told to create a record we are not
      // naming. cnameTargetUsage.test.ts already guards the label from being
      // rendered raw; it cannot see the request that never answered.
      if (cnameLookupFailed) {
        body = `${proof} We could not load the record that brings players to us - reload the page to see it.`;
      } else if (cnameTargets.length === 1) {
        body = `${proof} For players to reach it, add a CNAME to ${cnameTargets[0]}, or an A record to one of our edge addresses.`;
      } else if (cnameTargets.length > 1) {
        // One target per region, and the choice decides which edges answer the
        // customer's players - so they pick, we do not pick for them.
        body = `${proof} For players to reach it, add a CNAME to whichever of these is your region - ${cnameTargets.join(', ')} - or an A record to one of our edge addresses.`;
      }
      return { tone: 'text-(--warning)', title: 'Waiting for DNS', body };
    }
    case 'blocked':
      return {
        tone: 'text-(--warning)',
        title: 'Ownership not proven',
        body: 'The TXT record below was not found in time, so the route was removed. Publish it, check it, then add the route again.',
      };
    default:
      return {
        tone: 'text-(--error-light)',
        title: 'Blocked',
        body: 'Too many failed checks. Publish the TXT record below to prove you own this domain.',
      };
  }
}

// --- Managed gateway: two proofs per domain ---------------------------------

export type ProofState = 'pending' | 'verified' | 'lapsing' | 'blocked' | 'missing' | 'unknown';

export interface ProofRecord {
  purpose: 'platform' | 'provider';
  label: 'This platform' | 'Gateway provider';
  state: ProofState;
  name: string;
  value: string;
  lapsesAt?: string;
  // What the row says about its state, beside the badge; null: the badge says it.
  detail: string | null;
}

// Anything outside the known set is "unknown" rather than a crash: Core closes
// the provider's set, but a newer Core could still add a state.
function proofState(state: string, lapsesAt?: string): ProofState {
  if (state === 'verified') return lapsesAt ? 'lapsing' : 'verified';
  if (state === 'blocked' || state === 'permablocked') return 'blocked';
  if (state === 'pending' || state === 'missing') return state;
  return 'unknown';
}

const PROOF_DETAIL: Record<'platform' | 'provider', Partial<Record<ProofState, string>>> = {
  platform: {
    blocked: 'Ownership was not proven in time. Publish this record and check again.',
    unknown: 'This platform reported a state this panel does not know.',
  },
  provider: {
    missing: 'Not registered at the gateway provider yet. Check now registers it.',
    blocked: 'The gateway provider refuses this domain.',
    unknown: 'The gateway provider reported a state this panel does not know.',
  },
};

/**
 * The TXT records a claim needs: one locally, two on the managed gateway (a
 * claim carrying `provider`, null included), both at the same name.
 */
export function ownDomainRecords(d: CustomDomain): ProofRecord[] {
  const name = d.txtName || d.provider?.txtName || `_dylaris-verify.${d.domain}`;
  const local = proofState(d.state, d.lapsesAt);
  const records: ProofRecord[] = [{
    purpose: 'platform', label: 'This platform',
    state: local, name, value: d.txtValue ?? '', lapsesAt: d.lapsesAt, detail: PROOF_DETAIL.platform[local] ?? null,
  }];
  if (d.provider === undefined) return records;
  const p = d.provider;
  const state = p ? proofState(p.state, p.lapsesAt) : 'unknown';
  records.push({
    purpose: 'provider', label: 'Gateway provider',
    state, name, value: p?.txtValue ?? '', lapsesAt: p?.lapsesAt,
    detail: p ? PROOF_DETAIL.provider[state] ?? null : PROVIDER_UNKNOWN_TEXT,
  });
  return records;
}

/** A lapsing record still counts: the route stays until its lapse time. */
const proven = (r: ProofRecord) => r.state === 'verified' || r.state === 'lapsing';

/** Whether routes may be added on the domain, and the one line that says so. */
export function ownDomainSummary(records: ProofRecord[]): { usable: boolean; text: string } {
  const open = records.filter(r => !proven(r));
  if (open.length === 0) {
    return records.some(r => r.state === 'lapsing')
      ? { usable: true, text: 'Usable for now, but a TXT record has gone missing. Publish it again before the date shown, or the routes on this domain are removed.' }
      : { usable: true, text: 'Both records are verified. You can add routes on this domain.' };
  }
  if (open.length > 1) return { usable: false, text: 'Not usable yet: neither record is verified.' };
  if (open[0].purpose === 'platform') return { usable: false, text: 'Not usable yet: this platform has not verified its record.' };
  return open[0].state === 'unknown'
    ? { usable: false, text: "Not usable yet as far as we know: the gateway provider's status is unknown." }
    : { usable: false, text: 'Not usable yet: the gateway provider has not verified its record.' };
}

export const PROOF_BADGE: Record<ProofState, { cls: string; label: string }> = {
  verified: { cls: 'badge-success', label: 'Verified' },
  pending: { cls: 'badge-warning', label: 'Pending' },
  lapsing: { cls: 'badge-warning', label: 'Needs attention' },
  blocked: { cls: 'badge-error', label: 'Blocked' },
  missing: { cls: 'badge-neutral', label: 'Not created yet' },
  unknown: { cls: 'badge-neutral', label: 'Unknown' },
};

/** The provider plan's own-domain cap for the whole platform, as one line. */
export function ownDomainCapLine(limit: CustomDomainList['limit']): string | null {
  if (!limit) return null;
  const prefix = 'Own-domain addresses on this platform:';
  if (limit.maxCustomDomains === null || limit.maxCustomDomains === undefined) return `${prefix} ${limit.used}, no limit`;
  if (limit.maxCustomDomains === 0) return `${prefix} none allowed by the plan`;
  return `${prefix} ${limit.used} of ${limit.maxCustomDomains}`;
}

/** A provider failure as the provider row says it: Core's text, else the sentence for its code. */
export function providerErrorNote(e: { code?: string; message?: string } | null | undefined): string | null {
  if (!e) return null;
  if (e.message) return e.message;
  return e.code ? providerErrorText(e.code) : null;
}

export const PROVIDER_UNKNOWN_TEXT = 'The gateway provider could not be reached. Its status is unknown.';
export const PROVIDER_COOLDOWN_TEXT = 'The gateway provider can be checked again in a few minutes.';

function formatDate(iso: string): string {
  const t = Date.parse(iso);
  return Number.isNaN(t) ? 'the deadline' : fmtDateTime(t);
}

function timeLeft(deadlineAt?: string): string | null {
  if (!deadlineAt) return null;
  const ms = Date.parse(deadlineAt) - Date.now();
  if (Number.isNaN(ms) || ms <= 0) return null;
  const h = Math.floor(ms / 3_600_000);
  const m = Math.floor((ms % 3_600_000) / 60_000);
  return h > 0 ? `${h}h ${m}m left` : `${m}m left`;
}

export function CustomDomainsPanel() {
  const [domains, setDomains] = useState<CustomDomain[] | null>(null);
  const [cnameTargets, setCnameTargets] = useState<string[]>([]);
  const [busy, setBusy] = useState<string | null>(null);
  const [note, setNote] = useState<{ domain: string; text: string; ok: boolean } | null>(null);
  const [copied, setCopied] = useState<string | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [cnameLookupFailed, setCnameLookupFailed] = useState(false);
  const [customEnabled, setCustomEnabled] = useState(false);
  const [newDomain, setNewDomain] = useState('');
  const [adding, setAdding] = useState(false);
  const [addError, setAddError] = useState('');
  // Managed gateway only.
  const [limit, setLimit] = useState<CustomDomainList['limit']>(undefined);
  const [listProviderError, setListProviderError] = useState<CustomDomainList['providerError']>(undefined);
  // The provider side of the last check: its cooldown, or its refusal text.
  const [providerNote, setProviderNote] = useState<{ domain: string; cooldown: boolean; error: string | null } | null>(null);
  const [reloading, setReloading] = useState(false);
  const { gatewayProvider } = useAppData();
  const remoteMode = gatewayProvider === 'remote';

  const load = useCallback(async () => {
    try {
      const list = await listCustomDomains();
      setDomains(list.domains);
      setLimit(list.limit);
      setListProviderError(list.providerError);
      setLoadError(null);
    } catch (e) {
      // Deliberately NOT an empty list: this panel renders nothing at all for
      // one, so a failed load used to remove the whole section - including the
      // TXT record that is the only way out of a permanent block.
      setLoadError(e instanceof Error ? e.message : 'Could not load your domains.');
      setDomains([]);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  // The label alone is not an instruction. Combining it with the hoster bases is
  // what turns it into a record the customer can actually create, and
  // /gateway/route-options is the endpoint that carries both - the same one the
  // route picker and the gateway settings tab read.
  useEffect(() => {
    getGatewayRouteOptions()
      .then(o => {
        // fetchAPI RESOLVES with { success: false } on a 4xx/5xx, so the catch
        // below never ran for an HTTP failure - the failure arrived here, as an
        // object with no cnameTarget, and became an empty target list that is
        // indistinguishable from "the operator configured none".
        if (o?.success === false) {
          setCnameLookupFailed(true);
          setCnameTargets([]);
          return;
        }
        setCnameLookupFailed(false);
        setCustomEnabled(!!o?.customDomainsEnabled);
        setCnameTargets(cnameTargetsFor(o?.cnameTarget || '', o?.hosterDomains || []));
      })
      .catch(() => setCnameLookupFailed(true));
  }, []);

  const getToken = async (domain: string) => {
    setBusy(domain);
    setNote(null);
    try {
      await issueCustomDomainToken(domain);
      await load();
    } catch (e) {
      // Without this the button spun and then looked untouched, which reads as
      // "nothing happened" rather than as a failure worth retrying.
      setNote({
        domain,
        ok: false,
        text: e instanceof Error ? e.message : 'Could not create a verification record.',
      });
    } finally {
      setBusy(null);
    }
  };

  const verify = async (domain: string) => {
    setBusy(domain);
    setNote(null);
    setProviderNote(null);
    try {
      // ok: true is now reachable only when the request actually succeeded.
      // It used to be unconditional, so a 409 ("the TXT record was not found
      // yet") printed Core's failure message in success green under a domain
      // that stayed blocked.
      const r = await verifyCustomDomainTXT(domain);
      // On the managed gateway green means both sides are done; a call that
      // advanced one side still leaves the domain unusable.
      const ok = r.ok && (r.local === undefined || (r.local === 'verified' && r.provider === 'verified'));
      setNote({ domain, ok, text: r.message });
      if (r.providerCooldown || r.providerError) {
        setProviderNote({ domain, cooldown: r.providerCooldown, error: providerErrorNote(r.providerError) });
      }
      await load();
    } catch (e) {
      setNote({
        domain,
        ok: false,
        text: e instanceof Error ? e.message : 'The record was not found yet.',
      });
    } finally {
      setBusy(null);
    }
  };

  const add = async () => {
    const domain = newDomain.trim().toLowerCase();
    if (!domain) return;
    setAdding(true);
    setAddError('');
    try {
      await addCustomDomain(domain);
      setNewDomain('');
      await load();
    } catch (e) {
      setAddError(e instanceof Error ? e.message : 'Could not add the domain.');
    } finally {
      setAdding(false);
    }
  };

  const remove = async (domain: string) => {
    const ok = await confirmDialog({
      title: `Remove ${domain}?`,
      message: 'It leaves your own domains. Adding it again later gives it a new TXT record to publish.',
      confirmLabel: 'Remove',
      destructive: true,
    });
    if (!ok) return;
    setBusy(domain);
    setNote(null);
    try {
      await removeCustomDomain(domain);
      await load();
    } catch (e) {
      setNote({ domain, ok: false, text: e instanceof Error ? e.message : 'Could not remove the domain.' });
    } finally {
      setBusy(null);
    }
  };

  const copy = (value: string) => {
    navigator.clipboard.writeText(value).then(() => {
      setCopied(value);
      setTimeout(() => setCopied(null), 1600);
    });
  };

  const reload = async () => {
    setReloading(true);
    try {
      await load();
    } finally {
      setReloading(false);
    }
  };

  if (domains === null) {
    return <div className="h-24 rounded-md bg-(--base-02) animate-pulse" aria-hidden />;
  }
  if (loadError) {
    return (
      <div className="flex items-start gap-2 rounded-md border border-(--warning)/30 bg-(--warning-ghost) p-4 text-xs text-(--warning-light)">
        <AlertTriangle size={14} className="mt-0.5 shrink-0" />
        <div className="flex-1">
          <p className="text-(--base-09)">We could not load your own domains.</p>
          <p className="mt-0.5">{loadError}</p>
        </div>
        <button type="button" onClick={load} className="btn btn-secondary btn-sm shrink-0">
          Try again
        </button>
      </div>
    );
  }
  // Hidden for an account that uses only our addresses on a platform with own
  // domains off; once they are on, the add form is the way in.
  if (domains.length === 0 && !customEnabled) return null;

  return (
    <div className="space-y-3 rounded-md border border-(--base-03) bg-(--base-01) p-4">
      <div className="flex items-center gap-2 text-sm font-medium text-(--base-09)">
        <Globe size={15} className="text-(--accent-light)" />
        Your own domains
      </div>

      {customEnabled ? (
        <form
          className="flex flex-col gap-[5px]"
          onSubmit={e => { e.preventDefault(); add(); }}
        >
          <label htmlFor="custom-domain-add" className="input-label">Add a domain</label>
          <div className="flex items-center gap-2">
            <input
              id="custom-domain-add"
              type="text"
              value={newDomain}
              onChange={e => setNewDomain(e.target.value)}
              placeholder="play.example.com"
              className="input-field text-sm flex-1"
              disabled={adding}
            />
            <button type="submit" className="btn btn-primary btn-sm" disabled={adding || !newDomain.trim()}>
              {adding ? 'Adding…' : 'Add'}
            </button>
          </div>
          <p className="text-xs text-(--base-06)">
            {remoteMode
              ? 'You get two TXT records to publish, one for this platform and one for the gateway provider. Once both are verified you can create routes on the domain; nothing runs out meanwhile.'
              : 'You get a TXT record to publish. Once we find it you can create routes on the domain; nothing runs out meanwhile.'}
          </p>
          {addError ? <p className="text-xs text-(--warning)" role="alert">{addError}</p> : null}
        </form>
      ) : null}

      {ownDomainCapLine(limit) ? <p className="text-xs text-(--base-06)">{ownDomainCapLine(limit)}</p> : null}

      <ul className="space-y-3">
        {domains.map((d) => {
          if (d.provider !== undefined) {
            return (
              <RemoteClaim
                key={d.domain}
                d={d}
                cnameTargets={cnameTargets}
                busy={busy === d.domain}
                reloading={reloading}
                providerNote={providerNote && providerNote.domain === d.domain ? providerNote : null}
                listProviderError={providerErrorNote(listProviderError)}
                note={note && note.domain === d.domain ? note : null}
                copied={copied}
                onCopy={copy}
                onRemove={() => remove(d.domain)}
                onGetToken={() => getToken(d.domain)}
                onVerify={() => verify(d.domain)}
                onReload={reload}
              />
            );
          }
          const info = describeClaim(d, cnameTargets, cnameLookupFailed);
          const left = d.state === 'pending' ? timeLeft(d.deadlineAt) : null;
          // A healthy verified claim has nothing to check; the button would only
          // be told "already verified".
          const canCheck = d.state !== 'verified' || !!d.lapsesAt;
          return (
            <li key={d.domain} className="rounded-md border border-(--base-03) bg-(--base-02) p-3">
              <div className="flex flex-wrap items-center gap-2">
                <span className="font-mono text-xs text-(--base-09)">{d.domain}</span>
                <span className={`inline-flex items-center gap-1 text-xs ${info.tone}`}>
                  {d.state === 'verified' && !d.lapsesAt ? <ShieldCheck size={13} /> : null}
                  {d.state === 'pending' ? <Clock size={13} /> : null}
                  {d.state === 'blocked' || d.state === 'permablocked' || d.lapsesAt ? <AlertTriangle size={13} /> : null}
                  {info.title}
                </span>
                {left ? <span className="text-xs text-(--base-06)">{left}</span> : null}
                <button
                  type="button"
                  onClick={() => remove(d.domain)}
                  disabled={busy === d.domain}
                  title="Remove"
                  aria-label={`Remove ${d.domain}`}
                  className="ml-auto p-1.5 text-(--base-06) hover:text-(--error-light) focus-visible:text-(--error-light) transition-colors disabled:opacity-40"
                >
                  <Trash2 size={14} />
                </button>
              </div>
              <p className="mt-1 text-xs text-(--base-06)">{info.body}</p>

              {d.state !== 'verified' && !d.txtValue ? (
                <button
                  type="button"
                  onClick={() => getToken(d.domain)}
                  disabled={busy === d.domain}
                  className="btn btn-secondary btn-sm mt-2"
                >
                  {busy === d.domain ? 'Working…' : 'Show me the record to add'}
                </button>
              ) : null}

              {d.txtValue ? (
                <div className="mt-2 space-y-2">
                  <div className="flex flex-col gap-[5px]">
                    <label className="input-label">TXT record name</label>
                    <div className="flex items-center gap-2">
                      <code className="input-mono flex-1 truncate rounded-md border border-(--base-03) bg-(--base-01) px-3 py-2 text-xs text-(--base-08)">
                        {d.txtName}
                      </code>
                      <button type="button" onClick={() => copy(d.txtName!)} className="btn btn-secondary btn-sm">
                        {copied === d.txtName ? <Check size={12} /> : <Copy size={12} />}
                      </button>
                    </div>
                  </div>
                  <div className="flex flex-col gap-[5px]">
                    <label className="input-label">TXT record value</label>
                    <div className="flex items-center gap-2">
                      <code className="input-mono flex-1 truncate rounded-md border border-(--base-03) bg-(--base-01) px-3 py-2 text-xs text-(--base-08)">
                        {d.txtValue}
                      </code>
                      <button type="button" onClick={() => copy(d.txtValue!)} className="btn btn-secondary btn-sm">
                        {copied === d.txtValue ? <Check size={12} /> : <Copy size={12} />}
                      </button>
                    </div>
                  </div>
                  {canCheck ? (
                    <button
                      type="button"
                      onClick={() => verify(d.domain)}
                      disabled={busy === d.domain}
                      className="btn btn-primary btn-sm"
                    >
                      {busy === d.domain ? 'Checking…' : "I've added it - check now"}
                    </button>
                  ) : null}
                </div>
              ) : null}

              {note && note.domain === d.domain ? (
                <p className={`mt-2 text-xs ${note.ok ? 'text-(--success-light)' : 'text-(--warning)'}`}>
                  {note.text}
                </p>
              ) : null}
            </li>
          );
        })}
      </ul>
    </div>
  );
}

// One claim on the managed gateway: the platform's record and the provider's,
// at one name. Each row carries its state as text, never as colour alone.
function RemoteClaim({
  d, cnameTargets, busy, reloading, providerNote, listProviderError, note, copied, onCopy, onRemove, onGetToken, onVerify, onReload,
}: {
  d: CustomDomain;
  cnameTargets: string[];
  busy: boolean;
  reloading: boolean;
  providerNote: { cooldown: boolean; error: string | null } | null;
  // The list's provider failure, shown on every provider row.
  listProviderError: string | null;
  note: { text: string; ok: boolean } | null;
  copied: string | null;
  onCopy: (value: string) => void;
  onRemove: () => void;
  onGetToken: () => void;
  onVerify: () => void;
  onReload: () => void;
}) {
  const records = ownDomainRecords(d);
  const summary = ownDomainSummary(records);
  const name = records[0].name;
  // A healthy pair has nothing to check; the provider being unreachable is
  // worth a check too, since Check now asks it again.
  const canCheck = records.some(r => r.state !== 'verified');
  const nameId = `own-domain-name-${d.domain}`;
  return (
    <li className="rounded-md border border-(--base-03) bg-(--base-02) p-3">
      <div className="flex items-center gap-2">
        <span className="min-w-0 break-all font-mono text-xs text-(--base-09)">{d.domain}</span>
        <button
          type="button"
          onClick={onRemove}
          disabled={busy}
          title="Remove"
          aria-label={`Remove ${d.domain}`}
          className="ml-auto shrink-0 p-1.5 text-(--base-06) hover:text-(--error-light) focus-visible:text-(--error-light) transition-colors disabled:opacity-40"
        >
          <Trash2 size={14} />
        </button>
      </div>
      <p className={`mt-1 flex items-start gap-1 text-xs ${summary.usable ? 'text-(--success-light)' : 'text-(--warning)'}`}>
        {summary.usable && !records.some(r => r.state === 'lapsing')
          ? <ShieldCheck size={13} className="mt-px shrink-0" />
          : <AlertTriangle size={13} className="mt-px shrink-0" />}
        {summary.text}
      </p>
      {cnameTargets.length > 0 ? (
        <p className="mt-1 text-xs text-(--base-06)">
          For players to reach it, add a CNAME to {cnameTargets.length === 1 ? cnameTargets[0] : `whichever of these is your region - ${cnameTargets.join(', ')}`}.
        </p>
      ) : null}

      <div className="mt-3 space-y-3">
        <p className="text-xs text-(--base-07)">
          Add both values as TXT records at the name below. Most DNS providers let you add two TXT records with the same name.
        </p>
        <div className="flex flex-col gap-[5px]">
          <span id={nameId} className="input-label">TXT record name (the same for both)</span>
          <div className="flex items-center gap-2">
            <code aria-labelledby={nameId} title={name} className="input-mono min-w-0 flex-1 truncate rounded-md border border-(--base-03) bg-(--base-01) px-3 py-2 text-xs text-(--base-08)">
              {name}
            </code>
            <button type="button" onClick={() => onCopy(name)} aria-label="Copy the TXT record name" title="Copy" className="btn btn-secondary btn-sm shrink-0">
              {copied === name ? <Check size={12} /> : <Copy size={12} />}
            </button>
          </div>
        </div>

        {records.map(r => {
          const badge = PROOF_BADGE[r.state];
          const valueId = `own-domain-${r.purpose}-${d.domain}`;
          return (
            <section key={r.purpose} aria-label={`${r.label} record`} className="space-y-2 rounded-md border border-(--base-03) bg-(--base-01) p-3">
              <div className="flex flex-wrap items-center gap-2">
                <span className="text-xs font-medium text-(--base-09)">{r.label}</span>
                <span className={`badge ${badge.cls}`}>{badge.label}</span>
              </div>

              {r.purpose === 'provider' && d.provider === null ? (
                <div className="flex flex-wrap items-center gap-2">
                  <p className="flex-1 text-xs text-(--warning)">{r.detail}</p>
                  <button type="button" onClick={onReload} disabled={reloading} className="btn btn-secondary btn-sm shrink-0">
                    {reloading ? 'Retrying…' : 'Try again'}
                  </button>
                </div>
              ) : r.value ? (
                <div className="flex flex-col gap-[5px]">
                  <span id={valueId} className="input-label">TXT record value</span>
                  <div className="flex items-center gap-2">
                    <code aria-labelledby={valueId} title={r.value} className="input-mono min-w-0 flex-1 truncate rounded-md border border-(--base-03) bg-(--base-02) px-3 py-2 text-xs text-(--base-08)">
                      {r.value}
                    </code>
                    <button type="button" onClick={() => onCopy(r.value)} aria-label={`Copy the ${r.label.toLowerCase()} TXT value`} title="Copy" className="btn btn-secondary btn-sm shrink-0">
                      {copied === r.value ? <Check size={12} /> : <Copy size={12} />}
                    </button>
                  </div>
                </div>
              ) : r.purpose === 'platform' && r.state !== 'verified' ? (
                <button type="button" onClick={onGetToken} disabled={busy} className="btn btn-secondary btn-sm">
                  {busy ? 'Working…' : 'Show me the record to add'}
                </button>
              ) : null}

              {r.detail && !(r.purpose === 'provider' && d.provider === null) ? (
                <p className={`text-xs ${r.state === 'blocked' ? 'text-(--warning)' : 'text-(--base-06)'}`}>
                  {r.detail}
                </p>
              ) : null}

              {r.state === 'lapsing' && r.lapsesAt ? (
                <p className="text-xs text-(--warning)">
                  This record cannot be found. Publish it before {formatDate(r.lapsesAt)}, or the routes on this domain are removed.
                </p>
              ) : null}
              {r.purpose === 'provider' && (providerNote?.error || listProviderError) ? (
                <p className="text-xs text-(--warning)" role="status">{providerNote?.error || listProviderError}</p>
              ) : null}
              {r.purpose === 'provider' && providerNote?.cooldown ? (
                <p className="text-xs text-(--base-06)">{PROVIDER_COOLDOWN_TEXT}</p>
              ) : null}
            </section>
          );
        })}

        {canCheck ? (
          <button type="button" onClick={onVerify} disabled={busy} className="btn btn-primary btn-sm">
            {busy ? 'Checking…' : "I've added them - check now"}
          </button>
        ) : null}
      </div>

      {note ? (
        <p className={`mt-2 text-xs ${note.ok ? 'text-(--success-light)' : 'text-(--warning)'}`} role="status">
          {note.text}
        </p>
      ) : null}
    </li>
  );
}
