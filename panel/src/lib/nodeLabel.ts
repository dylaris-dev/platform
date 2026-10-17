import { nodeKind, type NodeKind, type NodeKindFields } from './nodeKind';

// How a node is labelled in the UI.
//
// `token` is NOT a label. Since the node-pairing hardening, Core mints an
// unguessable UUID as the node identity and keeps the node-reported hostname in
// a separate display_name column. Older rows had token == hostname, so the
// panel used to render the token and looked right - until the first node
// enrolled under the new scheme showed up as a bare UUID.
//
// Precedence: the admin-editable display name, then the row name, then the id.
// The token never appears here; where an operator needs it (deploy snippets,
// --token) it is shown explicitly as the identity, not as a name.
export function nodeLabel(node: { displayName?: string; name?: string; id?: number | string }): string {
    const display = node.displayName?.trim();
    if (display) return display;
    const name = node.name?.trim();
    if (name) return name;
    return node.id !== undefined ? `Node ${node.id}` : 'Node';
}

const KIND_WORD: Record<NodeKind, string> = { platform: 'Platform', external: 'External', byon: 'BYON' };

// A node in a move/transfer picker: the label plus what kind of machine it is
// and where, because two nodes can share a display name and "which one" is the
// whole question when picking a move target.
export function nodePickerLabel(node: Parameters<typeof nodeLabel>[0] & NodeKindFields & { region?: string }): string {
    const where = [KIND_WORD[nodeKind(node)], node.region?.trim()].filter(Boolean).join(', ');
    return `${nodeLabel(node)} (${where})`;
}

// The label for a node id, from a list that may not hold it (a node the
// caller may not place on still has to be named as the move source).
export function nodeLabelById(nodes: Parameters<typeof nodePickerLabel>[0][], id: number | undefined): string {
    if (!id) return 'an unknown node';
    const n = nodes.find(x => x.id === id);
    return n ? nodePickerLabel(n) : `Node ${id}`;
}
