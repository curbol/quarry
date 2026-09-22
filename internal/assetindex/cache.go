package assetindex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/curbol/quarry/internal/safewrite"
)

// indexVersion is bumped whenever the scan logic changes (what's indexed, how it's
// classified), so a cached index from older logic is rebuilt rather than reused. It
// also keys the unpacked-archive tree, so a change to what extraction writes belongs
// here too: an archive whose bytes never changed keeps its fingerprint, and only the
// version tells the old extraction apart from what the current code would produce.
const indexVersion = 27

// SkippedFile records a library file the scan could not read. A damaged archive
// costs its own contents, not the rest of the library, so the failure is carried
// here for the caller to report instead of aborting the build.
type SkippedFile struct {
	RelPath string `json:"relPath"`
	Reason  string `json:"reason"`
}

// Index is the in-memory, on-disk-cacheable catalog of a library. Content requests
// resolve through byID (never by reconstructing a path from client input), and the
// unpacked-archive cache under CacheDir is keyed by each archive's fingerprint so a
// changed archive re-extracts.
//
// An index is built by one goroutine and then read by many: Lookup, Open and
// OpenThumbnail are safe to call concurrently, and everything that rewrites the asset
// set is unexported and runs before the index is handed to a server. Rebuilding one
// in place while it is being served would race every reader and strand the resolved
// link roots Open checks against.
type Index struct {
	Version      int               `json:"version"`
	Root         string            `json:"root"`
	Assets       []Asset           `json:"assets"`
	ArchivePrint map[string]string `json:"archivePrint"` // abs archive path -> stat fingerprint
	LoosePrint   map[string]string `json:"loosePrint"`   // abs loose path -> stat fingerprint
	Skipped      []SkippedFile     `json:"skipped,omitempty"`

	// Suppressed holds what dedup dropped: the archive entries a loose twin covers,
	// and the loose Sidekick byproducts an assembled character supersedes. Both are
	// cached because reuse is keyed on a file's stat print, which does not move when
	// the thing that suppressed it goes away — delete the loose twin, or the pack
	// holding the character, and a refresh reusing only the survivors would carry the
	// suppression forward and the entry would never come back. So this slice is not
	// all archive entries, and nothing reading it may assume a Kind.
	Suppressed []Asset `json:"suppressed,omitempty"`

	// FollowSymlinks is the setting this index was built under, kept because it
	// changes what the scan covers: a cache built the other way describes a
	// different library. LinkRoots are the resolved targets it followed, and with
	// Root they bound every path serving will open.
	FollowSymlinks bool     `json:"followSymlinks,omitempty"`
	LinkRoots      []string `json:"linkRoots,omitempty"`

	cacheDir string
	byID     map[string]*Asset

	rootsOnce     sync.Once
	resolvedRoots []string

	// extractMu guards all three maps. extractions single-flights one archive's unpack;
	// archiveMus holds each archive's reader/rebuild lock (see archiveMu); rebuilt
	// records the extractions a torn member has already been repaired for, each entry
	// a channel closed once that repair has finished, so readers that did not win the
	// claim can wait for it instead of answering for it.
	extractMu   sync.Mutex
	extractions map[string]*extraction
	archiveMus  map[string]*sync.RWMutex
	rebuilt     map[string]chan struct{}

	// liveUnpacked is the set of archive fingerprints this scan reached, recorded by
	// refresh at the moment it finished. PruneUnpacked reads it rather than re-deriving
	// from Assets, which is exported: a caller that filters or truncates the slice
	// before pruning would otherwise sweep the extractions of everything it removed,
	// and those are live for a second quarry sharing this cache dir. nil means no
	// refresh produced this index, which is the one state the sweep must refuse.
	liveUnpacked map[string]bool

	zips zipReaders
}

// fingerprint identifies a file by path, size, and mtime, so any re-download or edit
// that moves the size or the mtime invalidates the cached enumeration and extraction
// of it. One that preserves both — a copy made with rsync --times or cp -p over a
// file of identical length — does not, and needs --reindex.
func fingerprint(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	return fingerprintOf(path, fi.Size(), fi.ModTime()), nil
}

// fingerprintOf is the print itself, over a stat the caller already has. The walk stats
// every file it enumerates, so refresh deriving the print from that rather than stating
// again is ~150k syscalls saved on a real library, on the path the user waits on — and
// it leaves one reading of a file's stat where there were two that could disagree about
// the file changing between them. The serving side has no such stat in hand and goes
// through fingerprint.
func fingerprintOf(path string, size int64, mod time.Time) string {
	key := path + "\x00" + strconv.FormatInt(size, 10) + "\x00" + strconv.FormatInt(mod.UnixNano(), 10)
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:12])
}

// Options select what a scan covers and where its regenerable state lives. The
// zero value plus a Root is the default scan.
type Options struct {
	Root     string
	CacheDir string
	// FollowSymlinks walks symlinked directories pointing outside the root, which is
	// how a library assembled across several drives is presented as one tree. It is
	// off by default: following wherever a link happens to point is a surprise worth
	// asking for, the same call `find -L` and `rg --follow` make. Asking for it is
	// also what authorises serving those files, since they sit outside the root that
	// otherwise bounds what the content API will open.
	FollowSymlinks bool
}

// absCacheDir makes the cache dir absolute the way Root already is. Everything derived
// from it — stateDir, the extraction tree, the prune — is a path this package writes
// through, and containment is checked against a resolved copy, so a relative one would
// have the check and the writes disagreeing about which directory they mean the moment
// anything changed the working directory. Empty stays empty: that is "no cache dir".
func absCacheDir(cacheDir string) (string, error) {
	if cacheDir == "" {
		return "", nil
	}
	return filepath.Abs(cacheDir)
}

// checkCacheDir refuses a cache dir inside the scan root. The tree quarry promises
// not to write to is not somewhere to put the index and every unpacked archive, and
// the next run would index its own output.
//
// It lives here rather than at the call site because this package does the writing
// and derives where state goes from these same options: a caller reaching for Build
// or LoadOrBuild directly would otherwise bypass the guarantee entirely.
func checkCacheDir(root, cacheDir string) error {
	if cacheDir == "" {
		return nil
	}
	if !contains(resolve(root), resolve(cacheDir)) {
		return nil
	}
	return fmt.Errorf("cache dir %s is inside the scan root %s; pick one outside it with --cache", cacheDir, root)
}

// checkCacheDirLinks applies the same rule to the symlink targets a walk followed.
// checkCacheDir runs before the walk, when the root is the only part of the library
// anyone knows about; --follow-symlinks widens the library to these targets, which
// Open serves from as readily as the root, so containment has to be asked again once
// they are known. Without it a cache dir on the far side of a link is written to, then
// walked and indexed by the next run, then swept by PruneUnpacked.
//
// Asked both ways round. A link whose target sits inside the cache dir overlaps just
// as badly in the other direction: the walk indexes every extracted member as a loose
// library file and re-reads the index JSON — hundreds of megabytes, its print moving
// every time a save rewrites it — on every run.
func checkCacheDirLinks(cacheDir string, linkRoots []string) error {
	if cacheDir == "" {
		return nil
	}
	rc := resolve(cacheDir)
	for _, lr := range linkRoots {
		rl := resolve(lr)
		if contains(rl, rc) {
			return fmt.Errorf("cache dir %s is inside %s, a directory this scan follows a symlink into; pick one outside the library with --cache", cacheDir, lr)
		}
		if contains(rc, rl) {
			return fmt.Errorf("this scan follows a symlink into %s, which is inside the cache dir %s; point the link outside it, or pick another cache dir with --cache", lr, cacheDir)
		}
	}
	return nil
}

// Build scans a library from scratch into a fresh index. It is Refresh over an
// empty index: with nothing cached to reuse, every entry takes the re-derive path.
func Build(opt Options) (*Index, error) {
	absRoot, err := filepath.Abs(opt.Root)
	if err != nil {
		return nil, err
	}
	if opt.CacheDir, err = absCacheDir(opt.CacheDir); err != nil {
		return nil, err
	}
	if err := checkCacheDir(absRoot, opt.CacheDir); err != nil {
		return nil, err
	}
	ix := &Index{
		Version: indexVersion, Root: absRoot, cacheDir: opt.CacheDir,
		FollowSymlinks: opt.FollowSymlinks,
		ArchivePrint:   map[string]string{}, LoosePrint: map[string]string{},
	}
	if err := ix.refresh(); err != nil {
		return nil, err
	}
	return ix, nil
}

// cacheable reports whether the cache can reproduce path and these assets byte for
// byte. The index is JSON, and encoding/json replaces an invalid UTF-8 byte with
// U+FFFD without reporting it — so a locator carrying one (a zip entry name older
// Windows tooling stored in CP437, a file name that is not UTF-8 on a filesystem that
// does not care) comes back from the cache naming something the archive does not
// carry. The card stays in the grid, resolves by id and is still taggable, and its
// bytes 404 on every run after the first, which --reindex repairs for exactly one run
// before writing the same cache again.
//
// Declining the print is the same rule a failed derivation follows: the print
// describes the file, not whether what was read survives the round trip. The cost is
// that such a file is re-derived every run — its path is a map key and mangles the
// same way, so there is nothing to look it up by either — and the alternative is a
// locator that stops being a JSON string, which is a schema change.
func cacheable(path string, assets []Asset) bool {
	if !utf8.ValidString(path) {
		return false
	}
	for i := range assets {
		s := &assets[i].Source
		if !utf8.ValidString(s.Entry) || !utf8.ValidString(s.FilePath) ||
			!utf8.ValidString(s.ArchivePath) || !utf8.ValidString(s.Pathname) ||
			!utf8.ValidString(s.Guid) || !utf8.ValidString(s.Clip) {
			return false
		}
	}
	return true
}

// refresh re-walks the library, reusing the cached enumeration of every archive
// and the cached fingerprint of every loose file whose stat fingerprint is
// unchanged, re-deriving only changed or new files. This avoids re-decompressing
// every unitypackage and re-reading every loose file's bytes on each run.
//
// Reuse is only sound for entries derived by this version's scan logic, so an index
// carrying another version's is emptied first and re-derived whole. Enforcing that here
// rather than in LoadOrBuild is what keeps the guarantee from depending on the one
// caller that currently checks: load does not inspect Version, so an index decoded from
// a cache written by other scan logic reaches this function directly.
func (ix *Index) refresh() error {
	if ix.Version != indexVersion {
		ix.Assets, ix.Suppressed = nil, nil
		ix.ArchivePrint, ix.LoosePrint = map[string]string{}, map[string]string{}
		ix.Version = indexVersion
	}
	entries, skipped, linkRoots, err := walkLibrary(ix.Root, ix.FollowSymlinks)
	if err != nil {
		return err
	}
	ix.LinkRoots = linkRoots
	if err := checkCacheDirLinks(ix.cacheDir, linkRoots); err != nil {
		return err
	}
	// The walk's own skips, before enumeration appends its own. A file the walk reached
	// and could not read is a different thing from a directory it could not look inside,
	// and only the second hides archives from the keep-set below.
	walkSkips := skipped
	// Positions, not values: an Asset is ~350 bytes, so both mapping the previous set
	// and flattening it would hold a second copy of a 150k-asset library alongside the
	// one being rebuilt. The survivors and the suppressed are addressed as one space so
	// an archive's cached enumeration is reused whole and dedup gets the same full set
	// to decide over that a fresh scan would.
	nKept := len(ix.Assets)
	prevAt := func(i int) Asset {
		if i < nKept {
			return ix.Assets[i]
		}
		return ix.Suppressed[i-nKept]
	}
	oldByArchive := map[string][]int{}
	oldByLoose := map[string][]int{}
	indexPrev := func(base int, set []Asset) {
		for i := range set {
			if set[i].Source.Kind == SourceLoose {
				oldByLoose[set[i].Source.FilePath] = append(oldByLoose[set[i].Source.FilePath], base+i)
			} else {
				oldByArchive[set[i].Source.ArchivePath] = append(oldByArchive[set[i].Source.ArchivePath], base+i)
			}
		}
	}
	indexPrev(0, ix.Assets)
	indexPrev(nKept, ix.Suppressed)
	newPrint := map[string]string{}
	newLoose := map[string]string{}
	var assets []Asset
	reuse := func(idx []int) {
		for _, i := range idx {
			assets = append(assets, prevAt(i))
		}
	}
	// describes reports whether cached entries still describe the file at the path the
	// walk reached them by. The stat print keys on the resolved path, while RelPath,
	// Vendor, Pack and Variant are all derived from the path the walk took to get
	// there — the same file only under --follow-symlinks, where renaming a link moves
	// every display field without moving the print. Reused blind, a renamed drive keeps
	// the old name in the grid, in the vendor facet and in `path:` search until the
	// file's own size or mtime happens to move.
	describes := func(idx []int, rel string) bool {
		if len(idx) == 0 {
			return true
		}
		got := prevAt(idx[0]).RelPath
		return got == rel || strings.HasPrefix(got, rel+"::")
	}
	live := map[string]bool{}
	for _, e := range entries {
		fp := fingerprintOf(e.path, e.size, e.modTime)
		if e.kind != SourceLoose {
			// Recorded off the walk's own stat, whether or not the enumeration that
			// follows keeps the print. A refresh whose second pass over an archive
			// failed drops it from ArchivePrint while keeping the assets the first pass
			// produced, and those are served out of an extraction that must survive.
			live[fp] = true
		}
		if e.kind == SourceLoose {
			if ix.LoosePrint[e.path] == fp && describes(oldByLoose[e.path], e.rel) {
				newLoose[e.path] = fp
				reuse(oldByLoose[e.path])
				continue
			}
			a, skip := looseAssets(e)
			// A failed derivation is deliberately left out of newLoose: the stat print
			// describes the file, not whether reading it worked, so caching one would
			// keep serving the degraded result even after the cause was fixed. One the
			// cache cannot represent is left out on the same ground — see cacheable.
			if skip != nil {
				skipped = append(skipped, *skip)
			} else if cacheable(e.path, a) {
				newLoose[e.path] = fp
			}
			assets = append(assets, a...)
			continue
		}
		newPrint[e.path] = fp
		// Keyed on the print alone, not on having cached assets: an archive whose every
		// entry was dropped by dedup leaves no assets behind, and demanding some would
		// re-decompress it on every single run.
		if ix.ArchivePrint[e.path] == fp && describes(oldByArchive[e.path], e.rel) {
			reuse(oldByArchive[e.path])
			continue
		}
		a, skip := archiveAssets(e)
		// Same rule the loose path follows: a derivation that did not fully succeed is
		// left out of newPrint, because the print describes the file rather than
		// whether reading it worked. Whatever assets came back are still kept — a
		// package whose character assembly failed still contributes everything else.
		if skip != nil {
			skipped = append(skipped, *skip)
		}
		if skip != nil || !cacheable(e.path, a) {
			delete(newPrint, e.path)
		}
		assets = append(assets, a...)
	}
	// An archive the walk could not look at is not an archive that is gone, and live
	// cannot tell the two apart on its own: it holds what the walk reached, so a pack
	// behind a directory this run could not read loses its extraction — hundreds of MB
	// per Synty pack, and deleted out from under a second quarry that is still serving
	// from it, which --addr exists to allow. A skip the walk itself recorded is the
	// evidence that it could not look, so everything the previous index placed under one
	// keeps its extraction for this run. An unmounted drive behind a plain directory
	// leaves no skip and is indistinguishable from a deletion; that case still sweeps.
	if len(walkSkips) > 0 {
		reached := make(map[string]bool, len(entries))
		for _, e := range entries {
			if e.kind != SourceLoose {
				reached[e.path] = true
			}
		}
		for path, fp := range ix.ArchivePrint {
			if reached[path] || len(oldByArchive[path]) == 0 {
				continue
			}
			// The archive's own display rel, which is what a skip is recorded under —
			// an archive asset's RelPath carries the entry after it.
			archiveRel, _, _ := strings.Cut(prevAt(oldByArchive[path][0]).RelPath, "::")
			for _, sk := range walkSkips {
				if archiveRel == sk.RelPath || strings.HasPrefix(archiveRel, sk.RelPath+"/") {
					live[fp] = true
					break
				}
			}
		}
	}
	ix.ArchivePrint = newPrint
	ix.LoosePrint = newLoose
	ix.liveUnpacked = live
	ix.Skipped = skipped
	// The previous set is dead here: prevAt, reuse and describes are all inside the loop
	// above, and everything they kept has been copied into assets. Released before dedup
	// rather than after, because dedup appends into two fresh slices — so holding it
	// across the call is a third copy of a 150k-asset library, at the one moment two are
	// already live.
	ix.Assets, ix.Suppressed = nil, nil
	kept, dropped := dedup(assets)
	ix.setAssets(kept)
	ix.Suppressed = dropped
	return nil
}

// load reads a cached index from disk and rebuilds its id lookup. It is unexported
// because a loaded index is not yet a usable one: nothing here checks Version or
// Root, and pairing a stale schema with current code is what refresh's re-derive
// guard and LoadOrBuild's match exist to prevent. Callers want LoadOrBuild.
//
// encoding/json buffers a whole top-level value before decoding it, so a 100MB-plus
// cache is briefly resident twice. See save for why that is accepted.
func load(cachePath, cacheDir string) (*Index, error) {
	f, err := os.Open(cachePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var ix Index
	if err := json.NewDecoder(f).Decode(&ix); err != nil {
		return nil, err
	}
	ix.cacheDir = cacheDir
	if ix.ArchivePrint == nil {
		ix.ArchivePrint = map[string]string{}
	}
	if ix.LoosePrint == nil {
		ix.LoosePrint = map[string]string{}
	}
	ix.setAssets(ix.Assets)
	return &ix, nil
}

// save writes the index JSON, creating parent dirs. The write goes to a temp file
// in the destination dir and is renamed into place: an interrupted in-place write
// would leave a truncated cache, and rebuilding one costs a full library scan.
//
// encoding/json marshals a top-level value whole before writing it, so a
// 100MB-plus cache is briefly resident twice, once as the index and once as its
// JSON. That is the cost of a single self-describing document, paid once per run
// off the serving path; capping it would mean a record-per-line format and the
// migration that comes with it.
func (ix *Index) save(cachePath string) error {
	dir := filepath.Dir(cachePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return safewrite.Atomic(cachePath, ".browse-index-*", func(w io.Writer) error {
		return json.NewEncoder(w).Encode(ix)
	})
}

// stateDir is where one library's regenerable state lives: its cached index and its
// unpacked-archive tree. It is keyed by what the walk covers, because both describe
// one library. Sharing a cache dir between libraries without this key means each
// run's PruneUnpacked deletes the other's extractions — including out from under a
// second instance already serving them, which `--addr` exists to allow.
//
// The scan root is half of what the walk covers and follow is the other half: under
// it the library is the root *and* every target the walk followed, which is why
// LoadOrBuild will not reuse an index built the other way. Keyed on the root alone,
// the two settings shared one tree, and the run that did not follow pruned every
// extraction reached through a link as unreferenced.
func stateDir(cacheDir, absRoot string, follow bool) string {
	key := absRoot
	if follow {
		key += "\x00follow"
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(cacheDir, "roots", hex.EncodeToString(sum[:stateDirNameLen/2]))
}

// stateDirNameLen is how many hex characters name one root's state directory. Named
// because sweepAbandonedRoots has to recognise the layout from the outside, and a
// hand-written second copy of the length is what lets a sweep quietly stop matching.
const stateDirNameLen = 12

func (ix *Index) stateDir() string { return stateDir(ix.cacheDir, ix.Root, ix.FollowSymlinks) }

// cacheFile is where one library's index JSON lives. Empty when there is no cache
// dir, which is also the state in which nothing is written. It takes the values
// rather than an index because LoadOrBuild needs the path before it has one, and the
// layout is worth keeping a single decision.
func cacheFile(cacheDir, absRoot string, follow bool) string {
	if cacheDir == "" {
		return ""
	}
	return filepath.Join(stateDir(cacheDir, absRoot, follow), "index.json")
}

// cachePath is where this index's JSON lives.
func (ix *Index) cachePath() string { return cacheFile(ix.cacheDir, ix.Root, ix.FollowSymlinks) }

// LoadOrBuild returns a usable index: a fresh build when reindex is set or no valid
// cache exists for these options, otherwise the cached index refreshed against the
// current tree. Where the cache lives is derived from the options, so no caller can
// pair one root's index with another's path. With no cache dir the index is built
// fresh every time and nothing is written.
// warn reports a non-fatal condition; nil discards.
func LoadOrBuild(opt Options, reindex bool, warn func(string)) (*Index, error) {
	if warn == nil {
		warn = func(string) {}
	}
	absRoot, err := filepath.Abs(opt.Root)
	if err != nil {
		return nil, err
	}
	opt.Root = absRoot
	if opt.CacheDir, err = absCacheDir(opt.CacheDir); err != nil {
		return nil, err
	}
	if err := checkCacheDir(absRoot, opt.CacheDir); err != nil {
		return nil, err
	}
	cachePath := cacheFile(opt.CacheDir, absRoot, opt.FollowSymlinks)
	if !reindex && cachePath != "" {
		// FollowSymlinks is part of the match: it decides what the walk covers, so a
		// cache built the other way is describing a different library. It is also part
		// of the path, so this re-reads what the name already separated — which is what
		// catches a file moved or copied between the two trees by hand.
		if ix, err := load(cachePath, opt.CacheDir); err == nil &&
			ix.Root == absRoot && ix.Version == indexVersion && ix.FollowSymlinks == opt.FollowSymlinks {
			if err := ix.refresh(); err != nil {
				return nil, err
			}
			saveCache(ix, warn)
			return ix, nil
		}
	}
	ix, err := Build(opt)
	if err != nil {
		return nil, err
	}
	saveCache(ix, warn)
	return ix, nil
}

// saveCache persists the index. The cache is expendable and the index in hand is
// usable without it, so a write failure is not fatal — but it is not swallowed
// either: silently failing here re-pays a whole library scan on every run.
func saveCache(ix *Index, warn func(string)) {
	p := ix.cachePath()
	if p == "" {
		return
	}
	if err := ix.save(p); err != nil {
		warn(fmt.Sprintf("could not write the index cache (%v); the library will be rescanned next run", err))
	}
}

func (ix *Index) setAssets(assets []Asset) {
	ix.Assets = assets
	ix.byID = make(map[string]*Asset, len(assets))
	for i := range ix.Assets {
		ix.byID[ix.Assets[i].ID] = &ix.Assets[i]
	}
}

// Lookup resolves an asset id to its asset. This is the only path from a client id
// to a locator, so an unknown id simply misses.
func (ix *Index) Lookup(id string) (Asset, bool) {
	a, ok := ix.byID[id]
	if !ok {
		return Asset{}, false
	}
	return *a, true
}
