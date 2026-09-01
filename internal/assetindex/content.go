package assetindex

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
		return ix.openUnpacked(a, "asset", a.Size)
	}
	return nil, 0, errors.New("unknown source kind")
}

// openUnpacked serves one extracted unitypackage member, rebuilding the extraction
// once if what is on disk is not the size the scan read out of the archive.
//
// An extraction is published by renaming a staged tree into place, which survives a
// crashing process but not a crashing machine: the rename can reach the journal with
// the data blocks still unwritten, and the members come back short. Nothing else would
// notice — the fast path is a stat of the directory, and the directory is named for
// the archive's fingerprint, which does not move for a file whose bytes did not change
// — so a pack torn that way would serve empty models until the cache dir was deleted
// by hand. Not even --reindex clears it.
//
// The check is a stat openFile already does. Fsyncing every member at extraction time
// would close the same hole, but a Synty package holds tens of thousands of them and
// the extraction is on the path a user is waiting on.
//
// want is the size the scan recorded; -1 for a member the index does not size.
func (ix *Index) openUnpacked(a Asset, member string, want int64) (io.ReadCloser, int64, error) {
	rc, n, err := ix.openUnpackedMember(a, member)
	// Only a member that opened and disagreed is evidence of a torn tree. A missing one
	// is an ordinary miss — a guid this package does not hold — and re-extracting
	// hundreds of MB to confirm that would be its own bug.
	if err != nil || !tornMember(n, want) {
		return rc, n, err
	}
	rc.Close()
	fp, err := fingerprint(a.Source.ArchivePath)
	if err != nil {
		return nil, 0, err
	}
	// Two things produce a disagreement here and only one of them is repairable. An
	// archive replaced in place since the scan — the ordinary way a pack is updated —
	// extracts correctly under its new print while the size this asset carries is the
	// old one, so a rebuild reproduces the disagreement every time. Left to the repair
	// path that is a full re-extraction per request, forever. Report it as the miss it
	// is instead, the way openZipEntry reports an entry the archive stopped carrying,
	// and leave the tree alone. An archive with no recorded print took a degraded
	// enumeration, which says nothing either way, so it keeps the repair.
	if recorded, known := ix.ArchivePrint[a.Source.ArchivePath]; known && recorded != fp {
		return nil, 0, fmt.Errorf("%s changed since it was indexed: %w", filepath.Base(a.Source.ArchivePath), fs.ErrNotExist)
	}
	// Repaired once per extraction. The repair costs a full decompress, and a second
	// disagreement after one means the cause was never the tree, so retrying would
	// spend that cost on every request for the rest of the run — and each discard
	// deletes the tree this archive's healthy members are being served from.
	if !ix.claimRebuild(fp) {
		return nil, 0, tornError(a, member, n, want)
	}
	if err := ix.discardExtraction(a.Source.ArchivePath); err != nil {
		return nil, 0, err
	}
	rc, n, err = ix.openUnpackedMember(a, member)
	if err == nil && tornMember(n, want) {
		rc.Close()
		return nil, 0, tornError(a, member, n, want)
	}
	return rc, n, err
}

// tornMember reports whether an extracted member's size is evidence that its tree was
// published before the bytes reached the disk. want is the size the scan recorded; for
// a member the index does not size, emptiness is the evidence instead, since no member
// a unitypackage ships is empty and a preview.png is at minimum its 8-byte signature.
// Without that, the one member with nothing to compare against is also the one the
// rebuild can never reach: the fast path is a stat of a directory named for a print
// that does not move, so a blank preview would outlive even --reindex.
func tornMember(n, want int64) bool {
	if want < 0 {
		return n == 0
	}
	return n != want
}

func tornError(a Asset, member string, n, want int64) error {
	if want < 0 {
		return fmt.Errorf("extracted %s %s is empty", a.RelPath, member)
	}
	return fmt.Errorf("extracted %s is %d bytes, want %d", a.RelPath, n, want)
}

// claimRebuild reports whether this caller should rebuild the extraction named by fp,
// claiming it so no later one repeats the work. See openUnpacked for why it is once.
func (ix *Index) claimRebuild(fp string) bool {
	ix.extractMu.Lock()
	defer ix.extractMu.Unlock()
	if ix.rebuilt[fp] {
		return false
	}
	if ix.rebuilt == nil {
		ix.rebuilt = map[string]bool{}
	}
	ix.rebuilt[fp] = true
	return true
}

// openUnpackedMember opens one extracted member, holding the archive's read lock from
// the extraction check through the open. A discard takes the same lock exclusively, so
// a reader can no longer pass ensureExtracted's stat of a published tree and then open
// a path a concurrent discard has already removed. That window was not an instruction
// gap but the whole duration of the removal, which over a package holding tens of
// thousands of members answered 404 for sibling after sibling whose own bytes were
// never torn.
//
// The lock is released once the file is open rather than held for the stream: the
// descriptor keeps the member's bytes readable through an unlink, so a rebuild landing
// mid-read costs the reader nothing.
func (ix *Index) openUnpackedMember(a Asset, member string) (io.ReadCloser, int64, error) {
	mu := ix.archiveMu(a.Source.ArchivePath)
	mu.RLock()
	defer mu.RUnlock()
	p, err := ix.unpackedEntry(a, member)
	if err != nil {
		return nil, 0, err
	}
	return openFile(p)
}

// archiveMu is the lock serialising one archive's readers against its rebuild. It is
// keyed on the archive path rather than on the fingerprint naming the extraction
// directory, because what has to be serialised is this archive's readers against this
// archive's discard, and the fingerprint is a stat that can move under a file being
// replaced while the lock is held.
func (ix *Index) archiveMu(archivePath string) *sync.RWMutex {
	ix.extractMu.Lock()
	defer ix.extractMu.Unlock()
	if ix.archiveMus == nil {
		ix.archiveMus = map[string]*sync.RWMutex{}
	}
	mu := ix.archiveMus[archivePath]
	if mu == nil {
		mu = &sync.RWMutex{}
		ix.archiveMus[archivePath] = mu
	}
	return mu
}

// discardExtraction removes a published extraction so the next request rebuilds it,
// under the archive's write lock so no reader is between its extraction check and its
// open while the tree goes away. ensureExtracted clears its single-flight entry after
// every call precisely so a dest that disappears is extracted again rather than
// reported present by a spent Once.
func (ix *Index) discardExtraction(archivePath string) error {
	fp, err := fingerprint(archivePath)
	if err != nil {
		return err
	}
	mu := ix.archiveMu(archivePath)
	mu.Lock()
	defer mu.Unlock()
	return os.RemoveAll(filepath.Join(ix.unpackedDir(), fp))
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
	// A preview's size is not indexed — nothing reads it during enumeration — so there
	// is nothing to check it against.
	return ix.openUnpacked(a, "preview.png", -1)
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
//
// The keep-set is read off this index, so it may only be called on one a full Build or
// LoadOrBuild just produced for this root. Over an index anything narrowed — filtered,
// truncated, half-populated — it deletes the extractions of everything that was
// removed, and those are live for whoever is still serving them.
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
	// Keyed on what the index references, not on what it is willing to reuse. A
	// refresh whose second pass over an archive failed drops it from ArchivePrint while
	// keeping the assets its first pass produced, so a keep-set built from the print
	// alone deletes the extraction those assets are served from — re-decompressing the
	// package on every run, and, with a second quarry sharing this cache dir, deleting
	// it out from under one already serving it.
	live := make(map[string]bool, len(ix.ArchivePrint))
	for _, fp := range ix.ArchivePrint {
		live[fp] = true
	}
	referenced := map[string]bool{}
	for i := range ix.Assets {
		p := ix.Assets[i].Source.ArchivePath
		if ix.Assets[i].Source.Kind != SourceUnityPackage || p == "" || referenced[p] {
			continue
		}
		referenced[p] = true
		if fp, err := fingerprint(p); err == nil {
			live[fp] = true
		}
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
