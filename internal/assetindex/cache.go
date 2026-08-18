package assetindex

import (
	"bufio"
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
const indexVersion = 16

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
type Index struct {
	Version      int               `json:"version"`
	Root         string            `json:"root"`
	Assets       []Asset           `json:"assets"`
	ArchivePrint map[string]string `json:"archivePrint"` // abs archive path -> stat fingerprint
	LoosePrint   map[string]string `json:"loosePrint"`   // abs loose path -> stat fingerprint
	Skipped      []SkippedFile     `json:"skipped,omitempty"`

	// FollowSymlinks is the setting this index was built under, kept because it
	// changes what the scan covers: a cache built the other way describes a
	// different library. LinkRoots are the resolved targets it followed, and with
	// Root they bound every path serving will open.
	FollowSymlinks bool     `json:"followSymlinks,omitempty"`
	LinkRoots      []string `json:"linkRoots,omitempty"`

	cacheDir string
	byID     map[string]*Asset

	extractMu   sync.Mutex
	extractions map[string]*extraction

	zips zipReaders
}

// fingerprint identifies a file by path, size, and mtime, so a re-download or edit
// invalidates any cached enumeration or extraction of it.
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

// Build scans a library from scratch into a fresh index. It is Refresh over an
// empty index: with nothing cached to reuse, every entry takes the re-derive path.
func Build(opt Options) (*Index, error) {
	absRoot, err := filepath.Abs(opt.Root)
	if err != nil {
		return nil, err
	}
	ix := &Index{
		Version: indexVersion, Root: absRoot, cacheDir: opt.CacheDir,
		FollowSymlinks: opt.FollowSymlinks,
		ArchivePrint:   map[string]string{}, LoosePrint: map[string]string{},
	}
	if err := ix.Refresh(); err != nil {
		return nil, err
	}
	return ix, nil
}

// Refresh re-walks the library, reusing the cached enumeration of every archive
// and the cached fingerprint of every loose file whose stat fingerprint is
// unchanged, re-deriving only changed or new files. This avoids re-decompressing
// every unitypackage and re-reading every loose file's bytes on each run.
//
// Reuse is only sound for entries derived by this version's scan logic, so an index
// carrying another version's is emptied first and re-derived whole. Enforcing that
// here rather than at the call site is what keeps a caller that reaches for Load and
// Refresh directly from silently merging two schemes' assets into one index.
func (ix *Index) Refresh() error {
	if ix.Version != indexVersion {
		ix.Assets, ix.ArchivePrint, ix.LoosePrint = nil, map[string]string{}, map[string]string{}
		ix.Version = indexVersion
	}
	entries, skipped, linkRoots, err := walkLibrary(ix.Root, ix.FollowSymlinks)
	if err != nil {
		return err
	}
	ix.LinkRoots = linkRoots
	oldByArchive := map[string][]Asset{}
	oldByLoose := map[string][]Asset{}
	for _, a := range ix.Assets {
		if a.Source.Kind == SourceLoose {
			oldByLoose[a.Source.FilePath] = append(oldByLoose[a.Source.FilePath], a)
		} else {
			oldByArchive[a.Source.ArchivePath] = append(oldByArchive[a.Source.ArchivePath], a)
		}
	}
	newPrint := map[string]string{}
	newLoose := map[string]string{}
	var assets []Asset
	for _, e := range entries {
		if e.kind == SourceLoose {
			if fp, err := fingerprint(e.path); err == nil {
				newLoose[e.path] = fp
				if old, ok := oldByLoose[e.path]; ok && ix.LoosePrint[e.path] == fp {
					assets = append(assets, old...)
					continue
				}
			}
			assets = append(assets, looseAssets(e)...)
			continue
		}
		fp, err := fingerprint(e.path)
		if err != nil {
			skipped = append(skipped, SkippedFile{RelPath: e.rel, Reason: err.Error()})
			continue
		}
		newPrint[e.path] = fp
		if old, ok := oldByArchive[e.path]; ok && ix.ArchivePrint[e.path] == fp {
			assets = append(assets, old...)
			continue
		}
		a, skip := archiveAssets(e)
		if skip != nil {
			delete(newPrint, e.path)
			skipped = append(skipped, *skip)
			continue
		}
		assets = append(assets, a...)
	}
	ix.ArchivePrint = newPrint
	ix.LoosePrint = newLoose
	ix.Skipped = skipped
	ix.setAssets(dedup(assets))
	return nil
}

// Load reads a cached index from disk and rebuilds its id lookup. The JSON is
// decoded as a stream: a full-library cache runs past 100MB, and reading it whole
// first would hold those bytes alongside the index they decode into.
func Load(cachePath, cacheDir string) (*Index, error) {
	f, err := os.Open(cachePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var ix Index
	if err := json.NewDecoder(bufio.NewReader(f)).Decode(&ix); err != nil {
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

// Save writes the index JSON, creating parent dirs. The write goes to a temp file
// in the destination dir and is renamed into place: an interrupted in-place write
// would leave a truncated cache, and rebuilding one costs a full library scan. The
// JSON is encoded as a stream, so a 100MB-plus cache is never also held whole in
// memory beside the index it came from.
func (ix *Index) Save(cachePath string) error {
	dir := filepath.Dir(cachePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return safewrite.Atomic(cachePath, ".browse-index-*", func(w io.Writer) error {
		buf := bufio.NewWriter(w)
		if err := json.NewEncoder(buf).Encode(ix); err != nil {
			return err
		}
		return buf.Flush()
	})
}

// LoadOrBuild returns a usable index: a fresh build when reindex is set or no valid
// cache exists for these options, otherwise the cached index refreshed against the
// current tree. The result is written back to cachePath.
// warn reports a non-fatal condition; nil discards.
func LoadOrBuild(opt Options, cachePath string, reindex bool, warn func(string)) (*Index, error) {
	if warn == nil {
		warn = func(string) {}
	}
	absRoot, err := filepath.Abs(opt.Root)
	if err != nil {
		return nil, err
	}
	opt.Root = absRoot
	if !reindex {
		// FollowSymlinks is part of the match: it decides what the walk covers, so a
		// cache built the other way is describing a different library.
		if ix, err := Load(cachePath, opt.CacheDir); err == nil &&
			ix.Root == absRoot && ix.Version == indexVersion && ix.FollowSymlinks == opt.FollowSymlinks {
			if err := ix.Refresh(); err != nil {
				return nil, err
			}
			saveCache(ix, cachePath, warn)
			return ix, nil
		}
	}
	ix, err := Build(opt)
	if err != nil {
		return nil, err
	}
	saveCache(ix, cachePath, warn)
	return ix, nil
}

// saveCache persists the index. The cache is expendable and the index in hand is
// usable without it, so a write failure is not fatal — but it is not swallowed
// either: silently failing here re-pays a whole library scan on every run.
func saveCache(ix *Index, cachePath string, warn func(string)) {
	if err := ix.Save(cachePath); err != nil {
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
