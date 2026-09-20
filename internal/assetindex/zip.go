package assetindex

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

// openZip opens an archive for reading. archive/zip hands back a usable reader
// alongside ErrInsecurePath — for an entry with a non-local name or a backslash in it,
// which is what older Windows zip tooling emits — and the stdlib says outright that a
// program willing to accept such names should ignore the error and use the reader.
// entryPath and safeEntry are that willingness: the backslash spelling is read as the
// path it is, a name that still escapes its archive after that is dropped, and the rest
// of the archive is kept either way.
//
// Treating it as a failure instead dropped every safe entry in the archive too, and
// leaked the returned reader's descriptor, once per archive per scan and once per
// content request. It needs GODEBUG=zipinsecurepath=0 today; the stdlib documents that
// a future Go may make it the default, at which point the whole library goes with it.
func openZip(archivePath string) (*zip.ReadCloser, error) {
	zr, err := zip.OpenReader(archivePath)
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		return nil, err
	}
	return zr, nil
}

// entryPath reads an archive entry name as the path it means. archive/zip stores
// whatever the writer put in the header, and older Windows tooling writes "\" as the
// separator — the spelling archive/zip itself flags as insecure. Taken literally such a
// name is one long segment, and every rule that reads an entry as a path then misses:
// the card is named for its whole internal path, textures and UI files land in the
// plain image facet because the classifier's boundaries are "/", "_" and ":", a
// dot-directory inside it is not recognised as one so the packed tree indexes
// differently from the extracted one, and no loose twin can ever produce the same dedup
// key. Normalizing once here is what keeps all four reading the same path.
//
// Source.Entry keeps the stored spelling, because that is the key the central directory
// resolves; Source.EntryPath is this, and is what everything treating an entry as a
// path uses.
func entryPath(name string) string { return strings.ReplaceAll(name, `\`, "/") }

// safeEntry rejects archive entry names that are absolute or escape their archive
// via "..". Such names never enter the index, so the content API can never be
// tricked into serving a path outside the archive. It is applied to entryPath's
// reading of a name, never the raw one: a "..\..\x" written the Windows way is the
// same escape as "../../x" and has to be refused as one.
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

// isDir reports a directory entry, over both spellings a writer may use to mark one.
// p is the entry read as a path; f carries the MS-DOS attribute word, which is the
// half archive/zip can read on its own.
func isDir(f *zip.File, p string) bool {
	return f.FileInfo().IsDir() || strings.HasSuffix(p, "/")
}

// zipAssets enumerates the files inside a .zip as assets. Directory entries and
// unsafe names are skipped. displayRel is the archive's path relative to the
// library root (for RelPath); archivePath is absolute (for CopyPath and Open).
func zipAssets(archivePath, displayRel, vendor, pack, variant string) ([]Asset, error) {
	zr, err := openZip(archivePath)
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
		// Deduped on the stored name rather than on the path it reads as: two entries
		// spelled differently are two distinct members, each retrievable by its own
		// exact name, and collapsing them would lose one.
		p := entryPath(f.Name)
		// isDir asks the normalised path, not the stored name, for the same reason
		// every other rule on this line does: archive/zip reads a directory off the
		// MS-DOS attribute word or a trailing "/" in the stored name, and neither
		// fires for the backslash spelling entryPath exists to handle. Such an entry
		// became a card named for its last segment, sized 0, classified "other", and
		// fingerprinted crc32:0:0 — the print every genuinely empty entry in the
		// library shares, so tagging the phantom tagged all of them.
		if isDir(f, p) || !safeEntry(p) || skipEntry(p) || seen[f.Name] {
			continue
		}
		seen[f.Name] = true
		src := Source{Kind: SourceZip, ArchivePath: archivePath, Entry: f.Name}
		a := newAsset(src,
			path.Base(p),
			archiveRel(displayRel, p),
			vendor, pack, variant,
			int64(f.UncompressedSize64),
			crcFingerprint(f.CRC32, int64(f.UncompressedSize64)),
		)
		setImageDims(&a, f.Open)
		assets = append(assets, a)
	}
	return assets, nil
}

// openZipEntry streams one entry's bytes by exact-name match. The name comes from
// an indexed asset (never raw client input), and is re-validated defensively.
func (ix *Index) openZipEntry(archivePath, entry string) (io.ReadCloser, int64, error) {
	if !safeEntry(entryPath(entry)) {
		return nil, 0, fmt.Errorf("unsafe zip entry %q", entry)
	}
	ref, err := ix.zips.acquire(archivePath)
	if err != nil {
		return nil, 0, err
	}
	f := ref.byName[entry]
	if f == nil {
		ix.zips.release(ref)
		// Wrapped so this reads as the miss it is: the loose and unpacked branches of
		// Open return an fs.ErrNotExist from the filesystem, and browse tells a miss
		// from a real failure by that alone. Bare, an entry the archive stopped
		// carrying since the scan answered 500 where its siblings answer 404.
		return nil, 0, fmt.Errorf("entry %q not found in %s: %w", entry, filepath.Base(archivePath), fs.ErrNotExist)
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
//
// A cached reader is only reused while the archive it was opened from is still the one
// on disk, which is what the stat print records. A pack re-shipped in place keeps its
// path and its inode, so without that check the cached central directory would go on
// resolving entry names to offsets in a file that no longer has that shape: the entry
// removed outright came back as an empty body under the old entry's Content-Length,
// with no error anywhere. That is the same silent-wrong-bytes failure openUnpacked
// guards against with its size check, arriving by a different route.
type zipReaders struct {
	mu    sync.Mutex
	open  map[string]*zipRef
	order []string // least recently acquired first
}

type zipRef struct {
	rc      *zip.ReadCloser
	byName  map[string]*zip.File
	print   string // the archive's stat print when this was opened
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
	// Stat outside the lock: it is I/O, and every other content request would wait on
	// it. A racing writer can move the file between here and the lookup below, which
	// costs a reopen on the next request and never a stale read.
	print, err := fingerprint(path)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if ref := c.open[path]; ref != nil {
		if ref.print == print {
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
		// The archive moved under the cache. Retire the old reader rather than closing
		// it: a stream over the bytes it describes may still be in flight.
		c.retireLocked(path)
	}
	ref := &zipRef{refs: 1, print: print, ready: make(chan struct{})}
	if c.open == nil {
		c.open = map[string]*zipRef{}
	}
	c.open[path] = ref
	c.order = append(c.order, path)
	c.evictLocked()
	c.mu.Unlock()

	zr, openErr := openZip(path)
	err = openErr
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

// retireLocked unpublishes an archive so the next acquire opens it afresh, and closes
// it only if nothing is reading it. A reader still holding it closes it on release.
func (c *zipReaders) retireLocked(path string) {
	ref := c.open[path]
	delete(c.open, path)
	c.dropOrderLocked(path)
	if ref == nil {
		return
	}
	ref.evicted = true
	if ref.refs == 0 && ref.rc != nil {
		ref.rc.Close()
	}
}

func (c *zipReaders) evictLocked() {
	for len(c.order) > zipCacheSize {
		c.retireLocked(c.order[0])
	}
}
