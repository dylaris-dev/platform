// What a pack history row says, in words. Unknown actions fall back to the
// raw action name so a new one never renders as nothing.
const ACTIONS: Record<string, string> = {
    'pack.created': 'created the pack',
    'pack.updated': 'changed the pack settings',
    'pack.deleted': 'deleted the pack',
    'build.created': 'created a build',
    'build.updated': 'changed a build',
    'build.deleted': 'deleted a build',
    'build.published': 'published a build',
    'build.migrated': 'migrated a build to another version',
    'content.added': 'added content',
    'content.uploaded': 'uploaded a file',
    'content.removed': 'removed content',
    'content.side_changed': 'changed where a mod runs',
    'content.text_edited': 'edited a file',
    'content.replaced': 'replaced a file with its Modrinth version',
    'content.updated': 'updated mods',
    'share_link.created': 'created a share link',
    'share_link.revoked': 'revoked a share link',
    'collaborator.invited': 'invited a collaborator',
    'collaborator.changed': "changed a collaborator's rights",
    'collaborator.removed': 'removed a collaborator',
};

export function packAuditText(action: string, detail: Record<string, unknown> = {}): string {
    const base = ACTIONS[action] ?? action;
    const user = typeof detail.user === 'string' ? detail.user : '';
    if (action === 'collaborator.removed' && detail.left === true) return `left the packs${user ? ` (${user})` : ''}`;
    if (action.startsWith('collaborator.') && user) return `${base}: ${user}`;
    return base;
}
