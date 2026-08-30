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

All of it lives under `<cache>/roots/<hash of the scan root>/`, keyed by root because an
index and its extractions describe one library. `--root` indexes somewhere else for a run
and `--addr` lets two instances serve at once, so without that key each run's prune would
delete the other root's extractions — including out from under a server still serving them.
Where the state lives is derived inside `assetindex` from the options, so no caller can pair
one root's index with another's path. The cache dir may not sit inside the scan root: the
tree quarry promises not to write to is not somewhere to put the index and every unpacked
archive, and the next run would index its own output. Under `--follow-symlinks` the library
is the root *and* every target the walk followed, so the same refusal applies to those —
checked after the walk, since that is when they are known.

Reuse is keyed on a file's stat print alone, never on whether it left assets behind — an
archive whose every entry is deduped away by an extracted twin contributes nothing, and
demanding otherwise would re-decompress it on every run. The entries that dedup dropped
are cached alongside the ones it kept, because suppression is a property of the pair
rather than of the archive: the loose twin can be deleted while the archive's print does
not move, and a refresh reusing only the survivors would carry the suppression forward
and lose the asset with nothing reported. A derivation that failed is
deliberately *not* cached: the print describes the file, not whether reading it worked, so
caching one would keep serving the degraded result long after the cause was fixed.

One unreadable file or directory costs itself, not the run: a damaged archive, a
directory the user cannot read, a file that disappears mid-walk are all recorded as
skips and reported, because browse treats a build failure as fatal and would otherwise
refuse to start over one bad corner of a large library. An unreadable root is still an
error — that is not a partial library, it is no library.

A symlink pointing inside the root duplicates a file the walk already reaches by its
real path, so it is dropped. One pointing outside is followed only under
`--follow-symlinks` (`follow_symlinks` in `config.toml`), which is how a library
assembled across several drives is presented as one tree. Off by default, because
traversing wherever a link happens to point is a surprise `find` and `rg` also keep
behind a flag — and because asking for it is what authorises serving files outside the
root. Unfollowed, the link is recorded as a skip naming the flag: a symlinked pack
would otherwise take its whole contents out of the index silently. Followed, its files
are named through the link the user sees, the resolved target is recorded on the index,
and `Open` accepts a path under the root **or** under one of those targets, so
containment stays a real check rather than being switched off. Targets already walked
are not walked again, so a cycle terminates. The setting is part of what a cached index
matches on, since it changes what the scan covers.

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
only the GLB's JSON chunk for the animation names, then emits a per-clip asset whose bytes are
the whole file (`/api/content` serves the file; the preview plays one animation out of it).
Each clip fingerprints as `<file-fingerprint>#<clipName>`, so clips tag independently and stably.

glTF animation names are optional and need not be unique, so the two jobs that name would have
to do are split between two fields. `Source.Clip` carries a disambiguated label (`Walk (2)`,
`clip 3`) and is the identity — it is what the id and the fingerprint are built from, so two
same-named clips cannot collide. `Source.ClipIndex` is the animation's position in the file and
is what the preview looks up, because the label it disambiguated to may be a name no animation
in the file actually has. A root-motion (`_RM`) GLB is left whole (its clips would duplicate
the base file's) and becomes the base clips' root-motion sibling (below).

An animation that ships in two variants — one that travels (root motion baked in, an `_RM` file)
and one that animates in place — collapses to a single card in the `browse` layer (not
`assetindex`, which keeps both files as faithful assets). The in-place variant is the visible
card carrying `rootMotionId`, the RM variant's asset id; the lightbox's root-motion toggle loads
that file to show the travel, and the RM card is suppressed from the grid. Pairing groups assets
by `(vendor, pack, canonical file base)` where the canonical base strips the `_RM` token — a
trailing `_RM` (Quaternius/explosive GLBs) or a `_RM_` infix before a suffix (Synty FBX,
`..._180L_RM_Masc`) — and pairs a group's in-place animations to its RM sibling, preferring one in the same
directory, then the same archive. The directory
is a preference rather than part of the key because a pack laid out per character holds
several same-named clips with their own RM files, while another ships every RM in one
folder; keying on it would mispair the first and stop pairing the second. Because a
result card groups by name and size while pairing groups by pack, a card can span the
copy that owns the sibling and one that does not, so the card takes the sibling of
whichever of its copies has one. This is orthogonal to whether the clip has
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
`--root`, or another machine. A save rewrites the file whole, so loading refuses anything the next save would not put
back rather than dropping it: a key it does not recognize (the store travels between
machines that need not run the same quarry, and that is exactly when a newer version's
field would otherwise be destroyed by an older version's next edit), a color it cannot
parse, and a tag id defined twice, where the second row would silently win. The one thing
a save cannot preserve is comments, so it writes a header line saying so. quarry is otherwise read-only over the library; this is its
one write surface, guarded by a mutex and written atomically. Because there is no session,
the write endpoints require an `application/json` content-type, which forces a CORS
preflight the server does not answer and so keeps a page the user happens to have open from
writing to the store; a failed save reloads from disk rather than leaving memory ahead of it,
and a reload that itself fails is named in the response instead of leaving the UI showing an
edit that is not real.

That content-type only stops a *cross-origin* page, so a loopback listener also checks the
`Host` header: a domain whose DNS is re-pointed at 127.0.0.1 is same-origin with quarry, gets
no preflight, and can read every response — but the browser still sends the attacker's domain
in `Host`. A listener bound to a routable interface is a deliberate choice to serve other
machines, which have their own names for this one, so the check does not apply there.

A save rewrites the whole file, so it first checks the file is still the one it loaded and
returns `ErrStale` if not. The file it guards is the one it read: a save elsewhere is an
export and does not move the guard, and a store that has read nothing refuses to rewrite a
file that already exists at all. The store is meant to be hand-edited and committed to source
control, so an edit arriving from an editor, a `git checkout`, or a second quarry sharing the
user-wide store is a real possibility, and overwriting it would be total and silent.
