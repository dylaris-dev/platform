# Modpack authoring and collaboration

## Access

Open **Access > Manage modpack access**, **Modpacks > Collaborators**, or the
**Collaborators** button on a pack you own. The pack-level entry selects that
pack by default. Account-level access can cover selected packs or every pack,
including future ones.

Every invitation includes read access. Additional rights are independent:

- **Edit and publish:** modify content, builds and pack settings, and publish
  through the owner's stored Modrinth token.
- **Delete:** remove packs, builds and content.
- **Create:** create packs on the owner's account; available only for an
  account-wide invitation.

These grants do not grant server access. The platform's authoring switch and
per-user authoring permission remain ceilings for non-admin writers. An admin
can inspect another owner's pack, but is not automatically its editor. Only the
owner manages invitations; recipients can leave a collaboration.

Collaborators never receive the owner's Modrinth token. Publishing targets the
pack's stored project binding, or a newly created project returned by Modrinth;
a caller cannot select another target project in the request body. These are
DYLARIS permissions, not invitations to Modrinth's separate team system.

## Workspace

The left sidebar lists owned and shared packs with project artwork and their
latest release, or latest build if no release exists. These labels describe
build history, not a running deployment. Local packs without upstream artwork
use a package icon. **My modpacks** and **Invited / collaborating** have separate
sections and a shared search field. The sidebar uses the same width and collapse
control as Servers, aligned with the navbar branding. The pack overview lists the
builds. Inside a build, content filters use that space; **All builds** in the
header returns to the overview.

Installed content is grouped into **Mods**, **Resources**, and **Configs &
scripts**, with search and project artwork on the installed rows. Missing or
broken artwork falls back to a content-type icon. **Add content** opens the
Modrinth browser in the main workspace. Each installed entry is one row: name,
version and categories, then its Client/Server sides and actions. An empty
build offers Add from Modrinth and Upload directly. **Sharing** appears when
share links are enabled or a build already has links, and contains expiring share links;
The pack overview carries **Recommended build**, **Pack version alerts**,
**Collaborators** and **New build**; inside a build the header keeps **All builds**
and **New build** so the content list keeps its height. The alert dialog opens directly in one click
without selecting a build; it links to a pack-level page for managing existing
alerts. **All builds** returns to
the pack overview; the active overview/build is highlighted and the button
contains no build count. The sidebar includes a full authoring guide.
Build environment, counts and export/publish
actions stay in the right sidebar. At narrower widths it opens through
**Build details** to preserve content space. Recommended uses a purple star badge;
the newest release uses green. Both badges appear when the same build has both roles.
The Delete build trigger is neutral; the final destructive confirmation stays explicit.
Owners and administrators open pack-wide **Activity** from the header; it no
longer competes with the build actions in the details sidebar.

Installed and search cards use the Server Content visual style. Expand a card
for its project description, switch between short/full sanitized Markdown, or
open the project on Modrinth in a new tab. Direct uploads show local metadata;
link an uploaded mod to Modrinth to obtain its upstream description.

**Client** and **Server** independently select where each entry is installed.
At least one must stay selected. The environment filters distinguish client-only,
server-only and shared entries; sorting supports name, environment and updates.
Filtering changes the view only, not the build or its exported content.

## Content browser and feedback

Installed content and **Add content** share the Server Content category control.
Filter mods by environment and category; resource searches use resource/shader
categories. Categories on installed rows come from linked Modrinth metadata;
unlinked uploads remain visible under All categories. Search and manual version
selection remain available. **Install** selects the latest compatible stable
release, or a compatible beta/alpha if no stable release exists, using the build's
Minecraft version and loader. It never blindly installs the search result's latest
version. A project Modrinth marks as client-only (server `unsupported`, e.g.
Sodium) is installed on the client side only, so it stays out of the server
download; server-only projects likewise. Its required dependencies always go on
both sides. Projects added before this change keep their sides; adjust them per
row. Required dependency warnings still require review. Installed projects
show **Installed** and a permission-checked **Remove** action; removing confirms
all matching entries in this build. Read-only or frozen builds can browse without
receiving write rights; a read-only or frozen build says so once above its
content. Vanilla builds cannot install loader-specific mods. The Publish dialog
states that publishing freezes the build, also when Modrinth rejects the upload.

Results load in pages of 20: scrolling near the end loads the next page, and
**Load more** stays as a button (and the retry after an error); query changes
discard stale responses. Both installed and browse lists have their own scroll containers so
the last entry and pagination remain reachable. Filtering never changes exports.

Panel feedback stays copyable. Default success/error lifetimes are 5.2/9.1 seconds,
35% shorter than the previous defaults. Opacity starts declining halfway through.
Hover and keyboard focus restore full visibility and pause the outline and
remaining lifetime together; leaving resumes the remainder without resetting it.
A final short fade dismisses the card. Reduced motion disables the animated
outline and fading while preserving expiration and pause behavior.

New Modrinth projects use `LicenseRef-All-Rights-Reserved`, preserving the existing
all-rights-reserved intent with a valid SPDX expression. Legacy `arr` input is
normalized at the API-client boundary; explicitly supplied other licenses remain
unchanged. External moderation/token requirements still apply.

## Build creation

**New build** is a dedicated page. Choose an empty draft or copy an existing
build, then select Minecraft, loader and compatible loader version. A copy with
the same Minecraft version and loader retains exact artifact versions and environment
sides in independent content records. Config edits and removals in the copy do
not rewrite the source. Creation and content copying are one database transaction.
Changing Minecraft or loader in New build switches to migration: the availability
preview names missing/unverifiable projects, and leaving them out requires explicit
confirmation. This creates an independent draft, resolves compatible upstream
versions and copies local configs without converting them. Partial copy failures
are reported for review. Add a description/changelog to record the reason.
Available metadata is fetched through Core; compatible recommended/stable loader
builds are selected when possible. The proposed pack version starts at `0.1.0`
or the next patch and remains editable. Re-selecting the same Minecraft/loader
keeps the selected loader build; changing either reloads compatible choices.

Metadata failures have a retry action. No compatible loader build is a real
empty state, not a reason to guess a version. Vanilla builds do not use a mod
loader; use a compatible mod loader for mods.

## Recommendations and forks

**Recommended build** in the pack overview explicitly selects the pack's player-facing
recommendation. **Automatic: newest release** instead selects the newest release
channel build. New drafts do not change this source automatically. Without a
recommendation or release, pack alerts wait. Explicitly recommending an editable
draft is allowed and clearly labelled; its current mod list is then checked.
Recommendation does not freeze, publish or update installed/running servers.

**Delete build** is available in the overview and build details with delete
permission. Frozen or published builds require typing the exact version name.
The API also requires JSON `{"confirmVersion":"exact-version"}` when deleting
such a build. This removes local build metadata, downloads and share links;
other builds, installed server files and external Modrinth versions remain.
Deleting the recommendation clears it and alerts fall back to the newest remaining
release, or wait if none exists. Published builds cannot be edited or published
again; a failed, frozen publication can still be retried.

**Fork build into another pack** copies all pinned content, paths, environments,
configuration bytes and empty folders into an independent draft. Choose a new
pack owned by your account or an existing writable pack with no builds. Source
read access and target authoring rights are required, including normal global
authoring ceilings. Existing target settings remain unchanged. New packs do not
inherit the source's Modrinth binding, credentials, share links or collaborators.

Uploaded objects and catalog/version records are copied to the target owner.
Deleting or editing the source therefore does not invalidate the fork. Storage
copying is streamed and verified, then metadata commits atomically. Concurrent
copies cannot both populate the same empty target. Failed copies clean up only
their unreferenced staged objects; an ambiguous database commit retains files
until the outcome can be verified. Limits are 2000 entries, 1 GB per uploaded
object and 2 GB of uploaded objects per fork; linked Modrinth references do not
require copying the JARs. Large copies can take a while and can be cancelled.
An abrupt Core crash can leave private, unreferenced staging objects; clean them
only with reference-aware storage maintenance. These bounds are per operation,
not a new account-wide storage quota.

## Modrinth and local files

Search Modrinth from a build and choose a compatible version. Core checks the
project/version association, Minecraft version, loader and artifact metadata.
Resource and shader packs are checked against Minecraft without requiring a
Forge/Fabric mod tag. Existing dependencies are taken into account; use the
update/replace action rather than adding the same overlapping project twice.

For configuration and KubeJS files, select **Config/scripts** in Upload. Choose
files with a destination path or choose a folder to retain `config/`,
`defaultconfigs/` or `kubejs/`. ZIP bundles are extracted only when the explicit
bundle option is selected. A resource/shader ZIP remains its own file.

Paths are relative to the Minecraft instance and cannot traverse outside it.
Uploads retain their build-specific file/catalog identity. Overlapping output
paths are rejected instead of silently overwriting another entry. Remove or
edit the existing content before uploading a replacement at the same target.

**Configs & scripts** also uses the same file browser/editor as server files.
Create, copy, rename, edit and delete text files or folders in shared, client-only
or server-only scope. Edit and delete permissions are checked independently.
Editing is limited to 1 MB; binary files and legacy bundles use the full build
download. Empty folders are metadata and enter exports when they contain files.

## Favorites and Minecraft availability alerts

Star or watch a Modrinth project from installed content or search, in Modpacks
or Server Content. Favorites belong to the signed-in user. **Favorites & alerts**
lists saved projects and active checks; **Pack version alerts** in the pack
overview offers pack-level checks following the current Recommended build or
newest release.
The linked mod list is resolved again for each check; deleting an old source
build does not remove the pack alert. The checked build/version is shown in the
job and notification. A source change during lookup is retried before delivery.

Choose a loader and stable Minecraft targets, future stable releases, or both.
Resource/shader project alerts use Minecraft compatibility without a mod-loader tag.
Checks run daily, with an hourly retry after upstream errors. An initial check
usually starts within a minute. A notification appears only when all linked mods
support the target, once per watched Minecraft release, not for each mod build.
Specific jobs finish after all requested targets notify; future jobs persist.
Previously notified Minecraft versions remain delivered when the recommendation
changes, avoiding repeated messages for ordinary pack/mod updates.
Local/unlinked mods, dependencies, configs and gameplay need manual validation.
Removing pack access cancels its watch. Existing messages stay in the bell inbox
when read and are removed only by manual deletion. Mobile delivery is not implemented.

## Storage and backups

The configured artifact provider stores uploaded ZIP/config objects below
`packs/<owner>/mods/<artifact>/<hash>.zip`. Persisted `.mrpack` and cached server
archives live under an opaque, HMAC-derived `modpacks/` namespace. These paths
are download credentials and must not be published in logs or screenshots.
PostgreSQL holds build metadata, access grants, favorite/watch state and empty
folders. Back up both PostgreSQL and the configured local/S3 artifact provider.

Ordinary client exports are rendered without persisting or freezing the draft.
Publishing persists and freezes the artifact before calling Modrinth; an upstream
failure can therefore leave a frozen build that can be retried. Installing a
draft as a server may persist its cached snapshot without freezing it.

## Export and verification

`.mrpack` exports retain Modrinth references and embed local files under shared,
client-only or server-only overrides according to their selected side. Server
ZIP exports exclude client-only entries. Frozen builds remain immutable.

The build details sidebar has **Download client .mrpack** and **Download server
ZIP**. Each opens an installation guide before the download. The client archive
is imported into Modrinth App as a new instance. For DYLARIS server setup choose
**My modpacks** as the installation method and select the pack/build directly.
Manual ZIP installation requires Java, the matching Minecraft server and loader;
these are not bundled. The guide includes version-specific Java advice and
Windows/Linux launch examples for Fabric, Forge and NeoForge. Legacy Forge
releases use their own installed JAR instead of modern generated launch scripts.
Review the Minecraft EULA yourself before accepting it.

For a build with an uploaded Modrinth version, **Open in Modrinth App** uses the
official `modrinth://version/<id>` protocol. The app must be installed, and its
account must be allowed to access that version; visibility/moderation still apply.
Private DYLARIS drafts use manual `.mrpack` import instead of a public share link.
Modrinth project creation submits required empty arrays and a draft project.
The publish dialog defaults drafts to Beta and submits that displayed channel.
Every version is uploaded as a listed version (Beta or Release), so it appears in
the project's version list; until the project passes Modrinth review, only its
members can see it. Versions published before 2026.10.28.6 went up unlisted and
are missing from that list: open the version by its direct link on Modrinth and
change its visibility there.

Both downloads require pack read access. The server download uses
`GET /api/packs/{id}/builds/{buildId}/export?kind=server`; the default or
`kind=client` retains the existing .mrpack format. Server rendering reuses the
bounded, cached share renderer without creating a public link or requiring share
links to be enabled. A busy render returns 503 with Retry-After rather than
streaming an unfinished ZIP. Retry after a minute; large first renders can take
longer. Downloading does not publish or freeze a draft.

Verification for the 2026-10-27 changes used a separate local database and an
existing private Packwiz template: Minecraft 1.20.1, Forge 47.4.28, 119 mods,
one resource pack and 72 configuration/KubeJS files. All 120 linked entries
exported, and all local configuration bytes matched the source. Tests also
covered read/edit/delete grants, revocation, incompatible content and traversal.
No external Modrinth publication or real-user invitation was used for testing.

The workspace refinement was checked in a real isolated browser at 1100, 1440
and 1920 pixels, including read-only collaboration, search, descriptions,
environment saves/restoration and both download dialogs. The 355,989,306-byte
server ZIP contained exactly 176 server/shared paths; all 16 client-only paths
were excluded. All included linked JAR hashes and 72 configuration/script bytes
matched their pinned source. The download validation passed 185 assertions.
This verifies the archive, not a Minecraft boot or a launcher import.

The subsequent authoring expansion was verified against a fresh PostgreSQL schema
and the isolated Core. Tests cover concurrent exactly-once inbox delivery,
cross-user isolation, manual deletion and job completion. Browser checks exercised
file creation/edit/save/copy/rename/delete, the guide, All builds, alert creation
and deletion, and clipboard/hover/countdown behavior using an isolated clipboard.
A real migration produced 111 linked items and 72 byte-identical configs on
Minecraft 1.21.1 / NeoForge 21.1.265, explicitly omitting nine unavailable items.
The source retained its pinned versions, paths, sides and hashes. The publish
request is covered by a multipart contract test; external publication, native
launcher import and a Minecraft boot are not claimed as tested.

Recommendation/fork checks also used the actual local API and browser. The same
pack alert followed sources containing 119, 110 and two linked mods, waited when
the recommended draft was deleted, and resumed on the restored recommendation
without repeating its delivered Minecraft version. New-pack, existing-empty-pack
and cross-owner forks retained all 192 entries and 72 config bytes. Source-owner
writes to a private foreign fork were denied, target-owner edits worked, and a
temporary source pack was deleted without damaging its copy. The browser fork
dialog and recommendation selector, including reload persistence, were exercised.
An authenticated fork client export retained 120 pinned references and all 72
config bytes, denied an outsider, and did not persist or freeze the draft.
