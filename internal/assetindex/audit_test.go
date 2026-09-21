// Guard tests accumulated from audits. Two rules they are written to, both learned
// from guards that had quietly stopped checking anything:
//
// A guard over a value it does not own derives that value rather than restating it. A
// restated copy narrows the guard to whatever someone remembered to add to the copy,
// and it must also assert its own parsing found something, so a format change fails
// loudly rather than matching nothing.
//
// A guard over a constant straddles it: two inputs either side of it that land on
// different answers, then an assertion that the constant lies between them. Written
// well clear of it, a test pins only the direction and leaves the value free to drift.

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
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
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

// cacheFileFor is where LoadOrBuild keeps one library's index. Tests that reach for
// the cache file go through this rather than assembling a path, so the layout stays a
// single decision inside the package. follow is part of the address, not a detail: a
// following run and a non-following one over the same root are two libraries and keep
// two trees.
func cacheFileFor(t *testing.T, cacheDir, root string, follow bool) string {
	t.Helper()
	abs, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	return cacheFile(cacheDir, abs, follow)
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
	// A multi-clip GLB, with a duplicate name so the disambiguated label is exercised
	// too. Source.Clip and Source.ClipIndex are the two indexed fields whose loss is
	// invisible: every run after the first serves clips out of this cache, and a clip
	// that comes back without its index falls through to matching the *disambiguated*
	// label against the file's real animation names — which "Walk (2)" is not one of —
	// so the lightbox plays an arbitrary animation and nothing errors. Without a GLB in
	// this fixture the whole-asset comparison below had no clip to compare.
	writeGLB(t, mk("quaternius", "UAL", "UAL1.glb"), "Walk", "Walk", "Idle")

	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var clips int
	for _, a := range ix.Assets {
		if a.Source.Clip != "" {
			clips++
			if a.Source.ClipIndex == nil {
				t.Fatalf("%s was built with no clip index; the round trip below cannot check one", a.RelPath)
			}
		}
	}
	if clips != 3 {
		t.Fatalf("built %d clip assets, want 3; this fixture is no longer exercising the clip fields", clips)
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
		// Checked outside sameAsset, which compares the two marshalled: a field tagged
		// json:"-" vanishes from both sides and compares equal, so the one regression
		// that matters here — a clip field that stops being serialized — is exactly the
		// one that comparison cannot see.
		if want.Source.Clip == "" {
			continue
		}
		if got.Source.Clip != want.Source.Clip {
			t.Errorf("%s: clip label %q after the round trip, want %q", want.RelPath, got.Source.Clip, want.Source.Clip)
		}
		if got.Source.ClipIndex == nil {
			t.Errorf("%s: clip index is gone after the round trip; the preview falls back to matching a disambiguated label no animation carries", want.RelPath)
		} else if *got.Source.ClipIndex != *want.Source.ClipIndex {
			t.Errorf("%s: clip index %d after the round trip, want %d", want.RelPath, *got.Source.ClipIndex, *want.Source.ClipIndex)
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
	cachePath := cacheFileFor(t, cacheDir, root, false)

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
	cachePath := cacheFileFor(t, cacheDir, root, false)
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

	ix, err := Build(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	assets := ix.Assets
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
		ix, err := Build(Options{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		assets := ix.Assets
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

	ix, err := Build(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	assets := ix.Assets
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
		// skipEntry's other half, which had no archive-side test at all: the only .meta
		// assertion in the package was over a loose file, so it exercised the walk's own
		// check rather than this one. An extracted-Unity-project zip ships a .meta beside
		// every asset, so a regression here roughly doubles those archives' card count
		// with every one of them landing as a "data" card — the same silent doubling the
		// dot-path cases above exist to prevent.
		"SourceFiles/Models/Keep.fbx.meta":   "SIDECAR",
		"SourceFiles/Models/Keep.fbx.import": "SIDECAR",
	})
	ix, err := Build(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	assets := ix.Assets
	for _, a := range assets {
		if strings.Contains(a.Source.Entry, "/.") || strings.HasPrefix(a.Source.Entry, ".") {
			t.Errorf("indexed %q from inside a dot-path; the loose walk drops those", a.Source.Entry)
		}
		if ext := strings.ToLower(path.Ext(a.Source.Entry)); ext == ".meta" || ext == ".import" {
			t.Errorf("indexed the engine sidecar %q; the loose walk drops those too", a.Source.Entry)
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
	// Torn the way a crash mid-extraction leaves it: the rename landed, the data blocks
	// did not. A run of them rather than one, because that is the shape the failure has
	// — safewrite.Stream does not fsync, so every member still buffered when the machine
	// stopped comes back short together, and one grid load asks for several of them at
	// once. With a single torn member every reader but one is healthy, which hides
	// whether a reader that finds its own member short survives someone else's repair.
	const tornCount = 40
	for i := range tornCount {
		torn := ix.Assets[i]
		if err := os.WriteFile(filepath.Join(dir, torn.Source.Guid, "asset"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
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
	// And the reference taken to look is given back. Nothing else notices if it is not:
	// eviction only closes a reader at refs == 0, so a release dropped on this path
	// pins the archive's descriptor and its parsed central directory for the life of
	// the process — one per archive that ever answers a miss, which over a library of
	// re-shipped packs is every one of them.
	ix.zips.mu.Lock()
	ref, cached := ix.zips.open[archive]
	refs := 0
	if cached {
		refs = ref.refs
	}
	ix.zips.mu.Unlock()
	// The `cached` half is asserted, not merely guarded on: with no reader published at
	// all the reference check below has nothing to look at and passes over an empty
	// cache, which is exactly what a refactor that dropped the cache would leave.
	if !cached {
		t.Fatalf("no reader is published for %s after a miss; this guard is checking an empty cache", archive)
	}
	if refs != 0 {
		t.Errorf("the cached reader for %s still holds %d references after a miss; its descriptor is pinned for the process lifetime", archive, refs)
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

	// The original extraction is left alone, and — the part the cost lives in — no
	// extraction is built for the new archive either. The tree is named for the
	// archive's print, so a re-shipped one always misses the fast path; extracting it
	// produces a tree correct under its new print that every size in this index still
	// disagrees with, so the whole decompress is written and then never served from.
	// Counting the trees is what says that, where marking one only said the old tree
	// survived.
	trees := func() []string {
		t.Helper()
		ents, err := os.ReadDir(ix.unpackedDir())
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		slices.Sort(names)
		return names
	}
	before := trees()
	if len(before) != 1 {
		t.Fatalf("extractions before the rereads = %v, want the one the first read built", before)
	}
	for i := 0; i < 3; i++ {
		if err := read(); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("read %d = %v; want a stable fs.ErrNotExist", i, err)
		}
	}
	if after := trees(); !slices.Equal(before, after) {
		t.Errorf("extractions went %v -> %v: a mismatch the archive itself explains was paid for with a decompress", before, after)
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
	if err := os.MkdirAll(cacheFile(cacheDir, mustAbs(t, root), false), 0o755); err != nil {
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
// Both reuse branches carry the same guard and both have to be exercised: refresh keys
// the loose branch on LoosePrint and the archive branch on ArchivePrint, and each
// consults describes separately. An archive fixture is the one that matters most, since
// its extraction directory is keyed off the print that does not move either.
func TestRenamingAFollowedLinkRederivesItsDisplayFields(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, pack string)
		want  string // the asset name the rebuilt index must carry
	}{
		{"loose file", func(t *testing.T, pack string) {
			if err := os.WriteFile(filepath.Join(pack, "Tree.fbx"), []byte("FBXBYTES"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, "Tree.fbx"},
		{"unitypackage", func(t *testing.T, pack string) {
			writeUnityPackage(t, filepath.Join(pack, "POLYGON_Nature_Unity_2022_3_v1.unitypackage"), []unityGUID{
				{guid: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", pathname: "Assets/Nature/Tree.fbx", asset: "FBXBYTES"},
			})
		}, "Tree.fbx"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			outside := t.TempDir()
			pack := filepath.Join(outside, "Synty", "POLYGON_Nature")
			if err := os.MkdirAll(pack, 0o755); err != nil {
				t.Fatal(err)
			}
			tc.write(t, pack)
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
			// Renaming the link moves every display field without moving the file's own
			// stat print, which is what the reuse decision keys on.
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
			if a.Name != tc.want {
				t.Fatalf("second build has %q, want %q", a.Name, tc.want)
			}
			if a.Vendor != "synty-drive" || !strings.HasPrefix(a.RelPath, "synty-drive/") {
				t.Errorf("after the rename: vendor %q, relpath %q; want the path the walk now takes", a.Vendor, a.RelPath)
			}
		})
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

// absCacheDir looks redundant next to checkCacheDir, which resolves both sides itself
// and so refuses a relative cache dir inside the root either way. It is not: without it
// ix.cacheDir stays relative, and stateDir, the extraction tree, the staging dir, the
// cache file and the prune root are all derived from it — so containment is checked
// against one path while every write goes to another, and the whole lot moves with the
// working directory. Nothing else in the suite passes a relative cache dir.
func TestStatePathsAreAbsoluteFromARelativeCacheDir(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.fbx"), []byte("FBX"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Relative to the working directory, which the test binary owns; the point is the
	// shape of the value, not where it lands.
	rel := filepath.Join(t.TempDir(), "cache")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err = filepath.Rel(wd, rel)
	if err != nil {
		t.Skipf("cannot express %s relative to %s", rel, wd)
	}
	if filepath.IsAbs(rel) {
		t.Fatalf("%s is not relative; this test needs a relative cache dir", rel)
	}
	ix, err := Build(Options{Root: root, CacheDir: rel})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct{ name, path string }{
		{"stateDir", ix.stateDir()},
		{"unpackedDir", ix.unpackedDir()},
		{"stagingDir", ix.stagingDir()},
		{"cachePath", ix.cachePath()},
	} {
		if !filepath.IsAbs(p.path) {
			t.Errorf("%s = %q is relative: the index and every extraction would move with the "+
				"working directory, and containment was checked against a different path", p.name, p.path)
		}
	}
}

// A discard is how a torn extraction is repaired, so it must be all-or-nothing. Deleting
// the live tree in place is not: os.RemoveAll records its first error and keeps going,
// then cannot unlink the directory, leaving one that still exists with an arbitrary
// subset of its members gone — and ensureExtracted's fast path is a stat of exactly that
// directory, so the remains would be served as complete from then on, past a restart and
// past --reindex. Unpublishing by rename first is what makes the outcome binary.
func TestDiscardIsAllOrNothingAndLeavesNoTreeBehind(t *testing.T) {
	root, mk := libRoot(t)
	archive := mk("v", "Pack", "Pack_A_v1.unitypackage")
	const a1, a2 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "cccccccccccccccccccccccccccccccc"
	writeUnityPackage(t, archive, []unityGUID{
		{guid: a1, pathname: "Assets/One.fbx", asset: "ONEBYTES"},
		{guid: a2, pathname: "Assets/Two.fbx", asset: "TWOBYTES"},
	})
	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	rc, _, err := ix.Open(ix.Assets[0])
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()

	fp, err := fingerprint(archive)
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(ix.unpackedDir(), fp)
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("the extraction should be published by now: %v", err)
	}
	if err := ix.discardExtraction(archive); err != nil {
		t.Fatal(err)
	}
	// Wholly gone, not partly: a surviving directory is what the stat fast path would
	// read as a complete extraction.
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("stat(dest) = %v, want IsNotExist: a tree that still exists is treated as complete", err)
	}
	// And nothing parked in staging, which is where the condemned tree goes on its way
	// out — left there, it would sit until PruneUnpacked's age sweep.
	if entries, err := os.ReadDir(ix.stagingDir()); err == nil && len(entries) != 0 {
		t.Errorf("staging holds %d entries after a discard that succeeded", len(entries))
	}
	// Both members come back, so the discard really did lead to a fresh extraction
	// rather than to a hole that reads as a miss.
	for _, want := range []struct {
		i    int
		body string
	}{{0, "ONEBYTES"}, {1, "TWOBYTES"}} {
		rc, _, err := ix.Open(ix.Assets[want.i])
		if err != nil {
			t.Fatalf("re-open %d: %v", want.i, err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		if string(b) != want.body {
			t.Errorf("member %d = %q, want %q", want.i, b, want.body)
		}
	}
	// Discarding what is not there is how a concurrent instance's prune looks; it is
	// not a failure, and reporting one would fail a repair that already happened.
	if err := ix.discardExtraction(archive); err != nil {
		t.Errorf("discard of an already-absent extraction = %v, want nil", err)
	}
}

// The atomicity itself, which only shows under a removal that fails partway. A directory
// the process cannot unlink from is enough: os.RemoveAll then deletes the members it can
// reach, fails on the rest, and cannot unlink the extraction directory — leaving a
// directory that still exists and is missing members. ensureExtracted's fast path is a
// stat of that directory, so those members would 404 for good. Unpublished by a rename,
// the tree leaves whole and re-extracts.
func TestADiscardThatCannotDeleteStillUnpublishesTheWholeTree(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions this test uses to make a delete fail")
	}
	root, mk := libRoot(t)
	archive := mk("v", "Pack", "Pack_A_v1.unitypackage")
	const a1, a2 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "cccccccccccccccccccccccccccccccc"
	writeUnityPackage(t, archive, []unityGUID{
		{guid: a1, pathname: "Assets/One.fbx", asset: "ONEBYTES"},
		{guid: a2, pathname: "Assets/Two.fbx", asset: "TWOBYTES"},
	})
	cache := t.TempDir()
	// The condemned tree keeps its permissions wherever it is moved to, so the sweep is
	// over the whole cache dir rather than the one path. Registered before anything is
	// locked down, and it runs before TempDir's own cleanup.
	t.Cleanup(func() {
		filepath.WalkDir(cache, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				os.Chmod(p, 0o700)
			}
			return nil
		})
	})
	ix, err := Build(Options{Root: root, CacheDir: cache})
	if err != nil {
		t.Fatal(err)
	}
	rc, _, err := ix.Open(ix.Assets[0])
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()

	fp, err := fingerprint(archive)
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(ix.unpackedDir(), fp)
	if err := os.Chmod(filepath.Join(dest, a1), 0o500); err != nil {
		t.Fatal(err)
	}

	if err := ix.discardExtraction(archive); err != nil {
		t.Fatalf("discard = %v; the tree can be unpublished even when it cannot be deleted", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("stat(dest) = %v, want IsNotExist: a tree left half-deleted is read as complete "+
			"by the stat fast path, so its missing members never come back", err)
	}
	// The undeletable remains are parked in staging, outside the tree PruneUnpacked
	// sweeps by name, where its age sweep clears them.
	if entries, err := os.ReadDir(ix.stagingDir()); err != nil || len(entries) == 0 {
		t.Errorf("staging = %v (%v); the condemned tree should be waiting there", entries, err)
	}
	// Both members are served again, from a fresh extraction.
	for _, want := range []struct {
		i    int
		body string
	}{{0, "ONEBYTES"}, {1, "TWOBYTES"}} {
		rc, _, err := ix.Open(ix.Assets[want.i])
		if err != nil {
			t.Fatalf("re-open %d: %v", want.i, err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		if string(b) != want.body {
			t.Errorf("member %d = %q, want %q", want.i, b, want.body)
		}
	}
}

// The rebuild claim is spent once per extraction because a second disagreement means the
// tree was never the cause. A discard that failed repaired nothing, so holding the claim
// there answers tornError for every later request against the archive — including after
// the cause (a full disk, a read-only cache dir) is fixed.
func TestAFailedDiscardDoesNotSpendTheRebuildClaim(t *testing.T) {
	ix := &Index{}
	const fp = "crc32:dead:1"
	first, won := ix.claimRebuild(fp)
	if !won {
		t.Fatal("the first claim must be granted")
	}
	if again, won := ix.claimRebuild(fp); won {
		t.Fatal("the second claim must be refused while the first is held")
	} else if again != first {
		t.Error("a refused claim must hand back the holder's channel, or the caller waits on nothing")
	}
	ix.releaseRebuild(fp)
	if _, won := ix.claimRebuild(fp); !won {
		t.Error("a released claim must be grantable again, or a failed discard freezes the archive")
	}
}

// archive/zip returns a usable reader *alongside* ErrInsecurePath, for an entry whose
// name is non-local or holds a backslash — what older Windows zip tooling writes. Read
// as an ordinary failure it costs the whole archive, safe entries and all, and leaks
// the reader it was handed back. It takes GODEBUG=zipinsecurepath=0 today, which this
// test cannot set for itself (the value is read once at package init), so it asserts
// the property directly: the opener must keep the reader and drop only the bad names.
func TestAnInsecureEntryNameCostsItselfNotTheArchive(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "pack.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	// A backslash name is one of the two shapes archive/zip calls insecure. Written raw
	// because zw.Create would sanitise it.
	w, err := zw.CreateRaw(&zip.FileHeader{Name: `SourceFiles\Sword.fbx`})
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("SWORD"))
	w2, err := zw.Create("SourceFiles/Shield.fbx")
	if err != nil {
		t.Fatal(err)
	}
	w2.Write([]byte("SHIELD"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// openZip is what both the scan and the reader cache go through; ErrInsecurePath
	// must not reach either as a failure.
	zr, err := openZip(p)
	if err != nil {
		t.Fatalf("openZip refused an archive holding an insecure entry name: %v", err)
	}
	zr.Close()

	assets, err := zipAssets(p, "pack.zip", "v", "P", "")
	if err != nil {
		t.Fatalf("zipAssets: %v", err)
	}
	var names []string
	for _, a := range assets {
		names = append(names, a.Name)
	}
	if len(assets) == 0 {
		t.Fatal("the archive contributed nothing; one insecure name took every safe entry with it")
	}
	if !slices.Contains(names, "Shield.fbx") {
		t.Errorf("the safe entry is missing; got %v", names)
	}
	// The backslash entry is kept, and kept as the path it means. Asserting that it is
	// merely *present* is what the earlier version of this test did, through a condition
	// safeEntry can never satisfy — it does not look at backslashes, so the guard was
	// unreachable and every consequence below went unchecked.
	var back *Asset
	for i := range assets {
		if strings.Contains(assets[i].Source.Entry, `\`) {
			back = &assets[i]
		}
	}
	if back == nil {
		t.Fatal("the backslash entry was dropped; it is a member of the archive, not an escape")
	}
	if back.Name != "Sword.fbx" {
		t.Errorf("Name = %q, want Sword.fbx: the card is named for its whole internal path", back.Name)
	}
	if back.Source.EntryPath() != "SourceFiles/Sword.fbx" {
		t.Errorf("EntryPath = %q, want SourceFiles/Sword.fbx", back.Source.EntryPath())
	}
	if want := "pack.zip::SourceFiles/Sword.fbx"; back.RelPath != want {
		t.Errorf("RelPath = %q, want %q: no extracted twin can produce the other spelling", back.RelPath, want)
	}
	// Source.Entry keeps the stored spelling, because that is the key the central
	// directory resolves. Normalizing it would make the entry unservable.
	if back.Source.Entry != `SourceFiles\Sword.fbx` {
		t.Errorf("Source.Entry = %q, want the stored spelling", back.Source.Entry)
	}
}

// The dedup key is built from the entry read as a path, so a pack shipped both packed
// and extracted collapses to one card however the archive spelled its separators.
// Keyed on the stored spelling instead, the two never met: two cards for one file,
// differing only by a separator neither side displays.
func TestABackslashEntryDedupsAgainstItsExtractedTwin(t *testing.T) {
	root, mk := libRoot(t)
	archive := mk("v", "Pack", "Pack_A_v1.zip")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	// CreateRaw, because Create sanitises the separator away — which leaves the sizes
	// and the CRC to fill in here, and those are what dedup keys on alongside the path.
	data := []byte("STONE")
	w, err := zw.CreateRaw(&zip.FileHeader{
		Name: `Textures\Stone01.png`, Method: zip.Store,
		CRC32: crc32.ChecksumIEEE(data), CompressedSize64: uint64(len(data)), UncompressedSize64: uint64(len(data)),
	})
	if err != nil {
		t.Fatal(err)
	}
	w.Write(data)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	writeFile(t, mk("v", "Pack", "Textures", "Stone01.png"), "STONE")

	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var rels []string
	for i := range ix.Assets {
		rels = append(rels, ix.Assets[i].RelPath)
	}
	if len(ix.Assets) != 1 {
		t.Fatalf("indexed %d assets, want 1: the archive entry and its extracted twin are one file; got %v", len(ix.Assets), rels)
	}
	if ix.Assets[0].Source.Kind != SourceLoose {
		t.Errorf("the surviving asset is %v, want the loose twin", ix.Assets[0].Source.Kind)
	}
}

// The same reading applied to the rules that take an entry as a path. Each of these
// answered differently for the two spellings of one path, so an archive written by
// older Windows tooling indexed differently from the same tree shipped extracted.
func TestABackslashEntryIsReadAsThePathItMeans(t *testing.T) {
	if skipEntry(entryPath(`SourceFiles\.vscode\settings.json`)) != skipEntry("SourceFiles/.vscode/settings.json") {
		t.Error("a dot-directory is only recognised in one spelling: the packed tree keeps what the extracted one drops")
	}
	// An escape is an escape in either spelling.
	if safeEntry(entryPath(`a\..\..\b`)) {
		t.Error(`a\..\..\b was accepted; it escapes the archive just as ../../b does`)
	}
	if !safeEntry(entryPath(`Textures\Stone01.png`)) {
		t.Error("an ordinary Windows-spelled entry was rejected; it is a member, not an escape")
	}
	// Classification anchors on "/", "_" and ":", so the whole-path reading put every
	// texture and UI file in the plain image facet.
	for _, tc := range []struct{ entry, want string }{
		{`Textures\Stone01.png`, "texture"},
		{`UI\button.png`, "ui"},
	} {
		src := Source{Kind: SourceZip, ArchivePath: "/lib/v/P/pack.zip", Entry: tc.entry}
		a := newAsset(src, path.Base(entryPath(tc.entry)), archiveRel("v/P/pack.zip", entryPath(tc.entry)), "v", "P", "", 10, "crc32:1:10")
		if string(a.Category) != tc.want {
			t.Errorf("%s classified as %s, want %s", tc.entry, a.Category, tc.want)
		}
	}
}

// A derivation that failed is deliberately left out of the print maps: the stat print
// describes the file, not whether reading it worked, so caching one would freeze the
// degraded result in until the file's own size or mtime moved. The mode change below
// is what makes that observable — it fixes the cause without touching either.
//
// The loose half is the one that bites hardest. With the print cached, the second run
// finds no cached assets for the path, and describes answers true for an empty list, so
// the file is "reused" as nothing: the asset is gone from the index for good, and the
// skip that explained it was only ever reported on the first run.
func TestAFailedDerivationIsNotCachedAgainstTheFilesPrint(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 file, so there is no failure to recover from")
	}
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, pack string) string // returns the path to make unreadable
		want  string
	}{
		{"loose file", func(t *testing.T, pack string) string {
			p := filepath.Join(pack, "Tree.fbx")
			if err := os.WriteFile(p, []byte("FBXBYTES"), 0o644); err != nil {
				t.Fatal(err)
			}
			return p
		}, "Tree.fbx"},
		{"archive", func(t *testing.T, pack string) string {
			p := filepath.Join(pack, "POLYGON_Nature_Unity_2022_3_v1.unitypackage")
			writeUnityPackage(t, p, []unityGUID{
				{guid: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", pathname: "Assets/Nature/Tree.fbx", asset: "FBXBYTES"},
			})
			return p
		}, "Tree.fbx"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			pack := filepath.Join(root, "Synty", "POLYGON_Nature")
			if err := os.MkdirAll(pack, 0o755); err != nil {
				t.Fatal(err)
			}
			target := tc.write(t, pack)
			if err := os.Chmod(target, 0o000); err != nil {
				t.Fatal(err)
			}
			opt := Options{Root: root, CacheDir: t.TempDir()}
			first, err := LoadOrBuild(opt, false, nil)
			if err != nil {
				t.Fatalf("an unreadable file must cost itself, not the build: %v", err)
			}
			if len(first.Assets) != 0 {
				t.Fatalf("first build indexed %d assets from an unreadable file", len(first.Assets))
			}
			if len(first.Skipped) != 1 {
				t.Fatalf("first build recorded %d skips, want 1", len(first.Skipped))
			}

			// Mode only: size and mtime are untouched, so the stat print is the same one
			// the failed run saw. Caching it would make this run reuse the failure.
			if err := os.Chmod(target, 0o644); err != nil {
				t.Fatal(err)
			}
			again, err := LoadOrBuild(opt, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(again.Assets) != 1 {
				t.Fatalf("after the cause was fixed the index holds %d assets, want 1: the failure was cached against a print that does not move", len(again.Assets))
			}
			if got := again.Assets[0].Name; got != tc.want {
				t.Errorf("recovered asset is %q, want %q", got, tc.want)
			}
			if again.Assets[0].Fingerprint == "" {
				t.Error("the recovered asset has no fingerprint, so it cannot be tagged")
			}
			if len(again.Skipped) != 0 {
				t.Errorf("the skip survived the recovery: %+v", again.Skipped)
			}
		})
	}
}

// archiveMu is what stops a reader from passing ensureExtracted's stat of a published
// tree and then opening a path a concurrent discard has already removed. Every test
// that touches discardExtraction is otherwise sequential, so the property is only
// asserted through the torn-rebuild test's fallout: a reader that lost that race would
// report a miss for a sibling whose own bytes were never torn, and over a package with
// tens of thousands of members that is 404 after 404 for files that are right there.
//
// Read as: whatever a reader gets back, it is never a wrong answer. A discard removes
// the tree and the next reader rebuilds it, so a read either returns the real bytes or
// fails outright — it must never return short or empty content as though it were the
// file.
func TestReadersRacingADiscardNeverSeeAPartialFile(t *testing.T) {
	root, mk := libRoot(t)
	archive := mk("v", "Pack", "Pack_A_v1.unitypackage")
	members := make([]unityGUID, 0, 24)
	want := map[string]string{}
	for i := 0; i < 24; i++ {
		guid := fmt.Sprintf("%032x", i)
		body := strings.Repeat(fmt.Sprintf("m%02d", i), 400)
		members = append(members, unityGUID{guid: guid, pathname: fmt.Sprintf("Assets/M%02d.fbx", i), asset: body})
		want[guid] = body
	}
	writeUnityPackage(t, archive, members)
	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Assets) != len(members) {
		t.Fatalf("indexed %d assets, want %d", len(ix.Assets), len(members))
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// The discards run against the readers rather than after them, so a reader that
	// slipped between the stat and the open is what this is trying to produce.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			ix.discardExtraction(archive)
		}
	}()
	for i := range ix.Assets {
		wg.Add(1)
		go func(a Asset) {
			defer wg.Done()
			for n := 0; n < 20; n++ {
				rc, _, err := ix.Open(a)
				if err != nil {
					// Not tolerated. A discard that has finished leaves nothing to find,
					// and the next Open rebuilds before reading — so every read here has
					// a tree to read from. The one way to miss is to pass the stat of a
					// published tree and then open a path the discard has since removed,
					// which is the window archiveMu closes.
					t.Errorf("%s: %v — a reader passed the extraction check and then found the tree gone", a.RelPath, err)
					return
				}
				b, readErr := io.ReadAll(rc)
				rc.Close()
				if readErr != nil {
					t.Errorf("%s: read failed: %v", a.RelPath, readErr)
					return
				}
				if string(b) != want[a.Source.Guid] {
					t.Errorf("%s came back as %d bytes, want %d: a reader was served a tree a discard had already taken",
						a.RelPath, len(b), len(want[a.Source.Guid]))
					return
				}
			}
		}(ix.Assets[i])
	}
	// Stop the discards first, then let the readers finish, so the run ends with the
	// extraction in whatever state the last one left rather than mid-removal.
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()

	// And the tree is usable afterwards: a discard racing readers must leave something
	// a later request can rebuild from, not a half-removed directory the stat fast path
	// would read as complete.
	for i := range ix.Assets {
		rc, _, err := ix.Open(ix.Assets[i])
		if err != nil {
			t.Fatalf("%s is unreadable after the race: %v", ix.Assets[i].RelPath, err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil || string(b) != want[ix.Assets[i].Source.Guid] {
			t.Fatalf("%s reads back wrong after the race: %d bytes, %v", ix.Assets[i].RelPath, len(b), err)
		}
	}
}

// The loser of the rebuild claim waits for the winner's repair and then answers from
// the repaired tree, rather than reporting on the read that sent it there. Only the
// winner's path is pinned directly: TestATornExtractionIsRebuiltOnceNotPerRequest is
// sequential, and the concurrent torn-rebuild test covers the losers only by whichever
// goroutines happen to lose. Reported instead of waited on, the failure is a 500 for a
// member whose bytes the repair had already restored — and there is one of those per
// reader that arrived while the decompress was running, which over a package of tens of
// thousands of members is most of a grid page.
func TestALoserOfTheRebuildClaimAnswersFromTheRepairedTree(t *testing.T) {
	root, mk := libRoot(t)
	archive := mk("v", "Pack", "Pack_A_v1.unitypackage")
	members := make([]unityGUID, 0, 8)
	for i := 0; i < 8; i++ {
		members = append(members, unityGUID{
			guid:     fmt.Sprintf("%032x", i),
			pathname: fmt.Sprintf("Assets/M%02d.fbx", i),
			asset:    strings.Repeat(fmt.Sprintf("m%02d", i), 300),
		})
	}
	writeUnityPackage(t, archive, members)
	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for _, m := range members {
		want[m.guid] = m.asset
	}
	// Publish the extraction, then tear every member, so whoever wins the claim has a
	// real repair to make and everyone else is a loser with a torn read in hand.
	rc, _, err := ix.Open(ix.Assets[0])
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	fp, err := fingerprint(archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if err := os.WriteFile(filepath.Join(ix.unpackedDir(), fp, m.guid, "asset"), []byte("SHORT"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range ix.Assets {
		wg.Add(1)
		go func(a Asset) {
			defer wg.Done()
			<-start
			rc, _, err := ix.Open(a)
			if err != nil {
				t.Errorf("%s: %v — a caller that did not win the claim reported on its own torn read instead of waiting for the repair", a.RelPath, err)
				return
			}
			b, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				t.Errorf("%s: read failed: %v", a.RelPath, err)
				return
			}
			if string(b) != want[a.Source.Guid] {
				t.Errorf("%s came back as %d bytes, want %d: the repaired bytes were not what was served", a.RelPath, len(b), len(want[a.Source.Guid]))
			}
		}(ix.Assets[i])
	}
	close(start)
	wg.Wait()
}

// A Sidekick package unpacked beside itself is an ordinary layout, and the loose copies
// of a character's byproducts are not archive entries — so the rule that drops them
// inside the package never sees them, and ordinary dedup only ever drops the archive
// side. The grid then showed every assembled character alongside the prefab, material
// and combined mesh it exists instead of.
//
// The partial half is the point of the flag: a character missing a part is a torso and
// a hand, and those rows are what still show the whole thing.
func TestSidekickByproductsGoOnBothSidesOfAnExtractedPack(t *testing.T) {
	build := func(t *testing.T, sk string) []Asset {
		t.Helper()
		root, mk := libRoot(t)
		writeUnityPackage(t, mk("synty", "SIDEKICK_D", "SIDEKICK_D_Unity_v1.unitypackage"), []unityGUID{
			{guid: "sk1", pathname: "Assets/S/Characters/Hero.sk", asset: sk},
			{guid: "p1", pathname: "Assets/S/Characters/Hero.prefab", asset: "PREFAB"},
			{guid: "m1", pathname: "Assets/S/Characters/Hero.mat", asset: "MAT"},
			{guid: "c1", pathname: "Assets/S/Characters/Hero_CombinedMesh.asset", asset: "COMBINED"},
			{guid: "hd1", pathname: "Assets/S/Resources/SK_HEAD.fbx", asset: "HEADFBX"},
		})
		// The same package, extracted where it shipped.
		writeFile(t, mk("synty", "SIDEKICK_D", "Assets", "S", "Characters", "Hero.sk"), sk)
		writeFile(t, mk("synty", "SIDEKICK_D", "Assets", "S", "Characters", "Hero.prefab"), "PREFAB")
		writeFile(t, mk("synty", "SIDEKICK_D", "Assets", "S", "Characters", "Hero.mat"), "MAT")
		writeFile(t, mk("synty", "SIDEKICK_D", "Assets", "S", "Characters", "Hero_CombinedMesh.asset"), "COMBINED")
		writeFile(t, mk("synty", "SIDEKICK_D", "Assets", "S", "Resources", "SK_HEAD.fbx"), "HEADFBX")

		// Built twice through the cache, because the loose drop is re-decided on every
		// refresh while the .sk's bytes are only read on the pass that enumerates the
		// archive. Source.Complete is what carries the answer to the second run; without
		// it the byproducts come back the moment the archive's enumeration is reused.
		cache := t.TempDir()
		opt := Options{Root: root, CacheDir: cache}
		if _, err := LoadOrBuild(opt, false, func(string) {}); err != nil {
			t.Fatal(err)
		}
		ix, err := LoadOrBuild(opt, false, func(string) {})
		if err != nil {
			t.Fatal(err)
		}
		return ix.Assets
	}
	names := func(assets []Asset) []string {
		var out []string
		for i := range assets {
			out = append(out, string(assets[i].Source.Kind)+":"+assets[i].Name)
		}
		slices.Sort(out)
		return out
	}

	whole := build(t, "Name: Hero\nParts:\n- Name: SK_HEAD\n")
	got := names(whole)
	// The .sk's own loose twin stays — it is a plain data row, not a byproduct — and so
	// does the part mesh, which the character reaches by id.
	want := []string{"loose:Hero.sk", "loose:SK_HEAD.fbx", "unitypackage:Hero", "unitypackage:SK_HEAD.fbx"}
	if !slices.Equal(got, want) {
		t.Errorf("assembled character:\n got  %v\n want %v", got, want)
	}

	partial := build(t, "Name: Hero\nParts:\n- Name: SK_HEAD\n- Name: SK_ABSENT\n")
	for _, n := range []string{"loose:Hero.prefab", "loose:Hero.mat", "loose:Hero_CombinedMesh.asset"} {
		if !slices.Contains(names(partial), n) {
			t.Errorf("%s was dropped for a character missing a part; it is the row that still shows the whole one", n)
		}
	}
}

// Two Synty packages have identical internal trees, so a character's scope is only its
// own pack's. Without that the first pack's Hero.sk claims the second's loose
// Hero.prefab, and a pack with no Sidekick content at all loses files to one that has.
func TestOneSidekickPackDoesNotClaimAnothersFiles(t *testing.T) {
	root, mk := libRoot(t)
	sk := "Name: Hero\nParts:\n- Name: SK_HEAD\n"
	writeUnityPackage(t, mk("synty", "SIDEKICK_D", "SIDEKICK_D_Unity_v1.unitypackage"), []unityGUID{
		{guid: "sk1", pathname: "Assets/S/Characters/Hero.sk", asset: sk},
		{guid: "hd1", pathname: "Assets/S/Resources/SK_HEAD.fbx", asset: "HEADFBX"},
	})
	// A different pack, same internal tree, no .sk anywhere in it.
	writeFile(t, mk("synty", "POLYGON_W", "Assets", "S", "Characters", "Hero.prefab"), "OTHERPREFAB")

	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for i := range ix.Assets {
		if ix.Assets[i].Pack == "POLYGON_W" && ix.Assets[i].Name == "Hero.prefab" {
			found = true
		}
	}
	if !found {
		t.Error("another pack's Hero.prefab was claimed as a Sidekick byproduct")
	}
}

// The sweep deletes everything in the version's tree that is not in the keep-set, and
// a second quarry sharing this cache dir can be serving from any of it. Read off
// Assets — which is exported — a caller that filtered the slice first would sweep the
// extractions of everything it removed; read off a nil snapshot, it sweeps the lot.
func TestPruneRefusesAnIndexNoWalkProduced(t *testing.T) {
	cache := t.TempDir()
	// A real extraction, so a sweep that went ahead would have something to destroy.
	root, mk := libRoot(t)
	writeUnityPackage(t, mk("v", "Pack", "Pack_Unity_v1.unitypackage"), []unityGUID{
		{guid: "g1", pathname: "Assets/Rock.fbx", asset: "ROCKBYTES"},
	})
	real, err := Build(Options{Root: root, CacheDir: cache})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := real.Open(real.Assets[0]); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(real.unpackedDir())
	if err != nil || len(before) == 0 {
		t.Fatalf("the fixture extracted nothing to protect: %v", err)
	}

	// An index assembled by hand over the same cache dir: the shape a library caller
	// reaching past Build produces, and the shape a future in-place filter would leave.
	hand := &Index{Root: real.Root, Version: indexVersion, cacheDir: cache}
	if err := hand.PruneUnpacked(); !errors.Is(err, ErrPruneWithoutRefresh) {
		t.Errorf("PruneUnpacked = %v, want ErrPruneWithoutRefresh", err)
	}
	after, err := os.ReadDir(real.unpackedDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("the refused sweep still deleted: %d extractions became %d", len(before), len(after))
	}
	// And the index that did walk the library still prunes.
	if err := real.PruneUnpacked(); err != nil {
		t.Errorf("PruneUnpacked on a built index = %v", err)
	}
	if kept, _ := os.ReadDir(real.unpackedDir()); len(kept) != len(before) {
		t.Errorf("a real prune deleted a live extraction: %d became %d", len(before), len(kept))
	}
}

// Two readings of one file's stat print. refresh derives it from the stat the walk
// already took; everything serving derives it from a stat of its own. Disagreeing,
// every cached enumeration misses on every run and the whole library is re-derived
// each startup — with nothing reporting it but the clock.
func TestTheWalkAndTheServerAgreeOnAFilesPrint(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "pack.zip")
	if err := os.WriteFile(p, []byte("BYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	stated, err := fingerprint(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := fingerprintOf(p, fi.Size(), fi.ModTime()); got != stated {
		t.Errorf("fingerprintOf = %q, fingerprint = %q", got, stated)
	}
}

// The reader cache holds an archive's parsed central directory, and a Synty pack zip
// holds tens of thousands of entries while a grid page issues one content request per
// card — so re-parsing it per request is the dominant cost of serving from a zip. That
// is the whole reason the type exists, and nothing asserted it: replacing acquire with
// an unconditional openZip per request, and release with a Close, left every test in
// the repo green. TestAMissingZipEntryReportsAMiss is the only one that touches
// ix.zips at all, and its assertion is guarded by `cached`, so with no cache it never
// runs either.
//
// Reuse is made observable the way TestRefreshReusesCachedArchiveEnumeration makes
// enumeration reuse observable: the archive becomes unreadable while its size and mtime
// stay put, so acquire's print still matches and a hit serves from the descriptor it
// already holds, while a miss fails in zip.OpenReader.
func TestTheZipReaderCacheActuallyCaches(t *testing.T) {
	root, mk := libRoot(t)
	held := mk("v", "Pack", "Pack_A_v1.zip")
	writeZip(t, held, map[string]string{"Heart.fbx": "FBXHEART"})
	// Straddling the bound rather than restating it: one short of it the held reader is
	// still in the window, one past it the reader is gone.
	others := make([]string, 2*zipCacheSize)
	for i := range others {
		others[i] = mk("v", "Pack", fmt.Sprintf("Other_%02d_v1.zip", i))
		writeZip(t, others[i], map[string]string{"X.fbx": "XBYTES"})
	}
	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	read := func(archive string) error {
		for i := range ix.Assets {
			a := ix.Assets[i]
			if a.Source.ArchivePath != archive {
				continue
			}
			rc, _, err := ix.Open(a)
			if err != nil {
				return err
			}
			io.Copy(io.Discard, rc)
			return rc.Close()
		}
		t.Fatalf("no asset for %s", archive)
		return nil
	}

	if err := read(held); err != nil {
		t.Fatalf("first read: %v", err)
	}
	unreadable(t, held)

	// Still inside the window: served from the reader already open, with the file on
	// disk unopenable.
	for i := 0; i < zipCacheSize-1; i++ {
		if err := read(others[i]); err != nil {
			t.Fatalf("touching %s: %v", others[i], err)
		}
	}
	if err := read(held); err != nil {
		t.Fatalf("an archive read once and still inside the cache had to be reopened: %v", err)
	}
	// That hit made it the newest, so a further window's worth of distinct archives is
	// what pushes it out. Then the next read goes to the file — which is exactly what
	// every read would do if the cache were gone.
	for i := zipCacheSize - 1; i < 2*zipCacheSize-1; i++ {
		if err := read(others[i]); err != nil {
			t.Fatalf("touching %s: %v", others[i], err)
		}
	}
	if err := read(held); err == nil {
		t.Error("an evicted archive still served; the cache is not bounded by zipCacheSize")
	}
}

// acquire retires rather than closes a reader whose archive moved, because a stream
// over the bytes it describes may still be in flight. The refs guard inside
// retireLocked is covered through eviction; nothing reached it through the
// print-mismatch branch with a reader outstanding, so replacing that call with a delete
// plus a direct Close — a plausible simplification, since the cached directory is known
// to be wrong by then — passed the whole suite while killing in-flight responses.
func TestARewrittenArchiveDoesNotCloseAStreamOverTheOldBytes(t *testing.T) {
	root, mk := libRoot(t)
	archive := mk("v", "Pack", "Pack_A_v1.zip")
	writeZip(t, archive, map[string]string{"Heart.fbx": "ORIGINAL-BYTES-LONG-ENOUGH-TO-READ-IN-TWO-GOES"})
	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	rc, _, err := ix.Open(ix.Assets[0])
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	head := make([]byte, 8)
	if _, err := io.ReadFull(rc, head); err != nil {
		t.Fatal(err)
	}

	// Re-shipped in place, the way a pack update arrives: written beside and renamed
	// over, so the reader in flight keeps the inode it opened.
	next := mk("v", "Pack", "next.tmp")
	writeZip(t, next, map[string]string{"Heart.fbx": "REPLACED"})
	if err := os.Rename(next, archive); err != nil {
		t.Fatal(err)
	}
	// Any request for the same archive now: the print no longer matches, so acquire
	// retires the cached reader and opens the file again. Reading the new bytes is what
	// proves the retire branch ran rather than the cached directory being reused.
	fresh, _, err := ix.Open(ix.Assets[0])
	if err != nil {
		t.Fatalf("re-opening the re-shipped archive: %v", err)
	}
	newBytes, _ := io.ReadAll(fresh)
	fresh.Close()
	if string(newBytes) != "REPLACED" {
		t.Fatalf("the second read returned %q; the print check did not retire the stale reader", newBytes)
	}

	rest, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("the in-flight stream died when the archive was re-shipped: %v", err)
	}
	if got := string(head) + string(rest); got != "ORIGINAL-BYTES-LONG-ENOUGH-TO-READ-IN-TWO-GOES" {
		t.Errorf("the stream returned %q; it must finish over the bytes it started on", got)
	}
}

// archiveMu serialises an archive's readers against its rebuild: a reader holds it
// shared from the extraction check through the open, and discardExtraction takes it
// exclusively. What makes that airtight is that there is exactly one way in.
// openUnpackedMember takes the lock and then calls unpackedEntry, which is the only
// caller of ensureExtracted; a second call site — a prefetch, a warm-up, a debug
// endpoint — reopens the window between the check and the open, and the only test on
// the invariant is a 50ms spin-loop race that can pass without ever producing the
// interleaving. The structure is what can be asserted outright.
func TestNothingReachesAnExtractionOutsideTheArchiveLock(t *testing.T) {
	// Both callees are package-internal, so the call this guard exists to catch can be
	// written in any file here — a prefetch in scan.go reaches ensureExtracted exactly
	// as content.go does. Read the whole package: scoped to one file, the guard reports
	// a clean sweep of the only file that was never going to be the problem.
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	type fn struct{ file, name, body string }
	var all []fn
	var read int
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		read++
		// Split into top-level funcs so a call can be attributed to the one it sits in.
		funcs := regexp.MustCompile(`(?m)^func (?:\([^)]*\) )?(\w+)`).FindAllStringSubmatchIndex(string(src), -1)
		for i, m := range funcs {
			end := len(src)
			if i+1 < len(funcs) {
				end = funcs[i+1][0]
			}
			all = append(all, fn{file, string(src[m[2]:m[3]]), string(src[m[1]:end])})
		}
	}
	if read < 8 || len(all) < 40 {
		t.Fatalf("globbed %d non-test files holding %d functions; this guard has stopped reading the package", read, len(all))
	}
	callers := func(callee string) []string {
		var out []string
		for _, f := range all {
			if f.name != callee && strings.Contains(f.body, callee+"(") {
				out = append(out, f.file+":"+f.name)
			}
		}
		return out
	}
	for callee, allowed := range map[string][]string{
		"ensureExtracted": {"content.go:unpackedEntry"},
		"unpackedEntry":   {"content.go:openUnpackedMember"},
	} {
		got := callers(callee)
		if len(got) == 0 {
			t.Errorf("no caller of %s found; this guard has stopped checking anything", callee)
		}
		for _, c := range got {
			if !slices.Contains(allowed, c) {
				t.Errorf("%s calls %s, outside the one path that holds archiveMu (%v). A reader that "+
					"skips the lock races discardExtraction and opens a member from a tree being deleted",
					c, callee, allowed)
			}
		}
	}
	// And the one way in does take the lock.
	var entry string
	for _, f := range all {
		if f.name == "openUnpackedMember" {
			entry = f.body
		}
	}
	if entry == "" {
		t.Fatal("openUnpackedMember not found; this guard has stopped checking anything")
	}
	if !strings.Contains(entry, "archiveMu(") || !strings.Contains(entry, "RLock()") {
		t.Error("openUnpackedMember no longer takes archiveMu for reading")
	}
}

// claimRebuild hands out one repair per extraction, and releaseRebuild gives the claim
// back when the discard that repair depends on could not happen. Driving those two
// directly asserts the helper rather than the outcome: it would keep passing with the
// releaseRebuild call removed from openUnpacked, which is the behaviour it protects.
// The outcome is that a transient reason the discard failed, once fixed, still repairs.
func TestARebuildBlockedByTheCacheDirStillRepairsOnceItIsWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory modes are not enforced")
	}
	root, mk := libRoot(t)
	archive := mk("v", "Pack", "Pack_Unity_v1.unitypackage")
	writeUnityPackage(t, archive, []unityGUID{
		{guid: "g1", pathname: "Assets/Rock.fbx", asset: "ROCKBYTES"},
	})
	cache := t.TempDir()
	ix, err := Build(Options{Root: root, CacheDir: cache})
	if err != nil {
		t.Fatal(err)
	}
	a := ix.Assets[0]
	rc, _, err := ix.Open(a)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()

	// Tear the extraction the way an unclean shutdown does: the member is there and
	// short, which is what openUnpacked's size re-check is for.
	member := filepath.Join(ix.unpackedDir(), ix.ArchivePrint[archive], "g1", "asset")
	if err := os.WriteFile(member, []byte("TORN"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Now make the discard itself impossible: it moves the condemned tree into a temp
	// dir under stagingDir, which it cannot create one in.
	staging := ix.stagingDir()
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(staging, 0o500); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ix.Open(a); err == nil {
		os.Chmod(staging, 0o755)
		t.Fatal("a torn member read as fine while the repair could not run")
	}
	// The cause is fixed. The claim must not have been spent on the attempt that could
	// not happen, or this asset serves torn bytes for the life of the process.
	if err := os.Chmod(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	rc, _, err = ix.Open(a)
	if err != nil {
		t.Fatalf("the repair never ran after the reason it could not was fixed: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "ROCKBYTES" {
		t.Errorf("served %q after the repair, want the archive's bytes", got)
	}
}

// RootMotionVariant's doc comment is the stated authority on the conventions it knows,
// and design.md defers to it by name and by count. Both drifted: the table listed
// "_RootMotion" only as an infix while stripToken has accepted it as a suffix all
// along, and design.md said four. A reader who trusts either concludes a file that
// pairs correctly is a bug — and "fixing" it changes what the GLB-split gate does to
// every such file, silently, because the fingerprints do not move with it.
//
// Derived from the comment rather than restated: each bullet carries its own worked
// example, so the table is executable.
func TestTheRootMotionDocTableIsTrue(t *testing.T) {
	src, err := os.ReadFile("rootmotion.go")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(src[:strings.Index(string(src), "func RootMotionVariant")])
	// //   - "<token>" <kind>   <vendor...>   "<in>" -> "<out>"
	row := regexp.MustCompile(`(?m)^//\s+- .*"([^"]+)"\s*->\s*"([^"]+)"`)
	rows := row.FindAllStringSubmatch(doc, -1)
	if len(rows) < 4 {
		t.Fatalf("parsed %d worked examples out of the doc comment; this guard has stopped reading it", len(rows))
	}
	for _, m := range rows {
		in, want := m[1], m[2]
		got, isRM := RootMotionVariant(in)
		if !isRM || got != want {
			t.Errorf("the doc says %q -> %q, but RootMotionVariant returns (%q, %v)", in, want, got, isRM)
		}
	}
	// design.md names the count in words, and it is the file the version-bump rule is
	// written in. Spelled out rather than digits, so it is matched that way.
	words := map[int]string{3: "three", 4: "four", 5: "five", 6: "six", 7: "seven"}
	md, err := os.ReadFile(filepath.Join("..", "..", "docs", "design.md"))
	if err != nil {
		t.Fatal(err)
	}
	claim := regexp.MustCompile(`conventions it knows \(currently (\w+):`).FindStringSubmatch(string(md))
	if claim == nil {
		t.Fatal("design.md no longer states how many conventions RootMotionVariant knows; this guard has stopped reading it")
	}
	if want := words[len(rows)]; claim[1] != want {
		t.Errorf("design.md says %q conventions, the doc comment lists %d (%q)", claim[1], len(rows), want)
	}
}

// An archive entry and an extracted tree are read as paths, not as names, and the
// spellings a real library ships are not canonical: older Windows zip tooling writes
// "\" separators, some writers prefix every entry with "./", and a pack is commonly
// unpacked into a src/ subdirectory. Every rule downstream — the classifier's
// separators, the dot-directory skip, the dedup key, a Sidekick character's suppression
// scope — is written against one normalised reading, and each of those normalisations
// lives at a different call site.
//
// Two audits found the same shape twice: zipAssets asked archive/zip whether an entry
// was a directory, which reads the *stored* name and so missed "SourceFiles\Models\"
// entirely, and withinPackPath compared a raw pathname against a path.Dir-cleaned tree,
// so a src/ extraction kept every byproduct the assembled character exists instead of.
// Neither was visible to a suite that spells its fixtures canonically.
//
// So this indexes one library three ways and demands the same answer. It is deliberately
// a whole-index comparison rather than a rule-by-rule one: what matters is that no
// spelling reaches the grid differently, and a new rule is covered without being named
// here.
func TestEverySpellingOfOnePackIndexesTheSame(t *testing.T) {
	const sk = "Name: Hero\nParts:\n- Name: SK_HEAD\n"
	// The unitypackage is the same in all three; only the extracted tree beside it and
	// the zip's entry spellings move.
	pkg := func(prefix string) []unityGUID {
		return []unityGUID{
			{guid: "sk1", pathname: prefix + "Assets/S/Characters/Hero.sk", asset: sk},
			{guid: "p1", pathname: prefix + "Assets/S/Characters/Hero.prefab", asset: "PREFAB"},
			{guid: "m1", pathname: prefix + "Assets/S/Characters/Hero.mat", asset: "MAT"},
			{guid: "c1", pathname: prefix + "Assets/S/Characters/Hero_CombinedMesh.asset", asset: "COMBINED"},
			{guid: "hd1", pathname: prefix + "Assets/S/Resources/SK_HEAD.fbx", asset: "HEADFBX"},
		}
	}
	// A card is compared by what the grid shows and what a tag keys on. The id is left
	// out on purpose: it embeds an absolute path and each case builds its own temp root.
	//
	// The one difference that is not a defect is src/ in a loose RelPath: the file
	// genuinely sits there and the grid should say so. normSubpath exists for the dedup
	// key, not for display, so the segment is taken out here rather than in the scan.
	describe := func(assets []Asset) []string {
		out := make([]string, 0, len(assets))
		for i := range assets {
			a := &assets[i]
			rel := strings.Replace(a.RelPath, "/src/", "/", 1)
			out = append(out, fmt.Sprintf("%s|%s|%s|%s|%s|%s|%d|%s",
				a.Source.Kind, a.Name, rel, a.Category, a.Vendor, a.Variant, a.Size, a.Fingerprint))
		}
		slices.Sort(out)
		return out
	}
	build := func(t *testing.T, unityPrefix, extractUnder string, zipEntry func(string) string) []string {
		t.Helper()
		root, mk := libRoot(t)
		writeUnityPackage(t, mk("synty", "PACK", "PACK_Unity_v1.unitypackage"), pkg(unityPrefix))
		// A zip of an ordinary (non-Sidekick) tree, carrying a directory entry. The
		// directory is what the stored-name reading missed.
		writeZip(t, mk("synty", "PACK", "PACK_SourceFiles_v1.zip"), map[string]string{
			zipEntry("SourceFiles/Models/"):          "",
			zipEntry("SourceFiles/Models/Sword.fbx"): "SWORDBYTES",
			zipEntry("SourceFiles/Textures/T_A.png"): "PNGBYTES",
		})
		// The unitypackage extracted beside itself, which is where the loose half of
		// the Sidekick suppression applies.
		for _, f := range []struct{ rel, body string }{
			{"Assets/S/Characters/Hero.sk", sk},
			{"Assets/S/Characters/Hero.prefab", "PREFAB"},
			{"Assets/S/Characters/Hero.mat", "MAT"},
			{"Assets/S/Characters/Hero_CombinedMesh.asset", "COMBINED"},
			{"Assets/S/Resources/SK_HEAD.fbx", "HEADFBX"},
		} {
			parts := append([]string{"synty", "PACK"}, strings.Split(extractUnder+f.rel, "/")...)
			writeFile(t, mk(parts...), f.body)
		}
		// Twice through the cache: the loose Sidekick drop is re-decided on every
		// refresh while the .sk's bytes are read only on the pass that enumerates the
		// archive, so a spelling that breaks Source.Complete shows on the second run.
		cache := t.TempDir()
		opt := Options{Root: root, CacheDir: cache}
		if _, err := LoadOrBuild(opt, false, func(string) {}); err != nil {
			t.Fatal(err)
		}
		ix, err := LoadOrBuild(opt, false, func(string) {})
		if err != nil {
			t.Fatal(err)
		}
		return describe(ix.Assets)
	}

	canonical := build(t, "", "", func(s string) string { return s })
	if len(canonical) == 0 {
		t.Fatal("the canonical fixture indexed nothing; this guard is comparing two empty sets")
	}
	// The assembled character must actually be there, or every case agrees on a library
	// in which assembly never ran and the comparison proves nothing.
	if !slices.ContainsFunc(canonical, func(s string) bool { return strings.Contains(s, "|Hero|") }) {
		t.Fatalf("no assembled character in the canonical fixture; the byproduct rules are not being exercised:\n%v", canonical)
	}
	for _, s := range canonical {
		if strings.Contains(s, "|Hero.prefab|") || strings.Contains(s, "|Models|") {
			t.Fatalf("the canonical fixture itself keeps a byproduct or a directory entry: %q", s)
		}
	}

	for _, tc := range []struct {
		name         string
		unityPrefix  string
		extractUnder string
		zipEntry     func(string) string
	}{
		{"backslash-separated zip entries", "", "", func(s string) string { return strings.ReplaceAll(s, "/", `\`) }},
		{"./-prefixed unity pathnames", "./", "", func(s string) string { return s }},
		{"the pack extracted under src/", "", "src/", func(s string) string { return s }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := build(t, tc.unityPrefix, tc.extractUnder, tc.zipEntry)
			if !slices.Equal(got, canonical) {
				t.Errorf("this spelling indexes differently from the canonical one:\n got  %v\n want %v", got, canonical)
			}
		})
	}
}

// A prune that cannot remove one tree must still sweep the rest and report. Early-return
// is the natural refactor of the firstErr accumulation, and it is invisible: nothing
// else clears a stale extraction, so a single undeletable directory would strand every
// later one — hundreds of MB per Synty pack — behind a warning naming only the first.
// No existing prune test makes a remove fail, so that refactor passed the whole suite.
func TestPruneKeepsSweepingPastATreeItCannotRemove(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a read-only directory does not stop root")
	}
	root, mk := libRoot(t)
	cache := t.TempDir()
	writeUnityPackage(t, mk("v", "Pack", "Pack_Unity_v1.unitypackage"), []unityGUID{
		{guid: "g1", pathname: "Assets/Rock.fbx", asset: "ROCKBYTES"},
	})
	ix, err := Build(Options{Root: root, CacheDir: cache})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ix.Open(ix.Assets[0]); err != nil {
		t.Fatal(err)
	}

	// Three extractions no current walk reached, so all three are the sweep's to take.
	// The first by sort order is made undeletable; the other two prove the sweep did
	// not stop there. Named so the failing one sorts first whatever ReadDir returns.
	dir := ix.unpackedDir()
	stuck := filepath.Join(dir, "0-stuck")
	for _, name := range []string{"0-stuck", "1-stale", "2-stale"} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		// A non-empty directory: RemoveAll on an empty one succeeds even under a
		// read-only parent on some filesystems, and an unlink it cannot do is the point.
		if err := os.WriteFile(filepath.Join(p, "asset"), []byte("X"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(stuck, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(stuck, 0o755) })

	err = ix.PruneUnpacked()
	if err == nil {
		t.Fatal("a prune that could not remove a tree reported success")
	}
	for _, name := range []string{"1-stale", "2-stale"} {
		if _, statErr := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(statErr) {
			t.Errorf("%s survived: the sweep stopped at the tree it could not remove", name)
		}
	}
	// And the live extraction is untouched, which is the whole point of the keep-set.
	live, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var kept int
	for _, e := range live {
		if ix.liveUnpacked[e.Name()] {
			kept++
		}
	}
	if kept != 1 {
		t.Errorf("live extractions surviving = %d, want 1", kept)
	}
}

// Source.EntryPath and Source.Entry are a pair, and only one half was pinned by
// behaviour. Every rule that reads an entry as a path reads the normalised spelling;
// Source.Entry keeps the stored one, because that is the key the central directory
// resolves. Normalising it at the source, or normalising it again on the way into the
// lookup, is the obvious tidy-up and it is silent: every entry of an archive written by
// older Windows tooling stops resolving, openZipEntry reports fs.ErrNotExist, and browse
// answers 404 for a whole pack that is sitting right there.
//
// Kept apart from the enumeration guard, and with no loose twin, so the archive entry is
// the thing actually served rather than deduped away in favour of a real file.
func TestABackslashEntryIsServedByItsStoredSpelling(t *testing.T) {
	root, mk := libRoot(t)
	archive := mk("v", "Pack", "Pack_A_v1.zip")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	// CreateRaw, because Create sanitises the separator away.
	data := []byte("SWORDBYTES")
	w, err := zw.CreateRaw(&zip.FileHeader{
		Name: `SourceFiles\Sword.fbx`, Method: zip.Store,
		CRC32: crc32.ChecksumIEEE(data), CompressedSize64: uint64(len(data)), UncompressedSize64: uint64(len(data)),
	})
	if err != nil {
		t.Fatal(err)
	}
	w.Write(data)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var back *Asset
	for i := range ix.Assets {
		if strings.Contains(ix.Assets[i].Source.Entry, `\`) {
			back = &ix.Assets[i]
		}
	}
	if back == nil {
		t.Fatalf("no asset kept the stored backslash spelling; got %d assets", len(ix.Assets))
	}
	rc, size, err := ix.Open(*back)
	if err != nil {
		t.Fatalf("Open(%q) = %v; the entry the scan indexed cannot be served, so browse 404s the whole pack", back.Source.Entry, err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Errorf("Open returned %q, want %q", got, data)
	}
	if size != int64(len(data)) {
		t.Errorf("size = %d, want %d", size, len(data))
	}
}

// Regenerable state is addressed by what the walk covers, and --follow-symlinks is
// half of that: under it the library is the root and every target the walk followed.
// Keyed on the root alone, both settings shared one tree, so the run that did not
// follow saw every extraction reached through a link as unreferenced and swept it —
// out from under a second instance already serving them, which --addr exists to allow.
// The sequential half is visible here; the concurrent one is the same delete.
func TestNotFollowingDoesNotPruneWhatFollowingExtracted(t *testing.T) {
	outside := t.TempDir()
	drive := filepath.Join(outside, "drive2")
	pkg := filepath.Join(drive, "Pack.unitypackage")
	if err := os.MkdirAll(drive, 0o755); err != nil {
		t.Fatal(err)
	}
	writeUnityPackage(t, pkg, []unityGUID{
		{guid: "aaa", pathname: "Assets/M/thing.fbx", asset: "FBXBYTES"},
	})
	root, mk := libRoot(t)
	if err := os.Symlink(drive, mk("v", "Linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cacheDir := t.TempDir()

	following, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir, FollowSymlinks: true}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := following.ensureExtracted(pkg)
	if err != nil {
		t.Fatal(err)
	}

	// The second instance: same root, same cache dir, no follow. Its walk never reaches
	// the drive, so nothing there is in its keep-set.
	plain, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := plain.PruneUnpacked(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the non-following run pruned an extraction the following run serves: %v", err)
	}

	// The two trees being distinct is what makes that true, and it is the thing a
	// future change to stateDir would lose.
	if following.stateDir() == plain.stateDir() {
		t.Errorf("both settings share the state dir %s; each run's prune sweeps the other's extractions",
			following.stateDir())
	}
	// Including the index JSON, which otherwise overwrites the other setting's on every
	// alternating run.
	if following.cachePath() == plain.cachePath() {
		t.Errorf("both settings share the index at %s", following.cachePath())
	}
}

// reshipped asks whether the archive on disk is the one the scan described, and
// answers no only when it has a print to compare. The "no print recorded" half is not
// bookkeeping: refresh drops an archive's print whenever archiveAssets returns a note
// while keeping its assets (a package whose second pass failed after its first
// enumerated fine), and those assets stay in the index and stay clickable. Read as a
// mismatch, ensureExtracted refuses to decompress at all, so every asset in that pack
// answers 404 for the life of the run with nothing logged — and openUnpacked keeps the
// repair for the same reason, so a torn member behind a dropped print is still fixed.
func TestAnArchiveWithNoRecordedPrintStillServesAndStillRepairs(t *testing.T) {
	root, mk := libRoot(t)
	pkg := mk("v", "Pack", "Pack.unitypackage")
	writeUnityPackage(t, pkg, []unityGUID{
		{guid: "aaa", pathname: "Assets/M/thing.fbx", asset: "FBXBYTES"},
	})
	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Assets) != 1 {
		t.Fatalf("assets = %v, want the one member", names(ix.Assets))
	}
	// The state a degraded pass leaves behind: assets kept, print dropped.
	delete(ix.ArchivePrint, pkg)

	rc, _, err := ix.Open(ix.Assets[0])
	if err != nil {
		t.Fatalf("Open: %v — an archive the index describes but has no print for is unreachable", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "FBXBYTES" {
		t.Fatalf("read %q, want %q", got, "FBXBYTES")
	}

	// And the repair is still available to it, which is the reason this case keeps it:
	// with no print there is nothing to prove the archive was re-shipped, so a short
	// member can only be a torn extraction.
	dir, err := ix.ensureExtracted(pkg)
	if err != nil {
		t.Fatal(err)
	}
	member := filepath.Join(dir, "aaa", "asset")
	if err := os.WriteFile(member, []byte("FBX"), 0o644); err != nil {
		t.Fatal(err)
	}
	rc, _, err = ix.Open(ix.Assets[0])
	if err != nil {
		t.Fatalf("Open after tearing the member: %v — the rebuild was refused", err)
	}
	got, err = io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "FBXBYTES" {
		t.Errorf("read %q after the repair, want %q", got, "FBXBYTES")
	}
}

// A library's locators are whatever bytes its filesystem and its archives happen to
// hold, and the cache is JSON: encoding/json replaces an invalid UTF-8 byte with
// U+FFFD and says nothing. Older Windows zip tooling stores entry names in CP437, the
// same tooling the backslash-separator rule exists for, so this is the ordinary shape
// rather than a contrived one — and the failure it produced was a card that stays in
// the grid, resolves by id and is still taggable, whose bytes 404 on every run after
// the first, with --reindex repairing it for exactly one run.
//
// Built twice on purpose. The cache is the only component that changes the bytes, and
// TestEverySpellingOfOnePackIndexesTheSame — the other whole-index equivalence test —
// varies separators rather than encodings, so nothing here read a locator back.
func TestALocatorTheCacheCannotRepresentIsNotCached(t *testing.T) {
	const raw = "SourceFiles/Caf\xe9.fbx"
	if utf8.ValidString(raw) {
		t.Fatal("the fixture name is valid UTF-8; this test is asserting nothing")
	}
	data := []byte("FBXBYTES-HELLO")

	root, mk := libRoot(t)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// CreateRaw, because Create rejects a name it cannot store as UTF-8 by flagging it
	// rather than leaving the bytes alone.
	w, err := zw.CreateRaw(&zip.FileHeader{
		Name: raw, Method: zip.Store,
		CRC32: crc32.ChecksumIEEE(data), CompressedSize64: uint64(len(data)), UncompressedSize64: uint64(len(data)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mk("synty", "POLYGON", "POLYGON_SourceFiles_v1.zip"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	// A loose file of the same shape, whose path is a cache map *key* and mangles the
	// same way, and a unitypackage whose pathname is what the card is named after.
	if err := os.WriteFile(mk("synty", "POLYGON", "Caf\xe9.png"), []byte("PNGBYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeUnityPackage(t, mk("synty", "POLYGON", "POLYGON_Unity_2022_3_v1.unitypackage"), []unityGUID{
		{guid: "aaa", pathname: "Assets/P/Caf\xe9.fbx", asset: "UNITYBYTES"},
	})

	opt := Options{Root: root, CacheDir: t.TempDir()}
	for _, run := range []string{"first run, fresh scan", "second run, through the cache"} {
		ix, err := LoadOrBuild(opt, false, func(string) {})
		if err != nil {
			t.Fatalf("%s: LoadOrBuild: %v", run, err)
		}
		if len(ix.Assets) != 3 {
			t.Fatalf("%s: %d assets, want 3", run, len(ix.Assets))
		}
		for _, a := range ix.Assets {
			if !strings.Contains(a.RelPath, "Caf\xe9") {
				t.Errorf("%s: %q lost its name to the cache", run, a.RelPath)
			}
			rc, _, err := ix.Open(a)
			if err != nil {
				t.Errorf("%s: Open(%s): %v", run, a.RelPath, err)
				continue
			}
			rc.Close()
		}
	}
}

// A zip writer may leave the CRC field unset, and a zero CRC over non-empty bytes is
// the absence of a fingerprint rather than one — degrading it to "" is what stops every
// such entry of one size from sharing a print and tagging together. The entry is still
// an entry: it indexes, it serves, and it is only untaggable. Nothing exercised that
// end to end, so a scan that skipped it, or substituted a path-derived print for it,
// passed the whole suite.
func TestAZipEntryWithNoRecordedCRCStillIndexesAndServes(t *testing.T) {
	root, mk := libRoot(t)
	data := []byte("MODELBYTES")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateRaw(&zip.FileHeader{
		Name: "SourceFiles/Sword.fbx", Method: zip.Store,
		CRC32: 0, CompressedSize64: uint64(len(data)), UncompressedSize64: uint64(len(data)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mk("synty", "POLYGON", "POLYGON_SourceFiles_v1.zip"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Assets) != 1 {
		t.Fatalf("%d assets, want the entry indexed despite its missing CRC", len(ix.Assets))
	}
	a := ix.Assets[0]
	if a.Fingerprint != "" {
		t.Errorf("Fingerprint = %q, want empty: a zero CRC over non-empty bytes is the absence of one, and a constant here tags every such entry of this size together", a.Fingerprint)
	}
	rc, size, err := ix.Open(a)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != string(data) || size != int64(len(data)) {
		t.Errorf("served %q (%d bytes), want the entry's own content", got, size)
	}
}

// A directory this run could not read is not a directory whose packs are gone, and the
// keep-set cannot tell them apart on its own: it is built from what the walk reached.
// A drive offline for one run, a permission that slipped, and every extraction beneath
// it is swept — hundreds of MB per Synty pack, and deleted out from under a second
// quarry that is still serving them, which --addr exists to allow.
func TestPruneKeepsWhatTheWalkCouldNotLookAt(t *testing.T) {
	root, mk := libRoot(t)
	pkg := mk("synty", "P", "P_Unity_2022_3_v1.unitypackage")
	writeUnityPackage(t, pkg, []unityGUID{
		{guid: "aaa", pathname: "Assets/P/Rock.fbx", asset: "ROCKBYTES"},
	})
	opt := Options{Root: root, CacheDir: t.TempDir()}

	ix, err := LoadOrBuild(opt, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	live, err := ix.ensureExtracted(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("the extraction is not there to begin with: %v", err)
	}

	packDir := filepath.Dir(pkg)
	if err := os.Chmod(packDir, 0o000); err != nil {
		t.Skipf("cannot make a directory unreadable here: %v", err)
	}
	t.Cleanup(func() { os.Chmod(packDir, 0o755) })
	if f, err := os.Open(packDir); err == nil { // running as root: the chmod means nothing
		f.Close()
		t.Skip("this user can read a 0000 directory; the case cannot be staged")
	}

	next, err := LoadOrBuild(opt, false, func(string) {})
	if err != nil {
		t.Fatalf("a library with one unreadable corner must still index: %v", err)
	}
	if len(next.Skipped) == 0 {
		t.Fatal("the unreadable directory was not recorded as a skip; the retention below has nothing to read")
	}
	if err := next.PruneUnpacked(); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("the extraction of a pack behind an unreadable directory was swept: %v", err)
	}

	// And the other direction, which is what keeps this from being "never prune": a
	// pack genuinely deleted, with the directory readable, still loses its extraction.
	os.Chmod(packDir, 0o755)
	if err := os.Remove(pkg); err != nil {
		t.Fatal(err)
	}
	gone, err := LoadOrBuild(opt, false, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if err := gone.PruneUnpacked(); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Stat(live); !os.IsNotExist(err) {
		t.Errorf("the extraction of a deleted pack survived the prune (%v)", err)
	}
}

// State is keyed by root so two libraries sharing a cache dir cannot prune each other's
// extractions — which also means a root nobody opens again is consulted by nothing and
// kept forever: a 100MB index JSON plus every unitypackage extraction under it, stranded
// by a moved library, a config.toml pointed elsewhere, or one --follow-symlinks flip.
//
// Age is the evidence, and the cache dir is whatever the user named, so the bar for
// deleting a directory under it is that it looks like one quarry wrote.
func TestAnAbandonedRootsStateIsSweptByAge(t *testing.T) {
	cacheDir := t.TempDir()
	roots := filepath.Join(cacheDir, "roots")

	stale := filepath.Join(roots, "0123456789ab")
	fresh := filepath.Join(roots, "ba9876543210")
	theirs := filepath.Join(roots, "my own notes")
	for _, d := range []string{stale, fresh, theirs} {
		if err := os.MkdirAll(filepath.Join(d, "unpacked", "24", "deadbeef"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "index.json"), []byte(`{"version":25,"root":"/somewhere","assets":[]}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A directory of the user's that merely sits here, with no index of ours in it.
	notOurs := filepath.Join(roots, "cafebabe1234")
	if err := os.MkdirAll(notOurs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(notOurs, "index.json"), []byte("# my notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * staleRootAge)
	for _, d := range []string{stale, theirs, notOurs} {
		if err := os.Chtimes(filepath.Join(d, "index.json"), old, old); err != nil {
			t.Fatal(err)
		}
	}

	root, mk := libRoot(t)
	os.WriteFile(mk("synty", "P", "Rock.fbx"), []byte("FBX"), 0o644)
	ix, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.PruneUnpacked(); err != nil {
		t.Fatalf("prune: %v", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("a root untouched for %v survived (%v); its index and extractions are stranded forever", staleRootAge, err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a root whose index was written recently was swept: %v", err)
	}
	if _, err := os.Stat(theirs); err != nil {
		t.Errorf("a directory not named the way stateDir names one was swept: %v", err)
	}
	if _, err := os.Stat(notOurs); err != nil {
		t.Errorf("a directory holding a file of the user's rather than an index of ours was swept: %v", err)
	}
	// This run's own state is still here, whatever its mtime says.
	if _, err := os.Stat(ix.stateDir()); err != nil {
		t.Errorf("the running index's own state was swept: %v", err)
	}
}

// The cache-dir refusal is about overlap, and overlap has two directions. A link
// pointing into the cache dir puts the library on top of quarry's own output just as a
// cache dir inside the library does: the walk indexes every extracted member as a loose
// file and re-reads the index JSON, whose print moves every time a save rewrites it.
func TestALinkIntoTheCacheDirIsRefused(t *testing.T) {
	cacheDir := t.TempDir()
	inside := filepath.Join(cacheDir, "roots", "0123456789ab", "unpacked")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	root, mk := libRoot(t)
	if err := os.Symlink(inside, mk("lib", "extracted")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, err := Build(Options{Root: root, CacheDir: cacheDir, FollowSymlinks: true})
	if err == nil {
		t.Fatal("a scan that follows a link into the cache dir was allowed")
	}
	if !strings.Contains(err.Error(), "cache dir") {
		t.Errorf("error = %v, want one naming the cache dir so the user knows what to move", err)
	}
	// Not followed, the link is dropped like any other and the scan is fine.
	if _, err := Build(Options{Root: root, CacheDir: cacheDir}); err != nil {
		t.Errorf("without --follow-symlinks the link goes nowhere and the scan must still run: %v", err)
	}
}
