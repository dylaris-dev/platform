'use client';

import type { CSSProperties } from 'react';
import type { ModrinthCategory } from '@/lib/api/modrinth';
import { categoryLabel, visibleCategoriesFor } from '@/lib/modrinthCategories';

// Rendering upstream SVG as an image mask keeps it out of the panel DOM.
export function categoryIconMask(svg: string): CSSProperties {
    if (!/xmlns\s*=/.test(svg)) svg = svg.replace(/<svg\b/, '<svg xmlns="http://www.w3.org/2000/svg"');
    const url = `url("data:image/svg+xml;utf8,${encodeURIComponent(svg)}")`;
    return { maskImage:url, WebkitMaskImage:url, maskSize:'contain', WebkitMaskSize:'contain', maskRepeat:'no-repeat', WebkitMaskRepeat:'no-repeat', maskPosition:'center', WebkitMaskPosition:'center' };
}

export default function ContentCategories({ categories, projectType, value, onChange, loading=false }: {
    categories: ModrinthCategory[];
    projectType: Parameters<typeof visibleCategoriesFor>[1] | Parameters<typeof visibleCategoriesFor>[1][];
    value: string | null;
    onChange: (category: string | null) => void;
    loading?: boolean;
}) {
    const types=Array.isArray(projectType)?projectType:[projectType];
    const seen=new Set<string>();
    const options=types.flatMap(type=>visibleCategoriesFor(categories,type)).filter(category=>!seen.has(category.name)&&!!seen.add(category.name));
    return <section aria-label="Content categories">
        <h3 className="input-label">Categories</h3>
        <div className="mt-2 flex flex-col gap-1.5">
            <button className={`btn btn-sm justify-start ${!value?'btn-primary':'btn-secondary'}`} aria-pressed={!value} onClick={()=>onChange(null)}>All categories</button>
            {loading && <p role="status" className="text-xs text-(--base-06)">Loading categories...</p>}
            {!loading && !options.length && <p className="text-xs text-(--base-06)">No categories for this content type.</p>}
            {options.map(category=><button key={category.name} type="button" aria-pressed={value===category.name} onClick={()=>onChange(value===category.name?null:category.name)} className={`w-full flex items-center gap-2 px-2.5 py-1.5 rounded-md border text-[13px] capitalize transition-colors ${value===category.name?'bg-(--accent-ghost) border-(--accent-border) text-(--accent-light)':'bg-(--base-02) border-(--base-03) text-(--base-07) hover:bg-(--base-03) hover:border-(--base-04)'}`}><span className="shrink-0 w-4 h-4 bg-current" aria-hidden="true" style={categoryIconMask(category.icon)}/><span className="truncate">{categoryLabel(category.name)}</span></button>)}
        </div>
    </section>;
}
