---
description: Deep repo-scoped audit of quarry for invariants, correctness, tests, and refactors
argument-hint: "[scope]"
---

# Audit

Deep review of the quarry codebase: a Go CLI that indexes a local game-asset library
into a searchable catalog of individual assets and serves a web UI to search, preview,
and tag them. Reviews everything by default. If the user provides a scope (e.g. "audit
the fingerprint scheme", "audit the tag store's write path"), narrow to those areas.

Scope: $ARGUMENTS

## Step 1: Determine scope

- **No arguments:** review every `.go` file under `internal/` and at the repo root
  (`main.go`, `main_test.go`), the embedded frontend in `internal/browse/assets/`
  (`app.js`, `viewer.js`, `scene.js`, `thumbs.js`, `thumbworker.js`, `gridwindow.js`,
  `jobtracker.js`, `rigmatch.js`, `tagedit.js`, `cliptrim.js`, `icons.js`, `index.html`,
  `style.css`), the Node tests in `internal/browse/jstest/`, `install.sh`,
  `config.example.toml`, and `.github/workflows/ci.yml` and `release.yml`.
- **With scope:** interpret the user's wording to identify which packages, frontend
  modules, or root-level files to review. When in doubt, include more rather than less.

The two repo-config files are in scope because code depends on their contents, not
merely on their existing. `release.yml`'s platform list has to agree with
`selfupdate.releaseSuffix` and with the labels `install.sh` composes from `uname`, and
`config.example.toml` is copied verbatim into a `config.toml` that `config.Load`
rejects for any key it does not know, so a key here that `fileConfig` lacks is a config
file that fails on first use.

Do not review `internal/browse/assets/vendor/`: three.js, its `jsm/` add-ons, and the
woff2 fonts are third-party drops this repo does not maintain. Do not review
`docs/superpowers/specs/`: those are captured design records, not code. Ignore a
`quarry` binary in the repo root: it is a gitignored build artifact. Where an exclusion
is produced by code in this repo, review the producer instead.

## Step 2: Run baseline checks

Run these first. Proceed with the audit either way, since area findings may explain a
failure or reveal it as pre-existing. Report any failure as Tier 1, ahead of new
findings. Do not re-decide in prose what these commands decide: if the formatter is
clean, formatting is not a finding.

```bash
go build ./...
go test -race ./...                              # what CI runs; the whole suite under the detector
go vet ./...
gofmt -l .                                       # any output = unformatted files
node --test 'internal/browse/jstest/*.test.mjs'  # needs node on PATH; nothing installed
```

The whole suite runs in seconds and is fully offline: browse tests run against
`net/http/httptest` servers over indexes built in temp dirs, selfupdate against a stub
release server, and nothing touches the network or a real asset library. The Node tests
cover only the five THREE-free frontend modules (`gridwindow.js`, `jobtracker.js`,
`rigmatch.js`, `tagedit.js`, `cliptrim.js`); every other frontend module (`app.js`,
`viewer.js`, `scene.js`, `thumbs.js`, `thumbworker.js`) has no test at all, so a green
run says nothing about them. There is no Makefile, task runner, or linter config in the
repo: `go vet` and `gofmt` are the only static analysis, so nothing decides Go style
beyond them and nothing at all decides JavaScript style.

Three packages carry an `audit_test.go` (`internal/assetindex/`,
`internal/browse/`, `internal/selfupdate/`), holding guard tests accumulated from
earlier audits. Read the one covering your area before reporting: a failure mode it
already pins is not a new finding. Between them they pin most of what is listed under
Core invariants below, so they are also the fastest way to learn which rules already
have teeth and which are prose only.

One trap when sweeping the frontend: `scene.js` contains a literal NUL byte (a map-key
separator, around line 799), so GNU grep classifies it as binary and reports "binary
file matches" with no lines, and a piped `grep -n` over it looks like a clean miss.
Pass `-a` / `--binary-files=text` for any grep meant to cover the frontend, or the
largest module on the page silently answers "not found" to every pattern.

## Step 3: Dispatch review sub-agents

Use `feature-dev:code-reviewer` sub-agents to review the scoped files. Split by area so
agents run in parallel:

- **Scanning & identity**: `internal/assetindex/scan.go`, `zip.go`,
  `unitypackage.go`, `gltf.go`, `fingerprint.go`, `dims.go`, `classify.go`, `asset.go`,
  and `scan_test.go`, `fingerprint_test.go`, `gltf_test.go`, `dims_test.go`,
  `classify_test.go`, `symlink_test.go`, plus the scan half of `audit_test.go`. The walk
  and its symlink policy, archive traversal, the fingerprint scheme, category
  classification, GLB clip splitting, cheap metadata extraction.
- **Cache, extraction & atomic writes**: `internal/assetindex/cache.go`, `content.go`,
  `internal/safewrite/safewrite.go`, and `content_test.go`, `safewrite_test.go`, plus
  the cache half of `audit_test.go`. Index persistence and incremental refresh,
  `indexVersion` invalidation, the cache-dir-inside-root refusal, lazy extraction,
  pruning, streaming bytes out of archives, and the atomic-write primitives every other
  package writes through.
- **Vendor heuristics**: `internal/assetindex/sidekick.go`, `rootmotion.go`,
  `internal/browse/pairing.go`, and `sidekick_test.go`, `rootmotion_test.go`,
  `pairing_test.go`. Sidekick character assembly, root-motion token recognition and
  pairing. These encode vendor naming conventions and must degrade, not break, on
  unknown input.
- **Server, query & write guards**: `internal/browse/server.go`, `searchquery.go`,
  `cards.go`, `tags.go`, `links.go`, `writeguard.go`, `open.go`, and `server_test.go`,
  `searchquery_test.go`, `tags_test.go`, `links_test.go`, `audit_test.go`. Handler
  wiring, the Host guard, the query language, card grouping and facets, paging, the
  tag/link endpoints, and the one pipeline every store mutation goes through.
- **Tag store**: `internal/tagstore/tagstore.go`, `tagstore_test.go`. Palette,
  assignments, link groups, transitive merge, sorted TOML IO, the refuse-rather-than-drop
  load, the stale-file guard, `Discover`.
- **Frontend**: `internal/browse/assets/app.js`, `viewer.js`, `scene.js`, `thumbs.js`,
  `thumbworker.js`, `gridwindow.js`, `jobtracker.js`, `rigmatch.js`, `tagedit.js`,
  `cliptrim.js`, `icons.js`, `index.html`, `style.css`, and
  `internal/browse/jstest/*.test.mjs`. Grid recycling, bounded caches, the lightbox's
  shared WebGL context, worker job dispatch, rig matching, clip retargeting, and
  deciding where a padded clip actually stops. No build step and no bundler: modules
  resolve through the document's import map and by absolute `/static/` path.
- **CLI, config & self-update**: `main.go`, `main_test.go`, `internal/config/`,
  `internal/selfupdate/`, `install.sh`, `config.example.toml`,
  `.github/workflows/ci.yml`, `.github/workflows/release.yml`. Flag and subcommand
  dispatch, root and tag-store resolution, XDG paths, config file strictness, release
  fetch and binary replacement, token handling, and the platform labels the workflow
  publishes that the updater and the install script both have to ask for by name.

For each sub-agent, provide:
- The full list of files in its area, not a diff.
- The scope description from the user, if any, so it knows what to focus on.
- The review criteria below that apply to its area, plus the core invariants,
  which apply everywhere.

Tell sub-agents to read entire files rather than scanning for patterns. Finding a real
issue requires the surrounding context. Where a package documents its own contract,
hold the code to that contract.

### Priority tiers

Tier by consequence, not by category. The categories under each tier are examples, not
the definition.

**Tier 1 (must fix):** produces behavior a user or the data can observe as wrong, or
violates a core invariant. Bugs, races, corrupt or non-atomic writes, swallowed errors
that hide a failure, layer violations.

**Tier 2 (should fix):** correct today, but the next change in this area is likely to
break it, or a real invariant has no test. Significant duplication, resource leaks, API
shapes that invite misuse, meaningful refactors.

**Tier 3 (consider):** removes something concrete. If you cannot name what it removes,
leave it out.

### The finding gate

Every finding must name its failure scenario: the input, call sequence, or state that
produces the wrong outcome. "This could race" is not a finding. "Two concurrent writes
both read before either writes, so the second save clobbers the first" is.

A finding that cannot name a trigger is dropped, not downgraded. Report file and line,
what goes wrong, and a fix specific enough to act on.

### Review criteria

**Core invariants (hard rules, violations are Tier 1)**

These are the contracts the whole tool rests on. See `CLAUDE.md` and `docs/design.md`;
each package's doc comment restates its own share.

- **Tags and links key on content, never on `Asset.ID`.** `Asset.Fingerprint` is
  `crc32:<hex>:<size>` for zip entries and loose files, `uguid:<guid>` for unitypackage
  entries, and `<file-fingerprint>#<Source.Clip>` for a split GLB clip. `Asset.ID`
  embeds a machine-absolute path and a version-bearing archive name, so it is neither
  portable nor stable. *Violation:* any tag or link path that keys on `ID`; any change
  to how a fingerprint is derived, to an indexed field, or to what extraction writes
  without bumping `assetindex.indexVersion` (`cache.go`, currently 21). *Check:* grep
  `Fingerprint` and `\.ID` through `internal/tagstore/`, `browse/tags.go`,
  `browse/links.go`; confirm `indexVersion` is compared on cache load.
- **The library is read-only.** The tag store is the only thing quarry may write inside
  a user's tree; everything else goes under `<cache>/roots/<hash of the scan root>/`.
  The cache dir may not sit inside the scan root, and `checkCacheDir` compares paths
  resolved to their deepest existing ancestor precisely so the first run, when the
  cache dir does not exist yet, is the one caught. *Violation:* any `os.Create`,
  `WriteFile`, `Rename`, `Remove`, `MkdirAll`, or `Chmod` on a path derived from
  `Options.Root`, `Source.FilePath`, or `Source.ArchivePath`; a containment check that
  compares unresolved strings.
- **Symlink containment is a real check, not a switch.** A link pointing inside the
  root duplicates a file the walk already reaches, so it is dropped. One pointing
  outside is followed only under `--follow-symlinks`, and unfollowed it is recorded as
  a skip naming the flag. Followed, its resolved target is recorded on the index,
  `Index.Open` accepts a path under the root **or** under one of those targets, and the
  cache-dir refusal is re-run over them after the walk, since that is when they are
  known. *Violation:* a link dropped with no skip recorded; `Open` widening containment
  beyond the recorded targets; the post-walk cache-dir check skipped; a cycle that does
  not terminate.
- **Every tag-store write is atomic, fsynced, lock-held, and stale-checked.** Writes go
  through `safewrite.Atomic` (temp file in the destination's own directory, fsynced,
  then renamed) under the server's write lock, and `Save` returns `tagstore.ErrStale`
  if the file on disk is no longer the one `Load` read. *Violation:* a write that
  truncates the target first; a mutation of the in-memory store outside the lock; a
  rename without the preceding fsync; a save path that skips the staleness check. A
  failed save must reload from disk rather than leave memory ahead of the file, and a
  reload that itself fails must be named in the response.
- **Only the file a store read may be rewritten.** `Save` onto the path `Load` recorded
  re-checks the stamp; a save anywhere else is an export, permitted onto a path that
  does not exist and refused with `ErrStale` onto one that does, because a store that
  never read that file cannot know what rewriting it whole would destroy. That covers a
  `New()` store too, which has read nothing. `Reload(path)` re-homes the store onto
  `path`, so recovery must pass the path it just tried to save to. *Violation:* a
  `Save` to a second path treated as an ordinary write; a `Reload` from a path other
  than the one the store guards; anything relying on the zero `Store`, whose maps are
  nil and whose first `Assign` panics.
- **`Load` refuses what the next save would not put back.** A save rewrites the file
  whole, so loading must error rather than drop: a key this version does not recognize,
  a color it cannot parse, a tag id defined twice. *Violation:* a TOML decode that
  ignores undecoded keys, a malformed color silently defaulted, a duplicate id where
  the second row wins.
- **The store never prunes to the scanned set.** Assignments and groups for fingerprints
  outside the current index must survive a narrowed `--root`, a disabled pack, a moved
  library, or another machine. *Violation:* any filter of the store against the live
  index on load or save.
- **Every mutating endpoint is registered from `writeRoutes()` and requires an
  `application/json` content-type.** There is no session; the content-type is what
  forces a CORS preflight the server does not answer, and the list is what keeps a new
  handler from reaching the mux without also reaching the tests that iterate it.
  *Violation:* a mutating handler registered directly in `handler()`; an endpoint
  accepting a simple-request content type (form, text, or none); a handler reaching for
  the store instead of going through `writeguard.go`.
- **A loopback listener checks the `Host` header.** `guardHost` (`server.go`) closes DNS
  rebinding, which the content-type check cannot: a domain re-pointed at 127.0.0.1 is
  same-origin, gets no preflight, and can read every response. It is applied only on
  loopback, because `--addr` on a routable interface is a deliberate choice to serve
  machines that have their own names for this one, and `Serve` prints a warning to
  stderr on that branch, since there is no authentication anywhere and stepping aside
  silently is what makes an exposed listener easy to leave running. *Violation:* the
  guard dropped from the loopback path, extended to a routable one, or the routable
  branch made silent. *Check:* `isLoopback` is the only thing deciding which branch runs.
- **A facet count is reachable by the query that follows it.** `ungrouped(query)` is the
  single reading of `group=`, used by both `computeResults` and the choice between
  `s.facets` and `s.ungroupedFacets`; `buildFacets` returns both sets from one pass
  because a card count under-reports an asset-per-row response by however many copies a
  pack ships. A tag's `Count` is likewise cards, resolved through `cardOfFP`, with
  assignments outside the current index carried separately as `OffIndex` rather than
  folded in. *Violation:* a response pairing one grouping's results with the other's
  facets; a second, independent reading of `group=`; a count folding in fingerprints no
  filter can return.
- **Vendor heuristics stay additive.** Synty / kevdev / Quaternius knowledge lives in
  named helpers (`sidekick.go`, `rootmotion.go`, `classify.go`, `pairing.go`). An
  unrecognized vendor's files must still index, serve, and preview. *Violation:* a
  heuristic that drops, hides, or misclassifies an asset whose layout it does not
  recognize.
- **One unreadable file costs itself, not the run.** A damaged archive, an unreadable
  directory, a file that disappears mid-walk are recorded as `SkippedFile` and reported;
  an unreadable *root* is still an error, because that is not a partial library.
  *Violation:* a per-file error aborting the build, or a bad root degrading to an empty
  index. Equally: a derivation that failed must not be cached, since the stat print
  describes the file, not whether reading it worked.
- **The five Node-tested frontend modules import nothing.** `gridwindow.js`,
  `jobtracker.js`, `rigmatch.js`, `tagedit.js`, and `cliptrim.js` are THREE-free and
  dependency-free precisely so `node --test` can load them with no browser and nothing
  installed. *Violation:* any `import` added to one of them. *Check:* `grep -n '^import'`
  over the five files returns nothing.
- **The release labels agree in three places.** `.github/workflows/release.yml` builds a
  fixed list of `goos/goarch/label` triples and publishes `quarry-<version>-<label>.zip`;
  `selfupdate.releaseSuffix` names the asset `quarry update` asks for; `install.sh`
  composes the same label from `uname`. A label added or renamed in one place and not
  the others is an update or a first install that cannot find its asset, on a platform
  the release does build. *Violation:* any of the three edited alone. *Check:*
  `TestReleaseSuffixMatchesTheWorkflowLabels` and `TestInstallScriptComposesPublishedLabels`
  in `internal/selfupdate/audit_test.go` parse the other two files and pin this; both
  fail loudly if their regexes stop matching, so a green run is real evidence.
- **No machine-specific paths or personal data in the repo.** A hard-coded absolute
  path, home directory, or personal library location in code or committed files is a
  violation. Paths resolve via XDG with the documented precedence, and every resolved
  config or cache dir is absolute.

**Correctness**

- Fingerprint derivation (`fingerprint.go`): confirm `crcFingerprint` still degrades a
  zero CRC over non-empty bytes to `""` rather than to a constant every such entry of
  the same size would share and silently tag together, that an empty file's genuine
  zero CRC is distinguished by its size, and that `looseFingerprint` returns a read
  failure rather than an empty print. Tier 1.
- Clip identity (`scan.go`, `gltf.go`): `Source.Clip` is the disambiguated label the id
  and fingerprint are built from; `Source.ClipIndex` is the position the preview looks
  up, because the disambiguated label may name no animation in the file. Confirm the
  two are not conflated in either direction, and that two same-named clips cannot
  collide. Tier 1.
- Root-motion recognition (`assetindex.RootMotionVariant`): verify all four conventions
  (trailing `_RM`, `_RM_` infix, ` [RM]` bracket suffix, `_RootMotion_` infix) and
  that `stripToken`'s boundaries still leave `Warm`, `Storm`, and `arm` alone. It is the
  one recognizer shared by the GLB-split gate and browse pairing, so a change here moves
  both. Tier 1.
- Root-motion pairing (`pairing.go`): pairs within `(vendor, pack, canonical base)`.
  `pickRM` weights two terms rather than ordering them, same directory above same
  archive, so a same-directory RM in another archive beats a different-directory RM in
  this one. Directory is a preference, not part of the key. Which clip inside the chosen
  RM file plays is not decided here at all: an RM file is never split, so it arrives
  whole and the frontend matches the clip. Because a card groups by name and size while
  pairing groups by pack, a card takes the sibling of whichever copy has one. Verify the
  weighting and the cross-copy fallback. Tier 1/2.
- The search query parser (`searchquery.go`): verify `OR` binds looser than implicit AND,
  plus negation, quoted phrases, grouping, and field scoping against the grammar in the
  file's doc comment. Confirm `maxQueryBytes` truncation cannot split a multi-byte rune
  into a term, that `maxQueryDepth` actually bounds `parsePrimary`'s recursion, and that
  `dropUnmatchedClose` plus the run-to-the-end loop discard no term the user typed.
  Malformed input must degrade to a best effort, never error or panic. Tier 1/2.
- Card grouping and facets (`cards.go`, `server.go`): `groupKey` is the single notion of
  "one card", and `ungrouped()` the single reading of `group=`. `buildFacets` counts
  both ways in one pass over the library. Confirm the results and the facets in a
  response were selected by the same call, that the ungrouped counts really are per
  asset while the grouped ones are per card, and that `toDTO` leaves `Fingerprints` an
  empty slice rather than nil, so the two paths do not differ in response shape over an
  asset whose content could not be read. Tier 1.
- Tag palette counting (`tags.go`): `paletteLocked` turns each tag's fingerprints into
  the set of cards they land on via `cardOfFP`, counting assignments the index does not
  hold into `OffIndex` instead. `cardOfFP` is built once from the static index and skips
  root-motion-suppressed assets, so it must exclude exactly what the facets exclude.
  Confirm two copies of one file carrying a tag count once, and that an off-index
  assignment is never folded into a number `?tag=` cannot return. Tier 1/2.
- Tag union over a grouped card (`cards.go`, `tagedit.js`): a card's tags are the
  **union** over its fingerprints, so `tagmode=and` can match a card no single copy
  satisfies. The client mirror `nextTags` carries the same asymmetry: gaining the tag
  is certain as soon as any of the card's fingerprints is in the edit, losing it only
  when every one of them was. Verify both halves. Tier 1/2.
- Paging and facet math (`server.go`): a negative or out-of-range `offset`/`limit` must
  clamp against `defaultLimit`/`maxLimit`, not slice out of range, and paging must stay
  consistent across pages while a concurrent tag write bumps the generation counter.
  Tier 1.
- Link resolution (`links.go`, `tagstore`): groups are undirected and merge transitively;
  `includeRelated` relaxes the tag filter alone, so other facets and the text search
  still apply. Confirm a link never changes what tags a fingerprint carries. Tier 1/2.
- Sidekick assembly (`sidekick.go`): `parseSidekick` collects only the top-level `Parts:`
  block, so a `Name` nested under `ColorSet` or `BlendShapes` cannot leak in; byproducts
  are dropped by the character's own name as well as its directory, since two characters
  commonly share one. Verify a `.sk` referencing an absent part degrades rather than
  producing a broken character asset. Tier 1/2.
- Extraction integrity (`content.go`): `openUnpacked` re-checks the member's size against
  what the scan read, because a rename can reach the journal with the data blocks
  unwritten and the fast path is only a stat of a fingerprint-named directory. Confirm
  the check is still on every read path and that a rebuild is possible without deleting
  the cache by hand. `tornMember` is where "no recorded size" degrades to "empty",
  because the one member nothing sizes is also the one a rebuild could otherwise never
  reach. Tier 1.
- Torn versus re-shipped (`content.go`, `zip.go`): a size disagreement has two causes and
  only one is repairable. An archive replaced in place since the scan extracts correctly
  under its new print, so rebuilding reproduces the disagreement forever; that case is
  detected by comparing `ix.ArchivePrint` against a fresh fingerprint and reported as a
  miss wrapping `fs.ErrNotExist`, the same way `openZipEntry` reports an entry the
  archive stopped carrying, so browse answers 404 rather than 500. An archive with no
  recorded print took a degraded enumeration and keeps the repair. `claimRebuild` makes
  the repair once per extraction, because a second disagreement means the tree was never
  the cause and each discard deletes the tree healthy siblings are being served from.
  Verify the two causes cannot be confused, and that a genuinely torn tree is still
  repaired on the first request. Tier 1.
- Reader and rebuild serialisation (`content.go`): `archiveMu` is a per-archive `RWMutex`
  keyed on the archive path, not on the fingerprint, because the fingerprint is a stat
  that moves under a file being replaced while the lock is held. Readers hold it shared
  from the extraction check through the open and release it before streaming, since the
  descriptor survives an unlink. `discardExtraction` takes it exclusively. Confirm no
  path reaches `unpackedEntry` or `openFile` outside it, and that nothing holds it across
  a full stream. Tier 1.
- Cached zip readers (`zip.go`): `zipReaders.acquire` stats the archive and reuses a
  cached reader only while its recorded print matches, retiring rather than closing a
  reader whose archive moved, because a stream over the old bytes may be in flight. A
  pack re-shipped in place keeps its path and inode, so without the print check the
  cached central directory resolves entry names to offsets in a file with a different
  shape, and a removed entry comes back as an empty body under the old
  `Content-Length` with no error anywhere. Confirm the stat stays outside the lock and
  that eviction still cannot close a reader out from under a response. Tier 1.
- Archive entry sanitation (`zip.go`, `unitypackage.go`, `content.go`): an entry path
  with `..` or an absolute root must be rejected before it is joined to the extraction
  dir. `filepath.Join` on a cleaned relative path, never string concatenation. Tier 1.
- `deriveVariant` and `classify` (`scan.go`, `classify.go`): verify each branch against
  its condition, that a name not following the `<packDir>_<variant>_v<ver>` convention
  lands in the unknown-variant bucket rather than a wrong one, and that a Unity
  `preview.png` still overrides the thumbnail kind at scan time. Two near-misses are
  guarded explicitly and are the ones to re-check: an empty `packDir` (a file directly
  under a vendor dir, where every prefix test would pass on the bare `_`) and
  `bareVersion` (a pack shipping `<packDir>_v3`, where nothing is stripped and the
  version becomes the variant, one facet bucket per release). A variant nothing else
  shares is a bucket of one, which is why both return `""`. Tier 2.
- Loose/archive dedup (`scan.go`): `looseDedupKey` trims a split clip's `::<clip>` suffix
  so the file the clips came from is visible to the archive entry that copies it, since
  no asset carries the whole file's own `RelPath` once it is split, and every clip
  already carries the whole file's size. Confirm a split GLB still suppresses its archive
  twin and that the trim cannot swallow a `::` occurring in a real name. Tier 1/2.
- Rig matching (`rigmatch.js`): check `matchRig`, `coversBones`, `nameSeries`,
  `packRigCandidates`, `searchedSkeleton`, `stackedCharacter`, `clipsForAsset`,
  `clipsMatching`, `hasNamedBody`, and `storedBindFits` against their doc comments.
  Getting one wrong does not break the page; it previews a plausible-looking but wrong
  animation, or poses a clip onto a rig it does not fit. Tier 1/2.
- Clip trimming (`cliptrim.js`): `lastMotionTime` scores each track against its own peak
  per-keyframe change, so `MOTION_FLOOR` is relative and `STILL_TRACK` drops a merely
  noisy bone entirely; `trimmedDuration` cuts only when more than `DEAD_TAIL` of held
  pose remains. Both errors are silent: trim too eagerly and the end of a slow settle is
  cut, trim too little and every card in a padded pack holds a pose. Confirm the
  thresholds are still compared against the right quantities and that a clip which was
  never padded is left alone. Tier 2.
- Grid arithmetic (`gridwindow.js`): `wantedRange`, `visibleRange`, `needsRebuild`, and
  `spacerRows` over `LIVE` cards. A subtly wrong rebuild condition is invisible in the
  UI and merely rebuilds every row instead of every few hundred. Tier 2.
- Job dispatch (`jobtracker.js`): the per-request sequence number is what makes "the same
  asset, asked for again" distinct from "this exact request". Confirm a stale result
  cannot displace an object URL a visible image is still using. Tier 1/2.
- Edge cases: an empty library, a zero-asset pack, a corrupt archive mid-scan, an archive
  entry with a `..` path, a GLB whose JSON chunk is truncated, a `.sk` naming a part that
  is not present, a symlink cycle, a config.toml with an unknown key. Tier 1 if it
  crashes, escapes the extraction dir, or corrupts the index.

**Write integrity & the cache**

- The index cache and every extraction are written atomically and keyed so a stale entry
  can never be served as current. Confirm `indexVersion` is compared on load and a
  mismatch forces a rebuild rather than a partial merge. Tier 1.
- Cache reuse is keyed on a file's stat print alone, never on whether it left assets
  behind, and the entries dedup dropped are cached alongside the ones it kept:
  suppression is a property of the pair, so a refresh reusing only survivors would carry
  the suppression forward and lose an asset silently. Tier 1.
- Reuse also requires that the cached entries still *describe* the file at the path the
  walk reached (`refresh`'s `describes`). The print keys on the resolved path while
  `RelPath`, `Vendor`, `Pack` and `Variant` all come from the path the walk took, which
  under `--follow-symlinks` is the same file under a different name. Reused blind, a
  renamed drive keeps its old name in the grid, in the vendor facet and in `path:` search
  until the file's own size or mtime happens to move. Tier 1.
- Pruning removes only extractions the current index no longer references, plus trees
  from another `indexVersion`, and never reaches outside `<cache>/roots/<hash>/`. A prune
  that could delete a live extraction, or one another root or a concurrent instance is
  serving, is Tier 1. `PruneUnpacked` reads its keep-set off the index it is called on,
  so calling it on anything narrowed, filtered, truncated or half-populated deletes the
  extractions of everything missing, which are live for whoever is still serving them:
  confirm every caller passes an index a full `Build` or `LoadOrBuild` just produced.
- `safewrite.Atomic` and `safewrite.Stream` must remove the temp file on every failure
  path, preserve the target's permissions, and resolve a symlinked destination before
  choosing the temp directory. Tier 1.

**Concurrency**

- Scanning fans out over a large tree and tag writes arrive concurrently from the
  browser. Look for unguarded appends to shared slices or maps, a shared result written
  without the mutex, a loop variable captured by a goroutine. Anything `go test -race`
  would trip is Tier 1.
- The tag store's read lock must not be held across a disk write, and a failed write must
  not leave the in-memory store diverged from the file. Tier 1.
- Extraction is shared: confirm a failure reaches every waiter, that a partial extraction
  publishes nothing, and that a retry after a transient failure is possible. Tier 1.
- A memoized result set must be invalidated by the generation counter when a tag write
  lands, so a query cannot serve a set built before an edit the UI already showed. Tier 1.

**Resource use on a real library**

- The index holds ~150k assets in memory and the cached JSON runs past 100MB. Flag
  anything that loads an entire archive into memory to read one entry, retains an open
  handle per asset, or copies the whole index to answer one query. Tier 2.
- Missing `defer` cleanup: an unclosed `zip.ReadCloser`, file handle, or `resp.Body` is a
  leak a full scan hits thousands of times. Tier 1/2.

**Paths & portability**

- Config, cache, and tag-store paths resolve via XDG with the documented precedence
  (`--config` › `$QUARRY_CONFIG_DIR` › `$XDG_CONFIG_HOME` › `~/.config`; likewise
  `--cache` › `$QUARRY_CACHE_DIR` › `$XDG_CACHE_HOME` › `~/.cache`). `ResolveDir` and
  `ResolveCacheDir` report failure rather than falling back to a cwd-relative name.
  No baked-in machine path, no hard-coded `HOME`. Tier 1.
- `resolveXDG` always returns an absolute path, and the branches differ deliberately: a
  relative *flag* is resolved against the working directory (one invocation saying
  "here"), a relative `QUARRY_*_DIR` is an error (it lives in a shell rc and would give
  every directory its own index and unpacked tree), a relative `XDG_*_HOME` is ignored
  per the spec, and a relative `$HOME` is an error rather than a relative join. Tier 1;
  a relative cache dir is how the index and every extraction end up inside the
  read-only library. `absCacheDir` re-applies this in both `Build` and `LoadOrBuild`, so
  containment is checked against the same path the writes use. Tier 1.
- `resolveXDG` assembles its error from two plain strings so the `%w` stays visible to
  `go vet`'s printf check. A format string threaded through a parameter defeats that,
  and a dropped verb reaches the user as `%!(EXTRA ...)` with nothing reporting it.
  Tier 2.
- The scan root has no default: an unset root is a clear error, never a fallback to cwd.
  Tier 1, because indexing an arbitrary directory is a slow, surprising accident.
- An unreadable `config.toml`, or one setting a key this version does not know, is an
  error. A silently ignored `follow_symlinks` typo is a drive missing from the index with
  nothing said about it. Tier 1.
- `pairing.go` splits a loose file's path on the platform's separators and an archive
  entry's on `/` whatever the host is, because that is what the zip format stores.
  Confirm nothing else conflates the two. Tier 2.

**Secrets & the update path**

- `selfupdate` resolves a token from `GITHUB_TOKEN` / `GH_TOKEN` / the `gh` CLI. Confirm
  no token reaches an error message, a log line, or a URL; errors quote the request, and
  a leaked token in a terminal scrollback is a real credential. Tier 1.
- The binary replacement must leave a working executable on every failure path: a failed
  download leaves the old binary, an aside is restored or the failure is named, and no
  residue or stale aside survives. A dev-version sentinel is refused. Tier 1.
- `install.sh` runs under `set -euo pipefail` and is piped from `gh api` into `bash`.
  Confirm every variable is quoted, no unvalidated release field reaches a path or a
  command, and a failed download cannot leave a partial binary on `PATH`. Tier 1/2.
- A failure path in `install.sh` that ends in `err "..."` alone exits 0, because the
  status is `printf`'s, and a caller chaining the documented `curl | bash` on `&&` reads
  that as a clean install. Every `err` on a fatal path needs its own `exit 1`. Check the
  install target too: `mv file dir` moves the file *into* a directory of that name, so a
  directory at `${INSTALL_DIR}/${BINARY_NAME}` has to be refused rather than silently
  swallowing the binary. Tier 1.
- `selfupdate`'s extraction refuses an oversize entry rather than truncating it, since a
  truncated binary would replace a working one. Tier 1.

**Frontend**

- The grid stays responsive over ~150k assets by recycling a bounded number of cards and
  holding dropped rows open with spacers. Verify the recycling actually bounds the DOM and
  that per-card work stays cheap; flag any expensive paint or layout added per card. Tier 2.
- `thumbs.js` owns the three caches a scroll must keep bounded: rendered thumbnails,
  registered fonts, deferred per-card work. Every one holds something the browser will not
  release on its own (an object URL, a `FontFace`, an observed node). Confirm each eviction
  path revokes or unregisters what it drops. Tier 1/2.
- `viewer.js` shares one WebGL context across every lightbox open. Scenes, geometries,
  materials, and textures must be disposed when it closes; a leak here compounds across
  previews. Tier 1/2.
- Worker messaging (`thumbworker.js`, `jobtracker.js`): confirm a failed or slow thumbnail
  cannot wedge the serialized queue or leave a card spinning forever. Tier 2.
- Nothing user-derived (an asset name, a path, a tag label) may reach `innerHTML`. Tier 1.
- `scene.js` imports three by absolute path so it resolves in a worker, which has no import
  map, and that path must stay the same file the document's import map points `three` at,
  or the page loads two three instances. Everything else it imports (`rigmatch.js`,
  `cliptrim.js`, the `jsm/` loaders) must resolve by absolute `/static/` path for the same
  reason. Tier 1/2.
- `scene.js`'s loading manager blanks texture URLs and revokes the object URLs the FBX
  loader mints and never revokes. It is shared between the worker, where blanking is
  required because a scroll touches thousands of models, and the lightbox, where the
  user is looking at one model and its maps are what it looks like. `gltfManager` is the
  switch, chosen off `inWorker`. Confirm it is still a runtime choice and not module-wide,
  which would silently strip every glTF texture in the preview. Tier 1/2.
- `normalizeClip` (`scene.js`) clones each track's times before rebasing, because
  GLTFLoader shares one times buffer across a clip's tracks and across clips, so
  subtracting in place double-counts and collapses durations to zero. Any other in-place
  edit of a loaded clip's `times` or `values` has the same hazard. Tier 1.

**Duplication and extraction**

- Repeated blocks (5 or more lines, similar structure) across files that
  should be a shared helper. Tier 2.
- Ignore trivial similarity: both call the same stdlib helper, both have an
  error check. Only flag duplication where extracting it measurably reduces
  the bug surface or eases a likely change.

**Refactoring opportunities**

- Code that grew incrementally and would benefit from restructuring now that
  its shape is clear: a function doing three things that should be three; a
  type that accumulated responsibilities; data flow that got indirect when a
  simpler path exists. Tier 2.
- Every suggestion must name a concrete improvement. It **removes** something
  (an indirection, a duplicated pattern, a coordination point, a way for two
  things to get out of sync), **enables** something (makes X testable,
  unblocks a use case, lets a change land in one place instead of N), or
  **generalizes meaningfully** (one shape replaces N near-duplicate variants).
  "Same behavior, different shape" does not qualify, however elegant. If you
  cannot name what improved, leave it out.

**Test quality (assess each suite as a whole, not test by test)**

Go tests use the standard library `testing` package with table-driven cases and
`httptest` servers, with no testify. Frontend tests use Node's own runner over the five
THREE-free modules, with no `package.json` and nothing installed; do not propose growing
that into a general frontend harness. Each of `assetindex`, `browse`, and `selfupdate`
also carries an `audit_test.go` of guard tests from earlier audits; a new invariant
usually belongs there. A guard test over a file it does not own (the two in
`selfupdate/audit_test.go` that parse `release.yml` and `install.sh`) must fail loudly
when its own parsing stops matching, or it silently stops checking anything: treat a
cross-file guard with no such assertion as a finding.

- Significant production behavior with no test at all, especially the core
  invariants above and every branch of the logic named under Correctness.
  Tier 2.
- Clusters of overlapping tests that could consolidate into fewer, clearer
  cases. Tier 2.
- Tests asserting implementation details (internal call order, private
  fields) instead of observable behavior. They break on refactor for no
  value. Tier 2.
- Duplicated test setup or helpers that could be shared. Tier 2.
- Weak assertions (a count checked but not the values, the wrong field
  asserted). Tier 2 only where the weakness could mask a real bug.
- Do not suggest tests for trivial edge cases, for every possible nil input,
  or splitting working tests for purity. Fewer, stronger tests beat many
  fragile ones.

**Go idioms**

- Missing `defer` cleanup on files, archives, and response bodies. Tier 1/2.
- `%w` vs `%v` where a caller uses `errors.Is` / `errors.As`. `tagstore.ErrStale` and
  `assetindex.ErrNoThumbnail` are matched that way today (`browse/writeguard.go`,
  `browse/server.go`), so a `%v` anywhere on their path breaks the match silently;
  `ErrOutsideRoot` and `ErrNoCacheDir` are exported for the same use and are compared
  with `==` in tests, so wrapping either without `%w` would break those too. Tier 2.
- Ignored errors on operations that matter. Tier 2.
- Unnecessary exported surface, needless interfaces, inconsistent pointer/value
  receivers, naked returns in long functions. Tier 3.

**JavaScript idioms**

No linter runs over the frontend, so nothing else decides these.

- A `catch` that swallows without surfacing anything: a thumbnail that never appears and
  a console that says nothing is indistinguishable from a slow one. Tier 2.
- An event listener, `IntersectionObserver`, `ResizeObserver`, or `requestAnimationFrame`
  loop registered without a matching teardown, over a page the user keeps scrolling.
  Tier 1/2.
- An `await` in a loop over a result page where the work is independent. Tier 3.
- Module boundaries: `app.js` owns the page, `viewer.js` is the only three.js consumer on
  the main thread, `scene.js` is what the worker and the viewer share, and
  `gridwindow.js` / `jobtracker.js` / `rigmatch.js` / `tagedit.js` / `cliptrim.js` are the
  pure decisions below all of it. A three.js import appearing outside `viewer.js`,
  `scene.js`, and `thumbworker.js` is a boundary erosion; any import at all in one of the
  five pure modules breaks `node --test`. Tier 2.

**Cross-boundary consistency (flag here, synthesized in Step 4)**

When reviewing an area, note any exported API that looks easy to misuse
(parameter order, unclear units, implicit preconditions) and any call into
another area that assumes something about what it returns. These are inputs
for the cross-cutting analysis, which no single area agent can do.

## Step 4: Cross-cutting analysis

After the area agents report, trace each core invariant end to end across boundaries,
which no single agent could do:

1. **Fingerprint stability.** Follow a fingerprint from `assetindex/fingerprint.go`
   through `scan.go`'s asset construction, into the cached index (`cache.go`), out to a
   `[[assignment]]` row in `tagstore.go`, and back through a tag query in
   `browse/tags.go` and `cards.go`. Confirm the same file yields the same print across a
   re-index and a pack update, that a split clip's `<file-fp>#<Clip>` is stable when the
   file's animation order changes, and that `indexVersion` would force a rebuild if any
   of that moved.
2. **Read-only library.** Enumerate every filesystem write in the repo (grep
   `os.Create`, `os.WriteFile`, `os.Rename`, `os.Remove`, `os.MkdirAll`, `os.Chmod`, and
   `safewrite.` across `internal/` and `main.go`) and confirm each lands under
   `Index.stateDir()` or the resolved tag-store path, never under `Options.Root` or a
   followed symlink target. Then confirm `checkCacheDir` covers both the pre-walk root
   and the post-walk targets.
3. **Tag-store durability.** Trace a concurrent assign from `browse/writeguard.go`
   through the write lock, the in-memory mutation in `tagstore.go`, `safewrite.Atomic`,
   the `ErrStale` check, and a reload on failure. Confirm no interleaving loses a write
   or leaves memory and disk diverged, that a failed reload is named in the response, and
   that nothing prunes entries outside the current index. Then check the other direction:
   every `Save` and `Reload` call site in the repo passes the path the store was loaded
   from, so the export rule never fires on the store the server is actually editing.
4. **Write-endpoint coverage.** Read `writeRoutes()` in `server.go` alongside the tests
   that iterate it, then grep `mux.HandleFunc` for any mutating handler registered
   outside the list. Confirm every route in the list requires a JSON content-type and
   refuses when tagging is off, and that `guardHost` wraps the mux on a loopback listener.
5. **Tag-store resolution.** Trace `--tags` › nearest `quarry.tags.toml` walking up from
   cwd (`tagstore.Discover`) › the user-wide store in the config dir (`main.go`
   `resolveTagsPath`). Confirm the CLI never passes an empty path, so tagging is never
   silently off, and that `browse.Serve`'s empty-path "disabled" mode stays coherent for a
   library caller.
6. **Symlink containment.** Trace a followed target from `scan.go`'s walk, onto the
   index's recorded targets, through `cache.go`'s post-walk cache-dir refusal, and into
   `content.go`'s `underRoot` on the serving path. Confirm the scan cannot index a path
   `Open` would then refuse, and that the `follow_symlinks` setting is part of what a
   cached index matches on.
7. **Vendor-neutrality.** Pick an unrecognized vendor layout and trace it through
   `classify`, `deriveVariant`, `sidekick.go`, `RootMotionVariant`, and `pairing.go`.
   Confirm it indexes, serves, and previews rather than being dropped or mislabeled.
8. **Serving an archive that moved.** Take one archive from the walk in `scan.go`, its
   print in `cache.go`'s `ArchivePrint`, through `zipReaders.acquire`'s print comparison
   in `zip.go`, into `openUnpacked`'s torn-versus-re-shipped branch and
   `discardExtraction` in `content.go`, and out to the status `browse` returns. Confirm a
   re-shipped pack, a torn extraction, and an entry the archive never had are three
   distinguishable outcomes, that only the torn one rebuilds, that it rebuilds once, and
   that the other two reach the client as 404 rather than 500 or empty 200.
9. **Counting what a click returns.** Read `buildFacets` and `groupKey` in `cards.go`,
   `ungrouped()` and `handleAssets` in `server.go`, `cardOfFP` in `newServer`, and
   `paletteLocked` in `tags.go` together. Confirm every number the page renders beside a
   filter is produced by the same grouping the filter will apply, for `group=1` and
   `group=0` alike, and that root-motion suppression is applied identically in all three.
10. **Release labels.** Read the `platforms` list in `.github/workflows/release.yml`,
    `releaseSuffix` in `internal/selfupdate/selfupdate.go`, and the `os=` / `arch=`
    mapping in `install.sh` side by side. Confirm every platform the workflow publishes
    is one the updater can name and the script can compose, that the documented
    windows/arm64 exception still points at a label that is published, and that the two
    guard tests parsing those files would fail rather than pass vacuously if a format
    changed.

## Step 5: Verify and widen

For every finding from an area agent or from the cross-cutting analysis:

1. **Verify it yourself.** Read the file and line. Confirm the trigger is
   real. Drop anything speculative, cosmetic, or already handled elsewhere.
2. **Widen it.** Grep or glob for the same pattern across the whole
   codebase and report every occurrence, not just the one an agent happened
   to open.
3. **Mechanize what widens.** If a class has many instances and a tool could
   decide it, report one automation proposal instead of N findings: enable the
   lint rule, add the check, add a guard test. Check the lint configuration
   first, since a rule that exists but is disabled or not wired into CI is
   the cheapest fix available. This repo has no linter config at all, so for Go the
   cheapest mechanization is usually a guard test in the area's `audit_test.go`; adding
   a linter is a real proposal but a larger one, and adding a JavaScript ecosystem to a
   repo that deliberately has none needs the human's decision.
4. **Drop non-actionable observations.** Anything amounting to "noting this
   but it is fine" comes out.
5. **Deduplicate.** Merge findings that different agents reached from
   different angles into one item.

## Step 6: Report

Present the report and stop. Do not fix anything, do not write findings to
memory, and do not leave a findings file behind: the human triages in the
session, and saved findings become confidently wrong context later.

Organize by tier, then by category within a tier. Within a tier, order by
value against effort.

Each finding gets:
- File path and line numbers
- The failure scenario: what goes wrong and what triggers it
- A fix specific enough to act on
- One entry per pattern, with all occurrences grouped under it

**Test quality findings** are a cohesive assessment per area, not a list of
files. "The tagstore tests cover the transitive merge well but nothing exercises a
failed save leaving memory ahead of disk" beats "tagstore_test.go:42: missing test".

**Refactor findings** include a sketch of the target structure, or at minimum
name the functions and types that would result.

**Automation proposals** name the rule or check, the config file it goes in,
and roughly how many current findings it subsumes.

An area with no findings says so. A short report on sound code is a successful
audit, not a lazy one. No nits, no cosmetic notes, no "just flagging this".
