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
use a package icon. A separate build column stays visible while editing.

Installed content is grouped into **Mods**, **Resources**, and **Configs &
scripts**, with search and project artwork on the installed rows. Missing or
broken artwork falls back to a content-type icon. **Add content** opens the
Modrinth browser in the main workspace. **Tools & sharing** contains Minecraft
migration and download links. Build environment, counts and export/publish
actions stay in the right sidebar. Owners and administrators see recent
activity there and can open the complete audit history separately.

## Build creation

**New build** is a dedicated page. Choose an empty draft or copy an existing
build, then select Minecraft, loader and compatible loader version. Copying
requires the same Minecraft version and loader; use Minecraft migration for a
different environment. A copy retains exact artifact versions and environment
sides in independent content records. Config edits and removals in the copy do
not rewrite the source. Creation and content copying are one database transaction.
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

## Export and verification

`.mrpack` exports retain Modrinth references and embed local files under shared,
client-only or server-only overrides according to their selected side. Server
ZIP exports exclude client-only entries. Frozen builds remain immutable.

Verification for the 2026-10-27 changes used a separate local database and an
existing private Packwiz template: Minecraft 1.20.1, Forge 47.4.28, 119 mods,
one resource pack and 72 configuration/KubeJS files. All 120 linked entries
exported, and all local configuration bytes matched the source. Tests also
covered read/edit/delete grants, revocation, incompatible content and traversal.
No external Modrinth publication or real-user invitation was used for testing.
