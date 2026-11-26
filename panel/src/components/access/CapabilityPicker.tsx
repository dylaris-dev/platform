"use client";

import Checkbox from '@/components/ui/Checkbox';
import type { CatalogScope } from '@/lib/api/authzCatalog';
import { filterScopes, capIdsForScopes, withImpliedReads } from '@/lib/access/caps';

// Catalog-driven capability checkbox list. Renders every capability under the
// requested scopes, grouped by category. No hardcoded permission arrays - the
// catalog (from GET /api/authz/catalog) is the single source of truth.
//
// Checking a *.write capability also checks its *.read sibling (if the sibling
// exists in the catalog) as a presentation convenience; the two ids are still
// stored/enforced independently, so unchecking the write leaves the read checked.

interface CapabilityPickerProps {
    catalog: CatalogScope[];
    scopes: string[];
    selected: string[];
    onChange: (selected: string[]) => void;
    disabled?: boolean;
}

export default function CapabilityPicker({ catalog, scopes, selected, onChange, disabled }: CapabilityPickerProps) {
    const scopedCatalog = filterScopes(catalog, scopes);

    const handleToggle = (capId: string, checked: boolean) => {
        if (checked) {
            const next = [...selected, capId];
            const isWrite = capId.endsWith('.write');
            onChange(isWrite ? withImpliedReads(next, new Set(capIdsForScopes(catalog, scopes))) : next);
        } else {
            onChange(selected.filter(id => id !== capId));
        }
    };

    return (
        <div className="space-y-4">
            {scopedCatalog.map(scope => (
                <div key={scope.scope} className="space-y-4">
                    {scope.categories.map(cat => (
                        <div key={`${scope.scope}.${cat.category}`}>
                            <div className="srv-label mb-1.5">{cat.category}</div>
                            <div className="space-y-1.5">
                                {cat.capabilities.map(cap => {
                                    const checked = selected.includes(cap.id);
                                    return (
                                        <Checkbox
                                            key={cap.id}
                                            checked={checked}
                                            disabled={disabled}
                                            onChange={c => handleToggle(cap.id, c)}
                                            label={cap.label}
                                        />
                                    );
                                })}
                            </div>
                        </div>
                    ))}
                </div>
            ))}
        </div>
    );
}
