"use client";

import React, { useEffect, useRef, useState } from 'react';
import { Search, RefreshCw, AlertTriangle, Plus, ExternalLink, ChevronDown, Download, Trash2, Check } from 'lucide-react';
import { searchModrinth, getModrinthVersionsOrThrow, getModrinthCategories, type ModrinthSearchHit, type ModrinthVersion, type ModrinthCategory } from '@/lib/api/modrinth';
import { envLabel, envParam, matchesEnv, type EnvFilter } from '@/lib/modSide';
import { pickPreferredVersion } from '@/lib/modVersionCompare';
import { categoryLabel } from '@/lib/modrinthCategories';
import { EnvironmentFilter, EnvTag } from '@/components/mods/EnvironmentFilter';
import { ModrinthProjectActions } from '@/components/mods/ModrinthLibrary';
import ContentCategories from '@/components/mods/ContentCategories';
import ProjectIcon from '@/components/modpacks/ProjectIcon';
import ModProjectDetails from '@/components/mods/ModProjectDetails';
import { toast } from '@/components/ui/Toast';

interface ModrinthVersionBrowserProps {
    loader?: string;
    mcVersion?: string;
    projectType?: string;
    onPick: (projectId: string, versionId: string, hit: ModrinthSearchHit, version: ModrinthVersion) => void | Promise<void>;
    installedProjectIds?: ReadonlySet<string> | string[];
    disabled?: boolean;
    disabledTitle?: string;
    withFilters?: boolean;
    onRemove?: (projectId: string) => Promise<void>;
    removeDisabled?: boolean;
}

export default function ModrinthVersionBrowser({ loader, mcVersion, projectType='mod', onPick, installedProjectIds, disabled, disabledTitle, withFilters=false, onRemove, removeDisabled }: ModrinthVersionBrowserProps) {
    const installedSet=React.useMemo<ReadonlySet<string>>(()=>new Set(installedProjectIds || []),[installedProjectIds]);
    const [query,setQuery]=useState('');
    const [env,setEnv]=useState<EnvFilter>({client:false,server:false});
    const [category,setCategory]=useState<string|null>(null);
    const [categories,setCategories]=useState<ModrinthCategory[]>([]);
    const [categoriesLoading,setCategoriesLoading]=useState(true);
    const [sort,setSort]=useState<'relevance'|'downloads'|'follows'|'newest'|'updated'>('relevance');
    const [hits,setHits]=useState<ModrinthSearchHit[]>([]);
    const [searching,setSearching]=useState(true);
    const [searchError,setSearchError]=useState(false);
    const [attempt,setAttempt]=useState(0);
    const [total,setTotal]=useState(0);
    const [offset,setOffset]=useState(0);
    const [moreLoading,setMoreLoading]=useState(false);
    const [moreError,setMoreError]=useState(false);
    const [pending,setPending]=useState<string|null>(null);
    const [expanded,setExpanded]=useState<string|null>(null);
    const [versions,setVersions]=useState<ModrinthVersion[]>([]);
    const [versionsLoading,setVersionsLoading]=useState(false);
    const [versionsError,setVersionsError]=useState('');
    const [versionAttempt,setVersionAttempt]=useState(0);
    const [visibleVersions,setVisibleVersions]=useState(6);
    const generation=useRef(0);
    const mounted=useRef(true);
    const isMod=projectType==='mod' || projectType==='plugin';
    const missingCompatibility=withFilters&&(!mcVersion||(isMod&&(!loader||loader==='vanilla')));
    const side=isMod?envParam(env):undefined;
    useEffect(()=>{mounted.current=true;return()=>{mounted.current=false;};},[]);
    useEffect(()=>{let live=true;getModrinthCategories().then(result=>{if(live){setCategories(result);setCategoriesLoading(false);}});return()=>{live=false;};},[]);
    useEffect(()=>setCategory(null),[projectType]);
    const params=(nextOffset:number)=>({query:query.trim()||undefined,loaders:isMod&&loader&&loader!=='vanilla'?[loader]:undefined,versions:mcVersion?[mcVersion]:undefined,projectType:projectType as Parameters<typeof searchModrinth>[0]['projectType'],categories:category?[category]:undefined,side,index:sort,limit:20,offset:nextOffset});
    const filtered=(result:ModrinthSearchHit[])=>isMod?result.filter(hit=>matchesEnv(env,hit)):result;
    useEffect(()=>{
        const request=++generation.current;
        setSearching(true);setSearchError(false);setMoreLoading(false);setMoreError(false);setExpanded(null);setVersions([]);
        const timer=setTimeout(async()=>{
            const result=await searchModrinth(params(0));
            if(request!==generation.current || !mounted.current)return;
            if(result===null){setSearchError(true);setHits([]);setTotal(0);setOffset(0);}
            else {setHits(filtered(result.hits));setTotal(result.total_hits);setOffset(result.offset+result.hits.length);}
            setSearching(false);
        },350);
        return()=>{clearTimeout(timer);generation.current++;};
    // Values, rather than callback identities, define a search. Installing a row keeps its scroll position.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    },[query,loader,mcVersion,projectType,side,category,sort,attempt]);
    const loadMore=async()=>{
        const request=generation.current;setMoreLoading(true);setMoreError(false);
        const result=await searchModrinth(params(offset));
        if(request!==generation.current || !mounted.current)return;
        if(!result)setMoreError(true);
        else {
            setHits(previous=>[...previous,...filtered(result.hits).filter(hit=>!previous.some(old=>old.project_id===hit.project_id))]);
            setOffset(result.offset+result.hits.length);setTotal(result.total_hits);
            if(result.hits.length===0)setOffset(result.total_hits);
        }
        setMoreLoading(false);
    };
    useEffect(()=>{
        let live=true;setVersions([]);setVersionsError('');setVisibleVersions(6);
        if(!expanded){setVersionsLoading(false);return()=>{live=false;};}
        setVersionsLoading(true);
        getModrinthVersionsOrThrow(expanded,{loaders:isMod&&loader&&loader!=='vanilla'?[loader]:undefined,versions:mcVersion?[mcVersion]:undefined})
            .then(result=>{if(live)setVersions(result);})
            .catch(error=>{if(live)setVersionsError(error instanceof Error?error.message:'Could not load versions.');})
            .finally(()=>{if(live)setVersionsLoading(false);});
        return()=>{live=false;};
    },[expanded,loader,mcVersion,isMod,versionAttempt]);
    const act=async(hit:ModrinthSearchHit,version?:ModrinthVersion,remove=false)=>{
        if(pending || (remove?removeDisabled:disabled))return;
        if(!remove&&missingCompatibility){toast('Set Minecraft and a compatible loader before installing content.',false);return;}
        setPending(hit.project_id);
        try {
            if(remove){await onRemove?.(hit.project_id);return;}
            const candidates=version?[version]:await getModrinthVersionsOrThrow(hit.project_id,{loaders:isMod&&loader&&loader!=='vanilla'?[loader]:undefined,versions:mcVersion?[mcVersion]:undefined});
            if(!mounted.current)return;
            const chosen=pickPreferredVersion(candidates,{loaders:isMod&&loader?[loader]:[],mcVersions:mcVersion?[mcVersion]:[]});
            if(!chosen)throw new Error(`No compatible version of ${hit.title} matches this build.`);
            await onPick(hit.project_id,chosen.id,hit,chosen);
        } catch(error){toast(error instanceof Error?error.message:'Content action failed.',false);}
        finally {if(mounted.current)setPending(null);}
    };
    return <div className="flex flex-1 min-h-0 min-w-0 gap-3 overflow-hidden" aria-label="Modrinth content browser">
        {withFilters && <aside className="w-52 shrink-0 min-h-0 overflow-y-auto card p-3 space-y-4" aria-label="Browse filters">
            <div className="text-xs text-(--base-06) space-y-1"><strong className="text-(--base-09)">Build compatibility</strong><p>MC {mcVersion || 'Unspecified'} · {loader || 'Vanilla'}</p><p>Install prefers the newest compatible release, then beta or alpha.</p>{missingCompatibility&&<p className="text-(--warning-light)">Set Minecraft and a compatible loader before installing.</p>}</div>
            {isMod && <EnvironmentFilter value={env} onChange={setEnv}/>}
            <ContentCategories categories={categories} projectType={projectType as 'mod'|'plugin'|'resourcepack'|'shader'} value={category} onChange={setCategory} loading={categoriesLoading}/>
        </aside>}
        <div className="flex flex-col flex-1 min-w-0 min-h-0 overflow-hidden gap-3">
            <div className="flex items-center gap-2 shrink-0"><div className="relative flex-1 min-w-0"><Search size={13} className="absolute left-3 top-1/2 -translate-y-1/2 text-(--base-06)"/><input aria-label="Search Modrinth content" className="input-field input-icon-left w-full" value={query} onChange={event=>setQuery(event.target.value)} placeholder={`Search ${projectType==='mod'?'mods':projectType==='resourcepack'?'resource packs':'shaders'}...`}/></div><select aria-label="Sort Modrinth results" className="input-field w-auto text-xs" value={sort} onChange={event=>setSort(event.target.value as typeof sort)}><option value="relevance">Relevance</option><option value="downloads">Downloads</option><option value="follows">Followers</option><option value="newest">Newest projects</option><option value="updated">Recently updated</option></select></div>
            {isMod && !withFilters && <EnvironmentFilter value={env} onChange={setEnv} inline/>}
            <section className="flex-1 min-h-0 overflow-y-auto space-y-2.5 pb-4" aria-label="Modrinth results">
                {searching?<p role="status" className="text-sm text-(--base-06) py-6 text-center"><RefreshCw size={13} className="inline animate-spin"/> Searching...</p>:searchError?<div role="alert" className="card p-4 text-sm">Search failed. <button className="btn btn-secondary btn-sm" onClick={()=>setAttempt(value=>value+1)}>Retry search</button></div>:!hits.length?<p className="text-sm text-(--base-06) py-6 text-center">No matches for these filters.</p>:hits.map(hit=>{
                    const installed=installedSet.has(hit.project_id);const busy=pending===hit.project_id;
                    return <article key={hit.project_id} className="card p-3" aria-label={hit.title}>
                        <div className="flex items-start gap-3">
                            <button onClick={()=>setExpanded(value=>value===hit.slug?null:hit.slug)} aria-expanded={expanded===hit.slug} className="flex items-start gap-3 flex-1 min-w-0 text-left"><ProjectIcon url={hit.icon_url} name={hit.title} className="w-11 h-11"/><div className="flex-1 min-w-0"><div className="font-medium text-sm truncate" title={hit.title}>{hit.title}</div><p className="text-xs text-(--base-06) mt-1 line-clamp-2">{hit.description}</p><div className="text-[10px] text-(--base-06) mt-1">by {hit.author} · {hit.downloads.toLocaleString()} downloads</div><div className="flex gap-1 flex-wrap mt-2">{isMod&&envLabel(hit)&&<EnvTag label={envLabel(hit)!}/>}<span className="text-[10px] text-(--base-06)">{(hit.categories||[]).slice(0,5).map(categoryLabel).join(' · ')}</span></div></div><ChevronDown size={14} className={`shrink-0 mt-1 ${expanded===hit.slug?'rotate-180':''}`}/></button>
                            <div className="flex flex-col items-end gap-2 shrink-0">
                                {installed && <span className="mono-label text-(--success-light) flex items-center gap-1"><Check size={12}/>Installed</span>}
                                {withFilters && (installed?<button className="btn btn-secondary btn-sm" disabled={removeDisabled||!!pending||!onRemove} onClick={()=>act(hit,undefined,true)} aria-label={`Remove ${hit.title}`}>{busy?<RefreshCw size={12} className="animate-spin"/>:<Trash2 size={12}/>}Remove</button>:<button className="btn btn-primary btn-sm" title={disabled?disabledTitle:'Install the newest compatible release, with beta/alpha fallback'} disabled={disabled||!!pending||missingCompatibility} onClick={()=>act(hit)} aria-label={`Install ${hit.title}`}>{busy?<RefreshCw size={12} className="animate-spin"/>:<Download size={12}/>}Install</button>)}
                                <div className="flex items-center"><ModrinthProjectActions projectId={hit.project_id} title={hit.title} loader={loader}/><a href={`https://modrinth.com/project/${encodeURIComponent(hit.project_id)}`} target="_blank" rel="noopener noreferrer" aria-label={`Open ${hit.title} on Modrinth`} className="btn btn-ghost btn-sm"><ExternalLink size={13}/></a></div>
                            </div>
                        </div>
                        {expanded===hit.slug && <div className="mt-4 pt-4 border-t border-(--base-03) space-y-3"><ModProjectDetails projectId={hit.project_id}/><h3 className="input-label">Compatible versions</h3>{versionsLoading?<p role="status" className="text-xs">Loading versions...</p>:versionsError?<div role="alert" className="text-xs text-(--error-light)">{versionsError}<button className="btn btn-secondary btn-sm" onClick={()=>setVersionAttempt(value=>value+1)}>Retry versions</button></div>:!versions.length?<p className="text-xs text-(--base-06)"><AlertTriangle size={12} className="inline"/> No compatible versions.</p>:versions.slice(0,visibleVersions).map(version=><button key={version.id} className="btn btn-secondary btn-sm w-full justify-start text-left" disabled={disabled||!!pending||missingCompatibility||(withFilters&&installed)} onClick={()=>act(hit,version)} title={disabled?disabledTitle:undefined}><Plus size={12}/><span className="min-w-0 truncate">{version.version_number} · {version.version_type} · MC {version.game_versions.join(', ')}</span></button>)}{!versionsLoading&&!versionsError&&versions.length>visibleVersions&&<button className="btn btn-secondary btn-sm" onClick={()=>setVisibleVersions(value=>value+12)}>Show more versions ({versions.length-visibleVersions} remaining)</button>}</div>}
                    </article>;
                })}
                {!searching&&!searchError&&<footer className="text-center text-xs text-(--base-06) space-y-3 py-3"><p>{hits.length} projects shown · {total.toLocaleString()} matching projects</p>{offset<total&&<button className="btn btn-secondary btn-sm" disabled={moreLoading} onClick={loadMore}>{moreLoading?'Loading...':moreError?'Retry more results':'Load more results'}</button>}{moreError&&<p role="alert">Could not load more projects. Existing results are retained.</p>}</footer>}
            </section>
        </div>
    </div>;
}
