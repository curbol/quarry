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
	"sync"

	"github.com/curbol/quarry/internal/safewrite"
)

// indexVersion is bumped whenever the scan logic changes (what's indexed, how it's
// classified), so a cached index from older logic is rebuilt rather than reused. It
// also keys the unpacked-archive tree, so a change to what extraction writes belongs
// here too: an archive whose bytes never changed keeps its fingerprint, and only the
// version tells the old extraction apart from what the current code would produce.
const indexVersion = 20

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

	// Suppressed holds the archive entries dedup dropped in favour of a loose twin.
	// They are cached because reuse is keyed on the archive's stat print, which does
	// not move when the twin outside it is deleted: without them a refresh would
	// reuse only the survivors and the entry would never come back.
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

	// extractMu guards both maps. extractions single-flights one archive's unpack;
	// archiveMus holds each archive's reader/rebuild lock (see archiveMu).
	extractMu   sync.Mutex
	extractions map[string]*extraction
	archiveMus  map[string]*sync.RWMutex

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
	key := path + "\x00" + strconv.FormatInt(fi.Size(), 10) + "\x00" + strconv.FormatInt(fi.ModTime().UnixNano(), 10)
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:12]), nil
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
func checkCacheDirLinks(cacheDir string, linkRoots []string) error {
	if cacheDir == "" {
		return nil
	}
	rc := resolve(cacheDir)
	for _, lr := range linkRoots {
		if contains(resolve(lr), rc) {
			return fmt.Errorf("cache dir %s is inside %s, a directory this scan follows a symlink into; pick one outside the library with --cache", cacheDir, lr)
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

// refresh re-walks the library, reusing the cached enumeration of every archive
// and the cached fingerprint of every loose file whose stat fingerprint is
// unchanged, re-deriving only changed or new files. This avoids re-decompressing
// every unitypackage and re-reading every loose file's bytes on each run.
//
// Reuse is only sound for entries derived by this version's scan logic, so an index
// carrying another version's is emptied first and re-derived whole. Enforcing that
// here rather than at the call site is what keeps a caller that reaches for Load and
// Refresh directly from silently merging two schemes' assets into one index.
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
	for _, e := range entries {
		fp, err := fingerprint(e.path)
		if err != nil {
			skipped = append(skipped, SkippedFile{RelPath: e.rel, Reason: err.Error()})
			continue
		}
		if e.kind == SourceLoose {
			if ix.LoosePrint[e.path] == fp {
				newLoose[e.path] = fp
				reuse(oldByLoose[e.path])
				continue
			}
			a, skip := looseAssets(e)
			// A failed derivation is deliberately left out of newLoose: the stat print
			// describes the file, not whether reading it worked, so caching one would
			// keep serving the degraded result even after the cause was fixed.
			if skip != nil {
				skipped = append(skipped, *skip)
			} else {
				newLoose[e.path] = fp
			}
			assets = append(assets, a...)
			continue
		}
		newPrint[e.path] = fp
		// Keyed on the print alone, not on having cached assets: an archive whose every
		// entry was dropped by dedup leaves no assets behind, and demanding some would
		// re-decompress it on every single run.
		if ix.ArchivePrint[e.path] == fp {
			reuse(oldByArchive[e.path])
			continue
		}
		a, skip := archiveAssets(e)
		// Same rule the loose path follows: a derivation that did not fully succeed is
		// left out of newPrint, because the print describes the file rather than
		// whether reading it worked. Whatever assets came back are still kept — a
		// package whose character assembly failed still contributes everything else.
		if skip != nil {
			delete(newPrint, e.path)
			skipped = append(skipped, *skip)
		}
		assets = append(assets, a...)
	}
	ix.ArchivePrint = newPrint
	ix.LoosePrint = newLoose
	ix.Skipped = skipped
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
// unpacked-archive tree. It is keyed by scan root because both describe one library.
// Sharing a cache dir between roots without this key means each run's PruneUnpacked
// deletes the other root's extractions — including out from under a second instance
// already serving them, which `--addr` exists to allow.
func stateDir(cacheDir, absRoot string) string {
	sum := sha256.Sum256([]byte(absRoot))
	return filepath.Join(cacheDir, "roots", hex.EncodeToString(sum[:6]))
}

func (ix *Index) stateDir() string { return stateDir(ix.cacheDir, ix.Root) }

// cacheFile is where one library's index JSON lives. Empty when there is no cache
// dir, which is also the state in which nothing is written. It takes the two values
// rather than an index because LoadOrBuild needs the path before it has one, and the
// layout is worth keeping a single decision.
func cacheFile(cacheDir, absRoot string) string {
	if cacheDir == "" {
		return ""
	}
	return filepath.Join(stateDir(cacheDir, absRoot), "index.json")
}

// cachePath is where this index's JSON lives.
func (ix *Index) cachePath() string { return cacheFile(ix.cacheDir, ix.Root) }

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
	if err := checkCacheDir(absRoot, opt.CacheDir); err != nil {
		return nil, err
	}
	cachePath := cacheFile(opt.CacheDir, absRoot)
	if !reindex && cachePath != "" {
		// FollowSymlinks is part of the match: it decides what the walk covers, so a
		// cache built the other way is describing a different library.
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
