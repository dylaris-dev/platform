'use client';

import { createContext, useContext } from 'react';

// The selected server's status as the shell shows it (see shownStatus in
// lib/powerControls), so a tab shows the same word as the header. Null outside
// a server shell; callers fall back to Core's status.
export const ShownStatusContext = createContext<string | null>(null);

export const useShownStatus = (fallback: string) => useContext(ShownStatusContext) ?? fallback;
