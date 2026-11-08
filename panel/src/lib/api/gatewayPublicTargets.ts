import { API_URL, getAuthHeader, handleResponse } from '@/lib/api/core';

// Whether this account's API keys may create gateway routes that point at
// public addresses. Off by default; the panel's own route form is unaffected.
// A failed request raises: reading it as "off" would show a switch that may be
// on, and an admin would turn it on a second time without knowing.
const url = (userId: string) => `${API_URL}/admin/users/${encodeURIComponent(userId)}/gateway-public-targets`;

export const getGatewayPublicTargets = async (userId: string): Promise<boolean> => {
  const data = await handleResponse(await fetch(url(userId), { headers: getAuthHeader() }));
  if (!data?.success) throw new Error(data?.message || 'Could not load the setting.');
  return !!data.enabled;
};

export const setGatewayPublicTargets = async (userId: string, enabled: boolean): Promise<boolean> => {
  const data = await handleResponse(await fetch(url(userId), {
    method: 'PUT',
    headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
    body: JSON.stringify({ enabled }),
  }));
  if (!data?.success) throw new Error(data?.message || 'Could not save the setting.');
  return !!data.enabled;
};
