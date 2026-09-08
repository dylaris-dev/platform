# Platform updates

Release notes for people who RUN the platform: self-hosters and operators.
Customer-facing notes for BYON and route-only live in `hosted.md`.

Newest release first. The format is fixed and checked in CI - see the
"Release notes" section of the repository's CLAUDE.md before editing.

<!-- Everything in this file is English, including text dictated in German. -->

## 2026.09.08.6

### Features
- **The user picker puts you first.** Owner and assignment lists now open with
  your own account, then everyone holding a role, then everyone else, each
  alphabetically. `core` `panel`

### Breaking
- Nothing.

### Security
- **A tenant's own machine is no longer a placement target for operators.** The
  target-node picker and the transfer dialog now offer only machines you may
  actually place on, and the create and transfer calls refuse the rest.
  Auto-placement and the rebalancer already worked this way; the explicit
  node choice did not. `core` `panel`

### Fixes
- **A node could be locked out of the platform for good.** When several Core
  replicas issued a node's credential at the same moment, the node kept one and
  Core stored another, and every reconnect after that was refused. Re-pair such
  a node from Settings once this release is running. `core`

## 2026.09.08.5

### Features
- **Every "Test connection" in Settings now says which step failed.** It reaches
  the host first and only then authenticates, so a hostname typo, a firewall, a
  wrong password and a missing database stop arriving as the same sentence.
  Buttons grey out while a test runs, and an unreachable host answers in about
  three seconds instead of hanging. `core` `panel`
- **Four new Link metrics in the statistics picker**, covering how often a
  player was carried across an edge restart and how much data that took. One of
  them, "Resumes refused", counts the sessions that were ended rather than
  resumed with a gap in them. `core`

### Breaking
- **Long-term statistics need a database of their own now.** Recording into the
  platform's own database at hour resolution is gone, and so is the mode that
  chose between the two. Name a PostgreSQL with TimescaleDB in Settings; with
  none named nothing is recorded, and the switch cannot be turned on without
  one. Switching recording OFF still works with the form empty. `core` `panel`

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.09.08.4

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- **Scheduled platform backups now actually run, and retention now actually
  prunes.** The schedule and retention fields were saved and read back but
  nothing acted on them, so a job set to "every 24h" never ran and old bundles
  were kept forever. A backup started by hand prunes too, so a job you only run
  yourself no longer keeps every bundle it ever wrote. `core`

## 2026.09.08.3

### Features
- **Restore a platform bundle.** Upload one - from this installation or from
  another Dylaris - and pick what comes back: the database, the Library, the
  Modpacks, or any combination. The screen tells you what the bundle holds
  before you commit to it. `core` `panel`
- **A restored database goes into a new, empty database you name, never over the
  one Core is running on.** The platform keeps working throughout, a failed
  restore costs nothing, and you switch over by restarting Core with the new
  `DB_*` values once you have checked the result. `core` `panel`

### Breaking
- Nothing.

### Security
- **Restored credentials are re-encrypted for the installation that receives
  them.** Node secrets, storage credentials, the Modrinth token and secret
  settings are stored under the cluster secret of whichever platform wrote them;
  without this step a restore looks like it worked while every node fails to
  authenticate. Anything that cannot be re-encrypted is left untouched and
  reported, never rewritten. `core`
- A bundle entry that would write outside its own area is skipped and reported
  rather than obeyed. A bundle is a file somebody hands you, and it may not have
  come from Dylaris. `core`

### Fixes
- Nothing.

## 2026.09.08.2

### Features
- **Settings, Platform Backups.** Back up the platform itself: the database,
  the Library, Modpacks, and servers you select individually, by owner, or all
  at once. Each run says what it covered and what it skipped. `core` `panel`
- Selected servers are backed up by their OWN backup job, so each archive keeps
  that owner's quota, retention and destination and stays individually
  restorable. A server with no enabled job is reported rather than silently left
  out. `core`
- The statistics database is shown but not yet covered - it is TimescaleDB and
  needs its own handling. Selecting it records the gap in the run instead of
  backing it up half way. `core` `panel`

### Breaking
- Nothing.

### Security
- **A platform bundle is encrypted with a passphrase you set once, and losing it
  loses every bundle taken under it.** There is no recovery: a bundle is restored
  on machines that hold no copy of the passphrase, which is what makes it safe to
  keep anywhere. Write it down outside Dylaris. `core` `panel`
- Downloading a bundle needs the settings WRITE permission, not read: it contains
  the whole database, and with it every node secret and stored credential.
  Downloads stream through Core rather than a pre-signed link, so no URL that
  opens the installation outlives the session. `core`

### Fixes
- Nothing.

## 2026.09.08

### Features
- Groundwork for platform-wide backups: Core can now assemble one bundle from a
  selected set of components and seal it under the backup passphrase. Nothing
  offers it yet. `core`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.09.07.14

### Features
- **Import a backup archive as a new sub-server.** Setup has a Backup tab: pick
  a `.tar.gz` you downloaded from Dylaris and its files are restored into a
  fresh sub-server. An archive written by 2026.09.07 or later also carries its
  own description, so the loader, versions and mod list are set for you instead
  of being retyped. `core` `node` `panel`
- An archive from an older Dylaris, or from somewhere else, still imports its
  files - you then set the start command and the loader yourself, which is what
  an archive that describes nothing can honestly offer. `core` `node` `panel`

### Breaking
- Nothing.

### Security
- An imported description can no longer name anything on the platform that
  wrote it. The pack reference inside an archive is a row id in the source
  installation's database, so it is dropped on import rather than pointed at
  whatever holds that id here. `core`

### Fixes
- Nothing.

## 2026.09.07.13

### Features
- Groundwork for platform-wide backups: Core can now re-encrypt every stored
  credential from one CLUSTER_SECRET to another, which both a bundle restore on
  a different instance and a secret rotation need. Nothing offers it yet. `core`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.09.07.12

### Features
- Groundwork for importing a backup archive into a new sub-server: the node can
  now unpack one and read the description it carries. Nothing offers it yet -
  the panel side follows - but the node has to understand it first, so deploy
  this node before the release that does. `node`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.09.07.11

### Features
- **A backup now describes itself.** Each archive carries what it contains -
  how each sub-server was installed and which mods were on it - written as its
  first entry, so a downloaded archive can be read on any Dylaris rather than
  only on the one that made it. `core` `node`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- **Restoring a backup now restores the mod list too.** Before, only the files
  rolled back: a mod installed after the backup stayed listed in the panel with
  no jar behind it, and a mod uninstalled after the backup came back as a
  running jar the panel knew nothing about. Archives made before this update
  carry no mod list, and a restore of one leaves the list untouched rather than
  guessing. `core` `node`

## 2026.09.07.10

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- **Backups to a saved storage connection work again.** Every one of them failed
  with "presigned upload not supported for this backend", including
  S3-compatible targets like R2 that can presign perfectly well. Backup rows
  pointing straight at S3, a shared path or node-local storage were never
  affected. `core`
- Settings, Backups is now called **Server Backups**, and says so: it covers the
  archives of game servers, not the platform's own database. `panel`

## 2026.09.07.9

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- **A modpack's `.mrpack` is no longer stored under a guessable path.** The Node
  downloads it over the unauthenticated Solder mirror, and the old path started
  with the pack owner's user id, which the public Solder API prints in every mod
  download URL. The directory is now derived from `CLUSTER_SECRET`. Builds you
  have already published keep working; nothing to migrate. `core`

### Fixes
- Nothing.

## 2026.09.07.8

### Features
- **Solder is addressed per account.** Each user claims a handle under Account,
  Solder Keys and gets their own URL, `/solder/u/{handle}/api/`. Their pack list
  contains only their packs, and a key only verifies on its owner's address.
  `core` `panel`
- **Pack slugs are unique per account instead of platform-wide.** Two customers
  can both call a pack `skyfactory`; before, the first one to take a name held
  it for everybody. `core`

### Breaking
- **The shared `/solder/api/` is retired** and answers with the new address
  instead. Anyone who linked a Solder there has to enter their own URL on the
  Technic Platform again. A bare slug on a shared URL has no single answer once
  slugs are per account, so the two changes are one. `core`
- **A Solder handle is set once and cannot be changed.** The Technic Platform
  stores the address inside every linked modpack and a launcher keeps it in the
  installed copy, so a rename would silently stop each of them updating. `core`

### Security
- Nothing.

### Fixes
- Renaming a pack's Solder slug onto one that is taken answers 409 with the same
  message creating one does, instead of a flat server error. `core`

## 2026.09.07.7

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- **Solder Keys and Solder Clients are now in the account menu.** Both pages
  existed and neither was reachable: the keys page was linked from nowhere at
  all, so the key that links a Solder to the Technic Platform could only be
  found by typing the URL. `core` `panel`

## 2026.09.07.6

### Features
- **A Solder API key issued by the Technic Platform can now be entered under
  Account, Solder Keys.** Linking a self-hosted Solder to technicpack.net was
  impossible before: Technic issues the key on your Technic profile and verifies
  that exact value, while Core could only mint one of its own that Technic has
  never seen. Leave the field empty for a launcher key, which still works as
  before. `core` `panel`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- A key already registered here is refused with a clear message instead of a
  server error. `core` `panel`

## 2026.09.07.5

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- **The Solder API root answered only with a trailing slash.** `/solder/api`
  fell through to the panel and returned HTML, so the Technic Platform reported
  an invalid Solder URL for a Solder that was working one character away. Both
  spellings now answer the same JSON. The URL to enter at technicpack.net is
  `https://<your panel>/solder/api/`. `core`
- **Solder key verification now returns `created_at`**, which upstream
  TechnicSolder has always sent alongside `valid` and `name`. It was missing,
  and a platform that reads the field would have rejected a valid key. `core`

## 2026.09.07.4

### Features
- Nothing.

### Breaking
- **Route-only locations no longer bring backup storage.** The included
  allowance is per purchased NODE now. A route-only location routes traffic to a
  server the customer runs themselves, so there is nothing of theirs here to
  back up - and a customer holding one of each was given twice what their node
  includes. Traffic allowances are unchanged and still count both. `core`
  `panel`
- **A customer holding only route-only locations falls through to the allowance
  under Settings, Backups**, like any owner without a node, instead of getting a
  node's worth of storage. Unset there still means no limit. `core`

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.09.07.3

### Features
- **The backup allowance for users without a subscription moved to Settings,
  Backups.** It used to sit under Settings, Billing as "Flat quota per tenant" -
  a screen that is hidden unless the hosted store is linked, so on a self-hosted
  install the one control over every user's backup storage was on a page nobody
  could open, while the guard reading it ran on every backup. `core` `panel`

### Breaking
- **The flat quota field is gone from Settings, Billing and its value moves on
  first start.** A stored `0` is NOT carried across: the old screen wrote one by
  itself when saved, so it says more about that defect than about anyone's
  policy. If you meant a cap of none, set it again under Settings, Backups.
  `core`

### Security
- Nothing.

### Fixes
- **Administrators are no longer subject to the backup allowance.** The operator
  of a hosted install had to sell themselves a subscription before their own
  servers could be backed up. The per-server node-local cap still applies to
  everyone, because it bounds a real disk. `core`
- **A backup to storage the user connected themselves no longer counts against
  the platform allowance.** Those bytes were already excluded from the usage
  figure and from retention; only the gate still charged for them, so a customer
  with their own bucket could pass the ceiling once and never back up again.
  `core`
- **The Usage screen judged backup storage by a different number than the guard
  did.** It read the per-user override alone, so a customer on their purchase's
  allowance was never shown as over while their backups were being refused.
  `core` `panel`

## 2026.09.07.2

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- **A backup storage limit of 0 GB refused every backup with an unfollowable
  message.** The refusal read "0 / 0 GB used - delete old backups", which
  neither names the cause nor can be acted on. It now says the allowance is
  zero and where to change it, and usage below a gigabyte is no longer rounded
  to 0. `core`
- **The user override screen called a platform quota of 0 GB "unlimited".** The
  API reported an unset platform limit as `0`, and the panel labelled that as no
  cap - the opposite of what the platform enforces, which is a cap of NONE.
  Check Settings, Billing if backups are being refused: a `0` stored there
  before this caps every tenant who holds no entitlement. `core` `panel`

## 2026.09.07

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- **Backups to S3-compatible storage failed with a checksum error.** The AWS SDK
  now attaches a CRC32 to every upload, and for a body whose size is not known
  in advance it sends that checksum in a trailer some providers do not
  implement, which answered `BadDigest ... Actual CRC32 was: 00000000`. Seen on
  Cloudflare R2. Backups and Core file storage share the client, so both are
  fixed. `core`
- The self-hosting guide described a separate `panel` container pulling
  `platform-panel`. That image does not exist: Core builds the panel into its
  own image and serves it on the API port. The guide now deploys four
  containers, not five. `core`
- **A backup target's prefix was collected, shown back, and then ignored.** An
  account's own S3 backup storage wrote every archive to the bucket root while
  the panel displayed it as `bucket/prefix`. If one of yours has a prefix set,
  new archives now land under it - the ones already in the root stay where they
  are and are no longer listed, so move them if you want them kept. `core`

## 2026.09.06.3

### Features
- The overlay key of a machine, and the key of a protected address, can now be
  replaced from the panel without taking anything apart. The machine keeps its
  name, its address and its servers; only the key in your compose file changes,
  and the old one stops opening new connections at once. `core` `panel`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- **The deploy snippet named images that do not exist.** After the move to the
  new registry the templates kept the old middle segment, so the compose file
  the panel handed out said `dylaris-dev/dylaris-gateway-warp` instead of
  `dylaris-dev/gateway-warp`, for warp, link and the node agent alike. Copy a
  fresh snippet. `core` `panel`
- An enrollment key that had already been used, or had expired, stayed in the
  list of keys waiting to be used. Adding one machine therefore looked like two
  entries to delete, next to the overlay key of that same machine. The list now
  shows exactly what counts against your plan. `core` `panel`

## 2026.09.06.2

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- **A Minecraft server created before the registry move kept starting from the
  old image address.** The stored runtime image was rewritten in the database,
  but a container never reads that column again: a restart reuses the previous
  container's image and a manually deleted one is rebuilt from the node's saved
  config. Both now rewrite the old address, so the server keeps starting once
  the local copy of the deleted image is gone. `node`

## 2026.09.06

### Features
- Nothing.

### Breaking
- **Every image moved to a new registry path.** The project now lives in the
  `dylaris-dev` organisation, so `ghcr.io/bartis-dev/dylaris-platform-*` is now
  `ghcr.io/dylaris-dev/platform-*` and `ghcr.io/bartis-dev/dylaris-gateway-*` is
  now `ghcr.io/dylaris-dev/gateway-*`. Update your compose or stack files before
  your next deploy; the old paths are not redirected. `core` `node` `panel`
- The stored Minecraft runtime image of every existing server is rewritten
  automatically on first boot after this update, so servers created before the
  move keep starting. Nothing to do, but the count appears in the Core log. `core`

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.09.04.6

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- A ticket is now filed under the region of the server it names, instead of
  always under `default`. The support inbox shows a region badge per ticket and
  offers a region filter once you run more than one region, both reading that
  field, so on a multi-region install every ticket looked like `default` and
  picking a real region emptied the list. Tickets opened before this update keep
  their old value. `core`
- The ticket API no longer accepts a `serverRegion` from the client. It is taken
  from the server row the request was already authorised against, so a ticket
  cannot claim a region its server does not have. `core`

## 2026.09.04.5

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- A node's self-reported region is now checked against your configured regions
  before it is stored, the same rule that already applied when an admin sets one
  by hand. A typo in `DYLARIS_REGION` used to be written straight through, which
  quietly misdirected Beam transfers, hid the node's servers from regional staff
  and let a region be deleted while servers still ran in it. An unknown value is
  reported once on the Errors screen instead. `core`
- Region access checks now ignore case and surrounding whitespace, matching how
  every other region comparison in the platform already worked. A region stored
  as `EU` no longer hides its servers from staff granted `eu`. `core`

## 2026.09.04.4

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Failures inside the shared-storage verifier now reach the Errors screen instead
  of the log alone - including the one that stopped a storage fault from being
  recorded at all, which used to hide every other fault with it. `core`
- A backup whose upload failed left the half-written archive behind, and nothing
  ever removed it: retention only prunes successful runs. On node-local storage it
  counted against the per-server backup quota and showed as used space while
  appearing in no backup list. It is now cleaned up. `node`

## 2026.09.04.2

### Features
- An account's email address can now be seen and changed under Users. Neither
  was possible before: the screen showed it nowhere and no endpoint could write
  it. The new address is stored unverified, and a verification goes out when the
  policy requires one. `core` `panel`

### Breaking
- Deleting servers is now ADMIN-ONLY. A non-admin who was granted the
  "can delete servers" flag loses it, including for their own servers - the
  stored flag is no longer read. Grant an admin role if someone must delete.
  `core` `panel`

### Security
- Nothing.

### Fixes
- The Region Access section no longer renders as a heading over empty space with
  a Save button under it. The picker hides itself when only one region exists;
  now the section around it follows. `panel`
- Region access can no longer be saved when the account's current access failed
  to load. The form falls back to "all regions", and saving that would have
  granted more than anyone chose. `panel`

## 2026.09.04

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Error streams from components that are gone now age out after 30 days instead
  of living forever. Each redeploy started a new stream under a new instance id
  and abandoned the old one, so Infrastructure, Errors was scanning and reading
  an ever-growing set - 88 streams here against a handful of live components.
  A stream still being written keeps its record. `core`

## 2026.09.03.15

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- A backup restore whose node goes offline is now shown as paused, with the
  reason. It stayed on "queued" with a pulsing spinner forever, because only the
  node's reply ever moves that row. Nothing is lost either way - the restore
  resumes when the node returns. `core` `panel`
- The backups screen no longer refreshes every five seconds forever behind such
  a restore. It treated the stuck row as work in progress and kept polling for
  as long as the tab was open. `panel`

## 2026.09.03.14

### Features
- Password-reset and registration mail failures now appear under Infrastructure,
  Errors. The reset endpoint reports success either way so that it cannot be
  used to tell a real address from an invented one, which left nobody able to
  see that mail was failing. `core` `panel`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- The authentication policy screen now says when no mail transport is
  configured. Every switch on it promises a message - verification links,
  password resets, inactivity warnings - and none of them were being sent.
  `core` `panel`
- The Tickets switch now warns when no enabled ticket category exists. A ticket
  must name one, so turning the module on without a category makes every ticket
  a customer submits fail. `core` `panel`

## 2026.09.03.13

### Features
- Core's own failures now reach Settings, Infrastructure, Errors. Its periodic
  jobs - dunning, the backup scheduler, the retention sweeps, the route
  republisher - reported only to the container log, which is discarded when Core
  is redeployed. `core`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.09.03.12

### Features
- The statistics record now covers the DDoS shield: whether it is loaded on each
  edge, the packets it passed, the packets it dropped by block or by rate limit,
  and how many addresses it is holding out. The edge has always published these
  and nothing had ever read them. `core` `panel`

### Breaking
- Nothing.

### Security
- Settings, Gateway, DDoS Protection now says when the shield is not loaded on
  an edge. Its only warning meant "no config saved yet", so one save made the
  page look settled while edges ran with no filter attached. An edge loads the
  shield when `XDP_ENABLED` is set in its deployment. `core` `panel`

### Fixes
- Nothing.

## 2026.09.03.11

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Node CPU, RAM and server count are read from the node's heartbeat, which the
  long-term recorder was not reading. Every machine was recorded at 0% CPU with
  0 servers, and node memory was never recorded at all. `core`
- A node that is online but has not reported now records no load figures rather
  than zeros, so "no measurement" stops looking like an idle machine. `core`

## 2026.09.03.10

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Gateway components publish their counters every 3 seconds as "events since my
  last message", and the recorder sampled one message every 30 seconds, so nine
  out of ten events were dropped. Player sessions, beam transfers and both
  handover figures now count every event. Earlier history stays low. `core`
- The bandwidth alert required every single reading in the window to be over the
  threshold, and a reading is a one-second sample, so one quiet second cleared
  it. It had never fired. It now asks whether the link was over the threshold
  for most of the window. `core`
- Warp rebalancing decides which hosts are hot from those same alerts, so it had
  never run either. `core`

## 2026.09.03.9

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- The Statistics headlines "Peak player throughput" and the per-player figure
  were recorded in bytes per second under a bits-per-second name, so both read
  an eighth of the truth while the edge charts beside them read it correctly.
  Corrected from this update on. `core`
- Statistics still spelled throughput "Mbps" while the rest of the panel had
  moved to "Mbit/s". Same numbers, one spelling. `core` `panel`
- Warp placement and rebalancing judged a host by its OUTBOUND traffic alone, so
  a host saturated inbound looked wide open and could still be handed new peers.
  Both directions now count, and the busier one decides. `core`
- The Gateway tab showed outbound throughput only. Each row now shows out and
  in, because a link carries its rated speed in each direction at the same time.
  `core` `panel`

## 2026.09.03.8

### Features
- Warp leaders and beam relays now record throughput as well as CPU and RAM, so
  the long-term record finally covers all three gateway components. It starts at
  this update; there is nothing to backfill. `core`
- Bandwidth is one row per host. Each service on it gets its own chart, framed
  at its cap, with outbound above the centre line and inbound below it, and a
  window switcher for 1 hour, 6 hours, 24 hours and 7 days. `core` `panel`
- Throughput is written as **Mbit/s** rather than Mbps everywhere. Mbps is read
  as megaBYTES often enough to matter on a screen used to judge headroom.
  `core` `panel`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Edge throughput in the long-term record was stored in BYTES per second under a
  bits-per-second name, so every long-term edge figure was an eighth of the
  truth. Corrected from this update on; anything recorded before it stays eight
  times too low. `core`
- Utilisation and the bandwidth alerts only ever measured OUTBOUND traffic. A
  component saturating its inbound direction read as idle and raised nothing -
  the likelier failure for a beam relay taking uploads. Both directions are now
  measured, and an alert says which one tripped. `core` `panel`
- The statistics catalogue offered beam traffic in and out, which nothing had
  ever recorded. Picking either drew an empty chart forever. `core`

## 2026.09.03.7

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- A Core that started before its statistics database was reachable stopped
  recording for good. During a stack deploy that is a matter of seconds, and it
  cost every statistic on the platform: recording runs on one Core, so that one
  starting first switched it off for the whole cluster. It now keeps trying, and
  says so in the log. `core`

## 2026.09.03.6

### Features
- On the Gateway tab, CPU and RAM each get their own box with a fixed 0-100%
  scale and the live value beside the chart. One chart carrying both lines made
  the reader match colours to tell two unrelated things apart. `core` `panel`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.09.03.5

### Features
- Warp leaders and beam relays now record CPU and RAM once a minute. The
  long-term record had this for edges only, so the two components without a
  screen had no history either. It starts from the update - there is nothing to
  backfill. `core`
- The Gateway tab draws a CPU and RAM graph per component, with one range
  switcher for the whole screen (15 min, 1 h, 6 h, 24 h). The rows are taller
  and the meters much wider. `core` `panel`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.09.03.4

### Features
- Infrastructure has a **Gateway** tab: edges, warp leaders and beam relays in
  one list, each with CPU, RAM and its own number - players, peers, transfers in
  flight. Warp leaders and beam relays had no screen at all before. `core` `panel`
- Bandwidth is now one row per host and one column per service, so services
  sharing a machine line up side by side. Every card carries a sparkline, and
  the range switcher offers 15 min, 1 h, 6 h and 24 h. `core` `panel`
- Routes moved from Infrastructure to Admin, next to Users. The old address
  redirects. `panel`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- The long-term statistics summary called a line **Edge restarts survived**. It
  counts restarts and does not measure whether anyone came through one, so it
  now reads **Edge restarts**. `core`

## 2026.09.03.3

### Features
- The statistics database form has a **This database has no password** tick box.
  A blank field means "keep the stored password", so it could not also say
  "there is none" - a password saved once had no way back off. `core` `panel`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.09.03.2

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- A database password that was EMPTY or contained a space could not connect.
  Connection strings were built unquoted, so an empty password consumed the
  field after it and the error named a database nobody had typed
  (`database "<user>" does not exist`); a space failed while quoting part of the
  password back at you. Affects the statistics database form and the
  cross-database migration screen. `core`

## 2026.09.03

### Features
- Nothing.

### Breaking
- `METRICS_DB_URL` is gone. Where the long-term statistics are written is set
  in the panel only, under Settings, Features, Long-term statistics, and stored
  in the Core database. `core`
- If you had that variable set, enter the same host, database, user and password
  in that card once and press Test connection - it is ignored now, and until
  then recording falls back to hour buckets in the Core database. `core`

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.09.02.21

### Features
- The long-term statistics switch and the database it records into are now one
  card under Settings, Features, saved by one action. They were two, and the
  switch could be turned on before anyone had chosen a target - which cannot be
  corrected later, because nothing is backfilled. `core` `panel`

### Breaking
- `/api/admin/settings/features` no longer carries `metrics`. The recording
  switch moved to `/api/admin/settings/metrics-db`, which writes it together
  with the target. The stored setting is unchanged, so nothing needs migrating -
  only a script that toggled statistics through the feature bundle. `core`

### Security
- Nothing.

### Fixes
- Turning recording on with a database that cannot be reached no longer half
  applies. The target is checked and stored first, so a refused one leaves the
  switch exactly where it was. `core`

## 2026.09.02.20

### Features
- Long-term statistics now choose their database in the panel, under Settings,
  Features. Either the Core database at hour resolution, or a separate
  TimescaleDB at minute resolution, with a connection test that reports what
  that target would actually give you before you commit to it. `core` `panel`
- Changing that target takes effect immediately. The recorder is swapped in
  place, finishing the pending flush on the way out, so no restart is needed and
  the minutes since the last write are not lost. `core` `panel`
- `METRICS_DB_URL` still wins where it is set: the form shows the stored target
  read-only and says which variable decides it, rather than silently accepting
  edits that would never be used. `core` `panel`

### Breaking
- Nothing.

### Security
- `golang.org/x/crypto` moved to v0.56.0. Two of the vulnerabilities it fixes
  were reachable from the node agent; the same version is now used everywhere
  this platform builds. `core` `node`

### Fixes
- No switch under Settings, Features could be saved. The card never registered
  as changed, so its save bar never appeared and nothing on it persisted -
  tickets, modpacks, BYON, auto-move, long-term statistics and user API keys
  alike. `core` `panel`

## 2026.09.02.19

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- The statistics counted customer machines in your own availability. `node.up`
  and the node and link totals now cover your cluster and external nodes only,
  so a tenant powering their box down at night cannot understate the uptime this
  platform delivered. `core` `panel`
- Customer nodes and links are recorded as their own counts instead - the
  hardware customers brought is worth measuring, it just cannot sit inside a
  figure about you. `core` `panel`

## 2026.09.02.18

### Features
- Infrastructure now always shows all three kinds of machine as their own tab -
  Nodes, External nodes and BYON - instead of hiding a tab until you have one of
  that kind. `core` `panel`
- BYON and route-only estates are counted on the BYON tab: how many nodes, links
  and warp peers your customers run, and how many are up. Counted only, never
  warned about. `core` `panel`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- What's New listed your customers' BYON nodes among your own, so "is anything
  of mine behind" was answered with hardware you cannot update. It now shows
  your cluster and, separately, your external nodes. `core` `panel`
- Settings, Status counted customer nodes and customer links, so a tenant
  switching their own machine off turned the platform amber. It reports the
  cluster and your external nodes only. `core` `panel`

## 2026.09.02.17

### Features
- The statistics now cover the gateway. Every edge restart is counted along with
  how many players were carried through it and how many were dropped, so the
  handover you rely on is measurable rather than assumed. `core` `panel`
- Warp reports its tunnels and the link reports its own, which had no telemetry
  at all until now - a customer's tunnel dropping and reconnecting was visible
  only in their own log. `core` `panel`
- Core, Postgres and Redis now record what they cost: memory, CPU, connections,
  query and cache rates, and the size of the database over time. `core` `panel`
- A Statistics tab under Infrastructure charts all of it over any period up to a
  year, with headline figures and a CSV or JSON export. `core` `panel`
- Concurrent panel users are counted for the first time, once per person however
  many tabs they have open. `core` `panel`
- None of this records anything until you switch it on under Settings, Features.
  It is off by default and history cannot be collected backwards, so the record
  starts the day you enable it. `core`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- The beam relay reported a live transfer count that was always zero: the field
  existed on both sides and nothing ever set it. `core`

## 2026.09.02.16

### Features
- Long-term statistics. Switch it on under Settings, Features and the platform
  starts recording what it handles - players, traffic, CPU and RAM per machine,
  and uptime per component - in buckets that survive rather than the 24 hours
  the live graphs keep. Off by default. `core` `panel`
- The record goes into this database at hour resolution by default, which needs
  no extension and costs a few hundred megabytes a year. Point `METRICS_DB_URL`
  at a TimescaleDB instance for minute resolution; `docker-stack.yml` carries a
  ready service to uncomment. `core`
- Nothing leaves the installation. This is a local record, and the platform
  still sends nothing anywhere. `core`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.09.02.15

### Features
- Infrastructure is now one address per tab. Every tab has its own URL, so a
  reload keeps you where you were and a screen can be sent to somebody. `panel`
- Machines are separated into Nodes, External and Customer nodes instead of one
  mixed list. The External and Customer tabs appear only when you have machines
  of that kind. `panel`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- A tab whose backend is missing now says so and offers the way back, instead of
  rendering an empty panel. Edges, Routes and Bandwidth are reachable by URL now,
  so each one checks for itself rather than relying on its button being hidden.
  `panel`

## 2026.09.02.14

### Features
- A tenant's metered traffic allowance can now be set on the tenant. Settings
  listed per-tenant overrides and offered no way to create one, so an exception
  for a single customer was reachable only through the API. `panel`
- The three warn-only traffic fields in the billing dialog say "warn only" now.
  They raise a banner on the tenant's usage page and are not the metered pools
  that are billed, which the new control beside them sets. `panel`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.09.02.13

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Deleting a customer now removes the nodes they brought, instead of turning
  their machine into a platform node. It used to keep its cached secret, its
  scoped Redis users and its heartbeat, and became eligible to receive other
  people's servers - the platform went on trusting hardware belonging to
  somebody who is no longer a customer. `core`

### Fixes
- Deleting an account that still owns servers no longer destroys anything first.
  The refusal came AFTER the cleanup, so the link kit, its Redis credentials and
  the protected addresses were already gone while the account survived. The
  check runs up front now and says how many servers are in the way. `core`

## 2026.09.02.12

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Automatic placement and rebalancing no longer put a server on a node that
  belongs to a different party. A tenant's own machines were already fenced off
  from everyone else, but a platform-owned server had no such rule and could be
  moved onto a customer's own hardware, which that customer has root on. Only
  reachable with BYON on; auto-move made it automatic. `core`
- Naming a node explicitly is unaffected, so an operator can still place
  anywhere deliberately. Only the pick nobody made is restricted, and when that
  leaves nothing to pick, the message now says how many customer nodes were
  passed over rather than reporting a capacity problem that is not there. `core`

### Fixes
- The deploy wizard's node preview and the actual placement now answer the same
  question. The preview is what tells an admin which machine a tag-based deploy
  will use, so the two being scoped differently would have named a node the
  deploy then refused. `core`

## 2026.09.02.11

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- The demo server's file browser now shows the content of `server.properties`
  (without its RCON password) and `eula.txt`, and nothing else. It used to show
  every file except those two, so anyone with an account could read a plugin's
  database credentials or a proxy forwarding secret off it. `core`
- Removing a ticket watcher no longer accepts a removal that matched nothing.
  Any account could write an audit entry into a stranger's ticket, and count
  upwards to learn which ticket numbers exist. `core`

### Fixes
- Failing to remove a ticket watcher now says so and refreshes the list. It used
  to leave the watcher on screen with nothing said. `core` `panel`

## 2026.09.02.10

### Features
- Jars placed by hand - by SFTP, the file manager or the desktop client - are
  now listed on the Content tab and can be identified against Modrinth from
  there. Identified ones become ordinary entries with update checks. This only
  existed on the Minecraft-version screen before. `core` `panel`
- A file that identify cannot link now shows the reason, and can be deleted
  from the list. That is how the duplicate jars left by the pre-fix update path
  are cleared: they resolve to a project that is already installed under
  another name, and Core says so. `core` `panel`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- The unknown-jar check reported a server whose node could not be reached as a
  server with nothing out of place. Core already distinguished the two; the
  panel flattened them. `core` `panel`

## 2026.09.02.9

### Features
- Roll a mod or plugin back to an earlier build. The three most recent builds a
  server actually ran are kept per mod, and going back uses the same install
  path, so it is verified and the current jar is replaced rather than joined.
  `core` `panel`
- Update all installed mods in one run, with a per-mod summary at the end that
  names what failed and what could not be checked, not only what worked.
  `core` `panel`
- Roll every mod back to the build it had before its last update, for when a
  bulk update turns out to be the problem. `core` `panel`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.09.02.8

### Features
- The build list of a mod now marks the build that is on the server, and says
  when one is still installing or failed to install. `core` `panel`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Updating a mod or plugin left the old jar in place beside the new one, so the
  server loaded both builds. The new file is downloaded and verified first, then
  swapped in, and only then is the old one deleted - a failed download now
  leaves the working build alone. `core` `node`
- An install that never landed was listed as installed. The node reports the
  outcome and a failed one says so, with the reason. Update the node to get
  this; an older node still installs correctly and is recorded the way it was
  before. `core` `node`

## 2026.09.02.7

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- The Link status panel said "No node is reporting its Link status yet" when it
  had failed to read the status at all. It now tells the two apart and keeps
  the last answer it got. `core` `panel`
- A server's Content tab said "No mods installed" when the installed list could
  not be loaded, which also cleared the installed badge from every browse
  result. `core` `panel`

## 2026.09.02.6

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- A failed request no longer reads as an ordinary state in the custom-domain
  panel. It used to hide itself entirely when the list could not be loaded,
  report a failed TXT check in success green, and do nothing visible when the
  unblock button failed. `core` `panel`
- A custom tab whose list request failed said "Tab not found. It may have been
  deleted." The three screens that read that list now tell a failure apart from
  a server that has no tabs. `core` `panel`

## 2026.09.02.5

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- The Content tab scrolls in three panes now - categories, results and the mod
  details each scroll on their own instead of moving the whole window. The
  players list does the same. `core` `panel`
- A server's custom tabs rendered their page in a frame 150 pixels tall
  regardless of the window. The frame now fills the tab. `core` `panel`

## 2026.09.02.4

### Features
- The Modrinth panel now has an "Open on Modrinth" button, in place of the small
  text link that sat next to the download count. `core` `panel`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- A mod's full description showed raw HTML markup instead of the badges,
  screenshots and tables it is written with. It now renders, sanitized, and the
  panel's image policy accepts any https host because mod authors host their
  screenshots wherever they like. `core` `panel`

## 2026.09.02.3

### Features
- Nothing.

### Breaking
- SFTP and the beam client now enforce the file capabilities per operation, as
  the panel's file API already did. An account whose role grants writing but not
  deleting can no longer delete through either. `core` `node`

### Security
- A member invited as a Builder could remove any file on the server over SFTP or
  through the beam client, while the same account was refused that over HTTP.
  Both surfaces asked only whether a session was allowed at all. `core` `node`

### Fixes
- Nothing.

## 2026.09.02.2

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Removing an account now removes what the account ran. The automatic
  inactive-account sweep deleted the row and left the tenant's route-only link
  credential, its tunnel key and its protected addresses in place, and nothing
  could ever find them again once the row was gone. `core`

### Fixes
- An account whose teardown fails is left in place instead of being removed
  anyway, so the record of who owned a live credential survives to the next
  attempt. `core`

## 2026.09.02

### Features
- Nothing.

### Breaking
- Two gateway settings are gone: the per-server address cap and the number
  beside the Minecraft port. Both were stored, shown back, and enforced by
  nothing. The port's on/off switch stays and still works. `core` `panel`

### Security
- Nothing.

### Fixes
- A comped account no longer gets unlimited backup storage. The included
  allowance is per unit held, a grant counted as none, and the resolution fell
  through to the platform-wide setting - which is unset by default and means no
  cap. A live grant is now worth one unit, so it brings the same allowance one
  purchase does. `core`

## 2026.09.01.14

### Features
- Nothing.

### Breaking
- The over-limit cutoff now takes effect. A tenant past the 72-hour grace loses
  their route-only links as well as their servers, and the change is kept: both
  admission gates and both reconciler queries ask about it. Until now the cutoff
  reversed itself within a minute and did nothing at all to a route-only tenant.
  `core`

### Security
- Nothing.

### Fixes
- Reactivating a tenant no longer hands the links back while they are still over
  their limits, which used to issue a working credential for one minute. `core`
- A lapsed entitlement on one product is now noticed while another is still
  active. The over-limit sweep asked whether a tenant was entitled to anything
  at all, so an expired grant on one product left what it had granted running
  for as long as anything else was paid for. `core`

## 2026.09.01.13

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Saving the gateway settings with the per-user address default left blank
  switched off the limit below it, so every tenant became uncapped while the
  page still showed a number. A blank field now clears that scope instead of
  answering for it. `core` `panel`
- A route limit of 0 no longer survives a restart as a broken value. A startup
  migration from an older convention rewrote it on every boot, and the result
  read as a cap that nobody could ever be under. `core`

### Fixes
- The two per-user address limits say what they do. The first was labelled as a
  total across all users and servers, which is not what it caps. `panel`

## 2026.09.01.12

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- The system event stream sent every account's identifier to every signed-in
  session. A frame naming an account now reaches that account and admins only.
  `core`

### Fixes
- Nothing.

## 2026.09.01.11

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- A node's Redis credential could write the daily upload counter of any account
  on the platform, because that key is named after a USER and the grant was
  fleet-wide. A node now holds only the counters of the users it actually serves.
  `core`

### Fixes
- Nothing.

## 2026.09.01.10

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- A node could write any server's migration progress, not just its own. The key
  now carries the reporting node's token, so Redis refuses a cross-node write
  and Core reads the key of the node it is waiting on. `core` `node`
- SCAN is withdrawn from the log-shipper, the link sidecar and route-only links.
  Redis does not filter SCAN by key permissions, so it returned every key name
  on the platform to a credential that lives in a tenant's own container. Only
  the node agent keeps it, because it genuinely walks the keyspace. `core`
- A route-only link's error stream is named by its link ID rather than by eight
  characters of its authentication token. `core`

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
- The data-traffic panel is shown even when nothing has moved through the relay
  this month. It only appeared once a row existed, so "you have used none" and
  "this is broken" looked identical. `core` `panel`
- A tenant entitled by an admin grant was told "there is no active
  subscription", which reads as broken access. A panel grant creates no store
  subscription by design, so the panel now says that is why - and that nothing
  is charged or stopped for going over. `core` `panel`

## 2026.09.01.8

### Features
- Traffic usage is now two panels - player traffic at the edge and data traffic
  through beam - and each pool shows how much of it came from a tenant's own
  node versus their protected addresses. `core` `panel`
- Metered-traffic consent is switched beside the usage bars on My
  Infrastructure instead of on the store page, where the person watching an
  allowance fill up could not reach it. `panel`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- A tenant with a one-node allowance could never add their first machine. A BYON
  machine needs two credentials and both counted against `max_nodes`, so the
  overlay key filled the allowance and the enroll token seconds later was
  refused. The unit of the cap is a machine now. `core`
- The same double count would have suspended a tenant after 72 hours for holding
  exactly the one machine they bought, and the panel's "used of limit" counted a
  third thing again. Both read the machine count now. `core` `panel`
- A traffic pool with no configured allowance is shown with its usage instead of
  being hidden, so transferred bytes nobody has set a limit for are visible.
  `panel`

## 2026.09.01.7

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- A manual grant wrote its quantity into the same column a Stripe purchase
  writes, so a granted tenant read as a paying one and kept the product for good
  once the grant expired. A grant is worth one machine of its kind now, derived
  rather than stored, and the quantity box is gone with it. `core`
- Buying BYON or route-only ends the manual grant it covers, so the account runs
  as a normal subscription from that moment. A cancellation does not end a
  grant. `core`
- Setting a node up from inside the Beam app is withdrawn, along with its button
  on My Infrastructure. The deploy snippet on that page is unchanged and is the
  way to do it. `core`
- Beam's settings button keeps its place when the window is resized. It
  remembered a pixel offset, so dragging it to the right edge of a small window
  left it in the middle of a large one. Update Beam to 0.9.8.

## 2026.09.01.6

### Features
- A tenant can remove their own machine. Two steps, and the second one names
  every server and sub-server it would delete; the servers go only if asked.
  `core`
- The deploy snippet for a bring-your-own node now has a Windows option, like
  route-only already had. `core`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- At the node limit the panel only offered the store, so a tenant who wanted to
  MOVE their machine was told to buy a second one. It now points at removing the
  machine holding the slot. `core`
- The deploy snippet no longer runs to full height, and the two columns on the
  infrastructure page line up again. `core`

## 2026.09.01.5

### Features
- A tenant whose own machine stops answering now sees it on every page, not only
  on My Infrastructure. It waits out a restart before speaking and says nothing
  while everything is connected. `core`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.09.01.4

### Features
- The grant rows under a user's billing now show how many days are left, not
  only the date they run out. `core`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- A user's BYON and route-only grants always read "Not granted", even with a
  live grant, which also hid the Extend and Remove buttons on those rows. The
  grant itself was working the whole time. `core`

## 2026.09.01.3

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Beam 0.9.5 could freeze on a white window. Its settings button re-attaches
  itself when the page removes it, and that retry had no ceiling, so a page that
  kept removing it locked the window up. Update Beam to 0.9.6.
- Beam wrote its settings file to disk on every proxied response, which put file
  access in front of every script and image the panel loads. It only happens on
  sign-in and sign-out now. Update Beam to 0.9.6.

## 2026.09.01.2

### Features
- Setting a node up on this machine has moved out of Beam's settings and onto My
  Infrastructure, beside the rest of your machines. Beam still runs the checks
  and writes the file. `core`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- "Your machines" showed an admin every tenant's BYON machine rather than their
  own. It is scoped to the signed-in account now; the full fleet is still on the
  node list. `core`
- Beam asked for a sign-in on every launch. It held the session in memory only,
  so closing the app threw it away. Update Beam to get this.
- Beam's settings button could vanish for the rest of a session and started in
  the bottom left, under the sidebar. It defaults to the bottom right now and
  puts itself back. Update Beam to get this.

## 2026.09.01

### Features
- Granting BYON or route-only now takes a quantity as well as a duration, so a
  grant no longer hands out access with no ceiling behind it. `core`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- A node managed the Minecraft containers of OTHER nodes on the same machine.
  Every node drives the host's Docker socket and nothing distinguished whose
  container was whose, so a second node's startup restarted the first node's
  running servers and both published stats for the same ones. Containers are
  labelled with the node that created them now. `node`
- The file manager's transfer limits could express neither "no limit" nor "none":
  a stored 0 fell through to the built-in default, so the one value meaning
  nobody may upload granted the default allowance instead. `core`
- A user created by assigning an orphaned server got no region access, which the
  server list reads as "no regions" rather than "not decided". `core`

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
- Beam's panel settings are one list now. The panel this build is for is always
  the first entry and cannot be removed or repointed; the ones you add sit under
  it with Edit and Remove, and the form at the top names them.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.08.31.17

### Features
- BYON and route-only can now be granted to a user separately, each with its own
  duration. Granting one used to replace the other, so holding both meant picking
  "Both" up front and giving them the same deadline. `core`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Deleting a route from the admin Routes screen reported success and left the
  address routing. Route-only addresses are Core's own, and the delete only told
  the gateway - so the stored row was written straight back a minute later.
  `core`
- A customer could not see their own servers. Region access is a staff filter and
  it was applied to owners too, so an account with no regions assigned - the
  default for a self-registered one - got an empty list while an admin saw the
  server running. `core`
- The sidebar divider now lines up with the sidebar edge in both the expanded and
  the collapsed state, and runs the full height of the bar. `core`
- Beam no longer shows a button offering to download Beam. `core`

## 2026.08.31.16

### Features
- Settings now explain themselves. The screens whose right answer is not on the
  label - overcommit, maintenance block levels, the database migration, backup
  targets, route allocation, regions - grew a help icon with the reasoning and
  the consequence behind it. `core`
- The node card's "Deploy bundle" is now "Show setup values" and says what it is
  for: getting a node's connection values back when you rebuild the host or lose
  the compose file. `core`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- The per-user limits screen described its own numbers backwards. It said 0 meant
  unlimited when 0 has always meant none, so an admin setting a tenant to
  "unlimited" gave them zero. The stored values were never wrong - only the text
  was - so nothing needs correcting beyond re-reading limits you set from that
  screen. `core`
- The per-user backup quota carried the same inverted description, and a test now
  fails the build if either wording comes back. `core`

## 2026.08.31.15

### Features
- The Dylaris Store page shows the subscription itself: which units, how much of
  the traffic and backup allowance is gone, and two switches deciding whether
  running out bills you or stops you. `core`
- The Beam desktop app can hold several panels and switch between them, each
  keeping its own sign-in, and its settings are reachable from a small button you
  can drag along the bottom of the window.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.08.31.14

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- What's New shows only what you have not read yet, as a summary you can expand,
  instead of every release it was sent. A current operator no longer opens it to
  a span reaching back days. `core`
- The red "mandatory" badge now means a component you run is below a required
  version, not that some release in the window once carried a deadline. `core`
- Buttons on the backup-storage page, the Solder pages and a modpack page render
  correctly. They named a style modifier without the base button class, so they
  had no padding and no rounded corners. `core`
- At rail width the sidebar shows the product icon instead of a lone letter, and
  Create becomes a single plus. `core`

## 2026.08.31.13

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- The Configure and Placement buttons on a node stay put and light up when their
  editor is open, instead of disappearing. Clicking the lit one closes what it
  opened. `core`

## 2026.08.31.12

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- The DNS check no longer reports a missing "API domain". Core serves the panel
  and the API together, so the browser calls /api on the panel's own domain and
  there is nothing to create; the row said no API address was set and pointed at
  a modpack setting that does not decide it. `core`
- Signing in to the Beam desktop app no longer bounces straight back to the
  login screen. The app now holds the panel session itself instead of relying on
  the embedded browser to store it.
- Beam's "cannot reach the panel" screen retries on its own and offers the app's
  settings, so a panel that is briefly down recovers without restarting the app.

## 2026.08.31.11

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- A node Core has no record of was told to re-pair with a recovery token, which
  is minted per node and cannot be issued for one that is not listed - a dead
  end. It now says to clear the cached identity on the node instead. `core`

## 2026.08.31.10

### Features
- Who sees the Custom Tabs entry in the navigation bar is now set under
  Settings -> Features, beside the rest of the custom-tab settings. The Modules
  screen shows the value but no longer lets you edit it there, because that row
  follows this one. `core`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.08.31.9

### Features
- The Beam desktop app can be downloaded from the navigation bar. It used to be
  reachable only from a server's Files page - the one screen that needs Beam to
  be useful in the first place. `core`
- The Custom Tabs module can no longer be deleted from Settings -> Modules. It is
  seeded by Core and re-created on every start, so deleting it only stranded the
  tabs that already existed. `core`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- A node whose identity Core no longer knew was enrolled again as a brand-new
  node every thirty seconds. The node refused to adopt the new identity, so it
  stayed down while the node list filled with rows that were never used. Core
  now refuses and names the recovery path instead. `core`
- The panel no longer compresses below 1100px; the page scrolls sideways
  instead. Narrower than that, navigation entries folded away and dense tables
  stopped being readable. `core`

## 2026.08.31.8

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Navigation entries past the first collapsed into the "More" menu at window
  widths where they fit, and once collapsed only a page reload brought them
  back. `core`

## 2026.08.31.7

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- A server that crashed while any of its files were still owned by root would be
  restarted into the same error and then stay down. The node now repairs
  ownership before EVERY start, not only when it creates the container. `node`

## 2026.08.31.6

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Minecraft servers no longer run as root. A plugin or mod now runs as an
  unprivileged user that can write its own world and nothing else - not the files
  the node keeps beside it, and not the host if it ever escapes the container.
  `node`
- The node hands each world to that user on the next start, so nothing is needed
  from you. Set `MC_RUN_AS=0` on the node to keep the old behaviour, or another
  uid if your server data is already owned by one. `node`

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
- Every page for ONE thing was blank: a server, a modpack, a module, a ticket, a
  proxied tab, a share link. They all reported "not found" while the list pages
  and the API were fine. `core`
- Creating the first admin left you at the login screen instead of signing you
  in. `core`
- Client-side navigation into any of those pages did a full page reload, because
  the data Next fetches for a soft navigation was not being served. `core`

## 2026.08.31.4

### Features
- Nothing.

### Breaking
- `api.dylaris.com` and any second hostname for the API are retired. Core serves
  the panel and `/api` on one host; point your reverse proxy there and drop the
  other rule. The build-time `NEXT_PUBLIC_API_URL` is gone with it. `core`

### Security
- Nothing.

### Fixes
- The Beam desktop app was refused on every save, upload and power action. It
  proxies the panel onto its own address, and the cross-site check did not
  recognise that address as the panel's. `core`

## 2026.08.31.3

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- The Beam desktop app could not authenticate its native side, so the file
  browser failed to open a transfer. It sent the panel an address the app is
  built to reject, and the refusal was silent. `core`

## 2026.08.31.2

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- The 2026.08.31 Core image was built WITHOUT the panel in it: every page
  answered with a placeholder saying so, while the API worked normally. Pull
  this release and the panel is there. `core`
- Proxied custom tabs stopped opening in 2026.08.31: the panel could no longer
  prove who it was to the tab's own hostname, so every private tab showed the
  "open it from the panel" card instead of its content. `core`
- `PANEL_API_URL` was documented but never reached Core from the bundled compose
  and stack files, so setting it did nothing. It is passed through now, and Core
  warns when it names a different origin than `FRONTEND_URL` - a browser cannot
  carry the session across that split. `core`

## 2026.08.31

**Update required now** - the panel is no longer a separate image.

### Features
- The panel is served by Core. It is compiled into the binary as a static
  bundle, so there is one image, one port and one version instead of two, and
  the API is on the same origin as the pages by construction. `core`
- The session is an HttpOnly cookie and the panel no longer stores a token
  anywhere. A script that runs on the page can no longer read the credential and
  carry it off; download links stop putting a JWT in the URL. The Authorization
  header keeps working for API keys and anything driving this API
  programmatically. `core`
- Backups on our storage are now kept for one week after suspension rather than
  three months. Backups on a bucket a tenant connected themselves are never
  deleted at any deadline. `core`

### Breaking
- The `panel` image and service are GONE. Remove the `panel` service from your
  compose or stack file and point your reverse proxy at Core on `:25500`; it
  serves the pages and `/api` together. Nothing else to configure - no path
  rule, no CORS. `core`
- `PANEL_API_URL` should now be EMPTY. Core serves the panel, so the API is
  same-origin and is found without being told. Set it only if you terminate the
  API on a second hostname; Core then renders it into both the page config and
  the CSP. `core`
- `FRONTEND_URL` is Core's own public URL now, not a separate panel host. It
  still drives email links and the tab-proxy same-site check. `core`
- `TAB_PROXY_HOST_SUFFIX` is set on Core ONLY. It used to be needed on the panel
  container as well, because the panel wrote its own CSP - and nothing compared
  the two values, so a mismatch broke every proxied tab silently. `core`

### Security
- Nothing.

### Fixes
- A local `docker build` no longer bakes a developer's `.env` into the image.
  The ignore pattern matched the repository root only, so `panel/.env` was in
  the build context and Next inlined its `NEXT_PUBLIC_*` values. CI never saw
  it, which is why it went unnoticed. `core`

## 2026.08.30.5

### Features
- Traffic allowances are now set per region for player traffic, and once for file
  transfers, under Settings, Traffic limits. The two are counted apart, so a
  tenant past one is not past the other. `core` `panel`
- Changing the Java version or the JVM flags no longer reinstalls the server. It
  used to run the installer over a live server directory because that was the
  only path that could rebuild a start command. `core` `node` `panel`
- The setup tab remembers how each sub-server was installed, the exact modpack
  included, and a version or modpack change now asks what to clear first with the
  usual answers ticked. Worlds are never on that list. `core` `node` `panel`
- Settings, Billing now carries the included and bookable backup storage per
  purchased unit. Lowering the bookable figure notifies every tenant who agreed
  to be charged for storage, since it moves the ceiling they agreed to.
  `core` `panel`
- Users can connect an S3 bucket of their own for backups, under Account, Backup
  storage. It needs the new `backupstorage.*` capabilities, which no preset
  grants: connecting arbitrary endpoints makes Core talk to hosts you did not
  choose. `core` `panel`

### Breaking
- Backup storage now scales with the purchase: 50 GB per unit held, editable
  under Settings, Billing. It was one figure for the whole account, so a tenant
  with three units got three times the addresses and traffic and one times the
  backup space. `core` `panel`
- Traffic allowances have no account-wide fallback any more. Until something is
  set under Settings, Traffic limits, nothing is capped and nothing is billed.
  The screen says so when it is empty. `core`
- The "Platform default" traffic scope is gone. Its rows were moved to the tenant
  default on upgrade, and writing to it is now refused. `core`

### Security
- Nothing.

### Fixes
- Backup archives now record which storage they went to. Changing a job's storage
  used to make every earlier archive unrestorable, undownloadable and
  undeletable while the panel still listed it as present. `core`
- Saving the Billing screen no longer caps every tenant's backups at zero. An
  unset R2 quota read back as "0", which the next save stored as a real cap of
  none, while the help text beside it called that "no cap". `core` `panel`
- Metered traffic consent reached Core again. It stopped being recorded when the
  ceiling left the provision payload: the receiving side required BOTH, so a
  tenant who switched metered billing on stayed recorded as having refused it
  and was stopped instead of billed. `core`
- The module bar no longer loses entries for good when the window is narrowed.
  They now come back when it is widened, and its overflow menu opens instead of
  being clipped out of sight. `panel`
- A domain route added during setup appears straight away. It used to read as
  already taken until the routes dialog was opened or the page reloaded. `panel`
- The Java version is now recognised from a selected modpack, which previously
  left whatever was chosen last in place. `panel`

## 2026.08.30.4

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Deleting a user now removes their protected addresses. Those rows had no
  constraint tying them to the account, and the republisher wrote every stored
  row back into Redis each minute, so a deleted user's address kept routing
  players indefinitely - and came back within a minute if the key was cleared
  by hand. `core`

### Fixes
- A region that starts carrying traffic on a server already being metered is now
  counted. Its first reading was mistaken for history and skipped, so the
  per-region breakdown fell permanently short of the total it breaks down.
  `core`
- Setting one half of a traffic limit to "default" is refused instead of stored
  as "no limit". An override meant to cap purchases alone silently granted
  unlimited included traffic. `core`
- Region and kind are validated when a limit is written. A misspelt one saved
  fine and limited nothing, which looks identical to a working limit. `core`
- The Errors tab no longer leaves an empty panel when the last error ages out
  while it is open. `panel`

## 2026.08.30.3

### Features
- Settings has a Traffic limits tab: the included allowance and the purchase cap
  per region, for player traffic and for file transfers separately. It lists
  every region an edge reports, including the ones nothing limits yet, because
  "no row" and "no limit" look the same from the database and only one of them
  is something an operator meant. `panel`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.08.30.2

### Features
- Traffic allowances can be set per region and per kind, with a purchase cap
  beside each one, and overridden per user. A cap of zero is a region where
  extra traffic cannot be bought at all, which is a different thing from no cap
  and is now expressible. `core`
- `/store/usage` returns the per-region breakdown with the allowance that
  applies to each cell, so the store can judge a ceiling per region instead of
  against one summed number. `core`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.08.30

### Features
- The Infrastructure view has an Errors tab. Every service already wrote its
  diagnostics to Redis and Core already sent them with the overview, and the
  panel dropped the field - so six components reported into nothing. Only
  errors and warnings drive the tab's count. `panel`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- The two columns on "My infrastructure" have a floor and stack when the window
  cannot hold both, instead of squeezing the compose file into a column too
  narrow to read it in. `panel`
- The scrollbar on that page sits at the edge of the content area again rather
  than floating mid-screen next to the centred column. `panel`

## 2026.08.29.15

### Features
- Traffic is now recorded per region and per kind, not just as one monthly
  total. Player traffic is attributed to the edge that served it and file
  transfers to the relay that carried them, which are not always the same
  region for the same account. `core`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.08.29.14

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Route-only traffic is now measured. Those addresses carry no server UUID, and
  metering was keyed on the server, so every byte a protected address moved
  reached no counter and no tenant's monthly usage. `core`
- The traffic aggregator no longer stops when a tenant owns no server. An
  account holding only route-only addresses was skipped entirely, which was the
  second half of the same gap. `core`

## 2026.08.29.13

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Five permissions that did nothing are gone from the role editor: "Delete any
  server", "Delete plans" and the three staff modpack ones. No endpoint ever
  checked them, so granting one conferred nothing and withholding it withheld
  nothing. Roles keep working; only the entries disappear. `core`

## 2026.08.29.12

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- The Redis credential inside a Minecraft container is now limited to the six
  keys the log shipper actually uses. It held the whole `dylaris:server:<uuid>:`
  namespace, which includes the marker that enforces the disk quota - so a
  plugin could delete it and the server was started straight back up. Applied on
  the next node connect; containers keep their credential. `core`

### Fixes
- A warp key that names a region with no leader is no longer placed there. The
  automatic choice already refused such a region; a key stating a preference
  skipped that check, and a machine enrolling into one gets an overlay address
  nothing ever programs. `core`

## 2026.08.29.11

### Features
- Nothing.

### Breaking
- A machine outside the cluster can no longer reach an edge's tunnel port
  through the warp overlay. Its edges must publish `:25560` publicly; until
  they do, route-only and BYON links have no path at all. `core`

### Security
- A tenant can no longer route their players through our overlay. Everything a
  customer machine could claim about itself is environment on that machine, so
  the warp allowlist is now what decides it. `core`
- A bring-your-own node now sends its link to the public edge address whatever
  `NODE_EXTERNAL` and `NODE_TAGS` say. Holding no `CLUSTER_SECRET` is what makes
  a node external, and that is not a claim the machine can clear. `node`

### Fixes
- Nothing.

## 2026.08.29.10

### Features
- The file you deploy on your own machine now sits permanently beside the form
  for both bring-your-own-node and protected addresses, instead of behind a
  fold-out. `panel`
- The address picker says what the region choice decides, and that a domain you
  own can be pointed at us with a CNAME. `panel`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Protected addresses now survive Redis losing its data. They are recorded in
  the database and written back within a minute; before, each one existed only
  as a Redis key that nothing ever rewrote, so a restart without persistence
  lost every one of them for good while managed routes came back on their own.
  `core`
- Revoking a link kit now removes its addresses even when Redis no longer lists
  them. The teardown read the live routing table, so an emptied cache made it
  report nothing to remove and leave the addresses behind. `core`
- Deleting a protected address whose Redis entry had gone answered "not found".
  Ownership is read from the stored record as well as the cache. `core`
- Addresses that already existed are recorded once at startup, so the repair
  above covers them too rather than only the ones created from now on. `core`

## 2026.08.29.9

### Features
- Nothing.

### Breaking
- `GET /api/gateway/links` now names each link by a digest instead of returning
  its tunnel token. Announced last release as a security fix; naming it here
  because a script reading the `token` field will not find it. `core`

### Security
- Nothing.

### Fixes
- A failed read of the warp region registry could rewrite a live region's
  subnet and re-enable a disabled one. Absence and "could not tell" arrived as
  the same answer, and the write path treated both as absent. `core`
- A peer is no longer assigned to a region that has no leader at all while a
  region with a leader is merely offline. `core`
- Editing a protected address only skips the address allowance for a
  route-only entry, not for a managed server's route on the same name. `core`

## 2026.08.29.8

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- The Gateway settings screen no longer shows its own defaults when it cannot
  load. Both panels used to lift the skeleton either way, so a failed request
  rendered as "no limits, no domains, everything off" - and nothing typed in
  could be saved, without saying so. `panel`

## 2026.08.29.7

### Features
- Nothing.

### Breaking
- Nothing.

### Security
- Route and link listings no longer return the tunnel token a link
  authenticates with. For a managed server that token is derived from the NODE,
  so it was the same credential for every server that node hosts, and reading
  it needed only `network.read` on one of them. `core`
- A holder of that token could open a tunnel to the edge and receive a share of
  the player connections meant for the real link. Nothing on the client side
  ever read the field. `core`

### Fixes
- Nothing.

## 2026.08.29.6

### Features
- Protected addresses can be edited. Change a route's link, local address or
  port in place instead of deleting it and creating it again. `core` `panel`

### Breaking
- Nothing.

### Security
- The route listing no longer returns each link's tunnel token. That token is
  what a link presents to claim its tunnel at the edge, and nothing read it.
  `core`

### Fixes
- Editing a route no longer counts as a new address, so an account at its
  address limit can still change a route it already owns. `core`

## 2026.08.29.5

### Features
- A link on a customer machine now reaches the edge over the internet instead of
  through the warp tunnel. Players no longer share that tunnel with the same
  customer's uploads, and a warp restart no longer drops their sessions.
  `core` `node` `panel`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.

## 2026.08.29.4

### Features
- Warp leaders register themselves. A leader announces its region, subnet and
  public endpoint, so adding an edge host no longer means typing a row into
  Settings -> Warp. Existing rows are kept, and a leader you disable stays
  disabled. `core` `panel`

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
- Enrolling into a region with no leader endpoint is now refused with a reason.
  It used to succeed and hand out an address with nothing to dial, so the
  machine retried a config that could not work, every five seconds, silently.
  `core` `panel`

## 2026.08.29.2

### Features
- Every setting in a deploy snippet is now marked "keep" or "EDIT", so it is
  clear what must stay as it is. `panel`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- A route limit stored as -1 refused every route instead of allowing any. It is
  the old spelling of "unlimited", and caps are tested as "count is at the
  limit", which a negative meets with nothing held. `core`
- The Gateway settings screen no longer starts its four route limits at that
  -1, which saving before the screen had loaded wrote to the database. `panel`

## 2026.08.29

### Features
- The deploy snippets now say how to create the compose file, with separate
  steps for Linux and for Windows, and a note for Portainer. `panel`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- BYON and route-only deploy snippets took Core's address from the panel's own
  URL. An install that serves the API on a second host handed the customer a
  file that could never enroll. `panel`

## 2026.08.28

### Features
- Updates now have a version. Core, the panel and every node report the release
  they were built from, and the updates view shows which of yours are behind.
  `core` `panel` `node`
- The updates view is no longer admin-only. Anyone signed in sees the releases
  that concern them and what their own machines report. `core` `panel`
- Releases are announced on our Discord. Pick the **Platform** role under
  Channels & Roles. The panel shows the same releases either way.
- One control and one meaning for every operator-set limit: no limit, or a
  number where 0 means none. Covers sub-servers, the node backup quota, ticket
  attachment quotas, beam uploads and route allowances. `core` `panel` `node`

### Breaking
- STARTTLS is now required when the mail encryption setting says starttls. It
  used to fall back to plain text and report success. Send a test mail before
  opening registration. `core`
- Changing the SMTP host, port or username now requires re-entering the
  password. Same for the mod-cache Redis address. `core`

### Security
- A blank password field no longer keeps the old credential when the server it
  belongs to has changed. SMTP and the mod-cache Redis both transmit theirs, so
  repointing a configuration would have handed it to the new host. `core`
- Changing a password now ends every other session for that account. A reset
  used to leave open sessions alive for up to 24 hours. `core`

### Fixes
- A limit of 0 now means none instead of switching the check off. Backup
  quotas, ticket attachment quotas and beam upload limits all tested "greater
  than zero" and so skipped themselves on that one value. `core` `node`
- The sub-server limit could express neither extreme: a saved 0 fell back to
  the built-in default, so none and unlimited both produced a cap of three.
  `core` `panel`
- A server whose node dies mid-install now says so instead of showing a
  spinner. The install resumes on its own when the node reconnects. `core`
  `panel`
- A node that missed a mandatory update is warned at connect and refused past
  the deadline, with the reason stated. `RELEASE_ENFORCE_MIN_VERSION=false`
  warns without refusing. `core` `node`
- A node cap now counts every pending node identity. Enrolment tokens and warp
  keys were counted by separate gates, so a cap of 2 could hand out 4. `core`
- A plan with zero protected addresses now means zero, not "unset". `core`
