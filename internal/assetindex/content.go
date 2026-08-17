package assetindex

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
		dir, err := ix.ensureExtracted(a.Source.ArchivePath)
		if err != nil {
			return nil, 0, err
		}
		return openFile(filepath.Join(dir, a.Source.Guid, "asset"))
	}
	return nil, 0, errors.New("unknown source kind")
}

// OpenThumbnail streams a Unity preview.png for an asset that has one.
func (ix *Index) OpenThumbnail(a Asset) (io.ReadCloser, int64, error) {
	if a.Source.Kind != SourceUnityPackage || !a.Source.HasPreview {
		return nil, 0, ErrNoThumbnail
	}
	if !ix.underRoot(a.Source.ArchivePath) {
		return nil, 0, ErrOutsideRoot
	}
	dir, err := ix.ensureExtracted(a.Source.ArchivePath)
	if err != nil {
		return nil, 0, err
	}
	return openFile(filepath.Join(dir, a.Source.Guid, "preview.png"))
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
	return filepath.Join(ix.cacheDir, "unpacked", strconv.Itoa(indexVersion))
}

// PruneUnpacked removes extraction directories the current index no longer
// references: every other index version's tree, plus, within this version's, every
// archive fingerprint absent from the index. The fingerprint includes the archive's
// mtime, so every pack update writes to a new directory and would otherwise strand
// the previous extraction (hundreds of MB per Synty pack) in the cache forever.
//
// It deletes whatever it does not recognize, including an in-flight "unpack-*" temp
// dir, so it must run before the server starts serving rather than alongside it.
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

	root := filepath.Join(ix.cacheDir, "unpacked")
	versions, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
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
		tmp, err := os.MkdirTemp(filepath.Dir(dest), "unpack-*")
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

	if ex.err != nil {
		// Re-arm so a later request retries: a failure (e.g. transient disk-full)
		// shouldn't poison this package for the whole process lifetime.
		ix.extractMu.Lock()
		if ix.extractions[fp] == ex {
			delete(ix.extractions, fp)
		}
		ix.extractMu.Unlock()
	}
	return dest, ex.err
}

// underRoot reports whether p resolves to a location inside the library root,
// following symlinks so a symlinked entry cannot escape.
func (ix *Index) underRoot(p string) bool {
	root := resolve(ix.Root)
	rp := resolve(p)
	rel, err := filepath.Rel(root, rp)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolve returns the symlink-resolved absolute path, falling back to a lexical
// clean when the path (or a parent) can't be resolved.
func resolve(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	return abs
}
