// Display labels + flag emoji for known region keys.
// Unknown keys fall back to a Title-Cased version of the raw key so admins
// can use whatever taxonomy they want without code changes.

interface RegionMeta {
    label: string;
    flag: string;
}

const REGION_META: Record<string, RegionMeta> = {
    'eu-central': { label: 'Europe Central', flag: '🇩🇪' },
    'eu-west':    { label: 'Europe West',    flag: '🇫🇷' },
    'eu-north':   { label: 'Europe North',   flag: '🇸🇪' },
    'eu-south':   { label: 'Europe South',   flag: '🇮🇹' },
    'us-east':    { label: 'US East',        flag: '🇺🇸' },
    'us-west':    { label: 'US West',        flag: '🇺🇸' },
    'us-central': { label: 'US Central',     flag: '🇺🇸' },
    'ca-central': { label: 'Canada Central', flag: '🇨🇦' },
    'sa-east':    { label: 'South America',  flag: '🇧🇷' },
    'ap-southeast': { label: 'Asia SE',      flag: '🇸🇬' },
    'ap-northeast': { label: 'Asia NE',      flag: '🇯🇵' },
    'ap-south':   { label: 'Asia South',     flag: '🇮🇳' },
    'oce-east':   { label: 'Oceania East',   flag: '🇦🇺' },
    'af-south':   { label: 'Africa South',   flag: '🇿🇦' },
    'me-central': { label: 'Middle East',    flag: '🇦🇪' },
};

export function regionLabel(key: string): string {
    const meta = REGION_META[key];
    if (meta) return meta.label;
    // Fallback: "eu-funky" → "Eu Funky"
    return key
        .split(/[-_]/)
        .map(s => s.charAt(0).toUpperCase() + s.slice(1))
        .join(' ');
}

export function regionFlag(key: string): string {
    return REGION_META[key]?.flag ?? '🌐';
}

/**
 * regionLabelFrom prefers the name an operator gave the region in Settings ->
 * Regions over the built-in map.
 *
 * The map above is a starting point for keys nobody has renamed, not an
 * authority: a region renamed in the panel used to keep showing its old label
 * everywhere it was rendered, because the label came from code and the name
 * lived in the database.
 *
 * The region list the panel caches holds only ENABLED regions, so a node parked
 * in a disabled one falls back to the map. That is the right way round - the
 * fallback is always a readable name, never a blank.
 */
export function regionLabelFrom(key: string, regions: { id: string; displayName?: string }[]): string {
    const named = regions.find(r => r.id === key)?.displayName?.trim();
    return named || regionLabel(key);
}
