# Audit

Deep code quality review of the quarry codebase. Reviews everything by default. If the user
provides a scope (e.g., "audit the fingerprint scheme", "audit the search query parser"), narrow
to those areas.

## Step 1: Determine scope

- **No arguments:** Review the entire codebase. Gather all `.go` files under `internal/` and the
  repo root (`main.go`, `main_test.go`), plus the embedded frontend in
  `internal/browse/assets/` (`app.js`, `scene.js`, `thumbworker.js`, `style.css`, `index.html`).
- **With scope:** Interpret the user's natural language to identify which packages and files to
  review. When in doubt, include more rather than less.

Do not review `internal/browse/assets/vendor/`: three.js and the bundled fonts are third-party
drops, not code this repo maintains.

## Step 2: Run baseline checks

Run these to establish a baseline. Proceed with the audit either way — sub-agent findings may
explain failures or reveal them as pre-existing. Report any failure as Tier 1, ahead of new audit
findings.

```bash
go build ./...
go test ./...
go test -race ./internal/browse/    # the tag store is mutated under a mutex by concurrent requests
go vet ./...
gofmt -l .                          # any output = unformatted files
```

The suite is fully offline: browse tests run against `net/http/httptest` servers over indexes
built in temp dirs, and nothing touches the network or a real asset library.

## Step 3: Dispatch review sub-agents

Use `feature-dev:code-reviewer` sub-agents to review the scoped files. Split by area so agents run
in parallel:

- **Scanning & identity** — `internal/assetindex/` (`scan.go`, `zip.go`, `unitypackage.go`,
  `gltf.go`, `fingerprint.go`, `dims.go`, `classify.go`, `asset.go`). Archive traversal, the
  fingerprint scheme, category classification, GLB clip splitting, cheap metadata extraction.
- **Vendor heuristics** — `internal/assetindex/sidekick.go`, `internal/assetindex/rootmotion.go`,
  `internal/browse/pairing.go`. Sidekick character assembly, root-motion token stripping and
  pairing. These encode vendor naming conventions and must degrade, not break, on unknown input.
- **Cache & content serving** — `internal/assetindex/cache.go`, `internal/assetindex/content.go`.
  Index persistence, `indexVersion` invalidation, incremental refresh, lazy extraction, pruning,
  and streaming bytes out of archives.
- **Server & query** — `internal/browse/server.go`, `searchquery.go`, `tags.go`, `links_test.go`.
  Handler wiring, the query language, paging, facets, tag/link endpoints, CSRF posture.
- **Tag store** — `internal/tagstore/`. Palette, assignments, link groups, transitive merge,
  atomic sorted TOML IO, `Discover`.
- **Frontend** — `internal/browse/assets/app.js`, `scene.js`, `thumbworker.js`. Grid rendering and
  culling, the lightbox, three.js scene setup, clip retargeting, worker messaging.
- **CLI & config** — `main.go`, `internal/config/`, `internal/selfupdate/`. Flag parsing, root and
  tag-store resolution, XDG paths, self-update.

For each sub-agent, provide:
- The full list of files in its area (not a diff).
- The scope description from the user (if any), so it understands what to focus on.
- The review criteria below that apply to its area, plus the core invariants (which apply
  everywhere).

Tell sub-agents to read entire files, not just scan for patterns. Understanding context is required
to find real issues. Each package's doc comment states its contract; hold the code to it.

### Priority tiers for sub-agents

Sub-agents must categorize every finding into a tier. Tier 3 findings should only be reported if
they are clearly valuable; when in doubt, leave them out.

**Tier 1 (must fix):** Bugs, invariant violations, correctness errors, data races, unatomic writes,
any write into the scanned library, swallowed errors that hide failures, machine paths in code.

**Tier 2 (should fix):** Significant duplication, meaningful refactors, missing test coverage for
important behavior, API design problems, resource leaks, unbounded memory on a large library.

**Tier 3 (consider):** Naming issues, minor API surface cleanup, test reorganization, idiom nits.

### Review criteria

**Core invariants (hard rules — violations are Tier 1)**

These are the contracts the whole tool rests on. See `CLAUDE.md` and `docs/design.md`.

- **Tags and links key on content, not on `Asset.ID`.** `Asset.Fingerprint` is
  `crc32:<hex>:<size>` for zip entries and loose files, `uguid:<guid>` for unitypackage entries,
  and `<file-fingerprint>#<clipName>` for a split GLB clip. `Asset.ID` embeds a machine-absolute
  path and a version-bearing archive name, so it is neither portable nor stable. Any tag or link
  path that keys on `ID`, or any change to how a fingerprint is derived without bumping
  `assetindex.indexVersion`, is a violation — a stale cache would then serve fingerprints that
  silently no longer match the stored assignments.
- **The library is read-only.** The tag store is the only thing quarry may write inside a user's
  tree. Any write, rename, chmod, or delete under the scan root is a violation. Extraction and
  index state belong under the cache dir.
- **Every tag-store write is atomic and mutex-guarded.** Writes go to a temp file in the
  destination dir then `os.Rename` into place, under the server's write lock. A write that
  truncates the target first, or mutates the in-memory store without the lock, is a violation.
  A failed save must reload from disk rather than leave memory ahead of the file.
- **The store never prunes to the scanned set.** Assignments and groups for fingerprints outside
  the current index must be preserved, so tags survive a narrowed `--root`, a moved library, or
  another machine. Any filter of the store against the live index on load or save is a violation.
- **Vendor heuristics stay additive.** Synty / kevdev / Quaternius knowledge lives in named
  helpers. An unrecognized vendor's files must still index, serve, and preview. A heuristic that
  drops, hides, or misclassifies an asset it does not recognize is a violation.
- **No machine-specific paths or personal data in the repo.** A hard-coded absolute path, home
  directory, or personal library location in code or committed files is a violation. Paths resolve
  via XDG with the documented precedence.

**Correctness**

- Fingerprint derivation (`internal/assetindex/fingerprint.go`): confirm each source kind produces
  a stable value, that a zip CRC of 0 or a missing GUID degrades to empty rather than to a
  colliding constant, and that two genuinely different assets cannot collide. Tier 1.
- The search query parser (`internal/browse/searchquery.go`): verify precedence (`OR` binds looser
  than implicit AND), negation, quoted phrases, grouping, and field scoping against the documented
  grammar. Malformed input must degrade to a best effort, never error or panic. Tier 1/2.
- Paging and facet math in `server.go`: a negative or out-of-range `offset`/`limit` must clamp, not
  slice out of range. Tier 1.
- Grouped-duplicate cards: a card's tags are the **union** over its fingerprints, so `tagmode=and`
  can match a grouped card no single copy satisfies. Verify the union is actually taken. Tier 1/2.
- Root-motion pairing: the canonical base must strip both a trailing `_RM` and an `_RM_` infix, and
  pair only within `(vendor, pack, base)`. A wrong base means a hidden card or a mispaired toggle.
  Tier 1/2.
- Edge cases: an empty library, a zero-asset pack, a corrupt archive mid-scan, an archive entry
  with a `..` path, a GLB whose JSON chunk is truncated, a `.sk` referencing a part that is not
  present. Tier 1 if it crashes, escapes the extraction dir, or corrupts the index.

**Write integrity & the cache**

- The index cache and every extraction must be written atomically and keyed so a stale entry can
  never be served as current. Confirm `indexVersion` is compared on load and a mismatch forces a
  rebuild rather than a partial merge. Tier 1.
- Archive entry paths must be sanitized before being joined to the extraction dir: a `..` or
  absolute entry must not escape. Use `filepath.Join` on a cleaned relative path, never string
  concatenation. Tier 1.
- Pruning removes only extractions the current index no longer references. A prune that could
  delete a live extraction, or that walks outside the cache dir, is Tier 1.

**Concurrency**

- Scanning fans out over a large tree; tag writes arrive concurrently from the browser. Look for
  unguarded appends to shared slices/maps, a shared result written without the mutex, a loop
  variable captured by a goroutine. Anything `go test -race` would trip is Tier 1.
- The tag store's read lock must not be held across a disk write, and a failed write must not
  leave the in-memory store diverged from the file. Tier 1.

**Resource use on a real library**

- The index holds ~150k assets in memory; the cached JSON is >100MB. Flag anything that loads an
  entire archive into memory to read one entry, retains an open handle per asset, or copies the
  whole index to answer one query. Tier 2.
- Missing `defer` cleanup: an unclosed `zip.ReadCloser`, file handle, or `resp.Body` is a leak that
  a full scan will hit thousands of times. Tier 1/2.

**Paths & portability**

- Config, cache, and tag-store paths resolve via XDG with the documented precedence
  (`--config` › `$QUARRY_CONFIG_DIR` › `$XDG_CONFIG_HOME` › `~/.config`; likewise
  `QUARRY_CACHE_DIR`). No baked-in machine path, no hard-coded `HOME`. Tier 1.
- The scan root has no default: an unset root must be a clear error, never a fallback to cwd.
  Tier 1 — indexing an arbitrary directory is a slow, surprising accident.

**Frontend**

- The grid must stay responsive over ~150k assets: verify off-screen culling actually culls and
  that per-card work stays cheap. Expensive paint (notably `backdrop-filter` on cards) is a real
  regression on CPU-composited browsers. Tier 2.
- three.js scenes, geometries, materials, and textures must be disposed when the lightbox closes;
  a leak here compounds across previews. Tier 1/2.
- Worker messaging: confirm a failed or slow thumbnail cannot wedge the queue or leave a card
  spinning forever. Tier 2.
- The write endpoints rely on an `application/json` content-type forcing a CORS preflight the
  server does not answer, which is what keeps a random open page from writing to the store.
  Any endpoint that accepts a simple-request content-type loses that protection. Tier 1.

**Duplication and extraction**

- Repeated blocks (5+ lines, similar structure) across files that should be a shared helper. Tier 2.
- Ignore trivial similarities (both call `filepath.Join`, both have an `if err != nil`). Only flag
  duplication where extracting it meaningfully reduces the bug surface or eases change.

**Refactoring opportunities**

- Code that grew incrementally and would benefit from restructuring now that its shape is clear: a
  function doing three things that should be three; a type that accumulated responsibilities; data
  flow that got indirect when a simpler path exists. Tier 2.
- Every refactor suggestion must point to a concrete, defensible improvement — it **removes**
  something (an indirection, a duplicated pattern, a coordination point, a way for two things to get
  out of sync), **enables** something (makes X unit-testable, unblocks a use case, lets a change
  land in one place instead of N), or **generalizes meaningfully** (one shape replaces N
  near-duplicate variants). "Same behavior, different shape" does not qualify, even if more elegant.
  If you can't name what concretely improved, leave it out.

**Test quality (review tests as a whole, not individually)**

Tests use the standard library `testing` package with table-driven cases and httptest servers — no
testify. Step back and look at each area's suite as a unit:
- Significant production behavior with no test at all — especially the core invariants (fingerprint
  stability, read-only library, atomic mutex-guarded writes, no pruning to the scanned set) and the
  query parser's precedence rules. Tier 2.
- Clusters of overlapping tests that could consolidate into fewer, clearer table cases. Tier 2.
- Tests asserting implementation details (internal call order, private field values) instead of
  observable behavior; they break on refactor for no value. Tier 2.
- Duplicated test-helper/setup code that could be shared. Tier 2.
- Weak assertions (checking a count but not the values, asserting the wrong field) — Tier 2 only if
  the weakness could mask a real bug.
- Do NOT suggest tests for trivial edge cases, every possible nil input, or splitting working tests
  for "purity." Fewer, stronger tests beat many fragile ones.

**Go idioms**

- Missing `defer` cleanup on files, archives, and response bodies. Tier 1/2.
- `%w` vs `%v` where a caller uses `errors.Is`/`As`. Tier 2.
- Ignored errors on operations that matter. Tier 2.
- Unnecessary exported surface, needless interfaces, inconsistent pointer/value receivers, naked
  returns in long functions. Tier 3.
- `go vet` / `gofmt` cleanliness. Tier 3 — but a real `go vet` finding (e.g. a copied lock, a lost
  struct copy, a bad Printf verb) is Tier 1.

**Cross-package consistency (sub-agents flag; synthesis happens in Step 4)**

- When reviewing a package, note any exported API that looks easy to misuse (parameter ordering,
  unclear units, implicit preconditions) and any call into another package that assumes something
  about what it returns. These are inputs for the cross-cutting analysis in Step 4.

## Step 4: Cross-cutting invariant analysis

After sub-agents report, trace each core invariant end-to-end across package boundaries — something
no single sub-agent could do:

1. **Fingerprint stability:** follow a fingerprint from where it is computed in
   `assetindex/fingerprint.go`, through the cached index, into a `[[assignment]]` row, and back out
   through a tag query. Confirm the same file yields the same fingerprint across a re-index and a
   pack update, that a split GLB clip's fingerprint is stable, and that `indexVersion` would force
   a rebuild if any of that changed.
2. **Read-only library:** enumerate every filesystem write in the codebase and confirm each lands
   in the cache dir or the resolved tag-store path, never under the scan root.
3. **Tag-store durability:** trace a concurrent assign through the server's lock, the in-memory
   mutation, the atomic save, and a reload. Confirm no interleaving loses a write or leaves memory
   and disk diverged, and that nothing prunes entries outside the current index.
4. **Tag-store resolution:** trace `--tags` › nearest `quarry.tags.toml` walking up from cwd ›
   the user-wide store in the config dir. Confirm tagging is never silently disabled by the CLI,
   and that `browse.Serve`'s empty-path "disabled" mode stays coherent for library callers.
5. **Vendor-neutrality:** pick an unrecognized vendor layout and trace it through `classify`,
   `sidekick`, `rootmotion`, and `pairing`. Confirm it indexes, serves, and previews rather than
   being dropped or mislabeled.

## Step 5: Verify and expand findings

For every finding from sub-agents or cross-cutting analysis:

1. **Verify it yourself.** Read the file and line number. Confirm the issue is real. Drop anything
   speculative, cosmetic, or not actually a problem.
2. **Search for the same pattern across the codebase.** If a sub-agent found an issue in one file,
   grep/glob for the same pattern elsewhere. Report all occurrences, not just the one the sub-agent
   happened to look at.
3. **Drop non-actionable observations.** If a finding amounts to "noting this but it's fine," remove
   it. Every item in the final report must be worth fixing.
4. **Deduplicate.** If multiple sub-agents flagged the same underlying issue from different angles,
   merge into one finding.

## Step 6: Report

Organize findings by tier, then by category within each tier.

**Format for each finding:**
- File path and line number(s)
- What's wrong (concrete, not vague)
- Suggested fix (specific enough to act on)
- If a pattern repeats across files, group all occurrences under one finding

**For test quality findings:** present as a cohesive assessment of each test area, not a list of
individual test files. "The tagstore tests cover the transitive merge well but nothing exercises a
failed save leaving memory ahead of disk" is better than "tagstore_test.go line 42: missing test."

**For refactor findings:** include a brief sketch of the target structure, not just "this should be
refactored." Show what the code would look like after, or at minimum name the functions/types that
would result.

No nits. No cosmetic notes. No "just flagging this." Only actionable items.
