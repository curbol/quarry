# quarry

Search and 3D-preview a local game-asset library in your browser, seeing inside
`.zip` and `.unitypackage` archives.

Point quarry at the tree where your asset packs live and it indexes every individual
asset inside them: models, sprites, textures, materials, animation clips. It then
serves a local page to search that catalog by name, filter by type / vendor / engine
variant, preview in 3D, tag things your way, and copy a path into your project.
Nothing leaves your machine and nothing is modified except the tag store.

Vendor-neutral, with extra knowledge of Synty packs: it assembles Sidekick modular
characters from their part meshes, pairs each in-place animation with its root-motion
sibling, and splits multi-clip animation libraries into one card per clip.

## Install

```bash
gh api repos/curbol/quarry/contents/install.sh --jq .content | base64 -d | bash
```

The repo is private, so the installer authenticates with `GITHUB_TOKEN`, `GH_TOKEN`,
or the `gh` CLI. It drops the binary in `~/.local/bin`. Afterwards `quarry update`
upgrades in place, and `quarry update <version>` pins a specific one.

## Build from source

```bash
go build -o quarry .      # requires Go 1.26+
```

## Setup

The scan root is the only setting with no default. Set it once:

```bash
mkdir -p ~/.config/quarry
cp config.example.toml ~/.config/quarry/config.toml   # then edit `root`
```

Or skip the file and pass `--root ~/code/raw-assets` (or set `QUARRY_ROOT`) per run.

```bash
quarry                            # index the configured root and open the browser
quarry --root ~/code/raw-assets   # index somewhere else for this run
quarry --reindex                  # rebuild the index from scratch
```

The index is cached and refreshed incrementally, so only the first run pays the full
scan. Flags: `--addr <host:port>` (default `localhost:8788`), `--reindex`, `--cache
<dir>` (index / unpacked-archive cache; default `~/.cache/quarry`, and it may not sit
inside the scan root), `--tags <path>`,
`--config <dir>`, `--follow-symlinks`.

If your library is spread across several drives and stitched together with symlinks,
pass `--follow-symlinks` (or set `follow_symlinks = true` in `config.toml`) so those
packs are indexed and served. Without it they are listed as skips on startup rather
than walked, since quarry otherwise stays inside the root you pointed it at.

## Browsing

Search by name, filter by type / vendor / engine variant, see thumbnails, click to
enlarge (images, plus live 3D for GLB and FBX), and copy an asset's path. Because
quarry reads inside `.zip` and `.unitypackage` archives, individual models, sprites,
and materials are searchable without you unpacking anything.

The same file shipped across engine variants or bundled in several packs collapses
into one card (a `×N` badge); the enlarged view lists every copy's path with a
copy-all. Toggle "group dupes" off to see each occurrence separately.

Animated models play in the viewer with a scrub bar and clip selector. A mesh-less
animation clip (Synty `ANIMATION_*` packs, kevdev, etc.) plays on a character whose
rig it matches, auto-picked from the same vendor's library assets. Use "change" to
swap the body, or "pin" one as the default for its rig. Cross-rig cases that need
true retargeting (e.g. an A-pose rig onto a T-pose body) are out of scope for the
preview; bake those offline. Textureless Synty source FBX render as neutral clay;
animation-only FBX (no mesh) show an icon.

## Tagging

Hover a card and hit the tag button to assign existing tags or type a new one
(created on the spot with a random color); the enlarged view has the same controls
plus a color swatch on each tag. Tagged assets show a thin colored sliver per tag
along the card's bottom edge, and the header **tags** filter narrows the grid to the
tags you pick, with an **ANY / ALL** toggle (match any selected tag, or only assets
carrying all of them) and a **manage** mode to rename, recolor, or delete tags. A tag
is just a label; `key:value` labels (e.g. `biome:forest`) are a convention, not
enforced.

Assets can also be **linked** into a set that belongs together (a UI frame and its
background fill, say). The enlarged view lists a linked asset's companions as *parts
of this set* (click to open one), and the tags filter's **linked** toggle folds each
match's companions into the grid, so a `verdict:` query keeps the frame and its fill
together instead of dropping the untagged one. Links are made through the API
(below), not the tag buttons.

Tags and links live in `quarry.tags.toml`. If one is found by walking up from the
working directory, that project's store is used and travels with it in source
control; otherwise quarry uses your user-wide store in the config dir, so tagging
works from anywhere and is never silently off. `--tags <path>` overrides both.

The file is yours to edit and to commit. Because a save rewrites it whole, quarry
refuses to save over a file that changed since it loaded it — so an edit from your
editor, a `git checkout`, or a second quarry is reported rather than silently
overwritten. Restart, or reload the page, to pick the change up.

Tags key on each asset's **content**, not its path or version, so a tag follows the
file across packs and survives a re-download: an unchanged file keeps its tags, and a
file the vendor re-exports starts fresh. The store is never pruned to whatever
happens to be scanned right now, so narrowing your root or moving to another machine
does not lose anything.

## API

The page is backed by a small JSON API you can script against. `GET /api/assets`
takes `q` (a Google-style query: space is AND, `OR` / `|` alternate, `-` excludes,
`"…"` is an exact phrase, `( )` groups, and `field:value` scopes a term to one field
— `name`, `pack`, `vendor`, `type`, `variant`, `ext`, `guid`, `path` — so `turn loop
vendor:kevdev -idle` works; a bare term matches name, pack, and path), repeatable
`type` / `vendor` / `variant` / `guid` filters, repeatable `tag` with `tagmode=and`
(match all selected tags) or `tagmode=or` (any; the default), `group=0` to keep
duplicates separate, `sort=path`, and `offset` / `limit`.

Each asset carries its `source` locator (`kind`, `archivePath`, `entry`, `guid`,
`pathname`, `hasPreview`, `clip`, `clipIndex`), pixel `width` / `height` for images, every
duplicate copy, its content `fingerprints`, current `tags`, and any linked `related`
fingerprints; `includeRelated=1` folds each tag match's linked companions into the
results.

A multi-animation `.glb` (an animation library like Quaternius, one file holding ~120
clips) is split into one card per clip: each shares the file's bytes and names its
animation in `source.clip`, with `source.clipIndex` giving its position in the file
(glTF names are optional and need not be unique, so the label and the lookup key are
separate fields). The clips are individually searchable, taggable, and previewable. An animation that also ships a root-motion (`_RM`) variant collapses to
one card carrying `rootMotionId` (the travel variant's id); the RM card is hidden and
the preview's toggle plays it to show the travel. `?guid=` resolves a scene or
prefab's asset references (bare GUIDs) straight back to the owning asset, so
composition extraction is one request rather than tar-scraping the archive.

`GET /api/content?id=` streams an asset's bytes and `GET /api/thumb?id=` its Unity
preview. `GET /api/tags` returns the palette, `POST/PATCH/DELETE /api/tags`
create/rename/recolor/delete a tag, `POST /api/assign` toggles a tag across an
asset's fingerprint set, `POST /api/link` links or unlinks a set of fingerprints as
one travel-together group, and `GET /api/related?fingerprint=` returns the cards
linked to a fingerprint set.

## Getting assets in the first place

quarry only reads what is already on disk. For Synty store purchases,
[synty-sync](https://github.com/curbol/synty-sync) mirrors your "Your Library" into a
local cache; point `root` at that cache, or at a wider tree holding several vendors.
