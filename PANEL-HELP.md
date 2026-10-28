# Personal library and guided panel help

**Favorites** is a top-level navigation entry and a persistent star shortcut.
It combines bookmarks from Server Content and Modpacks, with search, explanation
and personal daily availability checks. `/modpacks/favorites` redirects to the
canonical `/favorites` page. Saving a favorite does not install or subscribe it;
the version-alert bell is a separate action. All data remains user-scoped.

**Help** opens `/help`; the question-mark button opens contextual guided examples.
Servers and Modpacks also expose the button locally. Topics cover server creation,
BYON, route-only, Server Content, custom tabs, modpack authoring and favorites.
Next/Back and the example tabs switch the preview area without leaving the real
page, navigating away from unsaved work or activating a live control.

Build editors show content filters instead of a persistent version column.
Use **All builds** in the pack header to change versions. Category controls match
Server Content; Install resolves a compatible version, while Installed/Remove
reflect this build and its permissions. See [modpack authoring](MODPACKS.md).

## Safety and boundaries

The tour component only renders curated example data. It does not import an API
client, embed external sites, mint pairing commands, download private files or
issue mutations. Non-admin users can view examples of unavailable features;
the real permission and entitlement checks are unchanged. Examples are labelled
throughout and use `.invalid` addresses. Actual workflows still require their
normal permissions, subscriptions, admission and operator approval where applicable.

This is a guided preview, not automation over live controls or a functioning
sandbox of infrastructure. Tours do not create resources or grant access. They
do not start automatically, progress on timers, or persist a completed status.
Modal focus trapping, Escape, keyboard-operable tabs and the existing design
tokens are reused. At shorter window heights the tour body scrolls.

## Maintenance

Tour content lives in `panel/src/components/help/tours.ts`. Update it alongside
changes to the real workflows, and verify both administrator and ordinary-user
views. Browser verification must assert no mutation requests during every tour,
including creation, BYON and route-only examples. Keep credentials and executable
setup commands out of the examples. The complete modpack guide remains available
at `/modpacks/guide` for longer reference material.
