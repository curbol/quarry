package assetindex

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// versionSuffix matches a trailing Synty version token like "_v3" or "_v1_1_3".
var versionSuffix = regexp.MustCompile(`_v[0-9][0-9_]*$`)

// deriveVariant extracts the engine/format token from a Synty archive filename,
// whose base name is prefixed by its pack dir and suffixed by a version token:
// "<packDir>_<variant>_v<ver>.<ext>" → "<variant>". Returns "" when the
// convention doesn't hold (e.g. kevdev "Human Basic Motions.zip"), leaving the
// asset in the unknown-variant facet bucket.
func deriveVariant(packDir, filename string) string {
	base := strings.TrimSuffix(filename, filepath.Ext(filename))
	prefix := packDir + "_"
	if !strings.HasPrefix(base, prefix) {
		return ""
	}
	return versionSuffix.ReplaceAllString(base[len(prefix):], "")
}

// isSidecar reports extensions that are engine bookkeeping, not browseable assets
// (Unity .meta, Godot .import), so they never clutter the index.
func isSidecar(ext string) bool {
	return ext == "meta" || ext == "import"
}

// skipEntry reports archive entries that aren't browseable assets: dot-files
// (.editorconfig, .gitignore, …) and engine sidecars, matching the loose-file walk.
//
// Every segment is checked, not just the last: the loose walk drops a dot-directory
// with everything under it, so an archive carrying .git/config or
// SourceFiles/.vscode/settings.json has to lose them the same way, or the same tree
// indexes differently depending on whether it shipped packed or extracted.
func skipEntry(name string) bool {
	for _, seg := range strings.Split(name, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return isSidecar(strings.ToLower(strings.TrimPrefix(path.Ext(path.Base(name)), ".")))
}

// libEntry is one file discovered by walking the library: either an archive (to
// be enumerated into many assets) or a loose file (one asset). Sidecars, dot-files
// and dot-dirs are already filtered out.
type libEntry struct {
	kind    SourceKind // SourceZip / SourceUnityPackage / SourceLoose
	path    string     // absolute
	rel     string     // root-relative, slash form
	vendor  string
	pack    string
	name    string
	variant string // archives only
	size    int64  // loose only
}

// walkLibrary enumerates the browseable files under absRoot without opening any
// archive, so callers can decide per archive whether to re-enumerate or reuse a
// cached result. Dot-dirs (Synty working dirs) and engine sidecars are skipped. It
// also returns the resolved targets of every symlinked directory it followed, which
// bound what serving will later hand out (see Index.underRoot).
//
// A directory or file the walk cannot read is reported as a skip and the walk goes
// on: one unreadable corner of a large library must not cost the whole index, the
// same bargain archiveAssets strikes for a damaged archive. An unreadable root is
// the exception — that is not a partial library, it is no library — so it still
// fails, rather than quietly indexing nothing.
func walkLibrary(absRoot string, follow bool) ([]libEntry, []SkippedFile, []string, error) {
	// The walk starts at the resolved root, not the configured one: WalkDir lstats
	// its argument, so a root that is itself a symlink to the library would report
	// as a non-directory and the whole walk would end after one callback, indexing
	// nothing. Containment still uses the configured root, which resolves to the
	// same place.
	start := resolve(absRoot)
	w := &walker{root: absRoot, follow: follow, visited: map[string]bool{}}
	w.visited[start] = true
	err := w.tree(start, "")
	return w.entries, w.skipped, w.linkRoots, err
}

// walker carries one walk's accumulated result. It exists because following a
// symlinked directory means walking a second tree whose files must still be named
// relative to the root, which a single WalkDir closure cannot express.
type walker struct {
	root      string
	follow    bool
	entries   []libEntry
	skipped   []SkippedFile
	linkRoots []string
	// visited holds the resolved roots of the walks already made, so a link back into
	// a tree already covered — or into one covering it — terminates instead of
	// looping. A link is refused by containment (see covered); an ordinary descent
	// that lands on one of these roots is pruned by equality (see tree). The plain
	// lookup is enough there because WalkDir hands a symlinked directory to the
	// symlink branch rather than descending it, so every directory a walk descends
	// into is real and every walk starts at a resolved root.
	visited map[string]bool
}

func (w *walker) skip(rel string, err error) {
	w.skipped = append(w.skipped, SkippedFile{RelPath: rel, Reason: err.Error()})
}

// covered reports a directory some earlier walk already reached. Exact equality is
// not enough: two links into overlapping trees (one to a drive, another to a pack
// inside it) would otherwise walk the nested tree twice, indexing every file in it
// under two ids that resolve to the same bytes.
func (w *walker) covered(target string) bool {
	for v := range w.visited {
		if underRootPath(v, target) {
			return true
		}
	}
	return false
}

// rel names a walked path as the user sees it: relative to the library root, through
// whatever symlink led here rather than by the target's real location.
func rel(dir, prefix, p string) string {
	r, err := filepath.Rel(dir, p)
	if err != nil {
		r = filepath.Base(p)
	}
	r = filepath.ToSlash(r)
	if r == "." {
		r = ""
	}
	switch {
	case prefix == "":
		return r
	case r == "":
		return prefix
	}
	return prefix + "/" + r
}

// tree walks one directory. prefix is the root-relative path that led to it, empty
// for the library root itself — which is also what marks the root's own read error
// as fatal, where a followed link's is just a skip.
func (w *walker) tree(dir, prefix string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		r := rel(dir, prefix, p)
		if err != nil {
			if p == dir && prefix == "" {
				return err
			}
			w.skip(r, err)
			return nil
		}
		name := d.Name()
		// Ahead of the symlink branch: a dot-named entry is working state whichever
		// kind it is, and letting a link be handled first would either index a hidden
		// tree under --follow-symlinks or report a skip naming that flag for something
		// a plain dot-dir is dropped for silently.
		if p != dir && strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return w.symlink(p, r)
		}
		if d.IsDir() {
			// A tree another walk already covered is reached again by ordinary descent
			// when one link points inside another's target: the link to the inner tree
			// is followed first, so the outer one is not yet in visited to refuse it.
			// Pruning the subtree here rather than refusing the outer link keeps the
			// files that link alone reaches.
			if p != dir && w.visited[p] {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			w.skip(r, err)
			return nil
		}
		w.file(p, r, name, info)
		return nil
	})
}

// file records one browseable file. info is passed in because a symlinked file's own
// DirEntry describes the link, whose size is the length of the target path.
func (w *walker) file(p, r, name string, info os.FileInfo) {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	if isSidecar(ext) {
		return
	}
	vendor, pack := vendorPack(r)
	e := libEntry{path: p, rel: r, vendor: vendor, pack: pack, name: name}
	switch ext {
	case "zip":
		e.kind, e.variant = SourceZip, deriveVariant(pack, name)
	case "unitypackage":
		e.kind, e.variant = SourceUnityPackage, deriveVariant(pack, name)
	default:
		e.kind, e.size = SourceLoose, info.Size()
	}
	w.entries = append(w.entries, e)
}

// symlink decides what a symbolic link in the library becomes. filepath.WalkDir does
// not follow links, and a link's own DirEntry describes the link — it does not report
// itself as a directory, and its size is the length of the target path — so treating
// one as an ordinary file yields an asset with a fabricated size whose target is
// never walked.
//
// A link into the library duplicates a file the walk reaches by its real path, so it
// is dropped. A link out of the library is followed only when asked: serving refuses
// paths outside what the scan covered, and traversing wherever a link happens to
// point is the kind of surprise `find` and `rg` also keep behind a flag. Unfollowed,
// it is reported rather than dropped, because a whole pack behind one link would
// otherwise leave the index with nothing said.
func (w *walker) symlink(p, r string) error {
	target, err := filepath.EvalSymlinks(p)
	if err != nil {
		w.skip(r, err)
		return nil
	}
	if underRootPath(w.root, target) {
		return nil
	}
	if !w.follow {
		w.skip(r, fmt.Errorf("symlink to %s, which is outside the library root; pass --follow-symlinks to index it", target))
		return nil
	}
	info, err := os.Stat(target)
	if err != nil {
		w.skip(r, err)
		return nil
	}
	if !info.IsDir() {
		// The resolved file authorises itself and nothing else in its directory.
		// Without this Open refuses the very asset the scan just indexed.
		w.linkRoots = append(w.linkRoots, target)
		w.file(p, r, filepath.Base(p), info)
		return nil
	}
	if w.covered(target) {
		w.skip(r, fmt.Errorf("symlink to %s, which this scan has already walked", target))
		return nil
	}
	w.visited[target] = true
	w.linkRoots = append(w.linkRoots, target)
	// The target, not the link: WalkDir lstats its argument, so handing it the link
	// would visit the link itself and descend no further.
	return w.tree(target, r)
}

// enumerateArchive opens one archive entry and returns its assets, plus a note when
// a pass over the archive degraded without failing outright — assets it could still
// enumerate alongside a report of what it could not.
func enumerateArchive(e libEntry) ([]Asset, *SkippedFile, error) {
	switch e.kind {
	case SourceZip:
		a, err := zipAssets(e.path, e.rel, e.vendor, e.pack, e.variant)
		return a, nil, err
	case SourceUnityPackage:
		return unityAssets(e.path, e.rel, e.vendor, e.pack, e.variant)
	}
	return nil, nil, nil
}

// archiveAssets enumerates one archive, turning a read failure into a skip note
// instead of an error. A truncated or otherwise damaged file in a large library
// must not make the whole index unbuildable — browse treats a build failure as
// fatal, so one bad file would take the entire browser down with it.
// A partial failure returns both: the assets that were enumerated, and the note
// naming what was lost. The caller keeps the assets and declines to cache the
// archive's print, so the degraded pass is retried next run rather than frozen.
func archiveAssets(e libEntry) ([]Asset, *SkippedFile) {
	a, note, err := enumerateArchive(e)
	if err != nil {
		return nil, &SkippedFile{RelPath: e.rel, Reason: err.Error()}
	}
	return a, note
}

// looseAsset builds the single asset for a loose file entry, reading the file to
// compute its content fingerprint and, for an image, its pixel dimensions.
// Build/Refresh reuse a cached loose asset via LoosePrint so these reads only
// happen for new or changed files.
func looseAsset(e libEntry) (Asset, error) {
	fp, err := looseFingerprint(e.path)
	if err != nil {
		return Asset{}, err
	}
	a := newAsset(Source{Kind: SourceLoose, FilePath: e.path}, e.name, e.rel, e.vendor, e.pack, "", e.size, fp)
	if isDimExt(a.Ext) {
		if f, err := os.Open(e.path); err == nil {
			a.Width, a.Height = imageDims(readHead(f), a.Ext)
			f.Close()
		}
	}
	return a, nil
}

// isRootMotionVariant reports a root-motion sibling of an animation library (e.g.
// "UAL1_RM.glb" beside "UAL1.glb"). It is left whole so its clips don't duplicate the
// base file's; pairing the two as a root-motion toggle is a later concern.
func isRootMotionVariant(name string) bool {
	_, isRM := RootMotionVariant(strings.TrimSuffix(name, filepath.Ext(name)))
	return isRM
}

// clipAsset builds one virtual asset for a single embedded animation of a multi-clip
// model file. All clips of a file share its bytes (Source.FilePath); Source.Clip
// names which animation the preview plays. The content fingerprint combines the
// file's fingerprint with the clip name so each clip tags independently and stably.
func clipAsset(e libEntry, clip string, index int, fileFP string) Asset {
	s := Source{Kind: SourceLoose, FilePath: e.path, Clip: clip, ClipIndex: &index}
	fp := ""
	if fileFP != "" {
		fp = fileFP + "#" + clip
	}
	return Asset{
		ID:          id(s),
		Name:        clip,
		RelPath:     e.rel + "::" + clip,
		CopyPath:    e.path + "::" + clip,
		Category:    CategoryAnimation,
		Ext:         strings.ToLower(strings.TrimPrefix(filepath.Ext(e.name), ".")),
		Vendor:      e.vendor,
		Pack:        e.pack,
		Size:        e.size,
		Thumb:       ThumbGLB,
		Fingerprint: fp,
		Source:      s,
	}
}

// looseAssets builds the asset(s) for a loose file. A multi-animation .glb (a
// Quaternius-style animation library) is split into one virtual asset per embedded
// clip, all sharing the file's bytes; its root-motion (_RM) sibling is left whole.
// Everything else (including single-animation GLBs) is one asset.
//
// A non-nil skip means the derivation did not fully succeed. Any assets returned
// alongside it are still usable — a GLB whose clip list could not be read still
// previews whole — but the caller must not cache them against the file's stat
// print, or one transient read failure would be frozen in until the next
// --reindex, long after the cause was fixed.
func looseAssets(e libEntry) ([]Asset, *SkippedFile) {
	skip := func(err error) *SkippedFile {
		return &SkippedFile{RelPath: e.rel, Reason: err.Error()}
	}
	whole := func() ([]Asset, *SkippedFile) {
		a, err := looseAsset(e)
		if err != nil {
			return nil, skip(err)
		}
		return []Asset{a}, nil
	}
	if !strings.EqualFold(filepath.Ext(e.name), ".glb") || isRootMotionVariant(e.name) {
		return whole()
	}
	names, err := glbAnimationNames(e.path)
	if err != nil || len(names) < 2 {
		// A .glb whose container will not parse is not a failure to report: one whole
		// asset is the right answer for anything that is not a multi-clip animation
		// library, which includes every ordinary model. A file that cannot be read at
		// all is a different matter, and looseAsset reports that when it reads the
		// bytes for the fingerprint.
		return whole()
	}
	fp, err := looseFingerprint(e.path)
	if err != nil {
		return nil, skip(err)
	}
	labels := uniqueClipNames(names)
	out := make([]Asset, 0, len(labels))
	for i, n := range labels {
		out = append(out, clipAsset(e, n, i, fp))
	}
	return out, nil
}

// uniqueClipNames makes a GLB's animation names usable as identity. glTF names are
// optional and need not be unique, but the clip name is what distinguishes one
// virtual asset's id and fingerprint from another's, so duplicates would collide on
// both: two cards for the same clip, tagging either one tagging both.
func uniqueClipNames(names []string) []string {
	out := make([]string, 0, len(names))
	seen := make(map[string]int, len(names))
	for i, n := range names {
		if n == "" {
			n = fmt.Sprintf("clip %d", i+1)
		}
		if c, dup := seen[n]; dup {
			base := n
			for {
				c++
				n = fmt.Sprintf("%s (%d)", base, c)
				if _, taken := seen[n]; !taken {
					break
				}
			}
			seen[base] = c
		}
		seen[n] = 1
		out = append(out, n)
	}
	return out
}

// readHead reads up to dimsHeadBytes from r, enough to recover an image's
// dimensions without pulling a whole file into memory.
func readHead(r io.Reader) []byte {
	head, _ := io.ReadAll(io.LimitReader(r, dimsHeadBytes))
	return head
}

// Scan walks the library root and returns every browseable asset: loose files and
// the entries inside .zip / .unitypackage archives, de-duplicated (see dedup).
// Unreadable archives are skipped; Build reports why in Index.Skipped.
func Scan(root string) ([]Asset, error) {
	ix, err := Build(Options{Root: root})
	if err != nil {
		return nil, err
	}
	return ix.Assets, nil
}

// vendorPack derives the vendor (first path segment) and pack (second segment) of
// a root-relative path. A file directly under a vendor has no pack, and a file
// sitting loose in the library root (a receipt, a README) has neither — naming it
// its own vendor would give the filter a bucket holding one file.
func vendorPack(rel string) (vendor, pack string) {
	segs := strings.Split(rel, "/")
	if len(segs) < 2 {
		return "", ""
	}
	vendor = segs[0]
	if len(segs) >= 3 {
		pack = segs[1]
	}
	return vendor, pack
}

// dedup drops an archive entry when a loose file in the same pack matches it at the
// same pack-relative subpath (normalizing a leading "src/") and size. Keying on the
// full subpath, never the bare basename, is required: two genuinely-distinct files
// can share a basename+size across packs or subtrees. Only archive-vs-loose is
// de-duplicated; two archive variants of the same asset are kept (distinct
// deliverables, distinguished by the variant facet).
//
// The dropped entries are returned rather than discarded because suppression is a
// property of the pair, not of the archive: the loose twin can be deleted while the
// archive's stat print stays identical, and an incremental refresh that kept only
// the survivors would reuse the suppression along with them and lose the asset.
func dedup(assets []Asset) (kept, dropped []Asset) {
	looseKeys := make(map[string]struct{})
	for i := range assets {
		if assets[i].Source.Kind == SourceLoose {
			looseKeys[looseDedupKey(assets[i])] = struct{}{}
		}
	}
	for _, a := range assets {
		if a.Source.Kind != SourceLoose {
			if _, ok := looseKeys[archiveDedupKey(a)]; ok {
				dropped = append(dropped, a)
				continue
			}
		}
		kept = append(kept, a)
	}
	return kept, dropped
}

func normSubpath(s string) string { return strings.TrimPrefix(s, "src/") }

func dedupKey(vendor, pack, subpath string, size int64) string {
	return vendor + "\x00" + pack + "\x00" + normSubpath(subpath) + "\x00" + strconv.FormatInt(size, 10)
}

func looseDedupKey(a Asset) string {
	return dedupKey(a.Vendor, a.Pack, packSubpath(a.RelPath, a.Vendor, a.Pack), a.Size)
}

// packSubpath strips the vendor/pack prefix from a loose file's library-relative
// path, leaving the path within the pack — the half that describes the file rather
// than the release it shipped in.
//
// A file directly under a vendor dir has no pack, so the prefix is built rather than
// formatted: "vendor" + "/" + "" + "/" would be a doubled separator that matches
// nothing, silently leaving the whole path in place.
func packSubpath(relPath, vendor, pack string) string {
	prefix := vendor + "/"
	if pack != "" {
		prefix += pack + "/"
	}
	return strings.TrimPrefix(relPath, prefix)
}

func archiveDedupKey(a Asset) string {
	return dedupKey(a.Vendor, a.Pack, a.Source.EntryPath(), a.Size)
}
