package assetindex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	ix, err := Build(root, t.TempDir())
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

	ix, err := Build(root, t.TempDir())
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
