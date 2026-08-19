package assetindex

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNoThumbnail is returned by OpenThumbnail for an asset that has no dedicated
// thumbnail resource (only Unity preview.png assets do).
var ErrNoThumbnail = errors.New("asset has no thumbnail")

// ErrOutsideRoot guards against a stale cache pointing at a path no longer under
// the configured library root.
var ErrOutsideRoot = errors.New("asset path is outside the library root")

// Open streams an asset's bytes and its size. Loose files and zip entries are read
// directly; unitypackage assets are read from the extract-once cache. Every
// filesystem/archive path is confirmed to resolve under the library root first.
func (ix *Index) Open(a Asset) (io.ReadCloser, int64, error) {
	switch a.Source.Kind {
	case SourceLoose:
		if !ix.underRoot(a.Source.FilePath) {
			return nil, 0, ErrOutsideRoot
		}
		return openFile(a.Source.FilePath)
	case SourceZip:
		if !ix.underRoot(a.Source.ArchivePath) {
			return nil, 0, ErrOutsideRoot
		}
		return ix.openZipEntry(a.Source.ArchivePath, a.Source.Entry)
	case SourceUnityPackage:
		if !ix.underRoot(a.Source.ArchivePath) {
			return nil, 0, ErrOutsideRoot
		}
		p, err := ix.unpackedEntry(a, "asset")
		if err != nil {
			return nil, 0, err
		}
		return openFile(p)
	}
	return nil, 0, errors.New("unknown source kind")
}

// unpackedEntry is where one extracted unitypackage member sits, with the guid
// re-checked on the way. Every other branch of Open re-validates its locator before
// building a path from it (see openZipEntry), and this one has the same reason to: an
// Asset arriving here need not have come from this process's scan, since Open is
// exported and a cached index is decoded from JSON without inspection.
func (ix *Index) unpackedEntry(a Asset, member string) (string, error) {
	if !safeGuid(a.Source.Guid) {
		return "", fmt.Errorf("unsafe unitypackage guid %q", a.Source.Guid)
	}
	dir, err := ix.ensureExtracted(a.Source.ArchivePath)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, a.Source.Guid, member), nil
}

// OpenThumbnail streams a Unity preview.png for an asset that has one.
func (ix *Index) OpenThumbnail(a Asset) (io.ReadCloser, int64, error) {
	if a.Source.Kind != SourceUnityPackage || !a.Source.HasPreview {
		return nil, 0, ErrNoThumbnail
	}
	if !ix.underRoot(a.Source.ArchivePath) {
		return nil, 0, ErrOutsideRoot
	}
	p, err := ix.unpackedEntry(a, "preview.png")
	if err != nil {
		return nil, 0, err
	}
	return openFile(p)
}

func openFile(p string) (io.ReadCloser, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, fi.Size(), nil
}

// ErrNoCacheDir is returned when an index built without a cache dir is asked for
// content that has to be extracted. Falling back to a relative path would write an
// "unpacked" tree into the working directory, which may sit inside the library.
var ErrNoCacheDir = errors.New("index has no cache dir to extract into")

// unpackedDir is the root of this index version's extraction tree. The version is
// part of the path because an archive that never changed keeps its fingerprint: only
// the version distinguishes an extraction made by older code from what the current
// code would write.
func (ix *Index) unpackedDir() string {
	return filepath.Join(ix.stateDir(), "unpacked", strconv.Itoa(indexVersion))
}

// stagingDir is where an extraction is assembled before it is renamed into place. It
// deliberately sits outside the tree PruneUnpacked sweeps, because that sweep deletes
// whatever it does not recognise and a second quarry sharing this cache dir runs one
// every time it starts. Assembled inside that tree, a half-written extraction is
// removed out from under the run writing it, and the rename then publishes a package
// missing whatever had not been written yet — cached as complete from then on.
func (ix *Index) stagingDir() string {
	return filepath.Join(ix.stateDir(), "staging")
}

// staleStagingAge is how old an abandoned staging directory must be before a prune
// clears it. Anything younger could be an extraction another instance is writing
// right now, and the point of the sweep is to leave one less thing behind rather than
// to race one.
const staleStagingAge = 24 * time.Hour

// isLegacyIndex reports whether path is a cache file quarry wrote, rather than a
// file of the user's that happens to share the name. Only the head is read: the real
// thing runs to hundreds of megabytes, and the fields that identify it are at the
// front of the object.
func isLegacyIndex(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	return bytes.Contains(head[:n], []byte(`"version"`)) && bytes.Contains(head[:n], []byte(`"root"`))
}

// PruneUnpacked removes extraction directories the current index no longer
// references: every other index version's tree, plus, within this version's, every
// archive fingerprint absent from the index. The fingerprint includes the archive's
// mtime, so every pack update writes to a new directory and would otherwise strand
// the previous extraction (hundreds of MB per Synty pack) in the cache forever.
//
// Inside the swept tree it deletes whatever it does not recognise, which is why an
// extraction under way is assembled in stagingDir instead — outside that tree, where
// only one left behind long enough to be nobody's is cleared. A second quarry sharing
// this cache dir sweeps every time it starts, so the two have to be able to overlap.
func (ix *Index) PruneUnpacked() error {
	if ix.cacheDir == "" {
		return nil
	}
	var firstErr error
	remove := func(p string) {
		if err := os.RemoveAll(p); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// State was once written directly under the cache dir rather than per root, so a
	// cache written by an older quarry holds a tree nothing will ever consult again.
	//
	// Both halves of that layout have to be present before either is removed. The
	// cache dir is whatever --cache or QUARRY_CACHE_DIR named, taken verbatim, and
	// "unpacked" is a plausible name for a directory a user keeps their own work in;
	// finding one alone is not evidence quarry wrote it. Finding it beside an
	// index.json is.
	legacyUnpacked := filepath.Join(ix.cacheDir, "unpacked")
	legacyIndex := filepath.Join(ix.cacheDir, "index.json")
	if isLegacyIndex(legacyIndex) {
		if fi, err := os.Stat(legacyUnpacked); err == nil && fi.IsDir() {
			remove(legacyUnpacked)
			remove(legacyIndex)
		}
	}

	// A run killed mid-extraction leaves its staging directory behind, and nothing
	// else ever clears it. Age is the only thing separating that from an extraction
	// another instance has in flight.
	if entries, err := os.ReadDir(ix.stagingDir()); err == nil {
		for _, e := range entries {
			if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > staleStagingAge {
				remove(filepath.Join(ix.stagingDir(), e.Name()))
			}
		}
	}

	root := filepath.Join(ix.stateDir(), "unpacked")
	versions, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return firstErr
	}
	if err != nil {
		return err
	}
	current := strconv.Itoa(indexVersion)
	for _, v := range versions {
		if v.Name() != current {
			remove(filepath.Join(root, v.Name()))
		}
	}

	dir := ix.unpackedDir()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return firstErr
	}
	if err != nil {
		return err
	}
	live := make(map[string]bool, len(ix.ArchivePrint))
	for _, fp := range ix.ArchivePrint {
		live[fp] = true
	}
	for _, e := range entries {
		if !e.IsDir() || live[e.Name()] {
			continue
		}
		remove(filepath.Join(dir, e.Name()))
	}
	return firstErr
}

// extraction is one archive's single-flighted unpack. Every waiter holds the same
// pointer, so all of them read the same outcome — looking the result back up in a
// shared map would hand a nil error to whoever arrived after the first waiter
// cleared the entry to re-arm the retry.
type extraction struct {
	once sync.Once
	err  error
}

// ensureExtracted decompresses a unitypackage once into the cache and returns its
// unpacked dir. Extraction is single-flighted per archive fingerprint (concurrent
// grid fetches of the same package wait rather than each decompressing hundreds of
// MB), and is written to a temp dir renamed atomically into place so no reader ever
// sees a half-written entry.
func (ix *Index) ensureExtracted(archivePath string) (string, error) {
	if ix.cacheDir == "" {
		return "", ErrNoCacheDir
	}
	fp, err := fingerprint(archivePath)
	if err != nil {
		return "", err
	}
	dest := filepath.Join(ix.unpackedDir(), fp)
	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}

	ix.extractMu.Lock()
	if ix.extractions == nil {
		ix.extractions = map[string]*extraction{}
	}
	ex := ix.extractions[fp]
	if ex == nil {
		ex = &extraction{}
		ix.extractions[fp] = ex
	}
	ix.extractMu.Unlock()

	ex.once.Do(func() {
		if _, err := os.Stat(dest); err == nil {
			return
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			ex.err = err
			return
		}
		if err := os.MkdirAll(ix.stagingDir(), 0o755); err != nil {
			ex.err = err
			return
		}
		// Staged under the same state dir as dest, so the publishing rename stays on
		// one filesystem.
		tmp, err := os.MkdirTemp(ix.stagingDir(), "unpack-*")
		if err != nil {
			ex.err = err
			return
		}
		if err := extractUnityPackage(archivePath, tmp); err != nil {
			os.RemoveAll(tmp)
			ex.err = err
			return
		}
		if err := os.Rename(tmp, dest); err != nil {
			os.RemoveAll(tmp)
			// A racing run may have created dest first; that is success.
			if _, statErr := os.Stat(dest); statErr != nil {
				ex.err = err
			}
		}
	})

	// Cleared either way. On failure this re-arms the retry, so a transient disk-full
	// doesn't poison the package for the process lifetime. On success it means a dest
	// that later disappears — pruned by another instance sharing the cache dir — is
	// extracted again rather than reported present forever by a spent sync.Once.
	ix.extractMu.Lock()
	if ix.extractions[fp] == ex {
		delete(ix.extractions, fp)
	}
	ix.extractMu.Unlock()
	if ex.err != nil {
		return "", ex.err
	}
	return dest, nil
}

// underRoot reports whether p resolves inside what this index actually covers:
// the library root, or a directory the scan followed a symlink into. Resolving
// symlinks is what stops an unfollowed link from reaching outside; the link roots
// are what keep that check meaningful once following is on, instead of dropping it.
func (ix *Index) underRoot(p string) bool {
	// Root and LinkRoots are fixed for the index's lifetime, so they are resolved once
	// rather than re-lstat'd per side per request: a grid page is a hundred Opens.
	ix.rootsOnce.Do(func() {
		ix.resolvedRoots = make([]string, 0, 1+len(ix.LinkRoots))
		ix.resolvedRoots = append(ix.resolvedRoots, resolve(ix.Root))
		for _, lr := range ix.LinkRoots {
			ix.resolvedRoots = append(ix.resolvedRoots, resolve(lr))
		}
	})
	rp := resolve(p)
	for _, r := range ix.resolvedRoots {
		if contains(r, rp) {
			return true
		}
	}
	return false
}

// underRootPath is the containment test itself, taking the root as a parameter so
// the scan can apply the same rule to a symlink's target that Open will apply to it
// later: a link the scan indexes but Open would refuse is a card that cannot load.
func underRootPath(root, p string) bool {
	return contains(resolve(root), resolve(p))
}

// contains is the containment test over two already-resolved paths.
func contains(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolve normalizes a path for containment comparison, following symlinks as far as
// the filesystem allows and re-appending the part that does not exist yet.
//
// EvalSymlinks fails outright on a missing path, and the run a containment check has
// to catch is sometimes exactly the one where the path is not there — a cache dir
// does not exist until the run that creates it. Falling back to Abs for the whole
// path would then compare a symlink-resolved root against an unresolved candidate and
// call a directory plainly inside the root outside it.
func resolve(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	rest := ""
	for {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, rest)
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}
