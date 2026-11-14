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
//
// With the managed gateway a claim needs a second proof, the gateway
// provider's: its own TXT value at the same name, carried in `provider`.

import { API_URL, getAuthHeader, handleResponse } from '@/lib/api/core';
import { providerErrorText } from '@/lib/gatewayProvider';

// pending   - inside the grant, waiting for the TXT record
// verified  - proven, routes may be added freely; re-checked daily
// blocked   - one missed deadline; another attempt is allowed
// permablocked - out of attempts; only checking the TXT record lifts it
export type CustomDomainState = 'pending' | 'verified' | 'blocked' | 'permablocked';

/** The gateway provider's claim on a domain. `missing`: it holds none yet. */
export interface ProviderClaim {
  state: CustomDomainState | 'missing';
  txtName?: string;
  txtValue?: string;
  lapsesAt?: string;
}

export interface ProviderError {
  code: string;
  message: string;
}

export interface CustomDomain {
  domain: string;
  state: CustomDomainState;
  attempts: number;
  deadlineAt?: string;
  // The proof record. Shown after verification too: the domain is re-checked,
  // so the record has to stay published.
  txtName?: string;
  txtValue?: string;
  // Set while a verified domain's record is missing: when its routes go.
  lapsesAt?: string;
  // Managed gateway only. Absent in local mode; null when the provider could
  // not be read (the list's providerError says why).
  provider?: ProviderClaim | null;
}

export interface CustomDomainList {
  domains: CustomDomain[];
  // Managed gateway only: the provider plan's own-domain cap for the whole
  // platform (null = no cap, 0 = none) and how many this platform uses.
  limit?: { maxCustomDomains: number | null; used: number };
  providerError?: ProviderError;
}

/** A refused request. `code` is Core's refusal code when it sent one. */
export class CustomDomainError extends Error {
  code?: string;
  constructor(message: string, code?: string) {
    super(message);
    this.code = code;
  }
}

type Body = { success?: boolean; message?: string; error?: string; code?: string; [k: string]: unknown } | null;

/**
 * What a refusal says: Core's own message (session errors carry `message`,
 * gateway errors `error`), else the sentence for its code, else `fallback`.
 */
export function refusalText(body: Body, fallback: string): string {
  const msg = body?.message || body?.error;
  if (typeof msg === 'string' && msg) return msg;
  return body?.code ? providerErrorText(body.code, fallback) : fallback;
}

// handleResponse keeps only `message`, so the raw body is read from a clone:
// the gateway error shape puts its text in `error` and both carry `code`.
async function read(res: Response): Promise<{ data: any; raw: Body }> {
  let raw: Body = null;
  try {
    raw = await res.clone().json();
  } catch {
    // Not JSON; handleResponse keeps the status in its message.
  }
  return { data: await handleResponse(res), raw };
}

export const TOO_MANY_ATTEMPTS = 'Too many attempts, try again in a minute.';

function refused(data: any, raw: Body, fallback: string): CustomDomainError {
  // A non-JSON body has only handleResponse's "Request failed (502)" to say.
  let text = raw ? refusalText(raw, fallback) : (data?.message || fallback);
  // A 429 without text of its own (the limiters normally send one).
  if (data?.status === 429 && !raw?.message && !raw?.error) text = TOO_MANY_ATTEMPTS;
  return new CustomDomainError(text, typeof raw?.code === 'string' ? raw.code : undefined);
}

// These RAISE on a failed request, which handleResponse itself does not
// do - it resolves with { success: false, message }, and that is right for a
// caller which renders the message.
//
// The callers here render a STATE instead, and a state has no way to say "we
// could not ask". Flattened to their empty values, a 403 or a 502 read as: you
// own no domains, there is no verification token, the check succeeded. The last
// two are silent; the first is worse than silent, because the panel hides
// itself on an empty list and the only way back from a permanently blocked
// domain is the TXT record inside it.
export const listCustomDomains = async (): Promise<CustomDomainList> => {
  const res = await fetch(`${API_URL}/gateway/custom-domains`, { headers: getAuthHeader() });
  const { data, raw } = await read(res);
  if (!data?.success) throw refused(data, raw, 'Could not load your domains.');
  return {
    domains: data.domains ?? [],
    ...(data.limit ? { limit: data.limit } : {}),
    ...(data.providerError ? { providerError: data.providerError } : {}),
  };
};

/** Adds a domain of the customer's own and returns the TXT record to publish. */
export const addCustomDomain = async (domain: string): Promise<CustomDomain> => {
  const res = await fetch(`${API_URL}/gateway/custom-domains`, {
    method: 'POST',
    headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
    body: JSON.stringify({ domain }),
  });
  const { data, raw } = await read(res);
  if (!data?.success) throw refused(data, raw, 'Could not add the domain.');
  return data.domain;
};

/** Removes a domain; Core refuses it while a route still uses it. */
export const removeCustomDomain = async (domain: string): Promise<void> => {
  const res = await fetch(`${API_URL}/gateway/custom-domains/${encodeURIComponent(domain)}`, {
    method: 'DELETE',
    headers: getAuthHeader(),
  });
  const { data, raw } = await read(res);
  if (!data?.success) throw refused(data, raw, 'Could not remove the domain.');
};

/** Mints (or returns) the TXT record that proves ownership to this platform. */
export const issueCustomDomainToken = async (domain: string): Promise<CustomDomain> => {
  const res = await fetch(
    `${API_URL}/gateway/custom-domains/${encodeURIComponent(domain)}/txt-token`,
    { method: 'POST', headers: getAuthHeader() },
  );
  const { data, raw } = await read(res);
  if (!data?.success) throw refused(data, raw, 'Could not create a verification record.');
  return data.domain;
};

export type ProofSide = 'verified' | 'pending';

export interface VerifyResult {
  // A side was verified by this call, or both already are.
  ok: boolean;
  message: string;
  // Managed gateway only: each side's state after the check.
  local?: ProofSide;
  provider?: ProofSide;
  providerError?: ProviderError;
  // The provider checked this domain a moment ago and was not asked again.
  providerCooldown: boolean;
}

/**
 * Checks the published TXT record(s). Locally 409 means "not visible yet", not
 * "wrong", and Core says so in the message - which is why the message is what
 * the throw carries. With the managed gateway a 409 still reports both sides,
 * so it is an answer, not a throw.
 */
export const verifyCustomDomainTXT = async (domain: string): Promise<VerifyResult> => {
  const res = await fetch(
    `${API_URL}/gateway/custom-domains/${encodeURIComponent(domain)}/verify-txt`,
    { method: 'POST', headers: getAuthHeader() },
  );
  const { data, raw } = await read(res);
  const sides = raw && (raw.local !== undefined || raw.provider !== undefined);
  if (!data?.success && !sides) throw refused(data, raw, 'The record was not found yet.');
  const message = (typeof raw?.message === 'string' && raw.message) || (data?.success ? 'Verified.' : 'The record was not found yet.');
  const providerError = sides && raw?.providerError ? (raw.providerError as ProviderError) : undefined;
  return {
    ok: !!data?.success,
    message,
    ...(sides ? { local: raw!.local as ProofSide, provider: raw!.provider as ProofSide } : {}),
    ...(providerError ? { providerError } : {}),
    providerCooldown: !!sides && raw?.providerCooldown === true,
  };
};
