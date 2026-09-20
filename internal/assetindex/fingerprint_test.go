package assetindex

import (
	"hash/crc32"
	"os"
	"testing"
)

// wantCRC is the crc32+size fingerprint the scheme must produce for these bytes.
func wantCRC(content string) string {
	return crcFingerprint(crc32.ChecksumIEEE([]byte(content)), int64(len(content)))
}

func fpByName(assets []Asset) map[string]string {
	m := map[string]string{}
	for _, a := range assets {
		m[a.Name] = a.Fingerprint
	}
	return m
}

func TestFingerprintPerSourceKind(t *testing.T) {
	root, mk := libRoot(t)

	writeZip(t, mk("synty", "Foo_Pack", "Foo_Pack_SourceFiles_v3.zip"), map[string]string{
		"SourceFiles/Models/Heart.fbx": "FBXHEARTDATA",
	})
	writeUnityPackage(t, mk("synty", "Foo_Pack", "Foo_Pack_Unity_2022_3_v1_0_0.unitypackage"), []unityGUID{
		{guid: "aaa-guid", pathname: "Assets/Foo/Heart.prefab", asset: "PREFAB"},
	})
	os.WriteFile(mk("explosive", "RPG", "Sword.glb"), []byte("GLBSWORD"), 0o644)

	fps := fpByName(mustScan(t, root))

	if got, want := fps["Heart.fbx"], wantCRC("FBXHEARTDATA"); got != want {
		t.Errorf("zip fingerprint = %q, want %q", got, want)
	}
	if got, want := fps["Heart.prefab"], unityFingerprint("aaa-guid"); got != want {
		t.Errorf("unity fingerprint = %q, want %q", got, want)
	}
	if got, want := fps["Sword.glb"], wantCRC("GLBSWORD"); got != want {
		t.Errorf("loose fingerprint = %q, want %q", got, want)
	}
}

// A split clip's print is the file's, the disambiguated label appended. The other two
// schemes are pinned by construction above; this one was pinned by nothing — the clip
// tests count distinct prints and compare one run against the next, all of which stay
// green if the label is swapped for Source.ClipIndex. That swap looks like a
// simplification once ClipIndex exists, and it silently detaches every clip tag and
// link in the library, on a change bumping indexVersion cannot rescue: tags key on
// content, not on the cache.
//
// Built from Source.Clip rather than a literal so the label-versus-index half is pinned
// too: a print derived from the position fails this, and so does one derived from the
// raw glTF name, since the duplicate here is disambiguated before it is used.
func TestASplitClipsFingerprintIsTheFilesPlusItsLabel(t *testing.T) {
	root, mk := libRoot(t)
	p := mk("quaternius", "Anims", "Library.glb")
	writeGLB(t, p, "Idle", "Idle", "Walk")
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	fileFP := wantCRC(string(body))

	var clips int
	for _, a := range mustScan(t, root) {
		if a.Source.Clip == "" {
			continue
		}
		clips++
		if want := fileFP + "#" + a.Source.Clip; a.Fingerprint != want {
			t.Errorf("clip %q fingerprint = %q, want %q", a.Source.Clip, a.Fingerprint, want)
		}
	}
	if clips != 3 {
		t.Fatalf("split into %d clips, want 3; this test is not reading the split path", clips)
	}
	// The duplicate name must actually have been disambiguated, or "the label" and "the
	// raw glTF name" are the same string and the assertion above cannot tell them apart.
	labels := map[string]bool{}
	for _, a := range mustScan(t, root) {
		if a.Source.Clip != "" {
			labels[a.Source.Clip] = true
		}
	}
	if len(labels) != 3 {
		t.Errorf("clip labels = %v, want three distinct ones from two same-named animations", labels)
	}
}

// Byte-identical content shares one fingerprint across packs and across the
// zip/loose boundary, so a tag set on one copy applies to every copy.
func TestFingerprintSharedForIdenticalBytes(t *testing.T) {
	root, mk := libRoot(t)
	writeZip(t, mk("synty", "A", "A.zip"), map[string]string{"Models/Tree.fbx": "TREEBYTES"})
	writeZip(t, mk("synty", "B", "B.zip"), map[string]string{"Models/Tree.fbx": "TREEBYTES"})
	os.WriteFile(mk("synty", "C", "loose", "Tree.fbx"), []byte("TREEBYTES"), 0o644)

	want := wantCRC("TREEBYTES")
	for _, a := range mustScan(t, root) {
		if a.Name == "Tree.fbx" && a.Fingerprint != want {
			t.Errorf("%s/%s fingerprint = %q, want %q (identical bytes must share)", a.Pack, a.Name, a.Fingerprint, want)
		}
	}
}

// A cold Build and a Refresh over an unchanged tree yield identical fingerprints,
// and a changed loose file's fingerprint is recomputed.
func TestFingerprintStableAndRefreshRecomputes(t *testing.T) {
	root, mk := libRoot(t)
	cacheDir := t.TempDir()
	loose := mk("explosive", "RPG", "Sword.glb")
	os.WriteFile(loose, []byte("GLBSWORD"), 0o644)
	writeZip(t, mk("synty", "A", "A.zip"), map[string]string{"Models/Tree.fbx": "TREEBYTES"})

	ix, err := Build(Options{Root: root, CacheDir: cacheDir})
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.LoosePrint) == 0 {
		t.Fatal("LoosePrint not populated on Build")
	}
	before := fpByName(ix.Assets)
	if before["Sword.glb"] != wantCRC("GLBSWORD") || before["Tree.fbx"] != wantCRC("TREEBYTES") {
		t.Fatalf("cold fingerprints wrong: %+v", before)
	}

	// A second Build of the same tree is deterministic.
	ix2, err := Build(Options{Root: root, CacheDir: cacheDir})
	if err != nil {
		t.Fatal(err)
	}
	if got := fpByName(ix2.Assets); got["Sword.glb"] != before["Sword.glb"] || got["Tree.fbx"] != before["Tree.fbx"] {
		t.Errorf("second Build changed fingerprints: %+v vs %+v", got, before)
	}

	// Refresh over an unchanged tree preserves fingerprints.
	if err := ix.refresh(); err != nil {
		t.Fatal(err)
	}
	if got := fpByName(ix.Assets); got["Sword.glb"] != before["Sword.glb"] || got["Tree.fbx"] != before["Tree.fbx"] {
		t.Errorf("Refresh (no change) altered fingerprints: %+v vs %+v", got, before)
	}

	// Changing the loose file's bytes (and size) recomputes its fingerprint on Refresh.
	os.WriteFile(loose, []byte("GLBSWORD-EDITED-LONGER"), 0o644)
	if err := ix.refresh(); err != nil {
		t.Fatal(err)
	}
	if got, want := fpByName(ix.Assets)["Sword.glb"], wantCRC("GLBSWORD-EDITED-LONGER"); got != want {
		t.Errorf("Refresh after edit: fingerprint = %q, want %q", got, want)
	}
}

func mustScan(t *testing.T, root string) []Asset {
	t.Helper()
	ix, err := Build(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	assets := ix.Assets
	return assets
}

// A zero CRC over non-empty bytes is the absence of a fingerprint, not one. Degrading
// it to a constant would give every such entry of the same size one identity, so a
// tag on any of them would appear on all of them.
func TestCRCFingerprintDegradesRatherThanColliding(t *testing.T) {
	cases := []struct {
		name string
		crc  uint32
		size int64
		want string
	}{
		{"unset crc on real bytes is no fingerprint", 0, 4096, ""},
		{"another size, same absence — must not collide", 0, 512, ""},
		{"an empty file's crc is genuinely zero", 0, 0, "crc32:0:0"},
		{"an ordinary entry", 0x1a2b3c4d, 41700000, "crc32:1a2b3c4d:41700000"},
	}
	for _, c := range cases {
		if got := crcFingerprint(c.crc, c.size); got != c.want {
			t.Errorf("%s: crcFingerprint(%#x, %d) = %q, want %q", c.name, c.crc, c.size, got, c.want)
		}
	}
}
