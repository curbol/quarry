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
  `jobtracker.js`, `rigmatch.js`, `tagedit.js`, `icons.js`, `index.html`, `style.css`),
  the Node tests in `internal/browse/jstest/`, and `install.sh`.
- **With scope:** interpret the user's wording to identify which packages, frontend
  modules, or root-level files to review. When in doubt, include more rather than less.

Do not review `internal/browse/assets/vendor/`: three.js, its `jsm/` add-ons, and the
woff2 fonts are third-party drops this repo does not maintain. Do not review
`docs/superpowers/specs/`: those are captured design records, not code. Where an
exclusion is produced by code in this repo, review the producer instead.

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
cover only the four THREE-free frontend modules (`gridwindow.js`, `jobtracker.js`,
`rigmatch.js`, `tagedit.js`); every other frontend module (`app.js`, `viewer.js`,
`scene.js`, `thumbs.js`, `thumbworker.js`) has no test at all, so a green run says
nothing about them. There is no Makefile, task runner, or linter config in the repo:
`go vet` and `gofmt` are the only static analysis, so nothing decides Go style beyond
them and nothing at all decides JavaScript style.

Three packages carry an `audit_test.go` (`internal/assetindex/`,
`internal/browse/`, `internal/selfupdate/`), holding guard tests accumulated from
earlier audits. Read the one covering your area before reporting: a failure mode it
already pins is not a new finding.

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
  `icons.js`, `index.html`, `style.css`, and `internal/browse/jstest/*.test.mjs`. Grid
  recycling, bounded caches, the lightbox's shared WebGL context, worker job dispatch,
  rig matching and clip retargeting. No build step and no bundler: modules resolve
  through the document's import map and by absolute `/static/` path.
- **CLI, config & self-update**: `main.go`, `main_test.go`, `internal/config/`,
  `internal/selfupdate/`, `install.sh`. Flag and subcommand dispatch, root and
  tag-store resolution, XDG paths, config file strictness, release fetch and binary
  replacement, token handling.

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
  without bumping `assetindex.indexVersion` (`cache.go`, currently 20). *Check:* grep
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
  machines that have their own names for this one. *Violation:* the guard dropped from
  the loopback path, or extended to a routable one.
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
- **The four Node-tested frontend modules import nothing.** `gridwindow.js`,
  `jobtracker.js`, `rigmatch.js`, and `tagedit.js` are THREE-free and dependency-free
  precisely so `node --test` can load them with no browser and nothing installed.
  *Violation:* any `import` added to one of them. *Check:* `grep -n '^import'` over the
  four files returns nothing.
- **No machine-specific paths or personal data in the repo.** A hard-coded absolute
  path, home directory, or personal library location in code or committed files is a
  violation. Paths resolve via XDG with the documented precedence.

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
- Root-motion pairing (`pairing.go`): pairs within `(vendor, pack, canonical base)`,
  preferring an RM sibling in the same directory, then the same archive, then a matching
  clip over a whole-file RM. Directory is a preference, not part of the key. Because a
  card groups by name and size while pairing groups by pack, a card takes the sibling of
  whichever copy has one. Verify the preference order and the cross-copy fallback. Tier 1/2.
- The search query parser (`searchquery.go`): verify `OR` binds looser than implicit AND,
  plus negation, quoted phrases, grouping, and field scoping against the grammar in the
  file's doc comment. Confirm `maxQueryBytes` truncation cannot split a multi-byte rune
  into a term, that `maxQueryDepth` actually bounds `parsePrimary`'s recursion, and that
  `dropUnmatchedClose` plus the run-to-the-end loop discard no term the user typed.
  Malformed input must degrade to a best effort, never error or panic. Tier 1/2.
- Card grouping and facets (`cards.go`): `groupKey` is the single notion of "one card",
  used both per query and once at startup for the facets. Confirm nothing counts one way
  and filters another, which is what once made a facet advertise a count no filter could
  reach. Tier 1.
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
  the cache by hand. Tier 1.
- Archive entry sanitation (`zip.go`, `unitypackage.go`, `content.go`): an entry path
  with `..` or an absolute root must be rejected before it is joined to the extraction
  dir. `filepath.Join` on a cleaned relative path, never string concatenation. Tier 1.
- `deriveVariant` and `classify`: verify each branch against its condition, that a name
  not following the `<packDir>_<variant>_v<ver>` convention lands in the unknown-variant
  bucket rather than a wrong one, and that a Unity `preview.png` still overrides the
  thumbnail kind at scan time. Tier 2.
- Rig matching (`rigmatch.js`): check `matchRig`, `coversBones`, `nameSeries`,
  `packRigCandidates`, `searchedSkeleton`, and `stackedCharacter` against their doc
  comments. Getting one wrong does not break the page; it previews a plausible-looking
  but wrong animation, or poses a clip onto a rig it does not fit. Tier 1/2.
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
- Pruning removes only extractions the current index no longer references, plus trees
  from another `indexVersion`, and never reaches outside `<cache>/roots/<hash>/`. A prune
  that could delete a live extraction, or one another root or a concurrent instance is
  serving, is Tier 1.
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
  or the page loads two three instances. Tier 1/2.

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
`httptest` servers, with no testify. Frontend tests use Node's own runner over the four
THREE-free modules, with no `package.json` and nothing installed; do not propose growing
that into a general frontend harness. Each of `assetindex`, `browse`, and `selfupdate`
also carries an `audit_test.go` of guard tests from earlier audits; a new invariant
usually belongs there.

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
  the main thread, `scene.js` is what the worker and the viewer share. A three.js import
  appearing outside `viewer.js`, `scene.js`, and `thumbworker.js` is a boundary erosion.
  Tier 2.

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
   that nothing prunes entries outside the current index.
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
