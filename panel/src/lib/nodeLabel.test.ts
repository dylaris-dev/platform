import { describe, it, expect } from 'vitest';
import { nodeLabel, nodePickerLabel, nodeLabelById } from './nodeLabel';

describe('nodeLabel', () => {
    it('prefers the admin-editable display name', () => {
        expect(nodeLabel({ displayName: 'eu-node-00', name: 'uuid-here' })).toBe('eu-node-00');
    });

    it('falls back to the row name when no display name is set', () => {
        expect(nodeLabel({ name: 'legacy-hostname' })).toBe('legacy-hostname');
    });

    it('treats a whitespace-only display name as unset', () => {
        expect(nodeLabel({ displayName: '   ', name: 'legacy-hostname' })).toBe('legacy-hostname');
    });

    it('falls back to the id when neither is set', () => {
        expect(nodeLabel({ id: 7 })).toBe('Node 7');
    });

    // The regression this file exists for: the Infrastructure view rendered
    // `node.token || node.name`, which was the hostname only while token still
    // WAS the hostname. Under the pairing hardening the token is a Core-minted
    // UUID, and every node showed up as one.
    it('never uses the token as a label', () => {
        const node = { displayName: 'eu-node-00', name: 'x', token: '183940c8-f75c-4511-bb2f-b416539bfbc9' };
        expect(nodeLabel(node)).not.toContain('183940c8');
    });
});

describe('nodePickerLabel', () => {
    // The misclick this exists for: the move picker showed the node's UUID row
    // name, so an operator could not tell which machine they were sending a
    // customer's server to.
    it('names the node, its kind and region instead of the row name', () => {
        expect(nodePickerLabel({ id: 2, name: '183940c8-uuid', displayName: 'eu-node-00', tags: 'external', region: 'eu-central' }))
            .toBe('eu-node-00 (External, eu-central)');
    });

    it('calls an owned node BYON even when it carries the external tag', () => {
        expect(nodePickerLabel({ name: 'ms01', ownerId: 'u1', tags: 'external' })).toBe('ms01 (BYON)');
    });

    it('omits a missing region', () => {
        expect(nodePickerLabel({ name: 'eu-node-01' })).toBe('eu-node-01 (Platform)');
    });
});

describe('nodeLabelById', () => {
    const nodes = [{ id: 329, name: 'ms01', tags: 'external' }];

    it('labels a node in the list', () => {
        expect(nodeLabelById(nodes, 329)).toBe('ms01 (External)');
    });

    it('falls back to the id for a node outside the list', () => {
        expect(nodeLabelById(nodes, 5)).toBe('Node 5');
    });

    it('does not invent a node for a missing id', () => {
        expect(nodeLabelById(nodes, undefined)).toBe('an unknown node');
    });
});
