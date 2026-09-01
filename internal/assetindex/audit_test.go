package assetindex

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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

// writeFile creates path's parents and writes content. libRoot's mk covers fixtures
// inside the library; this covers the ones a symlink points at, which are outside it.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
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
	return cacheFile(cacheDir, abs)
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
	// Asserted by outcome, not by re-stating splitUnityName's own reject condition: a
	// test that only checks "if it was accepted, the guid is safe" holds for an
	// implementation that rejects everything, and never shows an ordinary name works.
	for _, tc := range []struct {
		in, guid, member string
		ok               bool
	}{
		{in: "a/b/asset", guid: "a", member: "b/asset", ok: true},
		{in: "./g/asset", guid: "g", member: "asset", ok: true}, // Unity writes some members this way
		{in: "../x/asset", ok: false},
		{in: "../asset", ok: false},
		{in: `..\x/asset`, ok: false},
		{in: `a\b/asset`, ok: false},
		{in: "asset", ok: false},   // no guid dir at all
		{in: "//asset", ok: false}, // empty guid
		// "." would extract to the package's own root, and as a fingerprint it is the
		// same "uguid:." for every archive that carries one.
		{in: "././asset", ok: false},
	} {
		guid, member, ok := splitUnityName(tc.in)
		if ok != tc.ok {
			t.Errorf("splitUnityName(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			continue
		}
		if ok && (guid != tc.guid || member != tc.member) {
			t.Errorf("splitUnityName(%q) = %q,%q; want %q,%q", tc.in, guid, member, tc.guid, tc.member)
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
	// Seeded under the live per-root tree, not the legacy <cacheDir>/unpacked one: the
	// legacy sweep removes that whole directory in one call, which would delete both
	// fixtures before either loop below ran and leave them untested.
	seed := func(parts ...string) string {
		dir := filepath.Join(append([]string{ix.stateDir(), "unpacked"}, parts...)...)
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

// A second quarry over the same root runs its own prune whenever it starts, and the
// prune removes whatever it does not recognise. An extraction assembled inside the
// swept tree was therefore deleted mid-write, and the rename that followed published
// a package missing whatever had not been written yet — cached as complete from then
// on. Staging lives outside that tree, and only an abandoned one is swept.
func TestPruneLeavesAnExtractionInFlight(t *testing.T) {
	root, mk := libRoot(t)
	os.WriteFile(mk("v", "Pack", "Sword.glb"), []byte("GLBBYTES"), 0o644)

	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// The guarantee is structural: staging is not somewhere the sweep walks.
	if strings.HasPrefix(ix.stagingDir(), ix.unpackedDir()) {
		t.Fatalf("staging %s sits inside the swept tree %s", ix.stagingDir(), ix.unpackedDir())
	}
	if err := os.MkdirAll(ix.stagingDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	inFlight, err := os.MkdirTemp(ix.stagingDir(), "unpack-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inFlight, "asset"), []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}
	abandoned, err := os.MkdirTemp(ix.stagingDir(), "unpack-*")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * staleStagingAge)
	if err := os.Chtimes(abandoned, old, old); err != nil {
		t.Fatal(err)
	}

	if err := ix.PruneUnpacked(); err != nil {
		t.Fatalf("PruneUnpacked: %v", err)
	}
	if _, err := os.Stat(inFlight); err != nil {
		t.Errorf("the prune deleted an extraction being written right now: %v", err)
	}
	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Errorf("an abandoned staging dir survived the prune (err=%v)", err)
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

// safeEntry is a predicate, and the thing that matters is that a hostile name never
// reaches an Asset at all. The unitypackage side has that end to end; the zip side had
// only the predicate, so a scan that stopped consulting it would still have passed.
func TestScanDropsAZipEntryThatWouldEscape(t *testing.T) {
	root, mk := libRoot(t)
	writeZip(t, mk("v", "Pack", "Pack_Unity_v1.zip"), map[string]string{
		"../escape.png":       "ESCAPED",
		"a/../../escape2.png": "ESCAPED",
		"Assets/Sword.fbx":    "FBXBYTES",
	})

	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range ix.Assets {
		if strings.Contains(a.Source.Entry, "..") || strings.Contains(a.RelPath, "..") {
			t.Errorf("indexed an escaping entry: %+v", a.Source)
		}
	}
	if len(ix.Assets) != 1 || ix.Assets[0].Name != "Sword.fbx" {
		t.Errorf("assets = %v, want only the one ordinary entry", names(ix.Assets))
	}
}

// A failure partway through extraction is the one os.RemoveAll(tmp) exists for: some
// members are already written when the read gives out, and publishing those would
// cache a package short whatever was never reached, with nothing ever re-reading it.
func TestExtractionFailingPartwayPublishesNothing(t *testing.T) {
	root, mk := libRoot(t)
	pkg := mk("synty", "Pack", "Pack_Unity_v1.unitypackage")
	guids := make([]unityGUID, 0, 24)
	for i := 0; i < 24; i++ {
		id := fmt.Sprintf("guid%02d", i)
		guids = append(guids, unityGUID{guid: id, pathname: "Assets/" + id + ".fbx", asset: strings.Repeat("FBXBYTES", 64)})
	}
	writeUnityPackage(t, pkg, guids)

	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// Cut the archive off mid-stream: the extraction gets underway and then the read
	// fails, which is what a damaged download looks like.
	full, err := os.ReadFile(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pkg, full[:len(full)*2/3], 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := ix.ensureExtracted(pkg); err == nil {
		t.Fatal("a truncated archive extracted without error")
	}
	published, err := os.ReadDir(ix.unpackedDir())
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, e := range published {
		t.Errorf("a failed extraction published %s", e.Name())
	}
	staged, err := os.ReadDir(ix.stagingDir())
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, e := range staged {
		t.Errorf("a failed extraction left %s staged", e.Name())
	}
}

// A derivation that failed is deliberately not remembered: the archive's print says
// what the file is, not whether reading it worked, so a transient disk-full must not
// poison the package for the rest of the process.
func TestExtractionRetriesAfterATransientFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits")
	}
	root, mk := libRoot(t)
	pkg := mk("synty", "Pack", "Pack_Unity_v1.unitypackage")
	writeUnityPackage(t, pkg, []unityGUID{
		{guid: "aaa111", pathname: "Assets/One.fbx", asset: "FBXBYTES-1"},
		{guid: "bbb222", pathname: "Assets/Two.fbx", asset: "FBXBYTES-2"},
	})

	cacheDir := t.TempDir()
	ix, err := Build(Options{Root: root, CacheDir: cacheDir})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cacheDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(cacheDir, 0o755) })
	if _, err := ix.ensureExtracted(pkg); err == nil {
		t.Fatal("extraction into an unwritable cache reported success")
	}

	os.Chmod(cacheDir, 0o755)
	dir, err := ix.ensureExtracted(pkg)
	if err != nil {
		t.Fatalf("extraction stayed poisoned after the cause was fixed: %v", err)
	}
	for _, guid := range []string{"aaa111", "bbb222"} {
		if _, err := os.Stat(filepath.Join(dir, guid, "asset")); err != nil {
			t.Errorf("the retry published an incomplete extraction, %s is missing: %v", guid, err)
		}
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

// An archive entry a loose twin suppressed must come back when that twin goes away.
// The cached asset set is post-dedup, so reusing an unchanged archive's cached
// enumeration alone would reuse the suppression with it and drop the asset for good:
// the file is still in the zip, and nothing would report it missing.
func TestRefreshRestoresAnEntryItsLooseTwinStopsSuppressing(t *testing.T) {
	root, mk := libRoot(t)
	cacheDir := t.TempDir()
	writeZip(t, mk("kevdev", "A", "A.zip"), map[string]string{"Animations/Idle.fbx": "IDLEBYTES"})
	loose := mk("kevdev", "A", "src", "Animations", "Idle.fbx")
	os.WriteFile(loose, []byte("IDLEBYTES"), 0o644)

	count := func(ix *Index) (n int) {
		for _, a := range ix.Assets {
			if a.Name == "Idle.fbx" {
				n++
			}
		}
		return n
	}

	ix, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := count(ix); got != 1 {
		t.Fatalf("Idle.fbx count = %d after the first build, want 1 (the loose copy suppresses the zip entry)", got)
	}

	if err := os.Remove(loose); err != nil {
		t.Fatal(err)
	}
	again, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := count(again); got != 1 {
		t.Errorf("Idle.fbx count = %d after the loose twin was deleted, want 1 (the zip entry it suppressed)", got)
	}

	// The whole point is that an incremental refresh describes the same library a
	// full scan would, so compare against one rather than only against a number.
	cold, err := LoadOrBuild(Options{Root: root, CacheDir: t.TempDir()}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := count(again), count(cold); got != want {
		t.Errorf("refreshed index has %d Idle.fbx, a fresh scan of the same tree has %d", got, want)
	}
}

// A zip may repeat an entry name. Serving resolves a name to the first match, so a
// second asset for the same name would be a card whose fingerprint describes bytes
// no request can reach — a tag applied to content the user never saw.
func TestDuplicateZipEntryNamesIndexOnce(t *testing.T) {
	root, mk := libRoot(t)
	zipPath := mk("vendor", "pack", "dup.zip")

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, body := range []string{"FIRSTBYTES", "SECONDBYTESXX"} {
		w, err := zw.Create("Models/Thing.fbx")
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zipPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	assets, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	var things []Asset
	for _, a := range assets {
		if a.Name == "Thing.fbx" {
			things = append(things, a)
		}
	}
	if len(things) != 1 {
		t.Fatalf("indexed %d assets named Thing.fbx, want 1", len(things))
	}
	// The survivor must be the entry serving resolves to, so its fingerprint
	// describes the bytes a content request actually returns.
	if want := crcFingerprint(crc32.ChecksumIEEE([]byte("FIRSTBYTES")), 10); things[0].Fingerprint != want {
		t.Errorf("fingerprint = %q, want %q (the first entry, which is what Open serves)", things[0].Fingerprint, want)
	}
}

// A tree indexes the same whether it shipped packed or extracted, so the archive
// walk drops the dot-paths the loose walk drops — including a dot-directory's whole
// contents, not just a dot-named file.
func TestDotPathsAreSkippedInsideArchivesToo(t *testing.T) {
	root, mk := libRoot(t)
	writeZip(t, mk("vendor", "pack", "p.zip"), map[string]string{
		"SourceFiles/Models/Keep.fbx":       "KEEP",
		"SourceFiles/.vscode/settings.json": "HIDDEN",
		".git/config":                       "HIDDEN",
		"SourceFiles/.editorconfig":         "HIDDEN",
	})
	assets, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range assets {
		if strings.Contains(a.Source.Entry, "/.") || strings.HasPrefix(a.Source.Entry, ".") {
			t.Errorf("indexed %q from inside a dot-path; the loose walk drops those", a.Source.Entry)
		}
	}
	if len(assets) != 1 {
		t.Errorf("indexed %d assets, want 1 (Keep.fbx)", len(assets))
	}
}

// A dot-named symlink is working state like any other dot-entry. Handling it as a
// link instead would report a skip naming --follow-symlinks for something a plain
// dot-dir is dropped for silently, and index a hidden tree when the flag is on.
func TestDotNamedSymlinksAreSkippedLikeDotDirs(t *testing.T) {
	root, mk := libRoot(t)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "Hidden.fbx"), []byte("HIDDEN"), 0o644)
	os.WriteFile(mk("vendor", "pack", "Keep.fbx"), []byte("KEEP"), 0o644)
	if err := os.Symlink(outside, filepath.Join(root, ".ref")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	for _, follow := range []bool{false, true} {
		ix, err := Build(Options{Root: root, FollowSymlinks: follow})
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range ix.Assets {
			if a.Name == "Hidden.fbx" {
				t.Errorf("follow=%v: indexed a file under a dot-named symlink", follow)
			}
		}
		for _, s := range ix.Skipped {
			if strings.HasPrefix(s.RelPath, ".ref") {
				t.Errorf("follow=%v: reported %q as a skip; a dot-entry is dropped silently", follow, s.RelPath)
			}
		}
	}
}

// The library is read-only: the tag store is the only thing quarry writes inside a
// user's tree, and everything here writes under the cache dir instead. Asserted over
// a whole scan-serve-prune cycle rather than one entry point, because the write that
// would break this is likelier to appear in extraction or pruning than in the walk.
func TestTheLibraryIsNeverWrittenTo(t *testing.T) {
	root, mk := libRoot(t)
	cacheDir := t.TempDir()
	writeZip(t, mk("synty", "Foo_Pack", "Foo_SourceFiles.zip"), map[string]string{
		"SourceFiles/Models/Heart.fbx":   "FBXHEART",
		"SourceFiles/Textures/Heart.png": "PNGHEART",
	})
	writeUnityPackage(t, mk("synty", "Foo_Pack", "Foo_Unity_2022_3_v1.unitypackage"), []unityGUID{
		{guid: "aaa", pathname: "Assets/Foo/Rock.prefab", asset: "PREFAB", preview: true},
	})
	os.WriteFile(mk("explosive", "RPG", "Sword.glb"), []byte("GLBBYTES"), 0o644)

	snapshot := func() map[string]string {
		out := map[string]string{}
		if err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			out[p] = fmt.Sprintf("%v|%d|%v|%v", d.IsDir(), fi.Size(), fi.Mode(), fi.ModTime())
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}

	before := snapshot()
	ix, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range ix.Assets {
		if rc, _, err := ix.Open(a); err == nil {
			io.Copy(io.Discard, rc)
			rc.Close()
		}
		if rc, _, err := ix.OpenThumbnail(a); err == nil {
			io.Copy(io.Discard, rc)
			rc.Close()
		}
	}
	if err := ix.PruneUnpacked(); err != nil {
		t.Fatal(err)
	}
	after := snapshot()

	for p, was := range before {
		switch now, still := after[p]; {
		case !still:
			t.Errorf("%s disappeared from the library", p)
		case now != was:
			t.Errorf("%s changed: %s -> %s", p, was, now)
		}
	}
	for p := range after {
		if _, had := before[p]; !had {
			t.Errorf("%s was created inside the library", p)
		}
	}
}

// The cache dir is whatever --cache or QUARRY_CACHE_DIR named, taken verbatim, so it
// can be a directory the user keeps other things in. A directory called "unpacked"
// there is not evidence quarry wrote it, and the prune must not delete it.
func TestPruneLeavesAUserDirectoryThatMerelyLooksLegacy(t *testing.T) {
	root, mk := libRoot(t)
	os.WriteFile(mk("v", "Pack", "Sword.glb"), []byte("GLBBYTES"), 0o644)
	cacheDir := t.TempDir()

	userWork := filepath.Join(cacheDir, "unpacked")
	if err := os.MkdirAll(userWork, 0o755); err != nil {
		t.Fatal(err)
	}
	notes := filepath.Join(userWork, "notes.txt")
	os.WriteFile(notes, []byte("months of work"), 0o644)
	userIndex := filepath.Join(cacheDir, "index.json")
	os.WriteFile(userIndex, []byte(`{"mine":true}`), 0o644)

	ix, err := Build(Options{Root: root, CacheDir: cacheDir})
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.PruneUnpacked(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{notes, userIndex} {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			t.Errorf("%s was deleted; quarry never wrote it", p)
		}
	}
}

// A cache dir an older quarry did write is still swept, so an upgrade does not
// strand the whole pre-per-root tree.
func TestPruneSweepsARealLegacyCache(t *testing.T) {
	root, mk := libRoot(t)
	os.WriteFile(mk("v", "Pack", "Sword.glb"), []byte("GLBBYTES"), 0o644)
	cacheDir := t.TempDir()

	old := filepath.Join(cacheDir, "unpacked", "16", "deadbeef")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(old, "asset"), []byte("old"), 0o644)
	legacyIndex := filepath.Join(cacheDir, "index.json")
	os.WriteFile(legacyIndex, []byte(`{"version":16,"root":"/somewhere","assets":[]}`), 0o644)

	ix, err := Build(Options{Root: root, CacheDir: cacheDir})
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.PruneUnpacked(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(cacheDir, "unpacked"), legacyIndex} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived; an older quarry's tree is regenerable state nothing will consult again", p)
		}
	}
}

// The refusal has to hold on the run that matters — the first one, when the cache dir
// does not exist yet. Resolving a missing path is what a naive check gets wrong: the
// root resolves through its symlinks and the cache dir does not, so a directory
// plainly inside the root compares as outside it, and quarry writes its index and
// every unpacked archive into the tree it promises to leave alone.
func TestCacheDirInsideASymlinkedRootIsRefused(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "lib")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	for _, c := range []struct {
		name     string
		cacheDir string
		want     bool
	}{
		{"not yet created, under the link", filepath.Join(link, "cache"), true},
		{"not yet created, under the resolved root", filepath.Join(real, "cache"), true},
		{"nested deeper", filepath.Join(link, "a", "b", "cache"), true},
		{"the root itself", link, true},
		{"genuinely outside", filepath.Join(t.TempDir(), "cache"), false},
	} {
		_, err := Build(Options{Root: link, CacheDir: c.cacheDir})
		refused := err != nil && strings.Contains(err.Error(), "inside the scan root")
		if refused != c.want {
			t.Errorf("%s: refused = %v, want %v (err = %v)", c.name, refused, c.want, err)
		}
	}
}

// A "." path segment is noise a writer emits, not a hidden name. Read as one, an
// archive written entirely that way enumerates to nothing — and because no error is
// raised, the empty enumeration is cached against the archive's stat print and never
// re-read. The whole pack disappears with nothing said.
func TestDotSegmentedArchiveEntriesStillIndex(t *testing.T) {
	root, mk := libRoot(t)
	writeZip(t, mk("v", "Pack", "Pack_SourceFiles_v1.zip"), map[string]string{
		"./Models/Heart.fbx": "FBXHEART",
		"Models/Rock.fbx":    "FBXROCK",
		// Still hidden: a real dot-name anywhere in the path.
		"./.git/config":              "GIT",
		"./Models/.DS_Store":         "DS",
		"./Models/.hidden/Ghost.fbx": "GHOST",
	})
	writeUnityPackage(t, mk("v", "Pack", "Pack_Unity_v1.unitypackage"), []unityGUID{
		{guid: "aaa", pathname: "./Assets/Sword.fbx", asset: "FBXSWORD"},
	})

	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, a := range ix.Assets {
		got[a.Name] = true
	}
	for _, want := range []string{"Heart.fbx", "Rock.fbx", "Sword.fbx"} {
		if !got[want] {
			t.Errorf("%s was dropped; indexed %v", want, names(ix.Assets))
		}
	}
	for _, hidden := range []string{"config", ".DS_Store", "Ghost.fbx"} {
		if got[hidden] {
			t.Errorf("%s was indexed; a real dot-segment is still hidden", hidden)
		}
	}
	if len(ix.Skipped) != 0 {
		t.Errorf("skipped %v; nothing here is unreadable", ix.Skipped)
	}
}

// Splitting a multi-clip .glb is the headline behaviour of the loose path, and every
// run after the first reaches it through the cache instead: refresh reuses whatever
// the previous index held for an unchanged file. A change that reused one asset per
// path rather than all of them would collapse a 120-card animation library to a single
// card on the second run, with every other test still green.
func TestRefreshKeepsEveryClipOfASplitGLB(t *testing.T) {
	root, mk := libRoot(t)
	glb := mk("quaternius", "UAL", "UAL1.glb")
	writeGLB(t, glb, "Walk", "Run", "Idle")
	cacheDir := t.TempDir()

	clips := func(ix *Index) map[string]string {
		t.Helper()
		out := map[string]string{}
		for _, a := range ix.Assets {
			if a.Source.Clip != "" {
				out[a.Source.Clip] = a.Fingerprint
			}
		}
		return out
	}

	first, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := clips(first)
	if len(want) != 3 {
		t.Fatalf("first build produced %d clips, want 3", len(want))
	}

	// Twice, because the first refresh writes the cache the second one reads back.
	for pass := 1; pass <= 2; pass++ {
		again, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir}, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := clips(again); !reflect.DeepEqual(got, want) {
			t.Fatalf("refresh %d gave clips %v, want %v", pass, got, want)
		}
	}

	// Editing the file re-derives them: the clip fingerprints carry the file's, so a
	// reuse that ignored the stat print would hand back the old ones.
	writeGLB(t, glb, "Walk", "Run", "Sprint")
	edited, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := clips(edited)
	if _, stale := got["Idle"]; stale || len(got) != 3 {
		t.Errorf("after an edit clips = %v, want Walk/Run/Sprint", got)
	}
	if got["Walk"] == want["Walk"] {
		t.Error("Walk kept its old fingerprint; the file's bytes changed")
	}
}

// The archive side of refresh has two tests that make reuse observable. The loose side
// had none, and a miss there is invisible in the output while costing a re-read and a
// CRC32 of every loose file in the library on every startup.
//
// Made observable the same way: the file is unreadable after the first build but its
// size and mtime are untouched, so a refresh that consults the cache never opens it and
// one that re-derives records a skip and loses the asset.
func TestRefreshReusesCachedLooseFingerprints(t *testing.T) {
	root, mk := libRoot(t)
	loose := mk("synty", "Pack", "Sword.glb")
	writeFile(t, loose, "GLBBYTES")
	cacheDir := t.TempDir()

	first, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Assets) != 1 {
		t.Fatalf("first build = %v, want the one loose file", names(first.Assets))
	}
	want := first.Assets[0].Fingerprint

	unreadable(t, loose)
	again, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Skipped) != 0 {
		t.Errorf("refresh re-read the file instead of reusing its cached fingerprint: %v", again.Skipped)
	}
	if len(again.Assets) != 1 || again.Assets[0].Fingerprint != want {
		t.Errorf("refresh gave %v, want the cached asset with fingerprint %s", names(again.Assets), want)
	}
}

// An assembled character has no bytes of its own: the frontend loads each part by id
// through /api/content, which only resolves for an asset the index kept. applySidekick
// resolves the parts from what survived its own pass, but dedup runs after it — so a
// part with a loose twin was suppressed and the character rendered short that limb.
func TestSidekickPartIDsResolveAfterDedup(t *testing.T) {
	root, mk := libRoot(t)
	writeUnityPackage(t, mk("synty", "SIDEKICK_D", "SIDEKICK_D_Unity_v1.unitypackage"), []unityGUID{
		{guid: "sk1", pathname: "Assets/S/Characters/Hero.sk", asset: "Name: Hero\nParts:\n- Name: SK_HEAD\n"},
		{guid: "hd1", pathname: "Assets/S/Resources/SK_HEAD.fbx", asset: "HEADFBX"},
	})
	// The same bytes at the same pack-relative subpath, extracted beside the package.
	writeFile(t, mk("synty", "SIDEKICK_D", "Assets", "S", "Resources", "SK_HEAD.fbx"), "HEADFBX")

	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	for i := range ix.Assets {
		if ix.Assets[i].Thumb == ThumbSidekick {
			parts = ix.Assets[i].Source.Parts
		}
	}
	if len(parts) == 0 {
		t.Fatal("the character did not assemble")
	}
	for _, pid := range parts {
		if _, ok := ix.Lookup(pid); !ok {
			t.Errorf("part id %s does not resolve through Lookup; dedup suppressed a mesh the character serves", pid)
		}
	}
}

// A Sidekick package extracted beside itself is an ordinary layout, and the .sk goes
// out with the rest of the tree. Dedup keys an archive entry on its pathname and size,
// neither of which assembly moves — so without a guard the loose twin wins and the
// character the pack exists for disappears, along with the byproducts assembly already
// dropped in its favour. The twin left behind is a plain data row: no parts, no
// thumbnail, nothing to preview.
func TestAssembledCharacterSurvivesItsLooseTwin(t *testing.T) {
	root, mk := libRoot(t)
	sk := "Name: Hero\nParts:\n- Name: SK_HEAD\n"
	writeUnityPackage(t, mk("synty", "SIDEKICK_D", "SIDEKICK_D_Unity_v1.unitypackage"), []unityGUID{
		{guid: "sk1", pathname: "Assets/S/Characters/Hero.sk", asset: sk},
		{guid: "hd1", pathname: "Assets/S/Resources/SK_HEAD.fbx", asset: "HEADFBX"},
	})
	writeFile(t, mk("synty", "SIDEKICK_D", "Assets", "S", "Resources", "SK_HEAD.fbx"), "HEADFBX")
	writeFile(t, mk("synty", "SIDEKICK_D", "Assets", "S", "Characters", "Hero.sk"), sk)

	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	for i := range ix.Assets {
		if ix.Assets[i].Thumb == ThumbSidekick {
			parts = ix.Assets[i].Source.Parts
		}
	}
	if len(parts) != 1 {
		t.Fatalf("assembled character has %d parts, want 1; dedup dropped it for the loose .sk", len(parts))
	}
	if _, ok := ix.Lookup(parts[0]); !ok {
		t.Errorf("part id %s does not resolve", parts[0])
	}
}

// --follow-symlinks widens the library to every target the walk followed, and Open
// serves from those as readily as from the root. The cache dir has to be outside all
// of them: inside one, the run writes its index and every unpacked archive into a tree
// the next run walks and this run prunes.
func TestCacheDirInsideAFollowedLinkTargetIsRefused(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "lib")
	outside := filepath.Join(base, "drive2")
	writeFile(t, filepath.Join(outside, "Vendor", "Pack", "a.fbx"), "FBX")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "drive2")); err != nil {
		t.Fatal(err)
	}
	opt := Options{Root: root, CacheDir: filepath.Join(outside, "quarry-cache"), FollowSymlinks: true}
	if _, err := LoadOrBuild(opt, true, func(string) {}); err == nil {
		t.Fatal("a cache dir inside a followed symlink target was accepted")
	}
	// Not followed, the target is not part of the library and the cache dir is fine.
	opt.FollowSymlinks = false
	if _, err := LoadOrBuild(opt, true, func(string) {}); err != nil {
		t.Errorf("unfollowed, the same cache dir is outside the library: %v", err)
	}
}

// refresh keeps the assets of an archive whose second enumeration pass failed while
// declining to cache its stat print, so the print is what the index will *reuse* and
// not what it *references*. A prune keyed on the print alone deletes the extraction
// those assets are served from.
func TestPruneKeepsAnExtractionTheIndexStillReferences(t *testing.T) {
	root, mk := libRoot(t)
	pkg := mk("v", "Pack", "Pack.unitypackage")
	writeUnityPackage(t, pkg, []unityGUID{
		{guid: "aaa", pathname: "Assets/M/thing.fbx", asset: "FBXBYTES"},
	})
	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	dir, err := ix.ensureExtracted(pkg)
	if err != nil {
		t.Fatal(err)
	}
	// The state a degraded pass leaves behind: assets kept, print dropped.
	delete(ix.ArchivePrint, pkg)
	if err := ix.PruneUnpacked(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("prune removed the extraction of an archive the index serves %d assets from: %v", len(ix.Assets), err)
	}
}

// An archive written with "./"-prefixed entries indexes (skipEntry tolerates the
// segment) and displays cleaned, so its dedup key has to be cleaned too — otherwise
// the entry and its extracted twin key differently and the library shows two cards for
// one file, differing only by a segment neither side displays.
func TestDotSegmentedEntriesDedupAgainstTheirTwin(t *testing.T) {
	for _, prefix := range []string{"", "./"} {
		t.Run("prefix"+prefix, func(t *testing.T) {
			root, mk := libRoot(t)
			writeUnityPackage(t, mk("v", "P", "P.unitypackage"), []unityGUID{
				{guid: "g1", pathname: prefix + "Models/Heart.fbx", asset: "HEARTBYTES"},
			})
			writeFile(t, mk("v", "P", "Models", "Heart.fbx"), "HEARTBYTES")
			ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			if len(ix.Assets) != 1 {
				t.Fatalf("kept %d assets, want the archive entry deduped against its loose twin", len(ix.Assets))
			}
			if ix.Assets[0].Source.Kind != SourceLoose {
				t.Errorf("survivor is %s, want the loose twin", ix.Assets[0].Source.Kind)
			}
		})
	}
}

// An extraction is published by a rename, which survives a crashing process but not a
// crashing machine. Nothing revalidates a published one — the fast path is a stat of a
// directory named for a fingerprint that does not move — so a short member would be
// served as the asset forever. The size the scan read is the authority.
func TestATruncatedExtractedMemberIsRebuilt(t *testing.T) {
	root, mk := libRoot(t)
	pkg := mk("v", "Pack", "Pack.unitypackage")
	writeUnityPackage(t, pkg, []unityGUID{
		{guid: "aaa", pathname: "Assets/M/thing.fbx", asset: "FBXBYTES"},
	})
	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	a := ix.Assets[0]
	rc, _, err := ix.Open(a)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()

	dir, err := ix.ensureExtracted(pkg)
	if err != nil {
		t.Fatal(err)
	}
	member := filepath.Join(dir, "aaa", "asset")
	if err := os.WriteFile(member, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	rc, n, err := ix.Open(a)
	if err != nil {
		t.Fatalf("opening an asset whose extracted member came back short: %v", err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if n != a.Size || string(b) != "FBXBYTES" {
		t.Errorf("served %d bytes %q, want %d bytes FBXBYTES", n, b, a.Size)
	}
}

// The zip reader cache publishes a slot under the mutex and then opens it with the
// mutex released, so waiters attach to an open already in flight, an eviction can land
// while one is under way, and a failed open has to unpublish before the next caller
// retries. Sequentially none of those overlap.
func TestConcurrentZipReads(t *testing.T) {
	root, mk := libRoot(t)
	const archives = zipCacheSize * 3
	for i := range archives {
		writeZip(t, mk("v", "Pack", fmt.Sprintf("Pack_A%02d_v1.zip", i)),
			map[string]string{"Heart.fbx": fmt.Sprintf("BYTES%02d", i)})
	}
	// A truncated archive so the unpublish-on-failure path runs alongside the rest.
	writeFile(t, mk("v", "Pack", "Pack_Broken_v1.zip"), "PK\x03\x04 not really a zip")

	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Assets) != archives {
		t.Fatalf("indexed %d assets, want %d", len(ix.Assets), archives)
	}
	broken := Asset{Source: Source{Kind: SourceZip, ArchivePath: mk("v", "Pack", "Pack_Broken_v1.zip"), Entry: "Heart.fbx"}}

	var wg sync.WaitGroup
	errs := make(chan error, archives*8)
	for range 4 {
		for i := range archives {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rc, _, err := ix.Open(ix.Assets[i])
				if err != nil {
					errs <- fmt.Errorf("open %s: %w", ix.Assets[i].Name, err)
					return
				}
				defer rc.Close()
				b, err := io.ReadAll(rc)
				if err != nil {
					errs <- err
					return
				}
				if want := ix.Assets[i].Fingerprint; want == "" {
					errs <- fmt.Errorf("asset %d has no fingerprint", i)
				} else if len(b) != int(ix.Assets[i].Size) {
					errs <- fmt.Errorf("read %d bytes, want %d", len(b), ix.Assets[i].Size)
				}
			}()
			wg.Add(1)
			go func() {
				defer wg.Done()
				if rc, _, err := ix.Open(broken); err == nil {
					rc.Close()
					errs <- errors.New("a truncated archive opened")
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// A torn member rebuilds the whole extraction, and the rebuild used to remove the tree
// out from under readers that had already passed ensureExtracted's stat of it. The
// removal is not instantaneous over a package holding thousands of members, so sibling
// after sibling whose own bytes were never torn opened a path that had just gone away —
// and browse answers that with a plain 404, indistinguishable from an asset that never
// existed.
func TestRebuildingATornExtractionDoesNotFailHealthySiblings(t *testing.T) {
	root, mk := libRoot(t)
	pkg := mk("v", "Pack", "Pack.unitypackage")
	const members = 400
	guids := make([]unityGUID, 0, members)
	for i := range members {
		guids = append(guids, unityGUID{
			guid:     fmt.Sprintf("g%04d", i),
			pathname: fmt.Sprintf("Assets/M/thing%04d.fbx", i),
			asset:    fmt.Sprintf("FBXBYTES-%04d", i),
		})
	}
	writeUnityPackage(t, pkg, guids)
	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Assets) != members {
		t.Fatalf("indexed %d assets, want %d", len(ix.Assets), members)
	}
	dir, err := ix.ensureExtracted(pkg)
	if err != nil {
		t.Fatal(err)
	}
	// One member torn the way a crash mid-extraction leaves it: the rename landed, the
	// data blocks did not.
	torn := ix.Assets[0]
	if err := os.WriteFile(filepath.Join(dir, torn.Source.Guid, "asset"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, members)
	start := make(chan struct{})
	open := func(a Asset) error {
		rc, _, err := ix.Open(a)
		if err != nil {
			return fmt.Errorf("open %s: %w", a.Name, err)
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		if err != nil {
			return fmt.Errorf("read %s: %w", a.Name, err)
		}
		if len(b) != int(a.Size) {
			return fmt.Errorf("%s: read %d bytes, want %d", a.Name, len(b), a.Size)
		}
		return nil
	}
	for i := range members {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := open(ix.Assets[i]); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	n := 0
	for err := range errs {
		if n < 3 {
			t.Error(err)
		}
		n++
	}
	if n > 0 {
		t.Errorf("%d of %d members failed while the torn one rebuilt", n, members)
	}
}

// browse tells a miss from a real failure by fs.ErrNotExist alone, answering the first
// with a 404 and the second with a 500 naming the cause. The loose and unpacked
// branches of Open get that from the filesystem; the zip branch builds its own error
// for an entry the central directory does not carry, and unwrapped it was the one miss
// in the set that came back as a server failure.
func TestAMissingZipEntryReportsAMiss(t *testing.T) {
	root, mk := libRoot(t)
	archive := mk("v", "Pack", "Pack_A_v1.zip")
	writeZip(t, archive, map[string]string{"Heart.fbx": "BYTES"})
	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Assets) != 1 {
		t.Fatalf("indexed %d assets, want 1", len(ix.Assets))
	}

	// The asset the index holds, for an entry the archive stopped carrying — a pack
	// re-shipped under the same name while quarry was running.
	gone := ix.Assets[0]
	gone.Source.Entry = "Gone.fbx"
	_, _, err = ix.Open(gone)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open = %v, want it to wrap fs.ErrNotExist so browse answers 404 rather than 500", err)
	}
	// Still legible: the wrap must not cost the message naming what was looked for.
	if err == nil || !strings.Contains(err.Error(), "Gone.fbx") {
		t.Errorf("Open error %v does not name the entry", err)
	}
}

// The reader cache holds an archive's parsed central directory, which maps an entry
// name to an offset in the file it was read from. A pack re-shipped in place keeps its
// path and its inode, so reusing that directory across the rewrite resolves names into
// a file that no longer has that shape. The entry removed outright was the worst of it:
// an empty body under the old entry's Content-Length, with nothing reporting a problem.
func TestARewrittenArchiveIsNotServedFromTheCachedDirectory(t *testing.T) {
	root, mk := libRoot(t)
	archive := mk("v", "Pack", "Pack_A_v1.zip")
	writeZip(t, archive, map[string]string{"Heart.fbx": "ORIGINAL-BYTES"})
	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Assets) != 1 {
		t.Fatalf("indexed %d assets, want 1", len(ix.Assets))
	}
	a := ix.Assets[0]
	read := func() (string, int64, error) {
		rc, n, err := ix.Open(a)
		if err != nil {
			return "", 0, err
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		return string(b), n, err
	}

	// Populates the cache with this archive's directory.
	if got, _, err := read(); err != nil || got != "ORIGINAL-BYTES" {
		t.Fatalf("read = %q, %v; want the original bytes", got, err)
	}

	// Re-shipped in place, the entry still there but longer.
	writeZip(t, archive, map[string]string{"Heart.fbx": "REPLACED-BYTES-AND-THEN-SOME"})
	got, n, err := read()
	if err != nil {
		t.Fatalf("read after the rewrite: %v", err)
	}
	if got != "REPLACED-BYTES-AND-THEN-SOME" {
		t.Errorf("read = %q, want the bytes the archive holds now", got)
	}
	if n != int64(len(got)) {
		t.Errorf("reported size %d, want %d: a Content-Length from the stale directory truncates the response", n, len(got))
	}

	// Re-shipped without the entry at all.
	writeZip(t, archive, map[string]string{"Other.fbx": "SOMETHING-ELSE-ENTIRELY"})
	got, _, err = read()
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("read = %q, %v; want fs.ErrNotExist for an entry the archive no longer carries", got, err)
	}
}

// A unitypackage replaced in place is the ordinary way a pack is updated, and the size
// the running index carries for its members is then the old one. Read as a torn tree,
// every request for a changed member discarded the whole extraction and decompressed
// the package again — per request, forever, taking the tree its healthy siblings were
// being served from with it — and still answered 500. The zip path has always handled
// the same update correctly; this pins the unitypackage one to the same outcome.
func TestReshippedUnityPackageIsAMissNotAnEndlessRebuild(t *testing.T) {
	root, mk := libRoot(t)
	archive := mk("v", "Pack", "Pack_A_v1.unitypackage")
	const guid = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	writeUnityPackage(t, archive, []unityGUID{{guid: guid, pathname: "Assets/Heart.fbx", asset: "ORIGINAL"}})
	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Assets) != 1 {
		t.Fatalf("indexed %d assets, want 1", len(ix.Assets))
	}
	a := ix.Assets[0]
	read := func() error {
		rc, _, err := ix.Open(a)
		if err != nil {
			return err
		}
		defer rc.Close()
		_, err = io.ReadAll(rc)
		return err
	}
	if err := read(); err != nil {
		t.Fatalf("first read: %v", err)
	}

	// Re-shipped in place: same path, same guid, a longer payload.
	time.Sleep(10 * time.Millisecond) // the stat print is size+mtime; move the mtime
	writeUnityPackage(t, archive, []unityGUID{{guid: guid, pathname: "Assets/Heart.fbx", asset: "REPLACED-AND-LONGER"}})

	err = read()
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("read after the reship = %v; want fs.ErrNotExist so browse answers 404 like the zip path", err)
	}

	// And the extraction is left alone, rather than discarded and rebuilt per request.
	fp, err := fingerprint(archive)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(ix.unpackedDir(), fp, "SENTINEL")
	if err := os.WriteFile(sentinel, []byte("x"), 0o644); err != nil {
		t.Fatalf("no extraction to mark: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := read(); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("read %d = %v; want a stable fs.ErrNotExist", i, err)
		}
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("the extraction was rebuilt for a mismatch the archive itself explains: %v", err)
	}
}

// A genuinely torn tree is still repaired — but once. The repair costs a full
// decompress, so a mismatch that survives it is not the tree's fault and repeating it
// would spend that cost on every request for the rest of the run.
func TestATornExtractionIsRebuiltOnceNotPerRequest(t *testing.T) {
	root, mk := libRoot(t)
	archive := mk("v", "Pack", "Pack_A_v1.unitypackage")
	const guid = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	writeUnityPackage(t, archive, []unityGUID{{guid: guid, pathname: "Assets/Heart.fbx", asset: "ORIGINALBYTES"}})
	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	a := ix.Assets[0]
	read := func() (string, error) {
		rc, _, err := ix.Open(a)
		if err != nil {
			return "", err
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		return string(b), err
	}
	if got, err := read(); err != nil || got != "ORIGINALBYTES" {
		t.Fatalf("first read = %q, %v", got, err)
	}

	fp, err := fingerprint(archive)
	if err != nil {
		t.Fatal(err)
	}
	member := filepath.Join(ix.unpackedDir(), fp, guid, "asset")
	// Torn the way a crash between the rename and the data blocks leaves it: the archive
	// is untouched, so the fingerprint still matches and only the member is short.
	if err := os.WriteFile(member, []byte("SHORT"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := read(); err != nil || got != "ORIGINALBYTES" {
		t.Fatalf("a torn member must be rebuilt from the archive: got %q, %v", got, err)
	}

	// Tear it again. The rebuild is spent, so this reports rather than re-extracting.
	if err := os.WriteFile(member, []byte("SHORT"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := read(); err == nil {
		t.Errorf("read = %q; want an error rather than a second full re-extraction", got)
	}
}

// A preview.png has no indexed size to check against, so the one member the size check
// cannot reach was also the one the rebuild could never reach: a blank one sits behind
// a stat of a directory named for a print that does not move, outliving --reindex.
// Emptiness is the evidence for it instead — no PNG is zero bytes.
func TestATornPreviewIsRebuilt(t *testing.T) {
	root, mk := libRoot(t)
	archive := mk("v", "Pack", "Pack_A_v1.unitypackage")
	const guid = "cccccccccccccccccccccccccccccccc"
	writeUnityPackage(t, archive, []unityGUID{{guid: guid, pathname: "Assets/Rock.fbx", asset: "FBXBYTES", preview: true}})
	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	a := ix.Assets[0]
	readThumb := func() (string, error) {
		rc, _, err := ix.OpenThumbnail(a)
		if err != nil {
			return "", err
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		return string(b), err
	}
	if got, err := readThumb(); err != nil || got != "PNGPREVIEW" {
		t.Fatalf("first thumbnail read = %q, %v", got, err)
	}
	fp, err := fingerprint(archive)
	if err != nil {
		t.Fatal(err)
	}
	preview := filepath.Join(ix.unpackedDir(), fp, guid, "preview.png")
	if err := os.WriteFile(preview, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := readThumb(); err != nil || got != "PNGPREVIEW" {
		t.Errorf("a zero-length preview must be rebuilt: got %q, %v", got, err)
	}
}

// The retry inside uniqueClipNames is what stops a generated label from landing on a
// real one. The duplicate case alone never reaches it — the first candidate is always
// free — so this uses a file that already numbers its own duplicates, which a pipeline
// that exported them once has produced.
func TestAGeneratedClipLabelCannotTakeARealOne(t *testing.T) {
	got := uniqueClipNames([]string{"Walk", "Walk (2)", "Walk"})
	want := []string{"Walk", "Walk (2)", "Walk (3)"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("uniqueClipNames = %v, want %v: the third clip took the second's label, and with it its id and fingerprint", got, want)
	}
	// The same collision arriving from the other side.
	got = uniqueClipNames([]string{"", "clip 1", ""})
	for i := range got {
		for j := range got {
			if i != j && got[i] == got[j] {
				t.Fatalf("uniqueClipNames = %v: %q is not unique", got, got[i])
			}
		}
	}
}

// A loose file unreadable on the first build must be skipped, not indexed with an empty
// fingerprint: an asset with no fingerprint is untaggable and unlinkable, and this one
// would not open either. Every other use of unreadable() here asserts cache reuse, so
// nothing covered the cold path.
func TestAnUnreadableLooseFileIsSkippedNotIndexedBlank(t *testing.T) {
	root, mk := libRoot(t)
	p := mk("v", "Pack", "Locked.fbx")
	if err := os.WriteFile(p, []byte("FBXBYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(p, 0o644) })
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 file regardless")
	}
	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatalf("one unreadable file must not fail the build: %v", err)
	}
	if len(ix.Assets) != 0 {
		t.Errorf("indexed %d assets, want 0: a file whose fingerprint could not be read is a skip, not a blank card", len(ix.Assets))
	}
	if len(ix.Skipped) != 1 || !strings.Contains(ix.Skipped[0].RelPath, "Locked.fbx") {
		t.Errorf("skipped = %v, want one entry naming Locked.fbx", ix.Skipped)
	}
}

// A cache that cannot be written is not fatal — the index in hand is usable — but it is
// not swallowed either: silently failing here re-pays a whole library scan every run.
// Both halves are load-bearing and neither was pinned, because no test in this package
// passed a warn that recorded anything.
func TestAnUnwritableCacheWarnsAndKeepsServing(t *testing.T) {
	root, mk := libRoot(t)
	if err := os.WriteFile(mk("v", "Pack", "Rock.fbx"), []byte("ROCK"), 0o644); err != nil {
		t.Fatal(err)
	}
	cacheDir := t.TempDir()
	// A directory where the index JSON goes: the write fails for every user, including
	// root, without needing a permission bit.
	if err := os.MkdirAll(cacheFile(cacheDir, mustAbs(t, root)), 0o755); err != nil {
		t.Fatal(err)
	}
	var warnings []string
	ix, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir}, false, func(m string) { warnings = append(warnings, m) })
	if err != nil {
		t.Fatalf("an unwritable cache must not fail the run: %v", err)
	}
	if len(ix.Assets) != 1 {
		t.Errorf("indexed %d assets, want 1", len(ix.Assets))
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "cache") {
		t.Errorf("warnings = %v, want one naming the cache", warnings)
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// Under --follow-symlinks the stat print keys on the resolved path while every display
// field comes from the path the walk took to reach it. Renaming the link moves the
// second and not the first, so a blind reuse kept the old drive's name in the grid, in
// the vendor facet and in `path:` search until the file's own size or mtime moved.
func TestRenamingAFollowedLinkRederivesItsDisplayFields(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	pack := filepath.Join(outside, "Synty", "POLYGON_Nature")
	if err := os.MkdirAll(pack, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pack, "Tree.fbx"), []byte("FBXBYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "drive2")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	opt := Options{Root: root, CacheDir: t.TempDir(), FollowSymlinks: true}
	first, err := LoadOrBuild(opt, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Assets) != 1 || first.Assets[0].Vendor != "drive2" {
		t.Fatalf("first build = %+v, want one asset under vendor drive2", first.Assets)
	}
	if err := os.Rename(link, filepath.Join(root, "synty-drive")); err != nil {
		t.Fatal(err)
	}
	again, err := LoadOrBuild(opt, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Assets) != 1 {
		t.Fatalf("second build indexed %d assets, want 1", len(again.Assets))
	}
	a := again.Assets[0]
	if a.Vendor != "synty-drive" || !strings.HasPrefix(a.RelPath, "synty-drive/") {
		t.Errorf("after the rename: vendor %q, relpath %q; want the path the walk now takes", a.Vendor, a.RelPath)
	}
}

// dedup exists so a pack shipping one file both loose and inside an archive shows one
// card. Splitting a multi-clip GLB into per-clip assets left nothing carrying the
// file's own path, so the archive's copy matched no loose key and survived beside the
// clips as a duplicate whole-file card.
func TestASplitGLBStillSuppressesItsArchiveTwin(t *testing.T) {
	for _, tc := range []struct {
		name  string
		anims []string
	}{
		{"single clip", []string{"Walk"}},
		{"multi clip", []string{"Walk", "Run"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, mk := libRoot(t)
			loose := mk("v", "Pack", "Anim.glb")
			writeGLB(t, loose, tc.anims...)
			b, err := os.ReadFile(loose)
			if err != nil {
				t.Fatal(err)
			}
			writeZip(t, mk("v", "Pack", "Pack_Unity_v1.zip"), map[string]string{"Anim.glb": string(b)})
			ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			for _, a := range ix.Assets {
				if a.Source.Kind == SourceZip {
					t.Errorf("the archive copy of %s survived as a separate card (%s)", loose, a.RelPath)
				}
			}
			if len(ix.Suppressed) != 1 {
				t.Errorf("suppressed %d entries, want the archive's one copy", len(ix.Suppressed))
			}
		})
	}
}
