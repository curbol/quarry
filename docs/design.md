# quarry Design

`quarry`: a Go CLI that indexes a local game-asset library into a searchable catalog of
individual assets and serves a web UI to search, preview, and tag them. Acquiring the packs
is out of scope and belongs to whatever put them on disk; for Synty store purchases that is
[synty-sync](https://github.com/curbol/synty-sync), which mirrors a purchased library into a
local cache. quarry reads what is already there.

## Goals

- Find one mesh, sprite, clip, or material by name across a library too large to browse by
  hand, without unpacking archives first.
- See it before committing to it: thumbnails in the grid, a real 3D preview on click.
- Organize the library by meaning rather than by the vendor's folder layout, in a way that
  survives the vendor re-shipping the pack.
- Never modify the library. The tag store is the only thing quarry writes into a user's
  tree; everything else it writes is regenerable state under the cache dir.

## Non-goals

- Downloading, updating, or otherwise acquiring assets.
- Conversion (FBX to glTF, sprite atlasing) or any offline bake.
- Editing assets, or writing anything into the scanned tree.
- True cross-rig animation retargeting. The preview plays a clip on a rig it matches; an
  A-pose rig onto a T-pose body is a bake-offline problem.

## CLI surface

```
quarry              # index the configured root and serve the UI (the whole tool)
quarry update       # self-replace the running binary from the latest GitHub release.
quarry version      # print the installed version.
```

Serving is what the tool is for, so it is what a bare `quarry` does rather than a
subcommand. Flags: `--root <dir>` (scan root), `--addr <host:port>` (default 8788),
`--reindex`, `--cache <dir>`, `--tags <path>`, `--config <dir>`. A stray positional is an
error rather than silently swallowing the flags after it.

## Configuration

`config.toml` in the XDG config dir carries `root`, the only setting with no default:
indexing whatever directory the user happened to be standing in would be a slow, surprising
accident, so an unset root is an error. `QUARRY_ROOT` then `--root` override it. The config
holds no account identity and no credentials, because quarry has no session and talks to
nothing.

## Index cache

The scan result is cached as JSON under the XDG cache dir and refreshed incrementally, so
only the first run pays the full walk. Archive contents are extracted lazily into a
fingerprint-keyed directory beside it, under a tree named for the index version. Both are
expendable: `--reindex` rebuilds from scratch, and `assetindex.indexVersion` is bumped
whenever the fingerprint scheme, an indexed field, or what extraction writes changes, so
stale caches rebuild themselves rather than serving wrong data. An archive whose bytes
never changed keeps its fingerprint, so the version is the only thing that can tell an
extraction made by older code from what the current code would write. Each pack update
extracts under a new fingerprint, so a prune on startup drops both the extractions the
current index no longer references and every tree from another version.

One unreadable file or directory costs itself, not the run: a damaged archive, a
directory the user cannot read, a file that disappears mid-walk are all recorded as
skips and reported, because browse treats a build failure as fatal and would otherwise
refuse to start over one bad corner of a large library. An unreadable root is still an
error — that is not a partial library, it is no library.

A symlink is not followed. One pointing inside the root duplicates a file the walk
already reaches by its real path, so it is dropped; one pointing outside is recorded as
a skip, because serving refuses any path resolving outside the root and indexing it
would only produce cards that cannot load. Reporting it matters most for a symlinked
pack directory, which would otherwise take its whole contents out of the index silently.

## Tag store

`quarry.tags.toml` records user tags and links. It resolves as `--tags`, else the nearest
one walking up from the working directory (a project's own store, committed with that
project), else the user-wide store in the config dir, so tagging is available from anywhere
rather than off outside a particular project. It holds a palette of `[[tag]]` definitions (`id` = the label text,
which is its identity; `color` = `#rrggbb`), `[[assignment]]` rows mapping a content
**fingerprint** to its tag ids, and `[[group]]` rows recording link groups, all sorted for
minimal diffs:

```toml
[[tag]]
  id = "hero"
  color = "#e11d48"
[[assignment]]
  fingerprint = "crc32:1a2b3c4d:41700000"
  tags = ["biome:forest", "hero"]
[[group]]
  fingerprints = ["crc32:2c54c32c:8635", "uguid:98960c3a158d24c4a933f0d99fb26946"]
```

Assignments and groups key on an asset's content, not its location or the browse `id` (which
embeds a machine-absolute path and a version-bearing archive name, so it is neither portable nor
stable across updates). The fingerprint is `crc32:<hex>:<size>` for zip entries and loose files
(the CRC is free from the zip central directory) and `uguid:<guid>` for unitypackage entries
(Unity's stable per-asset GUID). Byte-identical copies therefore share one fingerprint, so a tag
set once applies to every copy and survives a re-download of an unchanged file.

A multi-animation `.glb` (a Quaternius-style animation library, one file holding ~120 clips on a
shared rig) is split at scan time into one virtual asset per embedded clip: `assetindex` reads
only the GLB's JSON chunk for the animation names, then emits a per-clip asset whose `Source.Clip`
names the animation and whose bytes are the whole file (`/api/content` serves the file; the
preview plays `Source.Clip`). Each clip fingerprints as `<file-fingerprint>#<clipName>`, so clips
tag independently and stably. A root-motion (`_RM`) GLB is left whole (its clips would duplicate
the base file's) and becomes the base clips' root-motion sibling (below).

An animation that ships in two variants — one that travels (root motion baked in, an `_RM` file)
and one that animates in place — collapses to a single card in the `browse` layer (not
`assetindex`, which keeps both files as faithful assets). The in-place variant is the visible
card carrying `rootMotionId`, the RM variant's asset id; the lightbox's root-motion toggle loads
that file to show the travel, and the RM card is suppressed from the grid. Pairing groups assets
by `(vendor, pack, canonical file base)` where the canonical base strips the `_RM` token — a
trailing `_RM` (Quaternius/explosive GLBs) or a `_RM_` infix before a suffix (Synty FBX,
`..._180L_RM_Masc`) — and pairs a group's in-place animations to its RM sibling (the same clip
when the RM is also per-clip, else the whole-file RM). This is orthogonal to whether the clip has
a body to preview on: it only decides which file the toggle plays.

`GET /api/assets` filters by tags with a repeatable `tag` param combined by `tagmode`: `and`
matches cards carrying all selected tags, `or` (the default) any. A card is matched on the
**union** of tags over its fingerprints, so a grouped card can satisfy an `and` query even when no
single copy carries every tag. (The browse UI's ANY/ALL toggle is this `tagmode`.)

A `[[group]]` is an **undirected** set of fingerprints that belong together (a UI frame and its
background fill, say). Groups merge transitively: linking `{A,B}` then `{B,C}` yields `{A,B,C}`.
They are a result-expansion concern only, orthogonal to tags: a group never changes what tags a
fingerprint carries. `GET /api/assets?includeRelated=1` takes each tag match's linked companions
and folds them into the result (relaxing only the tag filter, so other facets and the text search
still apply); each card's `related` field lists its companions' fingerprints; `GET
/api/related?fingerprint=` resolves a fingerprint set's companions to whole cards (for the
lightbox "parts of this set" strip); `POST /api/link {fingerprints, on}` links or unlinks.

The store never prunes to a currently-scanned set: assignments and groups for fingerprints
outside the current view are preserved, so tags and links survive a disabled pack, a narrowed
`--root`, or another machine. A save rewrites the file whole, so loading refuses a key it
does not recognize rather than dropping it: the store travels between machines that need not
run the same quarry, and that is exactly when a newer version's field would otherwise be
destroyed by an older version's next edit. quarry is otherwise read-only over the library; this is its
one write surface, guarded by a mutex and written atomically. Because there is no session,
the write endpoints require an `application/json` content-type, which forces a CORS
preflight the server does not answer and so keeps a page the user happens to have open from
writing to the store; a failed save reloads from disk rather than leaving memory ahead of it.
