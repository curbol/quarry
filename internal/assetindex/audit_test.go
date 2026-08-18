package assetindex

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func libRoot(t *testing.T) (root string, mk func(...string) string) {
	t.Helper()
	root = t.TempDir()
	return root, func(parts ...string) string {
		p := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
}

// cacheFileFor is where LoadOrBuild keeps one root's index. Tests that reach for the
// cache file go through this rather than assembling a path, so the layout stays a
// single decision inside the package.
func cacheFileFor(t *testing.T, cacheDir, root string) string {
	t.Helper()
	abs, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(stateDir(cacheDir, abs), "index.json")
}

// A personal library is big and accumulates the odd partial copy. One unreadable
// archive must cost that archive, not the whole index — browse treats a build
// failure as fatal and would refuse to start.
func TestBuildSkipsUnreadableArchive(t *testing.T) {
	root, mk := libRoot(t)
	os.WriteFile(mk("good", "Pack", "Sword.glb"), []byte("GLBBYTES"), 0o644)
	os.WriteFile(mk("bad", "Pack", "Truncated_SourceFiles_v1.zip"), []byte("PK\x03\x04garbage"), 0o644)

	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatalf("one bad archive aborted the build: %v", err)
	}
	if len(ix.Assets) == 0 {
		t.Error("the readable file was dropped along with the bad archive")
	}
	if len(ix.Skipped) != 1 || !strings.Contains(ix.Skipped[0].RelPath, "Truncated") {
		t.Errorf("skipped = %+v, want the truncated zip reported", ix.Skipped)
	}
}

// The index cache is rewritten on every run; a write interrupted partway must not
// leave a half-file that forces a full rebuild of a multi-minute scan.
func TestSaveIsAtomicAndChecked(t *testing.T) {
	root, mk := libRoot(t)
	os.WriteFile(mk("v", "p", "Sword.glb"), []byte("GLBBYTES"), 0o644)
	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	cachePath := filepath.Join(dir, "browse-index.json")
	if err := ix.save(cachePath); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("Save left %d files behind, want just the index", len(entries))
	}

	// A directory in place of the cache file cannot be written: Save must say so.
	blocked := filepath.Join(t.TempDir(), "browse-index.json")
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ix.save(blocked); err == nil {
		t.Error("Save reported success writing over a directory")
	}
}

// Every indexed field has to survive the cache round trip; a field that serializes
// away comes back as a silently degraded asset.
func TestSaveLoadPreservesIndexedFields(t *testing.T) {
	root, mk := libRoot(t)
	writeUnityPackage(t, mk("synty", "SIDEKICK_X", "SIDEKICK_X_Unity_2021_3_v1_0_0.unitypackage"), []unityGUID{
		{guid: "sk1", pathname: "Assets/S/Characters/Warrior/Warrior_01.sk", asset: "Name: Warrior_01\nParts:\n- Name: SK_HEAD\n"},
		{guid: "hd1", pathname: "Assets/S/Resources/SK_HEAD.fbx", asset: "HEADFBX", preview: true},
	})
	os.WriteFile(mk("v", "p", "Pic.png"), encodePNG(t, 7, 11), 0o644)

	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(t.TempDir(), "browse-index.json")
	if err := ix.save(cachePath); err != nil {
		t.Fatal(err)
	}
	loaded, err := load(cachePath, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range ix.Assets {
		got, ok := loaded.Lookup(want.ID)
		if !ok {
			t.Fatalf("%s missing after reload", want.Name)
		}
		if !sameAsset(got, want) {
			t.Errorf("asset changed across the cache round trip:\n got %+v\nwant %+v", got, want)
		}
	}
}

func sameAsset(a, b Asset) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// A stale cache (older index version, or a different root) must be rebuilt rather
// than served: the fingerprint scheme and the indexed fields move together.
func TestLoadOrBuildRejectsStaleCache(t *testing.T) {
	root, mk := libRoot(t)
	os.WriteFile(mk("v", "p", "Sword.glb"), []byte("GLBBYTES"), 0o644)
	cacheDir := t.TempDir()

	ix, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ix.Version != indexVersion {
		t.Fatalf("built index has version %d", ix.Version)
	}
	cachePath := cacheFileFor(t, cacheDir, root)

	var raw map[string]any
	b, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	raw["version"] = indexVersion - 1
	raw["assets"] = []any{}
	b, _ = json.Marshal(raw)
	if err := os.WriteFile(cachePath, b, 0o644); err != nil {
		t.Fatal(err)
	}

	again, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Assets) == 0 || again.Version != indexVersion {
		t.Errorf("stale cache was reused: version=%d assets=%d", again.Version, len(again.Assets))
	}
}

// A corrupt cache file must fall back to a full build, not fail the command.
func TestLoadOrBuildRebuildsFromCorruptCache(t *testing.T) {
	root, mk := libRoot(t)
	os.WriteFile(mk("v", "p", "Sword.glb"), []byte("GLBBYTES"), 0o644)
	cacheDir := t.TempDir()
	cachePath := cacheFileFor(t, cacheDir, root)
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, []byte(`{"assets":[{"id":`), 0o644); err != nil {
		t.Fatal(err)
	}
	ix, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir}, false, nil)
	if err != nil {
		t.Fatalf("corrupt cache should rebuild, got %v", err)
	}
	if len(ix.Assets) == 0 {
		t.Error("rebuild produced no assets")
	}
}

// A Sidekick character whose part meshes are not in this archive cannot be
// assembled, so its prefab/material/mesh must stay browseable — dropping them as
// superseded byproducts leaves the character with no representation at all.
func TestUnassembledSidekickKeepsItsByproducts(t *testing.T) {
	root, mk := libRoot(t)
	writeUnityPackage(t, mk("synty", "SIDEKICK_X", "SIDEKICK_X_Unity_2021_3_v1_0_0.unitypackage"), []unityGUID{
		{guid: "sk1", pathname: "Assets/S/Characters/Warrior/Warrior_01.sk", asset: "Name: Warrior_01\nParts:\n- Name: SK_ABSENT\n"},
		{guid: "pf1", pathname: "Assets/S/Characters/Warrior/Warrior_01.prefab", asset: "PREFAB", preview: true},
		{guid: "mt1", pathname: "Assets/S/Characters/Warrior/Warrior_01.mat", asset: "MAT"},
	})
	assets, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	byExt := map[string]bool{}
	for _, a := range assets {
		byExt[a.Ext] = true
		if a.Ext == "sk" && a.Thumb == ThumbSidekick {
			t.Error("a character with no resolvable part was still upgraded to an assembled one")
		}
	}
	if !byExt["prefab"] {
		t.Errorf("the unassembled character lost its prefab; kept only %v", byExt)
	}
}

// The assembled case still supersedes its byproducts.
func TestAssembledSidekickDropsItsByproducts(t *testing.T) {
	root, mk := libRoot(t)
	writeUnityPackage(t, mk("synty", "SIDEKICK_Y", "SIDEKICK_Y_Unity_2021_3_v1_0_0.unitypackage"), []unityGUID{
		{guid: "sk1", pathname: "Assets/S/Characters/Warrior/Warrior_01.sk", asset: "Name: Warrior_01\nParts:\n- Name: SK_HEAD\n"},
		{guid: "pf1", pathname: "Assets/S/Characters/Warrior/Warrior_01.prefab", asset: "PREFAB", preview: true},
		{guid: "hd1", pathname: "Assets/S/Resources/SK_HEAD.fbx", asset: "HEADFBX", preview: true},
	})
	assets, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range assets {
		if a.Ext == "prefab" {
			t.Errorf("assembled character kept its superseded prefab: %s", a.RelPath)
		}
	}
}

// glTF animation names are optional and not required to be unique. Two clips with
// the same name build identical Sources, so they collide on both the id the content
// API resolves and the fingerprint tags key on — one card's tag would land on the
// other, and Lookup could only ever reach one of them.
func TestDuplicateClipNamesGetDistinctIdentities(t *testing.T) {
	root, mk := libRoot(t)
	writeGLB(t, mk("Quaternius", "AnimLib", "anims.glb"), "Walk", "Walk", "", "Run")

	assets, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 4 {
		t.Fatalf("got %d assets, want one per clip: %+v", len(assets), assets)
	}
	ids, fps, names := map[string]int{}, map[string]int{}, map[string]int{}
	for _, a := range assets {
		ids[a.ID]++
		fps[a.Fingerprint]++
		names[a.Name]++
	}
	for id, n := range ids {
		if n > 1 {
			t.Errorf("%d assets share id %s", n, id)
		}
	}
	for fp, n := range fps {
		if n > 1 {
			t.Errorf("%d assets share fingerprint %s (tagging one would tag the other)", n, fp)
		}
	}
	for name, n := range names {
		if name == "" {
			t.Error("an unnamed clip kept an empty name")
		}
		if n > 1 {
			t.Errorf("%d clips still display as %q", n, name)
		}
	}
}

// A hostile archive entry must never reach a filesystem path. Both guards are pure
// and load-bearing, and neither was covered.
func TestArchiveEntryNamesAreRejected(t *testing.T) {
	for _, name := range []string{"../escape.png", "/etc/passwd", "a/../../escape.png", "..", ""} {
		if safeEntry(name) {
			t.Errorf("zip entry %q accepted", name)
		}
	}
	for _, ok := range []string{"Assets/Sword.fbx", "a.png"} {
		if !safeEntry(ok) {
			t.Errorf("ordinary zip entry %q rejected", ok)
		}
	}
	for _, name := range []string{"../x/asset", "../asset", "a/b/asset"} {
		if guid, _, ok := splitUnityName(name); ok && (guid == ".." || strings.ContainsAny(guid, `/\`)) {
			t.Errorf("unitypackage name %q yielded an escaping guid %q", name, guid)
		}
	}
}

// A .unitypackage is a gzipped tar from an untrusted-ish archive; a malformed one
// must be skipped like any other bad archive, not panic or abort the build.
func TestBuildSkipsMalformedUnityPackage(t *testing.T) {
	root, mk := libRoot(t)
	os.WriteFile(mk("good", "Pack", "Sword.glb"), []byte("GLBBYTES"), 0o644)
	// Valid gzip header, garbage tar inside.
	os.WriteFile(mk("bad", "Pack", "Broken_Unity_2022_3_v1.unitypackage"),
		[]byte("\x1f\x8b\x08\x00\x00\x00\x00\x00\x00\xffgarbage"), 0o644)

	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatalf("a malformed unitypackage aborted the build: %v", err)
	}
	if len(ix.Assets) == 0 {
		t.Error("the readable file was dropped along with the bad archive")
	}
	if len(ix.Skipped) != 1 || !strings.Contains(ix.Skipped[0].RelPath, "Broken") {
		t.Errorf("skipped = %+v, want the malformed unitypackage reported", ix.Skipped)
	}
}

// Every pack update writes a new extraction dir keyed on the archive's mtime, and
// every index version writes under its own tree. Nothing removed either, so updates
// stranded hundreds of MB apiece and a version bump stranded the whole previous tree.
func TestPruneUnpackedDropsStaleExtractions(t *testing.T) {
	root, mk := libRoot(t)
	os.WriteFile(mk("v", "Pack", "Sword.glb"), []byte("GLBBYTES"), 0o644)
	cacheDir := t.TempDir()

	ix, err := Build(Options{Root: root, CacheDir: cacheDir})
	if err != nil {
		t.Fatal(err)
	}
	seed := func(parts ...string) string {
		dir := filepath.Join(append([]string{cacheDir, "unpacked"}, parts...)...)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "asset"), []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	staleFingerprint := seed(strconv.Itoa(indexVersion), "deadbeefdeadbeef")
	staleVersion := seed(strconv.Itoa(indexVersion-1), "cafebabecafebabe")

	if err := ix.PruneUnpacked(); err != nil {
		t.Fatalf("PruneUnpacked: %v", err)
	}
	for _, p := range []string{staleFingerprint, staleVersion} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("stale extraction %s survived the prune (err=%v)", p, err)
		}
	}
}

// The lexical check alone would pass a path that sits inside the root but points
// out of it. underRoot resolves symlinks first; only a test with a real symlink
// proves that, and the existing outside-root test uses a plain path the lexical
// check would already catch.
func TestOpenRejectsSymlinkEscapingRoot(t *testing.T) {
	root, mk := libRoot(t)
	os.WriteFile(mk("v", "Pack", "Sword.glb"), []byte("GLBBYTES"), 0o644)

	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "v", "Pack", "innocent.glb")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// The path is lexically inside the root; only resolving it reveals the escape.
	bad := Asset{Source: Source{Kind: SourceLoose, FilePath: link}}
	if _, _, err := ix.Open(bad); err != ErrOutsideRoot {
		t.Errorf("Open through a symlink out of the root err = %v, want ErrOutsideRoot", err)
	}
}

// A big library accumulates the odd corner the user cannot read — a restrictive
// mode, a half-synced network mount. Failing the walk there costs the whole index
// and browse refuses to start, which is the same bargain a damaged archive already
// avoids.
func TestUnreadableDirectoryIsSkippedNotFatal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits")
	}
	root, mk := libRoot(t)
	os.WriteFile(mk("v", "Pack", "Sword.glb"), []byte("GLBBYTES"), 0o644)
	locked := mk("v", "Locked", "x.png")
	os.WriteFile(locked, []byte("x"), 0o644)
	lockedDir := filepath.Dir(locked)
	if err := os.Chmod(lockedDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(lockedDir, 0o755) })

	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatalf("one unreadable directory aborted the whole scan: %v", err)
	}
	if len(ix.Assets) != 1 || ix.Assets[0].Name != "Sword.glb" {
		t.Errorf("readable assets = %+v, want just Sword.glb", ix.Assets)
	}
	if len(ix.Skipped) != 1 || !strings.Contains(ix.Skipped[0].RelPath, "Locked") {
		t.Errorf("skipped = %+v, want the unreadable dir reported", ix.Skipped)
	}
}

// Tolerating an unreadable subtree must not extend to the root: a mistyped --root
// that silently indexed nothing would look like an empty library.
func TestUnreadableRootIsFatal(t *testing.T) {
	if _, err := Build(Options{Root: filepath.Join(t.TempDir(), "does-not-exist"), CacheDir: t.TempDir()}); err == nil {
		t.Error("a missing root built an empty index instead of failing")
	}
}

// Dedup keys a loose file on its path within the pack. A file sitting directly under
// a vendor dir has no pack, and building that prefix by formatting left a doubled
// separator that matched nothing, so the copy never collapsed.
func TestDedupWithAndWithoutAPackDir(t *testing.T) {
	cards := func(layout ...string) int {
		root, mk := libRoot(t)
		dir := append(layout, "Heart.fbx")
		os.WriteFile(mk(dir...), []byte("FBXHEART"), 0o644)
		writeZip(t, mk(append(layout, "bundle.zip")...), map[string]string{"Heart.fbx": "FBXHEART"})
		assets, err := Scan(root)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, a := range assets {
			if a.Name == "Heart.fbx" {
				n++
			}
		}
		return n
	}
	if withPack, noPack := cards("synty", "Foo_Pack"), cards("synty"); withPack != 1 || noPack != 1 {
		t.Errorf("Heart.fbx cards: vendor/pack/ = %d, vendor/ = %d, want 1 each", withPack, noPack)
	}
}

// Extraction is single-flighted, so a failure has many callers waiting on it. Each
// has to be told what went wrong: re-reading the outcome from a shared map handed a
// nil error — indistinguishable from success — to everyone who arrived after the
// first waiter cleared the entry to re-arm the retry.
func TestFailedExtractionReachesEveryWaiter(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits")
	}
	root, mk := libRoot(t)
	pkg := mk("synty", "Pack", "Pack_Unity_v1.unitypackage")
	writeUnityPackage(t, pkg, []unityGUID{{guid: "abc123", pathname: "Assets/Heart.fbx", asset: "FBXBYTES"}})

	cacheDir := t.TempDir()
	ix, err := Build(Options{Root: root, CacheDir: cacheDir})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cacheDir, 0o500); err != nil { // no writes: the unpack dir cannot be made
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(cacheDir, 0o755) })

	const waiters = 24
	errs := make([]error, waiters)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = ix.ensureExtracted(pkg)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err == nil {
			t.Fatalf("waiter %d was told a failed extraction succeeded", i)
		}
	}
}

// An index built without a cache dir has nowhere to extract to. Joining onto an empty
// dir yields a relative path, so the unpack tree would land in the working directory
// — which may sit inside the library this tool never writes to.
func TestExtractWithoutACacheDirIsRefused(t *testing.T) {
	root, mk := libRoot(t)
	pkg := mk("synty", "Pack", "Pack_Unity_v1.unitypackage")
	writeUnityPackage(t, pkg, []unityGUID{{guid: "abc123", pathname: "Assets/Heart.fbx", asset: "FBXBYTES"}})

	ix, err := Build(Options{Root: root, CacheDir: ""})
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	t.Chdir(cwd)

	var unity Asset
	for _, a := range ix.Assets {
		if a.Source.Kind == SourceUnityPackage {
			unity = a
		}
	}
	if _, _, err := ix.Open(unity); !errors.Is(err, ErrNoCacheDir) {
		t.Errorf("Open err = %v, want ErrNoCacheDir", err)
	}
	entries, err := os.ReadDir(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("Open wrote %v into the working directory", entries)
	}
}

// A pack ships tens of thousands of entries and the grid fetches one asset per card,
// so archives stay open and shared. An eviction must not close a reader a response is
// still streaming through.
func TestZipReaderSurvivesEvictionMidStream(t *testing.T) {
	root, mk := libRoot(t)
	writeZip(t, mk("v", "Pack", "Pack_SourceFiles_v1.zip"), map[string]string{"Heart.fbx": "FBXHEART"})
	for i := 0; i <= zipCacheSize; i++ {
		writeZip(t, mk("v", "Pack", fmt.Sprintf("Pack_Other%d_v1.zip", i)), map[string]string{"X.fbx": "XBYTES"})
	}
	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string][]Asset{}
	for _, a := range ix.Assets {
		byName[a.Name] = append(byName[a.Name], a)
	}
	rc, _, err := ix.Open(byName["Heart.fbx"][0])
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	// Push the held archive out of the cache by touching more than it can hold.
	for _, a := range byName["X.fbx"] {
		other, _, err := ix.Open(a)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, other)
		other.Close()
	}

	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading an entry whose archive was evicted mid-stream: %v", err)
	}
	if string(b) != "FBXHEART" {
		t.Errorf("entry bytes = %q, want FBXHEART", b)
	}
}

// Prune has two halves and only the destructive one was covered: a fixture with no
// archives leaves `live` empty, so an implementation that deleted the whole unpacked
// tree would have passed. This pins the half that costs the user — a live extraction
// deleted out from under a running server means every asset in that pack 404s.
func TestPruneUnpackedKeepsLiveExtractions(t *testing.T) {
	root, mk := libRoot(t)
	writeUnityPackage(t, mk("synty", "P", "P_Unity_2022_3_v1.unitypackage"), []unityGUID{
		{guid: "aaa", pathname: "Assets/P/Rock.fbx", asset: "ROCKBYTES"},
	})
	cacheDir := t.TempDir()
	ix, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	var rock Asset
	for _, a := range ix.Assets {
		if a.Name == "Rock.fbx" {
			rock = a
		}
	}
	if rock.ID == "" {
		t.Fatal("the unitypackage entry is not in the index")
	}

	// Force the extraction, then note where it landed.
	rc, _, err := ix.Open(rock)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	live, err := ix.ensureExtracted(rock.Source.ArchivePath)
	if err != nil {
		t.Fatal(err)
	}

	stale := filepath.Join(ix.unpackedDir(), "deadbeefdeadbeef")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ix.PruneUnpacked(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("a stale extraction survived the prune")
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("the current index's own extraction was deleted: %v", err)
	}
	rc, _, err = ix.Open(rock)
	if err != nil {
		t.Fatalf("Open after prune: %v", err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if string(b) != "ROCKBYTES" {
		t.Errorf("content after prune = %q, want ROCKBYTES", b)
	}
}

// Two roots sharing one cache dir each prune against their own index. Without the
// state being keyed by root, the second run deletes the first's extractions — and
// `--addr` exists so two instances can be up at once, so it can happen underneath a
// server that is serving them.
func TestPruneDoesNotTouchAnotherRootsState(t *testing.T) {
	cacheDir := t.TempDir()
	build := func() (*Index, Asset) {
		t.Helper()
		root, mk := libRoot(t)
		writeUnityPackage(t, mk("synty", "P", "P_Unity_2022_3_v1.unitypackage"), []unityGUID{
			{guid: "aaa", pathname: "Assets/P/Rock.fbx", asset: "ROCKBYTES"},
		})
		ix, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir}, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range ix.Assets {
			if a.Name == "Rock.fbx" {
				return ix, a
			}
		}
		t.Fatal("no asset built")
		return nil, Asset{}
	}

	first, firstRock := build()
	firstDir, err := first.ensureExtracted(firstRock.Source.ArchivePath)
	if err != nil {
		t.Fatal(err)
	}

	second, _ := build()
	if err := second.PruneUnpacked(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(firstDir); err != nil {
		t.Errorf("the first root's extraction was pruned by the second root's run: %v", err)
	}
	rc, _, err := first.Open(firstRock)
	if err != nil {
		t.Fatalf("the first index can no longer serve: %v", err)
	}
	defer rc.Close()
}

// Extraction joins each member under a GUID directory, and the GUID comes straight out
// of a tar quarry did not produce. The predicates that reject a hostile name are unit
// tested; this pins the property that actually matters, across the two files that have
// to agree on it.
func TestExtractionCannotEscapeItsDirectory(t *testing.T) {
	_, mk := libRoot(t)
	pkg := mk("synty", "P", "P_Unity_2022_3_v1.unitypackage")
	writeUnityPackage(t, pkg, []unityGUID{
		{guid: "../../escaped", pathname: "Assets/P/Bad.fbx", asset: "BADBYTES"},
		{guid: "..", pathname: "Assets/P/Dots.fbx", asset: "DOTBYTES"},
		{guid: "/etc", pathname: "Assets/P/Abs.fbx", asset: "ABSBYTES"},
		{guid: "ok", pathname: "Assets/P/Good.fbx", asset: "GOODBYTES"},
	})

	sandbox := t.TempDir()
	dest := filepath.Join(sandbox, "unpacked")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := extractUnityPackage(pkg, dest); err != nil {
		t.Fatal(err)
	}

	var outside []string
	err := filepath.WalkDir(sandbox, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if rel, rerr := filepath.Rel(dest, p); rerr != nil || strings.HasPrefix(rel, "..") {
			outside = append(outside, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(outside) > 0 {
		t.Errorf("extraction wrote outside its directory: %v", outside)
	}
	if _, err := os.Stat(filepath.Join(dest, "ok", "asset")); err != nil {
		t.Errorf("the legitimate entry was not extracted: %v", err)
	}
}
