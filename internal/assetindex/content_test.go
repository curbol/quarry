package assetindex

import (
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// buildFixture creates a small library and returns a built index plus its root.
func buildFixture(t *testing.T) (*Index, string) {
	t.Helper()
	root, mk := libRoot(t)
	cacheDir := t.TempDir()
	writeZip(t, mk("synty", "Foo_Pack", "Foo_Pack_SourceFiles_v3.zip"), map[string]string{
		"SourceFiles/Models/Heart.fbx":   "FBXHEARTDATA",
		"SourceFiles/Textures/Heart.png": "PNGDATA",
	})
	writeUnityPackage(t, mk("synty", "Foo_Pack", "Foo_Pack_Unity_2022_3_v1_0_0.unitypackage"), []unityGUID{
		{guid: "aaa", pathname: "Assets/Foo/Heart.prefab", asset: "PREFABBYTES", preview: true},
		{guid: "ccc", pathname: "Assets/Foo/Rock.fbx", asset: "ROCKBYTES"},
	})
	os.WriteFile(mk("explosive", "RPG", "Sword.glb"), []byte("GLBBYTES"), 0o644)

	ix, err := Build(Options{Root: root, CacheDir: cacheDir})
	if err != nil {
		t.Fatal(err)
	}
	return ix, root
}

func find(t *testing.T, ix *Index, name string, kind SourceKind) Asset {
	t.Helper()
	for _, a := range ix.Assets {
		if a.Name == name && a.Source.Kind == kind {
			return a
		}
	}
	t.Fatalf("asset %q (%s) not found", name, kind)
	return Asset{}
}

func readAll(t *testing.T, rc io.ReadCloser) string {
	t.Helper()
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestOpenAcrossSources(t *testing.T) {
	ix, _ := buildFixture(t)

	loose := find(t, ix, "Sword.glb", SourceLoose)
	rc, size, err := ix.Open(loose)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, rc); got != "GLBBYTES" || size != int64(len("GLBBYTES")) {
		t.Errorf("loose Open = %q size %d", got, size)
	}

	zipEntry := find(t, ix, "Heart.fbx", SourceZip)
	rc, size, err = ix.Open(zipEntry)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, rc); got != "FBXHEARTDATA" || size != int64(len("FBXHEARTDATA")) {
		t.Errorf("zip Open = %q size %d", got, size)
	}

	unity := find(t, ix, "Rock.fbx", SourceUnityPackage)
	rc, _, err = ix.Open(unity)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, rc); got != "ROCKBYTES" {
		t.Errorf("unity Open = %q", got)
	}
}

func TestOpenThumbnail(t *testing.T) {
	ix, _ := buildFixture(t)
	prefab := find(t, ix, "Heart.prefab", SourceUnityPackage)
	if prefab.Thumb != ThumbPreview {
		t.Fatalf("prefab thumb = %s, want preview", prefab.Thumb)
	}
	rc, _, err := ix.OpenThumbnail(prefab)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, rc); got != "PNGPREVIEW" {
		t.Errorf("thumbnail = %q", got)
	}

	// An asset without a preview has no thumbnail.
	rock := find(t, ix, "Rock.fbx", SourceUnityPackage)
	if _, _, err := ix.OpenThumbnail(rock); err != ErrNoThumbnail {
		t.Errorf("Rock.fbx thumbnail err = %v, want ErrNoThumbnail", err)
	}
}

// Concurrent fetches from the same unitypackage must all return complete bytes,
// exercising the single-flight + atomic extract.
func TestConcurrentUnityExtract(t *testing.T) {
	ix, _ := buildFixture(t)
	rock := find(t, ix, "Rock.fbx", SourceUnityPackage)

	const n = 24
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rc, _, err := ix.Open(rock)
			if err != nil {
				errs <- err
				return
			}
			defer rc.Close()
			b, err := io.ReadAll(rc)
			if err != nil {
				errs <- err
				return
			}
			if string(b) != "ROCKBYTES" {
				errs <- io.ErrUnexpectedEOF
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Open: %v", err)
	}
}

func TestOpenRejectsOutsideRoot(t *testing.T) {
	ix, root := buildFixture(t)
	outside := filepath.Join(filepath.Dir(root), "escape.txt")
	os.WriteFile(outside, []byte("SECRET"), 0o644)
	bad := Asset{Source: Source{Kind: SourceLoose, FilePath: outside}}
	if _, _, err := ix.Open(bad); err != ErrOutsideRoot {
		t.Errorf("Open outside root err = %v, want ErrOutsideRoot", err)
	}
}

func TestLookupUnknownID(t *testing.T) {
	ix, _ := buildFixture(t)
	if _, ok := ix.Lookup("deadbeef"); ok {
		t.Error("unknown id resolved")
	}
	// A known id resolves.
	first := ix.Assets[0]
	if a, ok := ix.Lookup(first.ID); !ok || a.ID != first.ID {
		t.Error("known id did not resolve")
	}
}

func TestSaveLoadRefresh(t *testing.T) {
	root, mk := libRoot(t)
	cacheDir := t.TempDir()
	writeZip(t, mk("synty", "P", "P_SourceFiles_v1.zip"), map[string]string{"A/x.fbx": "X"})

	ix, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	n0 := len(ix.Assets)
	if n0 == 0 {
		t.Fatal("no assets built")
	}

	// Reload from cache and refresh: same count, ids resolve.
	ix2, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ix2.Assets) != n0 {
		t.Errorf("reload count = %d, want %d", len(ix2.Assets), n0)
	}
	if _, ok := ix2.Lookup(ix.Assets[0].ID); !ok {
		t.Error("id lookup broken after reload")
	}

	// Add a loose file, refresh picks it up.
	os.WriteFile(mk("explosive", "R", "new.glb"), []byte("G"), 0o644)
	ix3, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ix3.Assets) != n0+1 {
		t.Errorf("after add, count = %d, want %d", len(ix3.Assets), n0+1)
	}
	var added Asset
	for _, a := range ix3.Assets {
		if a.Name == "new.glb" {
			added = a
		}
	}
	if added.ID == "" {
		t.Fatal("the added file is not in the index")
	}

	// Remove a pack, refresh drops it. A refresh reuses whatever the cache holds for
	// every unchanged file, so an entry the walk no longer reaches has to fall out
	// rather than ride along and keep answering content requests.
	if err := os.RemoveAll(filepath.Join(root, "explosive")); err != nil {
		t.Fatal(err)
	}
	ix4, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ix4.Assets) != n0 {
		t.Errorf("after removing the pack, count = %d, want %d", len(ix4.Assets), n0)
	}
	if _, ok := ix4.Lookup(added.ID); ok {
		t.Errorf("%s still resolves after its pack was deleted", added.RelPath)
	}
}

// The version keys both the cached index and the unpacked tree, so entries derived by
// other scan logic must never be merged into a current index. LoadOrBuild guards this
// at the top, but Load and Refresh are exported separately and the guarantee has to
// hold for that route too.
func TestRefreshRebuildsAcrossAnIndexVersion(t *testing.T) {
	root, mk := libRoot(t)
	cacheDir := t.TempDir()
	writeZip(t, mk("synty", "P", "P_SourceFiles_v1.zip"), map[string]string{"A/x.fbx": "X"})

	ix, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := len(ix.Assets)

	// An index left behind by another version, carrying an asset the current scan
	// would never produce and a stale print that would let it be reused verbatim.
	ix.Version = indexVersion - 1
	ix.Assets = append(ix.Assets, Asset{ID: "ghost", RelPath: "gone/ghost.fbx",
		Source: Source{Kind: SourceLoose, FilePath: filepath.Join(root, "gone", "ghost.fbx")}})
	if err := ix.refresh(); err != nil {
		t.Fatal(err)
	}
	if ix.Version != indexVersion {
		t.Errorf("version = %d after refresh, want %d", ix.Version, indexVersion)
	}
	if len(ix.Assets) != want {
		t.Errorf("assets = %d, want %d rebuilt from the tree alone", len(ix.Assets), want)
	}
	if _, ok := ix.Lookup("ghost"); ok {
		t.Error("an asset from the previous version survived the refresh")
	}
}

// unreadable makes path unopenable and restores it when the test ends, so a refresh
// that re-enumerates the archive fails visibly instead of quietly costing a
// re-decompress. Skips as root, where the mode is not enforced.
func unreadable(t *testing.T, path string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: file modes are not enforced")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, fi.Mode()) })
	if f, err := os.Open(path); err == nil {
		f.Close()
		t.Skip("the file is still readable despite mode 0000")
	}
}

// Refresh must actually reuse what the cache holds for an unchanged archive. Every
// other test would also pass if it silently re-enumerated everything, which is what
// made a per-run re-decompress of every unitypackage invisible.
//
// Reuse is made observable by making the archive unreadable after the first build
// while leaving its size and mtime alone: the stat print is unchanged, so a refresh
// that reuses never opens the file, and one that re-enumerates records a skip and
// loses the entries.
func TestRefreshReusesCachedArchiveEnumeration(t *testing.T) {
	root, mk := libRoot(t)
	pkgPath := mk("synty", "P", "P_Unity_2022_3_v1.unitypackage")
	writeUnityPackage(t, pkgPath, []unityGUID{
		{guid: "aaa", pathname: "Assets/P/Original.fbx", asset: "ORIGINAL"},
	})
	cacheDir := t.TempDir()

	ix, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Assets) != 1 || ix.Assets[0].Name != "Original.fbx" {
		t.Fatalf("built assets = %v", names(ix.Assets))
	}

	unreadable(t, pkgPath)
	again, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Assets) != 1 || again.Assets[0].Name != "Original.fbx" {
		t.Errorf("assets after refresh = %v, want the cached entry: the archive was re-enumerated despite an unchanged stat print", names(again.Assets))
	}
	if len(again.Skipped) != 0 {
		t.Errorf("skipped = %v, want none: a reused archive is never opened", again.Skipped)
	}
}

// An archive every one of whose entries is deduped away by a loose twin contributes no
// assets at all. Keying reuse on "did this archive leave assets behind" rather than on
// its stat print meant such an archive was fully re-decompressed on every single run —
// hundreds of MB per Synty pack, forever.
func TestRefreshReusesAnArchiveThatContributesNothing(t *testing.T) {
	root, mk := libRoot(t)
	// The extracted twin beside the archive: dedup keeps the loose file and drops the
	// archive entry, so the archive contributes nothing to the index.
	os.WriteFile(mk("kevdev", "HBM", "src", "Walk.fbx"), []byte("FBXWALK"), 0o644)
	zipPath := mk("kevdev", "HBM", "HBM_SourceFiles_v1.zip")
	writeZip(t, zipPath, map[string]string{"src/Walk.fbx": "FBXWALK"})

	cacheDir := t.TempDir()
	ix, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Assets) != 1 || ix.Assets[0].Source.Kind != SourceLoose {
		t.Fatalf("assets = %v, want just the loose twin", names(ix.Assets))
	}

	unreadable(t, zipPath)
	again, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Skipped) != 0 {
		t.Errorf("skipped = %v, want none: the archive's print was unchanged, so it should never have been opened", again.Skipped)
	}
	if len(again.Assets) != 1 {
		t.Errorf("assets after refresh = %v, want the loose twin", names(again.Assets))
	}
}
