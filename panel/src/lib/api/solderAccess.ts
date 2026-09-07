import { API_URL, getAuthHeader } from "./core";

export interface SolderClient {
  id: number;
  uuid: string;
  name: string;
  ownerId: string;
  createdAt: string;
}

export interface SolderKey {
  id: number;
  name: string;
  ownerId: string;
  createdAt: string;
}

async function jget<T>(path: string): Promise<T> {
  const res = await fetch(`${API_URL}${path}`, { headers: getAuthHeader() });
  if (!res.ok) throw new Error((await res.json().catch(() => ({}))).message || "Request failed");
  return res.json();
}

export const listClients = () => jget<SolderClient[]>("/solder/clients");

export async function createClient(name: string): Promise<{ success: boolean; client?: SolderClient }> {
  const res = await fetch(`${API_URL}/solder/clients`, {
    method: "POST",
    headers: { "Content-Type": "application/json", ...getAuthHeader() },
    body: JSON.stringify({ name }),
  });
  const data = await res.json().catch(() => ({}));
  return { success: res.ok, client: data.client };
}

export async function deleteClient(id: number): Promise<{ success: boolean }> {
  const res = await fetch(`${API_URL}/solder/clients/${id}`, { method: "DELETE", headers: getAuthHeader() });
  return { success: res.ok };
}

export const listPackClients = (packId: number) => jget<SolderClient[]>(`/packs/${packId}/clients`);

export async function addPackClient(packId: number, clientId: number): Promise<{ success: boolean }> {
  const res = await fetch(`${API_URL}/packs/${packId}/clients/${clientId}`, { method: "POST", headers: getAuthHeader() });
  return { success: res.ok };
}

export async function removePackClient(packId: number, clientId: number): Promise<{ success: boolean }> {
  const res = await fetch(`${API_URL}/packs/${packId}/clients/${clientId}`, { method: "DELETE", headers: getAuthHeader() });
  return { success: res.ok };
}

export const listKeys = () => jget<SolderKey[]>("/solder/keys");

// key is the value issued by the Technic Platform (Edit Profile -> Solder
// Configuration). Leave it empty to have Core mint one instead, which is the
// launcher-access case and never the Platform-link one: Technic only ever
// verifies a key it issued itself.
//
// message is carried through because the two refusals here are both actionable -
// a malformed key and one already registered - and "Create failed" told the
// operator neither.
export async function createKey(name: string, key?: string): Promise<{ success: boolean; plaintext?: string; keyRow?: SolderKey; message?: string }> {
  const res = await fetch(`${API_URL}/solder/keys`, {
    method: "POST",
    headers: { "Content-Type": "application/json", ...getAuthHeader() },
    body: JSON.stringify({ name, key: key ?? "" }),
  });
  const data = await res.json().catch(() => ({}));
  return { success: res.ok, plaintext: data.plaintext, keyRow: data.key, message: data.message };
}

export async function deleteKey(id: number): Promise<{ success: boolean }> {
  const res = await fetch(`${API_URL}/solder/keys/${id}`, { method: "DELETE", headers: getAuthHeader() });
  return { success: res.ok };
}
