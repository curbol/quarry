# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`quarry` is a Go CLI that indexes a local game-asset library into a searchable
catalog of individual assets and serves a web UI to search, preview, and tag them.
It is read-only over the library: the tag store is its one write surface. See
`README.md` (user-facing) and `docs/design.md` (the authoritative design doc: fingerprint
scheme, tag store schema, index cache). Read `docs/design.md` before changing the
fingerprint scheme, the tag store format, or the cache layout.

## Build & test

```bash
go build -o quarry .            # requires Go 1.26+
go test ./...                   # full suite
go test ./internal/assetindex/ -run TestScan -v   # one package / one test
go vet ./...
gofmt -l .                      # list unformatted files
```

The frontend's pure decisions — the grid's card recycling, the thumbnail worker's job
dispatch, matching a mesh-less clip to a rig, folding a tag edit into a card, and
deciding where a padded clip actually stops — are checked separately, because a mistake
in any of them leaves the UI working and merely slow or subtly wrong:

```bash
node --test 'internal/browse/jstest/*.test.mjs'
```

Node's own runner, no `package.json` and nothing installed. Everything else in
`assets/` is exercised through the Go tests or not at all; do not grow this into a
general frontend harness without deciding to take on a second ecosystem.

There is no Makefile or task runner; use the `go` toolchain directly. The suite is
fully offline: browse tests run against `net/http/httptest` servers over indexes
built in temp dirs.

## Architecture

A single-purpose CLI. `main.go` `run()` parses flags and serves; a bare `quarry` is
the whole tool, with only `update` and `version` as subcommands. Layered `internal/`
packages, each with a package doc comment stating its contract:

- `config` — resolves settings by precedence: `config.toml` → env (`QUARRY_ROOT`) →
  flags. Config dir and cache dir are XDG-resolved (`ResolveDir`, `ResolveCacheDir`,
  both of which report failure rather than falling back to a cwd-relative name). The
  scan root has **no default**: an unset root is an error, never a guess at cwd. An
  unreadable `config.toml`, or one setting a key this version does not know, is an
  error too — a silently ignored `follow_symlinks` typo is a drive missing from the
  index with nothing said about it.
- `assetindex` — scans the library into a searchable index, seeing inside `.zip` and
  `.unitypackage` archives as well as loose files, and splitting a multi-animation
  `.glb` (a Quaternius-style animation library) into one virtual per-clip asset that
  shares the file's bytes (`Source.Clip` is the disambiguated label the id and
  fingerprint are built from; `Source.ClipIndex` is the position the preview looks
  up, since the label need not name any animation in the file); serves each asset's bytes and
  thumbnail on demand. It also assembles Synty **Sidekick** modular characters: a
  Sidekick pack ships no whole-character mesh, so `sidekick.go` parses each `.sk`
  definition, upgrades its entry into a character asset (`ThumbSidekick`,
  `Source.Parts` = the part FBX ids), and drops the per-character byproducts — matched
  by the character's own name as well as its directory, since two characters commonly
  share one. HTTP-free — the `browse` server queries it.
- `browse` — serves the web UI, querying an `assetindex.Index` and streaming asset
  bytes and thumbnails (three.js 3D previews, copy-path). Its frontend is plain ES
  modules under `assets/`, no build step: `app.js` is the page (grid, search, filters,
  tagging), `viewer.js` the lightbox's 3D preview and the only three.js consumer on the
  page, `thumbs.js` the three caches a scroll has to keep bounded (rendered thumbnails,
  registered fonts, deferred per-card work), `scene.js` the model/clip helpers the page
  shares with `thumbworker.js`, and `gridwindow.js` / `jobtracker.js` / `rigmatch.js` /
  `tagedit.js` / `cliptrim.js` the pure decisions the Node tests cover — all five
  THREE-free precisely so they can be. `includeRelated=1` folds
  each tag match's linked companions into results; `/api/link` and `/api/related`
  write and resolve links. It also pairs each in-place animation with its root-motion
  (`_RM`) sibling (`pairing.go`): the in-place card carries `rootMotionId` and the RM
  card is hidden, so the preview's toggle can play the travel variant.
- `tagstore` — `quarry.tags.toml`: a palette of user tags (label + color), per-asset
  **assignments keyed by content fingerprint**, and **link groups** (undirected sets
  of fingerprints that travel together, merged transitively). Pure model + atomic,
  sorted TOML IO; the `browse` server loads it, mutates it under a mutex, and
  re-saves on each change. `Discover` walks up from cwd to find a project store;
  `main.go` `resolveTagsPath` falls back to the user-wide store in the config dir.
  Rename onto an existing tag merges; links are result-expansion only, orthogonal to
  tags (a link never changes a fingerprint's tags); the store never prunes
  assignments or groups to a scanned set, so both survive a re-index / another machine.
- `selfupdate` — the `update` subcommand: fetches a GitHub release, downloads the
  current-platform binary, and atomically replaces the running executable. The repo
  is private, so it resolves a token from `GITHUB_TOKEN` / `GH_TOKEN` / the `gh` CLI.

### Key invariants (don't break these)

- **Tags and links key on content, not the browse `id`.** `Asset.Fingerprint`
  (`crc32:<hex>:<size>` for zip/loose, `uguid:<guid>` for unitypackage,
  `<file-fp>#<clip>` for a split GLB clip) is the tag and link identity; it is
  portable and stable across updates, unlike `Asset.ID` (a machine-absolute,
  version-bearing locator hash used only to serve bytes). Bump
  `assetindex.indexVersion` when the fingerprint scheme, any indexed field, or what
  archive extraction writes changes: it keys both the cached index and the unpacked
  tree, so stale state on either side rebuilds.
- **The library is read-only.** The tag store is the only thing quarry writes inside
  a user's tree; everything else it writes is regenerable state under the cache dir,
  in `<cache>/roots/<hash of the scan root>/` — keyed by root so two roots sharing a
  cache dir do not prune each other's extractions away. A cache dir inside the scan
  root is refused by `assetindex` itself, since that is the package that does the
  writing — comparing paths resolved to their deepest existing ancestor, because the
  run that has to be caught is the first one, when the cache dir does not exist yet.
  Under `--follow-symlinks` the same refusal covers every target the walk followed,
  which is only known once it has.
- **Tagging is never silently off.** With no project store discoverable, the
  user-wide store in the config dir is used. `browse.Serve` still honors an empty
  `tagsPath` as "disabled" so the package stays usable that way, but the CLI never
  passes one.
- **Vendor-neutral core, vendor-specific heuristics.** Synty/kevdev/Quaternius
  knowledge lives in named helpers (`sidekick.go`, `rootmotion.go`, `classify.go`)
  and must stay additive: an unrecognized vendor's files still index and preview.
- **No machine-specific paths or personal data in the repo.** `config.toml` (the scan
  root) lives in the config dir outside this repo; a tag store belongs with the
  project whose assets it tags. Both are gitignored here defensively.
