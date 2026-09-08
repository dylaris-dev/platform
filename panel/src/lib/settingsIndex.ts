// An index of individual SETTINGS, not of pages.
//
// The settings navigation lists 26 page names, and page names are the wrong
// question: nobody looking for Resend, a bucket, a throttle or a retention
// horizon knows which of the 26 it lives behind. Half of these are two levels
// deep now that the long pages are tabbed, which makes it worse rather than
// better.
//
// Every entry carries the words somebody would actually type - including the
// ones that are NOT on screen (an operator types "smtp" for Email, "s3" for
// Storage Connections, "2fa" for two-factor). That is the whole value of the
// index over a filter across the visible labels.

export interface SettingsEntry {
    /** Settings page slug, e.g. "users" for /settings/users. */
    page: string;
    /** Tab within the page, when it has one. Selected on arrival. */
    tab?: string;
    /** What the setting is called on screen. */
    label: string;
    /** Where it sits, for the result row. */
    where: string;
    /** Extra words to match on. Lowercase; the label is matched too. */
    keywords?: string[];
}

export const SETTINGS_INDEX: SettingsEntry[] = [
    // General
    { page: 'status', label: 'System health', where: 'General', keywords: ['status', 'health', 'checks', 'diagnostics'] },
    { page: 'modules', label: 'Navigation modules', where: 'General', keywords: ['nav', 'menu', 'sidebar', 'order', 'audience'] },
    { page: 'features', label: 'Feature switches', where: 'General', keywords: ['tickets', 'modpacks', 'byon', 'proxy', 'api keys', 'automove', 'flags'] },
    { page: 'features', label: 'Tab proxy', where: 'General → Features', keywords: ['tab', 'proxy', 'public links', 'iframe'] },
    { page: 'maintenance', label: 'Maintenance mode', where: 'General', keywords: ['maintenance', 'banner', 'downtime', 'closed'] },
    { page: 'database', label: 'Database migration', where: 'General', keywords: ['database', 'postgres', 'migrate', 'hypertable', 'timescale'] },

    // Access
    { page: 'users', tab: 'signin', label: 'Allow self-registration', where: 'User settings → Registration & sign-in', keywords: ['register', 'signup', 'registration'] },
    { page: 'users', tab: 'signin', label: 'Require email verification', where: 'User settings → Registration & sign-in', keywords: ['verify', 'verification', 'confirm', 'email'] },
    { page: 'users', tab: 'signin', label: 'Two-factor enforcement', where: 'User settings → Registration & sign-in', keywords: ['2fa', 'mfa', 'totp', 'otp', 'authenticator'] },
    { page: 'users', tab: 'signin', label: 'Password minimum length', where: 'User settings → Registration & sign-in', keywords: ['password', 'length', 'strength'] },
    { page: 'users', tab: 'signin', label: 'Reset link lifetime', where: 'User settings → Registration & sign-in', keywords: ['reset', 'forgot', 'expiry', 'ttl', 'link'] },
    { page: 'users', tab: 'email', label: 'Outgoing email', where: 'User settings → Email', keywords: ['smtp', 'resend', 'mail', 'sender', 'from', 'relay', 'port 587', 'starttls'] },
    { page: 'users', tab: 'email', label: 'Send a test email', where: 'User settings → Email', keywords: ['test', 'smtp', 'mail', 'try'] },
    { page: 'users', tab: 'accounts', label: 'Allow username changes', where: 'User settings → Accounts', keywords: ['rename', 'username', 'cooldown'] },
    { page: 'users', tab: 'accounts', label: 'Public demo account', where: 'User settings → Accounts', keywords: ['demo', 'readonly', 'showcase'] },
    { page: 'users', tab: 'retention', label: 'Auto-delete inactive users', where: 'User settings → Retention', keywords: ['delete', 'inactive', 'dormant', 'anonymize', 'gdpr', 'dsgvo', 'cleanup'] },
    { page: 'users', tab: 'retention', label: 'Server audit retention', where: 'User settings → Retention', keywords: ['audit', 'log', 'retention', 'history'] },
    { page: 'users', tab: 'questions', label: 'Security questions pool', where: 'User settings → Security questions', keywords: ['security question', 'recovery', 'reset'] },
    { page: 'roles', label: 'Roles and permissions', where: 'Access', keywords: ['role', 'permission', 'capability', 'authz', 'access'] },

    // Infrastructure
    { page: 'regions', label: 'Regions', where: 'Infrastructure', keywords: ['region', 'location', 'datacenter'] },
    { page: 'nodes', tab: 'nodes', label: 'Nodes', where: 'Infrastructure → Nodes', keywords: ['node', 'machine', 'agent', 'cpu', 'pinning', 'cpuset'] },
    { page: 'nodes', tab: 'placement', label: 'Server placement', where: 'Infrastructure → Nodes', keywords: ['placement', 'scheduling', 'disk', 'most free', 'storage order'] },
    { page: 'nodes', tab: 'enrollment', label: 'Node admission and enroll tokens', where: 'Infrastructure → Nodes', keywords: ['enroll', 'admission', 'join', 'token', 'allowlist', 'cidr', 'pairing'] },
    { page: 'gateway', tab: 'gateway', label: 'Routing mode and file access', where: 'Infrastructure → Gateway', keywords: ['routing', 'gateway', 'ip port', 'beam', 'sftp'] },
    { page: 'gateway', tab: 'gateway', label: 'Hoster domains and route limits', where: 'Infrastructure → Gateway', keywords: ['domain', 'subdomain', 'wildcard', 'cname', 'custom domain', 'route'] },
    { page: 'gateway', tab: 'gateway', label: 'Automatic DNS & certificates', where: 'Infrastructure → Gateway', keywords: ['dns', 'zone', 'cloudflare', 'acme', 'certificate', 'tls', 'lets encrypt', 'token'] },
    { page: 'gateway', tab: 'gateway', label: 'DNS check', where: 'Infrastructure → Gateway', keywords: ['dns', 'check', 'records', 'resolve', 'verify'] },
    { page: 'gateway', tab: 'xdp', label: 'DDoS protection', where: 'Infrastructure → Gateway', keywords: ['xdp', 'ddos', 'filter', 'attack', 'drop'] },
    { page: 'warp', label: 'External node enrollment keys', where: 'Infrastructure → Warp', keywords: ['warp', 'wireguard', 'external', 'byon', 'overlay', 'tunnel', 'policy', 'max connections'] },
    { page: 'warp', label: 'Warp regions and leaders', where: 'Infrastructure → Warp', keywords: ['warp', 'leader', 'subnet', 'region', 'overlay'] },
    { page: 'beam', label: 'Beam relay address', where: 'Infrastructure → Beam', keywords: ['beam', 'relay', 'remote', 'desktop', 'override'] },
    { page: 'beam', label: 'Bandwidth throttle', where: 'Infrastructure → Beam', keywords: ['bandwidth', 'throttle', 'speed', 'limit', 'mbit', 'rate'] },
    { page: 'beam', label: 'Upload limits', where: 'Infrastructure → Beam', keywords: ['upload', 'quota', 'daily', 'size', 'gib', 'cap'] },
    { page: 'beam', label: 'Beam download link', where: 'Infrastructure → Beam', keywords: ['beam', 'download', 'installer', 'client'] },

    // Storage
    { page: 'core-storage', label: 'Core file storage', where: 'Storage', keywords: ['core storage', 's3', 'local', 'files', 'backend'] },
    { page: 'storage-connections', label: 'Storage connections', where: 'Storage', keywords: ['s3', 'r2', 'cloudflare', 'hetzner', 'bucket', 'endpoint', 'access key', 'minio', 'credentials'] },
    { page: 'storage-migration', label: 'Storage migration', where: 'Storage', keywords: ['migrate', 'move', 'copy', 'transfer', 'storage'] },
    { page: 'backups', label: 'Server backup storages and retention', where: 'Storage', keywords: ['backup', 'restore', 'retention', 'schedule', 's3', 'server backup'] },
    { page: 'backups', label: 'Backup allowance per user', where: 'Storage', keywords: ['backup', 'quota', 'allowance', 'limit', 'gb', 'storage per user'] },
    { page: 'platform-backups', label: 'Platform backup jobs', where: 'Storage', keywords: ['platform backup', 'bundle', 'database backup', 'disaster recovery', 'migrate instance', 'export platform'] },
    { page: 'platform-backups', label: 'Backup passphrase', where: 'Storage', keywords: ['passphrase', 'platform backup', 'encryption', 'restore', 'bundle password'] },

    // Servers & content
    { page: 'servers', label: 'Sub-server limits', where: 'Servers & Content', keywords: ['sub-server', 'limit', 'max servers'] },
    { page: 'modpacks', label: 'Modpack storage', where: 'Servers & Content → Modpacks', keywords: ['modpack', 'storage', 'paths', 's3', 'solder', 'archive'] },
    { page: 'modpacks', label: 'Solder delivery mode', where: 'Servers & Content → Modpacks', keywords: ['solder', 'delivery', 'presigned', 'mirror', 'public', 'technic'] },
    { page: 'modpacks', label: 'Mod metadata cache', where: 'Servers & Content → Modpacks', keywords: ['redis', 'cache', 'modrinth', 'valkey', 'memory', 'metadata', 'separate redis'] },
    { page: 'filemanager', label: 'File manager transfer limits', where: 'Servers & Content', keywords: ['file', 'upload', 'download', 'limit', 'size'] },

    // Support
    { page: 'ticket-categories', label: 'Ticket categories', where: 'Support', keywords: ['ticket', 'category', 'queue'] },
    { page: 'canned-responses', label: 'Canned responses', where: 'Support', keywords: ['canned', 'template', 'macro', 'reply'] },
    { page: 'tickets', label: 'Ticket settings', where: 'Support', keywords: ['ticket', 'auto close', 'sla', 'notification'] },
    { page: 'ticket-db', label: 'Ticket database', where: 'Support', keywords: ['ticket', 'database', 'external', 'migrate', 'backup'] },

    // BYON
    { page: 'usage', label: 'Traffic usage', where: 'BYON', keywords: ['traffic', 'usage', 'bandwidth', 'meter', 'tb'] },
    { page: 'traffic-limits', label: 'Traffic limits per region', where: 'BYON', keywords: ['traffic', 'limit', 'included', 'allowance', 'region', 'quota', 'gb', 'tb', 'cap', 'overage'] },
    { page: 'billing', label: 'Billing defaults', where: 'BYON', keywords: ['billing', 'stripe', 'subscription', 'suspend', 'grace'] },
];

export interface SettingsHit extends SettingsEntry {
    /** Lower is better. Only used for ordering. */
    score: number;
}

/**
 * searchSettings ranks the index against a query.
 *
 * Deliberately not fuzzy. A settings search that matches "backup" for "beam"
 * because two letters line up is worse than one that finds nothing: the operator
 * cannot tell a bad match from a missing setting, and stops trusting the box.
 * Substring matching over label plus keywords is enough when the keywords carry
 * the words people actually type.
 */
export function searchSettings(query: string, index: SettingsEntry[] = SETTINGS_INDEX): SettingsHit[] {
    const q = query.trim().toLowerCase();
    if (q.length < 2) return [];

    const hits: SettingsHit[] = [];
    for (const entry of index) {
        const label = entry.label.toLowerCase();
        let score: number | null = null;

        if (label === q) score = 0;
        else if (label.startsWith(q)) score = 1;
        else if (label.includes(q)) score = 2;
        else if ((entry.keywords ?? []).some(k => k === q)) score = 3;
        else if ((entry.keywords ?? []).some(k => k.startsWith(q))) score = 4;
        else if ((entry.keywords ?? []).some(k => k.includes(q))) score = 5;
        else if (entry.where.toLowerCase().includes(q)) score = 6;

        if (score !== null) hits.push({ ...entry, score });
    }

    return hits.sort((a, b) => a.score - b.score || a.label.localeCompare(b.label)).slice(0, 8);
}

/** The URL a hit navigates to. The tab is a query param the page reads on mount. */
export function hrefFor(entry: SettingsEntry): string {
    return entry.tab ? `/settings/${entry.page}?tab=${entry.tab}` : `/settings/${entry.page}`;
}
