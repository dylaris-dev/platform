"use client";

import { useCallback, useEffect, useState } from 'react';
import { Globe, ShieldCheck, Clock, AlertTriangle, Check, Copy } from 'lucide-react';
import {
  listCustomDomains,
  issueCustomDomainToken,
  verifyCustomDomainTXT,
  type CustomDomain,
} from '@/lib/api/customDomains';
import { getGatewayRouteOptions } from '@/lib/api/types';
import { cnameTargetsFor } from '@/lib/cnameTargets';

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
      return {
        tone: 'text-(--success)',
        title: 'Verified',
        body: 'You have proven you own this domain. You can add routes on it.',
      };
    case 'pending': {
      // Two records, two jobs: the TXT record below proves the domain is yours,
      // the CNAME brings players to us. Pointing at us alone proves nothing
      // about WHICH account the domain belongs to.
      const proof = 'Publish the TXT record below to prove you own this domain. We check every 30 minutes.';
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
        title: 'Not set up in time',
        body: 'The route was removed. Publish the TXT record below, check it, then add the route again.',
      };
    default:
      return {
        tone: 'text-(--error)',
        title: 'Blocked',
        body: 'Too many failed checks. Publish the TXT record below to prove you own this domain.',
      };
  }
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

  const load = useCallback(async () => {
    try {
      setDomains(await listCustomDomains());
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
    try {
      // ok: true is now reachable only when the request actually succeeded.
      // It used to be unconditional, so a 409 ("the TXT record was not found
      // yet") printed Core's failure message in success green under a domain
      // that stayed blocked.
      setNote({ domain, ok: true, text: await verifyCustomDomainTXT(domain) });
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

  const copy = (value: string) => {
    navigator.clipboard.writeText(value).then(() => {
      setCopied(value);
      setTimeout(() => setCopied(null), 1600);
    });
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
  if (domains.length === 0) return null;

  return (
    <div className="space-y-3 rounded-md border border-(--base-03) bg-(--base-01) p-4">
      <div className="flex items-center gap-2 text-sm font-medium text-(--base-09)">
        <Globe size={15} className="text-(--accent-light)" />
        Your own domains
      </div>

      <ul className="space-y-3">
        {domains.map((d) => {
          const info = describeClaim(d, cnameTargets, cnameLookupFailed);
          const left = d.state === 'pending' ? timeLeft(d.deadlineAt) : null;
          return (
            <li key={d.domain} className="rounded-md border border-(--base-03) bg-(--base-02) p-3">
              <div className="flex flex-wrap items-center gap-2">
                <span className="font-mono text-xs text-(--base-09)">{d.domain}</span>
                <span className={`inline-flex items-center gap-1 text-xs ${info.tone}`}>
                  {d.state === 'verified' ? <ShieldCheck size={13} /> : null}
                  {d.state === 'pending' ? <Clock size={13} /> : null}
                  {d.state === 'blocked' || d.state === 'permablocked' ? <AlertTriangle size={13} /> : null}
                  {info.title}
                </span>
                {left ? <span className="text-xs text-(--base-06)">{left}</span> : null}
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
                  <button
                    type="button"
                    onClick={() => verify(d.domain)}
                    disabled={busy === d.domain}
                    className="btn btn-primary btn-sm"
                  >
                    {busy === d.domain ? 'Checking…' : "I've added it - check now"}
                  </button>
                </div>
              ) : null}

              {note && note.domain === d.domain ? (
                <p className={`mt-2 text-xs ${note.ok ? 'text-(--success)' : 'text-(--warning)'}`}>
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
