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
control as Servers, aligned with the navbar branding. A wider build column stays
visible while editing.

Installed content is grouped into **Mods**, **Resources**, and **Configs &
scripts**, with search and project artwork on the installed rows. Missing or
broken artwork falls back to a content-type icon. **Add content** opens the
Modrinth browser in the main workspace. **Sharing** contains expiring share links;
**Version alerts** manages daily availability checks. **All builds** returns to
the pack overview, and the sidebar includes a full authoring guide.
Build environment, counts and export/publish
actions stay in the right sidebar. Owners and administrators see recent
activity there and can open the complete audit history separately.

Installed and search cards use the Server Content visual style. Expand a card
for its project description, switch between short/full sanitized Markdown, or
open the project on Modrinth in a new tab. Direct uploads show local metadata;
link an uploaded mod to Modrinth to obtain its upstream description.

**Client** and **Server** independently select where each entry is installed.
At least one must stay selected. The environment filters distinguish client-only,
server-only and shared entries; sorting supports name, environment and updates.
Filtering changes the view only, not the build or its exported content.

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
lists saved projects and active checks; a build's **Version alerts** tab also
offers pack-level checks using a fixed snapshot of that build's linked mods.

Choose a loader and stable Minecraft targets, future stable releases, or both.
Checks run daily, with an hourly retry after upstream errors. An initial check
usually starts within a minute. A notification appears only when all linked mods
support the target, once per watched Minecraft release, not for each mod build.
Specific jobs finish after all requested targets notify; future jobs persist.
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
