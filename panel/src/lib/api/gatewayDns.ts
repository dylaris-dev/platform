// The DNS provider credential, entered here and stored by the gateway HUB.
//
// Core forwards this form and keeps no copy. That is the point: the Hub's
// reconciler deletes records it does not plan, so there is exactly one writer
// and exactly one place the credential lives. Nothing in this file ever receives
// the token back — `hasToken` is the only thing a screen needs to know.

import { API_URL, getAuthHeader, handleResponse, handleError } from '@/lib/api/core';

export interface DnsProviderOption {
    name: string;
    label: string;
}

// One managed name's certificate, as the gateway last saw it. Passed straight
// through by Core, so the shape belongs to the gateway.
export interface CertNameStatus {
    name: string;
    have: boolean;
    expires?: string;
    error?: string;
}

export interface CertStatus {
    last_run_at: string;
    error?: string;
    note?: string;
    names?: CertNameStatus[];
}

export interface GatewayDnsConfig {
    provider: string;
    zones: string[];
    enabled: boolean;
    // has_token says a credential is stored without revealing it. The field is
    // write-only end to end.
    has_token: boolean;
    // env_locked means a credential mounted as DNS_API_TOKEN on the hub wins
    // over anything saved here. Worth showing: otherwise a saved token that
    // never takes effect looks like a bug.
    env_locked: boolean;
    providers: DnsProviderOption[];

    // The certificate half shares the credential above, so it shares this form.
    acme_enabled: boolean;
    acme_email: string;
    acme_directory: string;
    acme_agreed: boolean;
    // Why issuance failed, when it did. It is the whole reason the status
    // travels: everything else about ACME is invisible when it works.
    cert_status?: CertStatus;
}

export interface GatewayDnsResponse {
    success: boolean;
    // available=false means no gateway is deployed (GATEWAY_HUB_URL unset on
    // Core). Not an error: a platform-only install has no records to write.
    available?: boolean;
    config?: GatewayDnsConfig;
    message?: string;
}

export async function getGatewayDns(): Promise<GatewayDnsResponse> {
    try {
        const res = await fetch(`${API_URL}/settings/gateway/dns`, { headers: getAuthHeader() });
        return (await handleResponse(res)) as GatewayDnsResponse;
    } catch (err) {
        return handleError(err) as GatewayDnsResponse;
    }
}

export interface GatewayDnsSave {
    provider: string;
    // Blank KEEPS the stored credential. Sending '' to mean "clear it" would
    // disable DNS every time someone edited a zone.
    token: string;
    zones: string[];
    enabled: boolean;
    acme_enabled: boolean;
    acme_email: string;
    acme_directory: string;
    acme_agreed: boolean;
}

export async function saveGatewayDns(body: GatewayDnsSave): Promise<GatewayDnsResponse> {
    try {
        const res = await fetch(`${API_URL}/settings/gateway/dns`, {
            method: 'PUT',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(body),
        });
        return (await handleResponse(res)) as GatewayDnsResponse;
    } catch (err) {
        return handleError(err) as GatewayDnsResponse;
    }
}

// parseZones turns the comma-separated field into the list the API takes.
// Exported so the rule is unit-testable: a trailing comma must not become a zone
// named '', which the backend would then refuse to match anything against.
export function parseZones(raw: string): string[] {
    return raw
        .split(',')
        .map(z => z.trim().toLowerCase())
        .filter(z => z.length > 0);
}

// One credential probe, as the gateway reported it. The shape belongs to the
// gateway; Core relays it.
export interface GatewayDnsProbe {
    ok: boolean;
    // False means the provider cannot list zones AT ALL. That is a property of
    // the provider, not a fault in the credential, and must not be rendered as
    // one - the remedy is to name the zones by hand, not to mint a new token.
    zone_listing: boolean;
    zones?: string[];
    message: string;
}

export interface GatewayDnsProbeResponse {
    success: boolean;
    available?: boolean;
    probe?: GatewayDnsProbe;
    message?: string;
}

// Try a credential without storing it. Blank token = test the stored one, which
// is how a configuration is re-checked without retyping a secret the form never
// shows back.
export async function probeGatewayDns(provider: string, token: string, signal?: AbortSignal): Promise<GatewayDnsProbeResponse> {
    try {
        const res = await fetch(`${API_URL}/settings/gateway/dns/probe`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ provider, token }),
            signal,
        });
        return (await handleResponse(res)) as GatewayDnsProbeResponse;
    } catch (err) {
        return handleError(err) as GatewayDnsProbeResponse;
    }
}
