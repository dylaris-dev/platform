# Release notes

Two files, one format, two audiences:

| File | Who reads it |
|---|---|
| `platform.md` | people who RUN the platform: self-hosters and operators |
| `hosted.md` | DYLARIS customers: BYON (they run a node) and route-only (they run nothing) |

Both are embedded into Core at build time (`release.go`) and served by
`GET /api/updates`. Core prefers the copy it fetches from the public raw URL so a
running install sees releases published after it was built, and falls back to the
embedded copy when that fetch fails - which is what an air-gapped install wants,
and which reads as "you are current" rather than as an error.

## The format

CI validates both files with the same parser Core uses (`pkg/release`), so a
malformed file fails the build before any image is built.

```markdown
## 2026.08.28

**Update required by 2026-09-05 14:00 UTC** - older nodes stop connecting.

### Features
- What changed, in one or two sentences, for the reader. `core` `node`

### Breaking
- Nothing.

### Security
- Nothing.

### Fixes
- Nothing.
```

- Newest block first, versions strictly descending (see "The version" below).
- All four sections, always, in this order: Features, Breaking, Security,
  Fixes. An empty one is `- Nothing.`, so "no security fixes" reads differently
  from "nobody filled this in".
- At most three entries per section reach the Discord announcement, which
  points to the full notes for the rest, so put the three that matter first.
- The services a reader must update to get the change go last, as inline code:
  `core`, `node`, `log-shipper` (the panel ships inside `core`). A push that
  newly names a service without rebuilding it fails CI.
- A wrapped line must be indented; an unindented line inside a release is an
  error.
- The optional required-update line goes directly under the version heading:
  `**Update required now**` or `**Update required by <when>**`, where `<when>`
  is `YYYY-MM-DD`, optionally followed by `HH:MM UTC`. A bare date means the end
  of that day. A reason may follow after ` - `.

Validate before pushing:

```
cd pkg && go run ./cmd/relnotes validate ../core/updates/platform.md ../core/updates/hosted.md
```

## The version

CalVer per REPO: `2026.08.28`, and `2026.08.28.2` for a second release the same
day. It is written by hand as the heading of a new block and CI stamps it into
every image built from that commit.

It is read across BOTH files, because the repo has one version and more than one
audience: a release that only concerns customers adds a block to `hosted.md` and
none to `platform.md`, and reading `platform.md` alone would stamp images with a
version older than the release they were built from.

## How a component knows its own version

Stamped at BUILD time, never derived from these files:

| Component | How |
|---|---|
| `core` | `RELEASE_VERSION` ldflags → `main.releaseVersion` |
| `node` | the same, sent in the heartbeat and at gRPC auth |
| `panel` | none of its own: it is compiled into Core, so its version is Core's |

Core must NOT read its version off the `platform.md` it embeds. After a
customers-only release that block is older than the image, so Core would report
itself behind for a release it already contains.

An empty version means UNKNOWN and is never treated as old. "Is X behind" is
answered per component - is there a release NEWER than X's version that NAMES X -
which is why one version per repo does not mark every component outdated at once.

## What replaced what

`feed.jsonl` was an append-only JSONL changelog whose LINE COUNT stood in for a
version. It worked, and every component already reported its own position, which
is why five nodes on five builds displayed correctly. What it could not do:

- a line count is not something a human can say, compare, or put in a message;
- two releases on one day were indistinguishable;
- there was no delivery channel but the panel, so a BYON customer who never
  opened it learned nothing.

It is DELETED here. The gateway repo's `updates/feed.jsonl` is frozen rather than
deleted, because images built against it still fetch it and read an unchanged
file as "no updates", which is correct for them.

## Announcements

A CI job posts one Discord message per audience whose top block is the release
being cut, as soon as the builds are green. Idempotency is a
git tag (`platform-<version>`, `hosted-<version>`) pushed BEFORE the message: that
push fails if the tag exists, which is the whole lock. No webhook secret
configured means a green no-op.
