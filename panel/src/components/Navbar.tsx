"use client";

import React, { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { usePathname } from 'next/navigation';
import { ChevronDown, Star } from 'lucide-react';
import { AppModule } from '../lib/api';
import { DynamicIcon } from '../lib/icons';
import { useAppData } from '@/lib/AppDataContext';
import { useLayout } from '@/lib/useBreakpoint';
import GuardedLink from '@/components/GuardedLink';
import HelpTourButton from '@/components/help/HelpTourButton';

export function withPersonalLibrary(modules: AppModule[]): AppModule[] {
  const entries: AppModule[] = [
    { id: -101, name: 'Favorites', type: 'internal', icon: 'star', url: '/favorites', isEnabled: true, isSystem: true, position: 98, accessRole: 'all' },
    { id: -102, name: 'Help', type: 'internal', icon: 'circle-help', url: '/help', isEnabled: true, isSystem: true, position: 99, accessRole: 'all' },
  ];
  return [...modules, ...entries.filter(entry => !modules.some(module => moduleHref(module) === entry.url))];
}

interface NavbarProps {
  children?: React.ReactNode;
  /** Rendered in the branding slot; collapses with the sidebar. */
  brand?: React.ReactNode;
}

// Map a DB-loaded module to its URL route.
//
// Every built-in module row is seeded WITH its route in `url` (see
// seedSystemModules), so that column is the mapping - not a name switch that
// has to be extended for each new module. It wasn't, which is why Tickets and
// Modpacks both opened /modules/<id> and rendered a "coming soon" placeholder
// while their real pages sat unreachable at /tickets and /modpacks.
//
// Only INTERNAL modules route by url; an iframe module's url is a foreign
// origin and belongs in the iframe on /modules/<id>, not in the address bar.
// The path check rejects "//host" (protocol-relative, i.e. off-site) so an
// admin-editable column can never turn a nav entry into an off-site redirect.
// Gateway was retired as a standalone module — its content moved into the
// Infrastructure module's Routes tab and any legacy DB row is filtered out
// server-side.
export function moduleHref(module: AppModule): string {
  const url = module.url?.trim();
  if (module.type === 'internal' && url && url.startsWith('/') && !url.startsWith('//')) {
    return url;
  }
  return `/modules/${module.id}`;
}

/**
 * The one entry that is never collapsed.
 *
 * Servers is where every session starts and where a stuck operator goes back
 * to, so it stays reachable in one click at every width. Everything else may
 * fold into the menu.
 */
export const PINNED_HREF = '/servers';

/**
 * How many of `widths` fit into `available`, given the room the overflow
 * trigger needs.
 *
 * Pure so the ratchet this replaced is coverable by a test. The old version
 * measured only the items it was CURRENTLY rendering, so once an entry had been
 * hidden its width was gone from the calculation - and widening the window could
 * never bring it back. It was a one-way door: every resize could take entries
 * away and none could return them.
 */
export function countThatFit(widths: number[], available: number, triggerWidth: number): number {
  if (available <= 0) return widths.length;
  let used = 0;
  for (let i = 0; i < widths.length; i++) {
    used += widths[i];
    // The trigger only has to fit while something is still left to collect.
    const needsTrigger = i < widths.length - 1;
    if (used > available - (needsTrigger ? triggerWidth : 0)) return i;
  }
  return widths.length;
}

/**
 * How many collapsible entries to show, from the width of the WHOLE row.
 *
 * rowWidth must be the navigation row, not the clipped strip inside it: the
 * pinned entry and the "More" trigger are SIBLINGS of that strip. Measuring the
 * strip made this a latch - the trigger is drawn only once something has
 * collapsed, so the box shrank BECAUSE it collapsed, and every later
 * measurement then agreed with the collapse. Only a reload, which starts with
 * everything visible and no trigger, could undo it.
 *
 * itemWidths includes the pinned entry first when hasPinned, matching the
 * measuring bench, and it is subtracted exactly once here.
 */
export function visibleCountFor(
  rowWidth: number,
  itemWidths: number[],
  hasPinned: boolean,
  triggerWidth: number,
): number {
  const widths = [...itemWidths];
  const pinnedWidth = hasPinned ? widths.shift() ?? 0 : 0;
  return countThatFit(widths, rowWidth - pinnedWidth, triggerWidth);
}

export default function Navbar({ children, brand }: NavbarProps) {
  const { modules, featureFlags } = useAppData();
  const pathname = usePathname();
  const { layout } = useLayout();

  const sortedModules = useMemo(() => withPersonalLibrary(modules)
    // Platform-wide tickets toggle hides the Tickets module entry-point.
    // The module row itself stays in the DB so flipping the toggle back on
    // restores the nav without re-seeding.
    .filter(m => featureFlags.tickets || m.name !== 'Tickets')
    .filter(m => m.isEnabled)
    .sort((a, b) => (a.position || 99) - (b.position || 99)), [modules, featureFlags.tickets]);

  const pinned = sortedModules.find(m => moduleHref(m) === PINNED_HREF);
  const rest = useMemo(
    () => sortedModules.filter(m => m !== pinned),
    [sortedModules, pinned],
  );

  // Labels are dropped before anything is hidden: an icon-only row fits roughly
  // three times as many entries, so at most widths nothing has to overflow at
  // all. Only when even that is not enough does the menu collect.
  const iconOnly = layout !== 'wide';

  // On the ROW, not on the clipped strip inside it - see visibleCountFor.
  const rowRef = useRef<HTMLDivElement>(null);
  const measureRef = useRef<HTMLDivElement>(null);
  const [visibleCount, setVisibleCount] = useState(rest.length);
  const [menuOpen, setMenuOpen] = useState(false);

  // How many fit. Measured rather than derived from a breakpoint, because the
  // count is admin-controlled: an install with four modules and one with
  // fourteen need different answers at the same width.
  //
  // Measured off a HIDDEN row that always holds every entry, which is the whole
  // repair. Measuring the rendered row could only ever see what was still
  // rendered, so the count could shrink and never grow again.
  useLayoutEffect(() => {
    const row = rowRef.current;
    const bench = measureRef.current;
    if (!row || !bench) return;

    const measure = () => {
      const items = Array.from(bench.querySelectorAll<HTMLElement>('[data-measure-item]'));
      if (items.length === 0) return;
      const gap = 4;
      const widths = items.map(i => i.offsetWidth + gap);
      const trigger = bench.querySelector<HTMLElement>('[data-measure-trigger]');
      const fits = visibleCountFor(
        row.clientWidth, widths, !!pinned, (trigger?.offsetWidth ?? 40) + gap);
      setVisibleCount(prev => (prev === fits ? prev : fits));
    };

    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(row);
    ro.observe(bench);
    return () => ro.disconnect();
  }, [rest.length, iconOnly, pinned]);

  // Close on navigation. GuardedLink owns its own click (it may pop the
  // unsaved-changes dialog instead of navigating), so the menu cannot close
  // itself from an onClick without defeating that guard.
  useEffect(() => { setMenuOpen(false); }, [pathname]);

  useEffect(() => {
    if (!menuOpen) return;
    const close = (e: MouseEvent) => {
      if (!(e.target as HTMLElement).closest('.nav-overflow')) setMenuOpen(false);
    };
    const esc = (e: KeyboardEvent) => { if (e.key === 'Escape') setMenuOpen(false); };
    document.addEventListener('mousedown', close);
    document.addEventListener('keydown', esc);
    return () => {
      document.removeEventListener('mousedown', close);
      document.removeEventListener('keydown', esc);
    };
  }, [menuOpen]);

  const visible = rest.slice(0, visibleCount);
  const overflow = rest.slice(visibleCount);

  const itemClass = (isActive: boolean) =>
    `btn text-sm ${iconOnly ? 'px-2 py-1.5' : 'px-3.5 py-1.5'} ${
      isActive
        ? 'bg-(--accent-ghost) text-(--accent-light) border border-(--accent-border)'
        : 'bg-transparent text-(--base-07) border border-transparent hover:bg-(--base-04)/50 hover:text-(--base-09)'
    }`;

  const isActiveHref = (href: string) => pathname === href || pathname.startsWith(href + '/');

  const itemBody = (module: AppModule, isActive: boolean) => (
    <>
      <DynamicIcon name={module.icon || 'grid-2x2'} size={18} className={`transition-colors ${isActive ? 'text-(--accent-light)' : 'text-(--base-06) group-hover:text-(--base-08)'}`} />
      {!iconOnly && <span className="tracking-wide">{module.name}</span>}
    </>
  );

  const renderItem = (module: AppModule) => {
    const href = moduleHref(module);
    const isActive = isActiveHref(href);
    return (
      <GuardedLink
        key={module.id}
        href={href}
        title={iconOnly ? module.name : undefined}
        className={itemClass(isActive)}
      >
        {itemBody(module, isActive)}
      </GuardedLink>
    );
  };

  // The bench copy is a plain span, never a link.
  //
  // aria-hidden hides it from a screen reader; it does NOT take it out of the tab
  // order. Rendering real links there put every module in the tab sequence twice,
  // the second time at -9999px - so tabbing through the nav walked focus off the
  // side of the screen with nothing visible to show where it had gone.
  const renderMeasureItem = (module: AppModule) => (
    <span key={module.id} data-measure-item className={itemClass(false)}>
      {itemBody(module, false)}
    </span>
  );

  return (
    <nav className="w-full bg-(--base-01) border-b border-(--base-03) flex items-center justify-between px-6 py-2.5 shrink-0 relative z-30">
      {/* Branding. Width is handed in by the shell so it stays aligned with the
          sidebar underneath it - the two columns only line up because they are
          the same width, and a collapsed sidebar must not leave the logo
          hanging over the content. */}
      {brand}

      {/* The measuring bench. Every entry, always, at the real styling and off
          the flow, so the widths it reports do not depend on what is currently
          on screen. aria-hidden and inert: it is furniture for the layout, not
          a second navigation for a screen reader to read out. */}
      <div
        ref={measureRef}
        aria-hidden="true"
        className="absolute -left-[9999px] top-0 flex items-center gap-1 pointer-events-none"
      >
        {pinned && renderMeasureItem(pinned)}
        {rest.map(renderMeasureItem)}
        <span data-measure-trigger className={itemClass(false)}>
          <span className="tracking-wide">More</span>
          <ChevronDown size={16} />
        </span>
      </div>

      {/* Navigation modules. Never scrolled out of reach: the strip used to be
          overflow-x-auto with the scrollbar hidden, so entries past the fold
          were unreachable AND invisible. */}
      <div ref={rowRef} className="flex items-center gap-1 flex-1 min-w-0">
        {pinned && renderItem(pinned)}

        {/* Only the collapsible half is clipped. The trigger sits OUTSIDE it,
            because a dropdown anchored inside an overflow-hidden box is drawn
            and then cut off - which is what made the old menu look like a
            button that did nothing. */}
        <div className="flex items-center gap-1 flex-1 min-w-0 overflow-hidden">
          {visible.map(m => renderItem(m))}
        </div>

        {overflow.length > 0 && (
          <div className="relative nav-overflow shrink-0">
            <button
              type="button"
              onClick={() => setMenuOpen(o => !o)}
              aria-haspopup="menu"
              aria-expanded={menuOpen}
              title={`${overflow.length} more`}
              className={itemClass(overflow.some(m => isActiveHref(moduleHref(m))))}
            >
              <span className="tracking-wide">More</span>
              <ChevronDown
                size={16}
                className={`transition-transform ${menuOpen ? 'rotate-180' : ''}`}
                aria-hidden="true"
              />
            </button>
            {menuOpen && (
              <div className="dropdown-menu right-0 mt-2 w-52 animate-fade-in origin-top-right">
                {overflow.map(module => (
                  <GuardedLink
                    key={module.id}
                    href={moduleHref(module)}
                    className="dropdown-item"
                  >
                    <DynamicIcon name={module.icon || 'grid-2x2'} size={18} className="mr-3 text-(--base-06)" />
                    {module.name}
                  </GuardedLink>
                ))}
              </div>
            )}
          </div>
        )}
      </div>

      {/* Right actions. Empty in the compact band - the shell moves them into
          the sidebar rail there, where there is room. */}
      <div className="flex items-center gap-3 shrink-0 pl-4">
        <GuardedLink href="/favorites" title="Personal favorites and version alerts" aria-label="Favorites & alerts" className={`btn btn-ghost btn-sm ${pathname.startsWith('/favorites') ? 'text-(--accent-light)' : ''}`}><Star size={15} /></GuardedLink>
        <HelpTourButton compact />
        {children}
      </div>
    </nav>
  );
}
