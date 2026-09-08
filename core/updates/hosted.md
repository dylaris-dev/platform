# Service updates

Release notes for DYLARIS customers: BYON (you run a node on your own hardware)
and route-only (we route, you run nothing).

Route-only entries name no service on purpose - there is nothing on your side to
update, and those entries tell you what changes on ours.

Newest release first. The format is fixed and checked in CI - see the
"Release notes" section of the repository's CLAUDE.md before editing.

<!-- Everything in this file is English, including text dictated in German. -->

## 2026.09.09

### Features
- **Confirmation and password-reset emails look different.** They now arrive as
  a formatted message with a button, and still carry a plain-text copy for
  readers and clients that prefer one. Nothing changes about what the links do.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.09.08.6

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- **Your own hardware is yours.** A node you brought yourself can no longer be
  chosen as the destination for somebody else's server, and it is no longer
  offered as one.

### Fixes
- **A node that could not reconnect stays fixable.** In rare cases a node ended
  up holding a different credential than we did and was refused on every
  attempt. That is fixed on our side; if your node has been offline and
  reconnecting in a loop, tell us and we will re-pair it.

## 2026.09.07.14

### Features
- **Import a backup into a server.** Under Setup, choose Backup and upload an
  archive you downloaded from your backups. Archives made from 2026.09.07
  onwards also bring back your loader, versions and mod list; older ones restore
  the files and leave those to you. BYON: update your node before using this.
  `node`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.09.07.11

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- **Restoring a backup now puts your mod list back as well.** Previously only
  the files were rolled back, so the Mods tab could show a mod that was no
  longer installed, or miss one that was running. Backups made from now on carry
  the list; older ones do not, and restoring one leaves your list as it is.
  `node`

## 2026.09.07.10

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- **Backups that failed to upload now go through.** If your server backups were
  failing with a storage error, the target was reachable all along and the
  upload was refused before it started. Nothing on your side to change, and no
  archive was lost - the runs that failed simply never wrote one.

## 2026.09.07.8

### Features
- **Your modpacks now live at your own Solder address.** Pick a handle under
  Account, Solder Keys and the page shows the URL to enter on your Technic
  profile. Only your packs are listed there, and your pack names only have to be
  unique to you: somebody else having a `skyfactory` no longer stops you.

### Breaking
- **If you already linked a Solder using the shared address, enter your own URL
  on the Technic Platform again.** The old shared address no longer serves
  packs; opening it tells you where yours is.
- **The handle is chosen once and cannot be changed later.** Technic stores it
  inside every linked modpack, so a change would stop installed copies from
  updating.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.09.07.7

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- **Solder Keys and Solder Clients are now in the account menu**, under your
  name at the top right. They were reachable only by typing the address.

## 2026.09.07.6

### Features
- **You can link your modpacks to the Technic Platform.** Copy the API key from
  your Technic profile under Edit Profile, Solder Configuration, add it under
  Account, Solder Keys, then enter the Solder URL on Technic and click Link
  Solder. The URL is your panel address followed by `/solder/api/`.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.09.07.5

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- **Linking a modpack to the Technic Platform could fail with "invalid Solder
  URL" even when everything was set up correctly.** The address worked only with
  a trailing slash, and key verification was missing a field the platform may
  read. Both are fixed; if you entered the URL before and it was refused, try it
  again.

## 2026.09.07.4

### Features
- Nothing.

### Breaking
- **Included backup storage now comes with your node, not with your route-only
  location.** A route-only location carries traffic to a server you run
  yourself, so it never had anything of yours to back up here. If you hold one
  of each, the storage included with your subscription is what your node
  includes rather than twice that. What you already have stored is untouched,
  and what you can book on top is unchanged.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.09.07.3

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- **Backups to a bucket you connected yourself are no longer refused for being
  over an allowance.** That allowance covers what we store for you, and once you
  point backups at your own storage it no longer applies. Nothing on your side
  needs changing.
- **The storage figure on your usage screen now matches what the backup guard
  actually enforces.** It could show you comfortably inside your allowance while
  a backup was being refused for exceeding it.

## 2026.09.07.2

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- **A refused backup now explains itself.** If your backup storage allowance is
  used up, or set to nothing at all, the message says which of the two it is and
  what to do about it. It used to read "0 / 0 GB used - delete old backups",
  which was neither.

## 2026.09.07

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- **Your own backup storage could refuse every upload with a checksum error.**
  If you pointed backups at an S3-compatible bucket, some providers rejected the
  write with a "BadDigest" message even though the credentials and the bucket
  were right. Nothing on your side needs changing; try the storage test again.
- **The folder you set on your own backup storage was ignored.** Archives went
  to the top of the bucket instead, even though the panel showed the folder you
  typed. New archives now go where you asked; anything already written stays at
  the top of the bucket and is no longer listed.

## 2026.09.06.3

### Features
- You can now replace the key of a machine, or of a protected address, from the
  panel. Nothing else changes: the same address, the same servers, the same
  name. Edit that one line in your compose file and deploy again when it suits
  you - what you are running now keeps running until you do.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- **A compose file copied in the last few days names an image that does not
  exist.** The address changed and the snippet was one segment out, so a fresh
  deploy could not pull warp, link or the node agent. Copy the snippet again
  from the panel; nothing else about your setup is affected.

## 2026.09.06.2

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- **A server you created before the address change kept starting from the old
  address.** It worked only because a copy was still on your machine; once that
  copy was cleaned up the server would have refused to start. Restarting a
  server now moves it to the new address on its own. Nothing for you to
  edit. `node`

## 2026.09.06

### Features
- Nothing.

### Breaking
- **The images you pull have a new address.** Anything you copied earlier that
  says `ghcr.io/bartis-dev/dylaris-gateway-link` or `-warp` becomes
  `ghcr.io/dylaris-dev/gateway-link` and `ghcr.io/dylaris-dev/gateway-warp`.
  Edit that one line in your compose file and pull again. Fresh snippets from
  the panel already carry the new address.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.09.04.3

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- The store link screens now say what linking actually moves. Connecting a
  different panel account brings the subscription across, not your servers,
  protected addresses or your own nodes, which stay with the account that
  created them. `panel`

## 2026.09.03.15

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- If your node goes offline while a backup is being restored, the panel now says
  the restore is paused and why, instead of showing it as still running. Nothing
  is lost: it picks up on its own once your node is back. Nothing for you to
  update - this is on our side.

## 2026.09.02.19

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.09.02.18

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing you need to do. Your machines are now tracked separately from ours, so
  taking your own node or router offline no longer registers as a fault on our
  side - and nothing about how yours are monitored or reachable has changed.

## 2026.09.02.17

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing you need to do. We now measure how many players stay connected when a
  routing server restarts, and how often the tunnel from your machine drops and
  comes back - so a problem on your route is something we can see rather than
  something you have to report.

## 2026.09.02.13

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Closing an account now also removes the machines you registered with it, so
  nothing of ours keeps talking to hardware that is no longer part of your
  account.

### Fixes
- Nothing.

## 2026.09.02.12

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Servers are no longer placed or moved onto hardware belonging to a different
  customer. If you run your own node, only your own servers can land on it.

### Fixes
- Nothing.

## 2026.09.02.11

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Two fixes to how tickets and the public demo server handle other people's
  data. Nothing you run is involved and no action is needed.

### Fixes
- Nothing.

## 2026.09.02.10

### Features
- Files you uploaded yourself now show up on the Content tab, where they can be
  matched against Modrinth. Matched ones become normal entries and get update
  checks like anything you installed from the panel.
- A file that cannot be matched says why, and can be deleted from there. This is
  how you clear the duplicate jars left behind by mod updates before this week:
  they are recognised as a second copy of something already installed.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- The same check reported "nothing out of place" when it had failed to look at
  all. It now says so.

## 2026.09.02.9

### Features
- You can roll a mod or plugin back to an earlier build. We keep the three most
  recent builds your server actually ran, and going back replaces the file the
  same careful way an update does.
- An "Update all" button updates every installed mod in one run and tells you
  afterwards what was updated, what was already current and what did not work.
  There is a matching way back for the whole set.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.09.02.8

### Features
- The build list of a mod now shows which build your server actually has, and
  whether an install is still running or failed.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Updating a mod or plugin left the old file next to the new one, so your server
  loaded both builds of it. Updating now replaces the file. The old build is
  only removed once the new one has downloaded and been verified, so a failed
  update leaves your server running what it had. `node`
- A mod that failed to install was still listed as installed. It now says so,
  with the reason. `node`

## 2026.09.02.7

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- The Content tab of a server said "No mods installed" when it had failed to
  load the list, and every mod in the search results looked uninstalled with
  it. It now says the list could not be loaded and offers to try again.

## 2026.09.02.6

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- The panel for your own domains no longer disappears when it cannot load, and
  a TXT check that did not find the record is no longer shown in green. If we
  cannot look up the record you need to add, the page says so instead of
  leaving it out of the instruction.
- Opening a custom tab while a request failed said the tab had been deleted. It
  now says the tab could not be loaded and offers to try again.

## 2026.09.02.5

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- The Content tab now scrolls in three separate panes, so the category list and
  the search results stay put while you scroll the other one. The players list
  behaves the same way.
- Custom tabs showed their page in a small frame instead of filling the tab.

## 2026.09.02.4

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- The full description of a mod now shows the pictures, tables and links it is
  written with, instead of a block of raw markup, and there is a button that
  opens the mod's own page. Nothing to update on your side.

## 2026.09.02.3

### Features
- Nothing.

### Breaking
- People you invited to your servers now get the same file permissions over SFTP
  and in the desktop client that they already had in the panel. Somebody whose
  role allows editing but not deleting can no longer delete files through those
  two. Check your invites if you relied on the old behaviour. `node`

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.09.01.14

### Features
- Nothing.

### Breaking
- Holding more addresses or nodes than your plan includes now actually stops
  your service once the 72-hour notice has passed, route-only included. You get
  an email when the clock starts, and removing the excess before the deadline
  ends it. Nothing to update on your side.
- The same notice now applies per product. If one part of your plan ends while
  another continues, what the ended part covered is no longer left running
  indefinitely.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.09.01.10

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Machines that customers run could reach further into our coordination service
  than they needed to. Their access is now scoped to their own machine. Nothing
  suggests this was used, and nothing you run needs changing.

### Fixes
- Nothing.

## 2026.09.01.9

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Your data traffic is shown even in a month where you have transferred nothing,
  instead of the panel disappearing.
- If your access was given to you rather than bought, the traffic billing switch
  now says so instead of reporting a missing subscription. Nothing is charged
  and nothing is stopped in that case.

## 2026.09.01.8

### Features
- Your traffic is now shown as two separate panels, player traffic and data
  traffic, with a breakdown of how much came from your own node and how much
  from your protected addresses.
- The switch for being billed past your included traffic now sits next to those
  numbers, under My Infrastructure.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Adding your first machine failed with "Node limit reached (1)" even though you
  had none. It works now, and nothing you did caused it.
- Data traffic with no configured allowance was not shown at all. It is now.

## 2026.09.01.7

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Buying BYON or route-only after being given it by hand now simply starts your
  subscription, and it runs as a normal subscription from then on.
- Setting up a node from inside the Beam app has been withdrawn. Use the deploy
  snippet on My Infrastructure, which is unchanged.
- Beam's settings button stays where you put it when you resize the window.
  Dragging it to the right edge of a small window used to leave it in the middle
  of a large one.

## 2026.09.01.6

### Features
- You can remove one of your own machines now, which is what frees its slot when
  you want to set it up somewhere else. You are asked twice, and shown every
  server and world that would go with it.
- Setting up a bring-your-own node has a Windows (Docker Desktop) option, the
  same as protected addresses already did.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- At your machine limit the panel only offered to sell you another location,
  even when what you wanted was to move the machine you already have.
- The deploy snippet no longer takes over the whole page.

## 2026.09.01.5

### Features
- If one of your machines stops answering, you now see it on every page instead
  of having to open My Infrastructure. A short restart does not trigger it.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.09.01.4

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing that affects you: this release fixes an admin screen on our side.

## 2026.09.01.3

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Beam 0.9.5 could hang on a blank white window. Update to 0.9.6, which fixes
  it; nothing on your machines is affected.
- Beam felt slow to load a page after signing in. It was writing to disk far
  more often than it needed to.

## 2026.09.01.2

### Features
- Setting a node up on the computer you are sitting at has moved out of Beam's
  settings and onto My Infrastructure, beside your other machines.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Beam asked you to sign in again every time you opened it. It kept your session
  in memory only, so closing the app threw it away.
- Beam's settings button could disappear for the rest of a session, and it
  started in the bottom left corner underneath the sidebar. It now sits in the
  bottom right and comes back on its own.

## 2026.09.01

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- If you run more than one node on a single machine, they could manage each
  other's servers - one starting up would restart the other's, players and all.
  Each node now only touches what it created. Update your node to get this. `node`

## 2026.08.31.20

### Features
- Beam can set a node up on the machine it is running on. It checks whether
  Docker is installed, running and reachable by your account, says which of those
  failed and what to do about it, then reserves a pairing token and writes a
  ready-to-run compose file - starting it for you, or leaving it for you to read
  first.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.08.31.19

### Features
- Beam now notices updates while you are working. It re-checks in the background
  and marks the settings button with a dot when a new version is waiting, instead
  of only looking once at startup on a screen you had already left.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.08.31.18

### Features
- Beam's settings now list your panels, with Dylaris always at the top so it
  cannot be lost. Panels you add can be named, edited and removed, and saving
  under a name you already used replaces that entry.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.08.31.17

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Your own servers could be missing from your server list while they were running
  perfectly well. Nothing was wrong with the servers and none of them were lost;
  the list was hiding them. No action needed on your side.
- Beam no longer shows a button offering to download Beam.

## 2026.08.31.15

### Features
- The panel now shows your subscription: how much of your traffic and backup
  allowance is used, and whether going over bills you or stops you. Both
  switches can be turned on and off from there.
- Beam can hold several panels and switch between them without signing in again,
  and its own settings now open from a small button you can drag along the bottom
  of the window. Update the app to get it.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.08.31.12

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Signing in to the Beam desktop app no longer sends you back to the login
  screen. Update the app to get the fix.
- When Beam cannot reach the panel it now keeps retrying and comes back on its
  own, and its settings are reachable from that screen.

## 2026.08.31.9

### Features
- The Beam desktop app can now be downloaded straight from the navigation bar,
  instead of only from a server's Files page.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- A node that had lost contact could keep re-registering instead of reconnecting,
  and stayed offline while doing so. Fixed on our side; nothing to update on
  yours.
- The panel no longer squeezes itself below a usable width on a narrow window.

## 2026.08.31.8

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Navigation entries could disappear into the "More" menu at window sizes where
  they fit, and only a page reload brought them back. Nothing to update on your
  side.

## 2026.08.31.7

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- After yesterday's privilege change, a server that crashed could be restarted
  into the same failure and stay down instead of recovering on its own. It now
  recovers automatically. `node`

## 2026.08.31.6

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Your server now runs with fewer privileges inside its container. Plugins and
  mods still work exactly as before; they can simply no longer reach anything
  outside your own server files.

### Fixes
- Nothing.

## 2026.08.31.5

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Opening a single server, modpack or ticket showed "not found". Every one of
  those pages works again; nothing on your side needed changing.

## 2026.08.31.4

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- The Beam desktop app refused every save and upload. It works again; update the
  app is not needed, the fix is on our side.

## 2026.08.31.2

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- The panel was briefly unreachable after yesterday's release, showing a
  placeholder page instead. It is back; nothing on your side needed changing.
- Custom tabs such as a map viewer would not open from the panel. They load
  again.

## 2026.08.31

### Features
- Nothing.

### Breaking
- Backups we hold for you are now kept for one week after a suspension instead
  of three months. During that week you can still see and download them, and
  your server files are untouched. Backups in a bucket you connected yourself
  are never deleted by us, at any point.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.08.30.5

### Features
- Backups now come with 50 GB of storage per subscription, and metered storage is
  there if you need more: off until you turn it on, stopping at a ceiling shown
  before you agree, and we tell you if that ceiling ever changes.
- Your panel shows this month's traffic under My infrastructure, one bar per
  allowance, and your account page shows how much more you may book on each.
  Player traffic and file transfers are separate, and you are warned at 80% and 90%.
- You can connect an S3 bucket of your own for backups, under Account, Backup
  storage. What lands there is not counted against your allowance and never
  billed, and we never delete from it - not on suspension, not ever.
- Changing your Java version or JVM flags no longer reinstalls the server. Your
  files stay exactly as they are and the change applies straight away.
- Setting up a server remembers which modpack you installed and puts it back when
  you edit it. Changing a version or a modpack asks what to clear first and ticks
  the usual answers for you; your world is never on that list.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- A domain you add while setting a server up now appears straight away. It used
  to look like the name was already taken until you reloaded the page.
- The menu bar no longer loses entries when the window is made narrower, and its
  overflow menu opens properly.

## 2026.08.30.4

### Features
- Nothing.

### Breaking
- If you have agreed to be billed for extra traffic, there is now a ceiling on
  how much may be bought in a region. Past it your servers stop rather than
  keep billing. Nothing changes until your provider sets one, and nothing on
  your side needs updating.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.08.30

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- On "My infrastructure", the compose file you copy now gets a column wide
  enough to show its longest line, and the two columns stack instead of
  shrinking when your window is narrow. Nothing for you to update.

## 2026.08.29.14

### Features
- Nothing.

### Breaking
- Traffic through a protected address now counts against your included traffic.
  It was not being counted at all, so your usage figure will start moving where
  it previously stayed flat. Nothing on your side changes and there is nothing
  for you to update.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.08.29.11

### Features
- Nothing.

### Breaking
- Your link now reaches us over the internet only, never through the tunnel it
  uses for everything else. It was doing both, and the tunnel route made your
  players share one connection with your own uploads and drop whenever that
  connection restarted.
- If you run your own node, this now holds no matter how it is configured. The
  setting that used to decide it is on your machine, so it no longer gets a
  vote. `node`

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.08.29.10

### Features
- The file you run on your machine is now shown permanently next to the form,
  for both your own node and your protected addresses. You no longer have to
  open a fold-out to get it back when you rebuild the machine.
- When you pick an address, the panel now says what the region choice decides
  for your players, and that you can point a domain you own at us instead.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- A protected address could disappear after maintenance on our side and never
  come back. Addresses are now stored durably and restored on their own; you do
  not need to create them again.
- Revoking a link kit now reliably removes the addresses that ran through it.

## 2026.08.29.6

### Features
- Your protected addresses can be edited. Change the local address or port in
  place instead of deleting the address and creating it again.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Changing an address you already have no longer counts against your address
  allowance. Fixed on our side.

## 2026.08.29.5

### Features
- Your machine now reaches our edges directly instead of through the tunnel.
  Your players no longer share that tunnel with your own uploads, and they are
  no longer dropped when it restarts. Copy your deploy file from the panel again
  to get it. `node`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.08.29.3

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- A tunnel that cannot be set up now says so instead of retrying quietly. If
  your warp log repeated "no leader endpoint", that was this. Fixed on our side.

## 2026.08.29.2

### Features
- Your deploy file now marks every setting as "keep" or "EDIT", and says how to
  create and start it.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Creating an address could fail with "You have used all -1 addresses" even on a
  plan that allows them. Fixed on our side; try again.

## 2026.08.29

### Features
- The deploy file for a BYON node or a route-only link now comes with the steps
  to create and start it, for Linux and for Windows.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- The deploy file could carry the wrong address for our API, so the tunnel never
  came up. Copy it from the panel again if yours did not connect. Fixed on our
  side.

## 2026.08.28

### Features
- Your node now reports which build it is running, so the panel can tell you
  when an update is waiting. `node`
- Updates are announced on our Discord. Pick the **BYON** or **Route-Only**
  role under Channels & Roles, or ignore it - the panel tells you the same.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- If your node goes offline mid-install, the panel now says so instead of
  showing a spinner. Nothing is lost; it resumes when the node reconnects.
  `node`
- Node allowances now count correctly. Two kinds of pending node identity were
  counted separately, so an account could exceed its plan and then be warned
  for it. Fixed on our side.
- An address allowance of zero now means zero. It was read as "no setting" and
  fell back to unlimited. Addresses on your own domain stay unlimited and
  uncounted. Fixed on our side.
