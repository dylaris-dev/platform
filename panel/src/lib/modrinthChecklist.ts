import type { ModrinthNag } from '@/lib/api/packs';

// Modrinth's publishing checklist (GET /v3/project/{id}/validate) names each
// item by a code; these are the sentences the panel shows for them. Codes from
// modrinth/code apps/labrinth/src/validate/project. An unknown code is still
// shown, readable, so a new Modrinth rule never disappears silently.
const TEXT: Record<string, string> = {
    upload_version: 'Upload at least one version (publish a build).',
    select_environment: 'Set the environment (client and server, or client only).',
    summary_too_short: 'Write a longer summary.',
    project_summary_matches_title: 'The summary must say more than the name.',
    project_summary_links: 'Remove links from the summary.',
    add_description: 'Write a description.',
    description_too_short: 'Write a longer description.',
    project_description_matches_summary: 'The description must not repeat the summary.',
    long_headers: 'Shorten the headings in the description.',
    description_ends_with_header: 'End the description with text, not a heading.',
    adjacent_headers: 'Put text between headings in the description.',
    project_name_version: 'Remove the version number from the name.',
    minecraft_title_clause: 'A short name must not contain "Minecraft".',
    select_license: 'Choose a license.',
    add_custom_license_details: 'Give the custom license a name and a link.',
    review_permissions: 'Confirm permissions for files that are not from Modrinth.',
    all_tags_selected: 'Select only the tags that fit, not all of them.',
    add_icon: 'Add an icon.',
    feature_gallery_image: 'Add a featured gallery image (on modrinth.com).',
    select_tags: 'Select tags that describe the pack.',
    add_links: 'Add links, for example a source, issue tracker or Discord.',
    check_disclosures: 'Check the content disclosures (on modrinth.com), for example AI-generated content.',
    too_many_tags: 'Use at most 8 tags.',
    missing_alt_text: 'Add alt text to gallery images (on modrinth.com).',
    moderator_feedback: 'Read the moderator feedback below.',
};

export function nagText(nag: ModrinthNag): string {
    const base = TEXT[nag.kind] || nag.kind.replace(/_/g, ' ').replace(/^./, c => c.toUpperCase()) + '.';
    const d = nag.details || {};
    if (typeof d.length === 'number' && typeof d.min_chars === 'number') return `${base} It has ${d.length} of at least ${d.min_chars} characters.`;
    return base;
}

/** Required items block "Submit for review"; Modrinth refuses it as well. */
export function blockingNags(nags: ModrinthNag[]): ModrinthNag[] {
    return nags.filter(nag => nag.severity === 'required');
}

export const MODRINTH_STATUS_LABEL: Record<string, string> = {
    draft: 'Draft - only you and your team see it',
    processing: 'In review by Modrinth moderators',
    approved: 'Public',
    unlisted: 'Unlisted - reachable by link only',
    private: 'Private',
    rejected: 'Rejected - see the moderator message',
    withheld: 'Withheld - see the moderator message',
    archived: 'Archived',
};

/** Submitting makes sense from a draft, or again after a rejection. */
export const canSubmitFrom = (status: string) => status === 'draft' || status === 'rejected' || status === 'withheld';
