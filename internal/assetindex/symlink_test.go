package assetindex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// filepath.WalkDir does not follow symlinks, and a link's own DirEntry describes the
// link: it does not report itself as a directory, and its size is the length of the
// target path. Treating one as an ordinary file therefore turned a symlinked pack into
// a single asset with a fabricated size whose contents were never walked — and said
// nothing about it. Open would refuse to serve any of it anyway, since the target lands
// outside the root, so the link has to be reported rather than indexed.
func TestWalkReportsSymlinkOutOfRoot(t *testing.T) {
	root, mk := libRoot(t)
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "Models"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("x", 5000)
	if err := os.WriteFile(filepath.Join(outside, "Models", "sword.glb"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(mk("synty", "Real", "axe.glb"), []byte("GLBBYTES"), 0o644)
	if err := os.Symlink(outside, filepath.Join(root, "synty", "Linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "Models", "sword.glb"), filepath.Join(root, "synty", "linked.glb")); err != nil {
		t.Fatal(err)
	}

	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range ix.Assets {
		if strings.Contains(a.RelPath, "Linked") || strings.Contains(a.RelPath, "linked.glb") {
			t.Errorf("indexed %q (size %d) from a symlink Open could never serve", a.RelPath, a.Size)
		}
	}
	if len(ix.Assets) != 1 || ix.Assets[0].Name != "axe.glb" {
		t.Errorf("assets = %v, want just the real file", names(ix.Assets))
	}
	// The whole pack behind the link is gone from the index, so the run has to say so.
	var reported []string
	for _, s := range ix.Skipped {
		reported = append(reported, s.RelPath)
	}
	if len(reported) != 2 {
		t.Errorf("skipped = %v, want both symlinks reported", ix.Skipped)
	}
	for _, s := range ix.Skipped {
		if !strings.Contains(s.Reason, "outside the library root") {
			t.Errorf("skip reason for %s = %q, want it to name the cause", s.RelPath, s.Reason)
		}
	}
}

// A link that stays inside the root points at a file the walk reaches by its real
// path, so indexing it too would show the same asset twice.
func TestWalkDropsSymlinkInsideRoot(t *testing.T) {
	root, mk := libRoot(t)
	os.WriteFile(mk("synty", "Pack", "axe.glb"), []byte("GLBBYTES"), 0o644)
	target := filepath.Join(root, "synty", "Pack", "axe.glb")
	if err := os.Symlink(target, filepath.Join(root, "synty", "Pack", "axe-alias.glb")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Assets) != 1 {
		t.Errorf("assets = %v, want the real file only", names(ix.Assets))
	}
	if len(ix.Skipped) != 0 {
		t.Errorf("skipped = %v; a link to a file already indexed is not worth reporting", ix.Skipped)
	}
}

func names(assets []Asset) []string {
	out := make([]string, len(assets))
	for i, a := range assets {
		out[i] = a.RelPath
	}
	return out
}

// Following is what makes a library assembled across several drives usable: the
// symlinked pack is walked, its files are named through the link the user sees, and
// the resolved target is recorded so serving will open what the scan covered.
func TestFollowSymlinksIndexesTheLinkedPack(t *testing.T) {
	root, mk := libRoot(t)
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "Models"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("x", 5000)
	if err := os.WriteFile(filepath.Join(outside, "Models", "sword.glb"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(mk("synty", "Real", "axe.glb"), []byte("GLBBYTES"), 0o644)
	if err := os.Symlink(outside, filepath.Join(root, "synty", "Linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	ix, err := Build(Options{Root: root, CacheDir: t.TempDir(), FollowSymlinks: true})
	if err != nil {
		t.Fatal(err)
	}
	var linked *Asset
	for i := range ix.Assets {
		if ix.Assets[i].Name == "sword.glb" {
			linked = &ix.Assets[i]
		}
	}
	if linked == nil {
		t.Fatalf("the linked pack was not indexed: %v", names(ix.Assets))
	}
	// Named through the link, so vendor/pack read as the user's tree, not the target's.
	if linked.RelPath != "synty/Linked/Models/sword.glb" {
		t.Errorf("relPath = %q, want it named through the link", linked.RelPath)
	}
	if linked.Vendor != "synty" || linked.Pack != "Linked" {
		t.Errorf("vendor/pack = %q/%q, want synty/Linked", linked.Vendor, linked.Pack)
	}
	// The real size, not the length of the link's target path.
	if linked.Size != int64(len(body)) {
		t.Errorf("size = %d, want %d", linked.Size, len(body))
	}
	if len(ix.Skipped) != 0 {
		t.Errorf("skipped = %v, want none once following is on", ix.Skipped)
	}
	// And it can actually be served: the scan recorded the target as a link root.
	rc, size, err := ix.Open(*linked)
	if err != nil {
		t.Fatalf("Open on a followed asset: %v", err)
	}
	rc.Close()
	if size != int64(len(body)) {
		t.Errorf("served size = %d, want %d", size, len(body))
	}
}

// Following must still refuse a path outside everything the scan covered, or the
// containment check has simply been switched off.
func TestFollowSymlinksStillRefusesAnUncoveredPath(t *testing.T) {
	root, mk := libRoot(t)
	outside := t.TempDir()
	os.WriteFile(mk("synty", "Real", "axe.glb"), []byte("GLBBYTES"), 0o644)
	if err := os.Symlink(outside, filepath.Join(root, "synty", "Linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	ix, err := Build(Options{Root: root, CacheDir: t.TempDir(), FollowSymlinks: true})
	if err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(t.TempDir(), "elsewhere.glb")
	os.WriteFile(stray, []byte("GLBBYTES"), 0o644)
	if _, _, err := ix.Open(Asset{Source: Source{Kind: SourceLoose, FilePath: stray}}); err != ErrOutsideRoot {
		t.Errorf("Open on a path under no link root = %v, want ErrOutsideRoot", err)
	}
}

// A link pointing at a tree the walk is already inside would recurse forever.
func TestFollowSymlinksStopsOnACycle(t *testing.T) {
	root, mk := libRoot(t)
	outside := t.TempDir()
	os.WriteFile(mk("synty", "Real", "axe.glb"), []byte("GLBBYTES"), 0o644)
	if err := os.WriteFile(filepath.Join(outside, "prop.glb"), []byte("GLBBYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	// outside/back -> outside, reached through root/synty/Linked -> outside
	if err := os.Symlink(outside, filepath.Join(outside, "back")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "synty", "Linked")); err != nil {
		t.Fatal(err)
	}

	done := make(chan *Index, 1)
	go func() {
		ix, err := Build(Options{Root: root, CacheDir: t.TempDir(), FollowSymlinks: true})
		if err != nil {
			t.Error(err)
		}
		done <- ix
	}()
	select {
	case ix := <-done:
		if len(ix.Assets) != 2 {
			t.Errorf("assets = %v, want the two real files exactly once", names(ix.Assets))
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the scan did not terminate on a symlink cycle")
	}
}

// The setting decides what the walk covers, so a cache built the other way describes
// a different library and has to be rebuilt rather than refreshed.
func TestFollowSymlinksSettingInvalidatesTheCache(t *testing.T) {
	root, mk := libRoot(t)
	cacheDir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "sword.glb"), []byte("GLBBYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(mk("synty", "Real", "axe.glb"), []byte("GLBBYTES"), 0o644)
	if err := os.Symlink(outside, filepath.Join(root, "synty", "Linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	off, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	on, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir, FollowSymlinks: true}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(on.Assets) <= len(off.Assets) {
		t.Errorf("turning following on kept the cached %d assets (%v); the linked pack should have appeared",
			len(off.Assets), names(on.Assets))
	}
	back, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Assets) != len(off.Assets) {
		t.Errorf("turning it off again left %d assets, want %d", len(back.Assets), len(off.Assets))
	}
}
