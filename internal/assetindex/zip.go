package assetindex

import (
	"archive/zip"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

// safeEntry rejects archive entry names that are absolute or escape their archive
// via "..". Such names never enter the index, so the content API can never be
// tricked into serving a path outside the archive.
func safeEntry(name string) bool {
	if name == "" || path.IsAbs(name) || strings.HasPrefix(name, "/") {
		return false
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// zipAssets enumerates the files inside a .zip as assets. Directory entries and
// unsafe names are skipped. displayRel is the archive's path relative to the
// library root (for RelPath); archivePath is absolute (for CopyPath and Open).
func zipAssets(archivePath, displayRel, vendor, pack, variant string) ([]Asset, error) {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, fmt.Errorf("open zip %s: %w", archivePath, err)
	}
	defer zr.Close()

	var assets []Asset
	// A zip may legally repeat an entry name, and naive writers do. Serving resolves a
	// name to the first match, so enumerating both would give two cards one id and one
	// set of bytes — the second carrying a fingerprint for content it never serves,
	// and so tagging what the user is not looking at.
	seen := make(map[string]bool, len(zr.File))
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !safeEntry(f.Name) || skipEntry(f.Name) || seen[f.Name] {
			continue
		}
		seen[f.Name] = true
		src := Source{Kind: SourceZip, ArchivePath: archivePath, Entry: f.Name}
		a := newAsset(src,
			path.Base(f.Name),
			archiveRel(displayRel, f.Name),
			vendor, pack, variant,
			int64(f.UncompressedSize64),
			crcFingerprint(f.CRC32, int64(f.UncompressedSize64)),
		)
		if isDimExt(a.Ext) {
			if rc, err := f.Open(); err == nil {
				a.Width, a.Height = imageDims(readHead(rc), a.Ext)
				rc.Close()
			}
		}
		assets = append(assets, a)
	}
	return assets, nil
}

// openZipEntry streams one entry's bytes by exact-name match. The name comes from
// an indexed asset (never raw client input), and is re-validated defensively.
func (ix *Index) openZipEntry(archivePath, entry string) (io.ReadCloser, int64, error) {
	if !safeEntry(entry) {
		return nil, 0, fmt.Errorf("unsafe zip entry %q", entry)
	}
	ref, err := ix.zips.acquire(archivePath)
	if err != nil {
		return nil, 0, err
	}
	f := ref.byName[entry]
	if f == nil {
		ix.zips.release(ref)
		return nil, 0, fmt.Errorf("entry %q not found in %s", entry, filepath.Base(archivePath))
	}
	rc, err := f.Open()
	if err != nil {
		ix.zips.release(ref)
		return nil, 0, err
	}
	return &zipEntryReader{rc: rc, ref: ref, cache: &ix.zips}, int64(f.UncompressedSize64), nil
}

// zipEntryReader holds the archive open for the entry's lifetime, releasing the
// cache's reference when the stream closes.
type zipEntryReader struct {
	rc    io.ReadCloser
	ref   *zipRef
	cache *zipReaders
	// once guards the refcount decrement. io.Closer permits a second Close, and a
	// second release here would drive the count negative — past the zero the eviction
	// path waits for, leaking the archive's descriptor for the process lifetime.
	once sync.Once
}

func (r *zipEntryReader) Read(p []byte) (int, error) { return r.rc.Read(p) }
func (r *zipEntryReader) Close() error {
	var err error
	r.once.Do(func() {
		err = r.rc.Close()
		r.cache.release(r.ref)
	})
	return err
}

// zipCacheSize is how many archives stay open. A grid page draws from a handful of
// packs at a time, so a small window covers it; the cost of each slot is one file
// descriptor plus that archive's parsed central directory.
const zipCacheSize = 8

// zipReaders keeps the parsed central directory of recently-served archives. A Synty
// pack zip holds tens of thousands of entries and a grid page fetches a content
// request per card, so re-reading the whole directory to stream a few kilobytes out
// of it is the dominant cost of serving from a zip.
//
// Readers are handed out under a reference count: eviction unpublishes an archive
// immediately but closes the file only once the last stream over it has finished, so
// a reader is never closed out from under a response in flight.
type zipReaders struct {
	mu    sync.Mutex
	open  map[string]*zipRef
	order []string // least recently acquired first
}

type zipRef struct {
	rc      *zip.ReadCloser
	byName  map[string]*zip.File
	refs    int
	evicted bool
	// ready is closed once rc/byName are populated (or err is set). It lets acquire
	// publish a slot and then open the archive with the cache mutex released: parsing
	// a pack zip's central directory is the expensive part, and holding the lock
	// across it stalls every other content request, including ones already cached.
	ready chan struct{}
	err   error
}

func (c *zipReaders) acquire(path string) (*zipRef, error) {
	c.mu.Lock()
	if ref := c.open[path]; ref != nil {
		ref.refs++
		c.touchLocked(path)
		c.mu.Unlock()
		<-ref.ready
		if ref.err != nil {
			c.release(ref)
			return nil, ref.err
		}
		return ref, nil
	}
	ref := &zipRef{refs: 1, ready: make(chan struct{})}
	if c.open == nil {
		c.open = map[string]*zipRef{}
	}
	c.open[path] = ref
	c.order = append(c.order, path)
	c.evictLocked()
	c.mu.Unlock()

	zr, err := zip.OpenReader(path)
	if err == nil {
		byName := make(map[string]*zip.File, len(zr.File))
		for _, f := range zr.File {
			// First wins, matching the order a scan of zr.File would have found them in.
			if _, dup := byName[f.Name]; !dup {
				byName[f.Name] = f
			}
		}
		ref.rc, ref.byName = zr, byName
	} else {
		ref.err = err
	}
	close(ref.ready)
	if err != nil {
		// Unpublish, so a later request retries rather than inheriting the failure.
		c.mu.Lock()
		if c.open[path] == ref {
			delete(c.open, path)
			c.dropOrderLocked(path)
		}
		c.mu.Unlock()
		c.release(ref)
		return nil, err
	}
	return ref, nil
}

func (c *zipReaders) release(ref *zipRef) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ref.refs--
	if ref.refs == 0 && ref.evicted && ref.rc != nil {
		ref.rc.Close()
	}
}

func (c *zipReaders) touchLocked(path string) {
	for i, p := range c.order {
		if p == path {
			c.order = append(append(c.order[:i:i], c.order[i+1:]...), path)
			return
		}
	}
}

func (c *zipReaders) dropOrderLocked(path string) {
	for i, p := range c.order {
		if p == path {
			c.order = append(c.order[:i:i], c.order[i+1:]...)
			return
		}
	}
}

func (c *zipReaders) evictLocked() {
	for len(c.order) > zipCacheSize {
		oldest := c.order[0]
		c.order = c.order[1:]
		ref := c.open[oldest]
		delete(c.open, oldest)
		if ref == nil {
			continue
		}
		ref.evicted = true
		if ref.refs == 0 && ref.rc != nil {
			ref.rc.Close()
		}
	}
}
