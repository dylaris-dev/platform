// A tenant's own custom-domain ownership claims.
//
// Pointing your own domain at the platform needs proof that you control it,
// because otherwise anyone could claim any name. The proof is a TXT record
// carrying a token minted for this account and this domain: a domain that
// merely resolves to us proves nothing about WHICH account it belongs to.
//
// Everything here is scoped to the signed-in user inside Core. A block is
// recorded per (user, domain), never per domain - a global block would let
// someone lock a competitor's domain out of the platform by entering it and
// never configuring it.

import { API_URL, getAuthHeader, handleResponse } from '@/lib/api/core';

// pending   - inside the grant, waiting for the TXT record
// verified  - proven, routes may be added freely
// blocked   - one missed deadline; another attempt is allowed
// permablocked - out of attempts; only checking the TXT record lifts it
export type CustomDomainState = 'pending' | 'verified' | 'blocked' | 'permablocked';

export interface CustomDomain {
  domain: string;
  state: CustomDomainState;
  attempts: number;
  deadlineAt?: string;
  // The proof record, present until the domain is verified.
  txtName?: string;
  txtValue?: string;
}

// These three RAISE on a failed request, which handleResponse itself does not
// do - it resolves with { success: false, message }, and that is right for a
// caller which renders the message.
//
// The callers here render a STATE instead, and a state has no way to say "we
// could not ask". Flattened to their empty values, a 403 or a 502 read as: you
// own no domains, there is no verification token, the check succeeded. The last
// two are silent; the first is worse than silent, because the panel hides
// itself on an empty list and the only way back from a permanently blocked
// domain is the TXT record inside it.
export const listCustomDomains = async (): Promise<CustomDomain[]> => {
  const res = await fetch(`${API_URL}/gateway/custom-domains`, { headers: getAuthHeader() });
  const data = await handleResponse(res);
  if (!data?.success) throw new Error(data?.message || 'Could not load your domains.');
  return data.domains ?? [];
};

/** Mints (or returns) the TXT record that proves ownership. */
export const issueCustomDomainToken = async (domain: string): Promise<CustomDomain> => {
  const res = await fetch(
    `${API_URL}/gateway/custom-domains/${encodeURIComponent(domain)}/txt-token`,
    { method: 'POST', headers: getAuthHeader() },
  );
  const data = await handleResponse(res);
  if (!data?.success) throw new Error(data?.message || 'Could not create a verification record.');
  return data.domain;
};

/**
 * Checks the published TXT record. 409 means "not visible yet", not "wrong",
 * and Core says so in the message - which is why the message is what the throw
 * carries.
 */
export const verifyCustomDomainTXT = async (domain: string): Promise<string> => {
  const res = await fetch(
    `${API_URL}/gateway/custom-domains/${encodeURIComponent(domain)}/verify-txt`,
    { method: 'POST', headers: getAuthHeader() },
  );
  const data = await handleResponse(res);
  if (!data?.success) throw new Error(data?.message || 'The record was not found yet.');
  return data.message || 'Verified.';
};
