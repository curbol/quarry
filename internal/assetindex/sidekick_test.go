package assetindex

import (
	"os"
	"reflect"
	"testing"
)

func TestParseSidekick(t *testing.T) {
	sk := "" +
		"Name: ElvenWarriors_01\n" +
		"Species: 5\n" +
		"Parts:\n" +
		"- Name: SK_ELVN_BASE_01_01HEAD_EV01\n" +
		"  PartType: Head\n" +
		"  PartVersion: 1\n" +
		"- Name: SK_ELVN_WARR_01_10TORS_HU01\n" +
		"  PartType: Torso\n" +
		"  PartVersion: 1\n" +
		"ColorSet:\n" +
		"- Name: NotAPart\n" +
		"BlendShapes:\n"

	name, parts := parseSidekick([]byte(sk))
	if name != "ElvenWarriors_01" {
		t.Errorf("name = %q, want ElvenWarriors_01", name)
	}
	want := []string{"SK_ELVN_BASE_01_01HEAD_EV01", "SK_ELVN_WARR_01_10TORS_HU01"}
	if !reflect.DeepEqual(parts, want) {
		t.Errorf("parts = %v, want %v (a Name under a later top-level key must not leak in)", parts, want)
	}
}

func TestParseSidekickEmpty(t *testing.T) {
	name, parts := parseSidekick([]byte("Name: Foo\nSpecies: 1\n"))
	if name != "Foo" || len(parts) != 0 {
		t.Errorf("parseSidekick(no Parts) = %q,%v; want Foo,[]", name, parts)
	}
}

// A Sidekick package's .sk data entries become assembled-character assets: model
// category, sidekick thumb, and Source.Parts listing the FBX parts' ids in the same
// package. The character's own id/fingerprint stay tied to the .sk guid (stable).
func TestScanSidekickCharacter(t *testing.T) {
	root, mk := libRoot(t)

	sk := "Name: Warrior_01\nParts:\n- Name: SK_HEAD\n- Name: SK_TORS\n- Name: SK_MISSING\n"
	writeUnityPackage(t, mk("synty", "SIDEKICK_X", "SIDEKICK_X_Unity_2021_3_v1_0_0.unitypackage"), []unityGUID{
		{guid: "sk1", pathname: "Assets/Synty/SidekickCharacters/Characters/Warrior/Warrior_01/Warrior_01.sk", asset: sk},
		{guid: "hd1", pathname: "Assets/Synty/SidekickCharacters/Resources/Meshes/SK_HEAD.fbx", asset: "HEADFBX", preview: true},
		{guid: "to1", pathname: "Assets/Synty/SidekickCharacters/Resources/Meshes/SK_TORS.fbx", asset: "TORSFBX", preview: true},
	})

	assets, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	var ch *Asset
	for i := range assets {
		if assets[i].Name == "Warrior_01" {
			ch = &assets[i]
		}
	}
	if ch == nil {
		t.Fatal("assembled character Warrior_01 not found")
	}
	if ch.Category != CategoryModel || ch.Thumb != ThumbSidekick {
		t.Errorf("character = %s/%s, want model/sidekick", ch.Category, ch.Thumb)
	}
	if ch.Fingerprint != unityFingerprint("sk1") {
		t.Errorf("fingerprint = %q, want the .sk guid print (stable identity)", ch.Fingerprint)
	}
	// Parts resolve to the two present FBX ids, in .sk order; the missing part is dropped.
	want := []string{
		id(Source{Kind: SourceUnityPackage, ArchivePath: ch.Source.ArchivePath, Guid: "hd1"}),
		id(Source{Kind: SourceUnityPackage, ArchivePath: ch.Source.ArchivePath, Guid: "to1"}),
	}
	if !reflect.DeepEqual(ch.Source.Parts, want) {
		t.Errorf("parts = %v, want %v", ch.Source.Parts, want)
	}
}

// Within a Sidekick package, the per-character byproducts under the Characters/ tree
// (the magenta prefab, its material, the combined-mesh and avatar .asset data) are
// dropped in favour of the assembled .sk character. The reusable parts (Resources/)
// and the character's textures stay browseable.
func TestScanSidekickDeclutter(t *testing.T) {
	root, mk := libRoot(t)
	base := "Assets/Synty/SidekickCharacters/"
	writeUnityPackage(t, mk("synty", "SIDEKICK_X", "SIDEKICK_X_Unity_2021_3_v1_0_0.unitypackage"), []unityGUID{
		{guid: "sk1", pathname: base + "Characters/W/W_01/W_01.sk", asset: "Name: W_01\nParts:\n- Name: SK_HEAD\n"},
		{guid: "hd1", pathname: base + "Resources/Meshes/SK_HEAD.fbx", asset: "HEADFBX"},
		{guid: "pf1", pathname: base + "Characters/W/W_01/W_01.prefab", asset: "PREFAB", preview: true},
		{guid: "mt1", pathname: base + "Characters/W/W_01/Materials/W_01.mat", asset: "MAT", preview: true},
		{guid: "av1", pathname: base + "Characters/W/W_01/Meshes/W_01-avatar.asset", asset: "AVATAR"},
		{guid: "tx1", pathname: base + "Characters/W/W_01/Textures/T_W_01ColorMap.png", asset: "PNG"},
	})

	assets, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	present := map[string]Asset{}
	for _, a := range assets {
		present[a.Name] = a
	}
	for _, gone := range []string{"W_01.prefab", "W_01.mat", "W_01-avatar.asset"} {
		if _, ok := present[gone]; ok {
			t.Errorf("%s should have been dropped as a Sidekick byproduct", gone)
		}
	}
	if _, ok := present["W_01"]; !ok {
		t.Error("the assembled character W_01 should remain")
	}
	if _, ok := present["SK_HEAD.fbx"]; !ok {
		t.Error("a Resources/ part should remain browseable")
	}
	if _, ok := present["T_W_01ColorMap.png"]; !ok {
		t.Error("the character texture should remain browseable")
	}
}

// Byproduct suppression is bounded to the character it belongs to, not to the directory
// its .sk sits in. Two characters commonly share a directory, and scoping by directory
// meant a character that failed to assemble lost its byproducts to its neighbour's tree
// — the exact opposite of what sidekickByproduct promises.
func TestSidekickSuppressionIsBoundedToItsOwnCharacter(t *testing.T) {
	root, mk := libRoot(t)
	writeUnityPackage(t, mk("synty", "SIDEKICK_X", "SIDEKICK_X_Unity_2021_3_v1_0_0.unitypackage"), []unityGUID{
		{guid: "sk1", pathname: "Assets/S/Characters/Warrior_01.sk", asset: "Name: Warrior_01\nParts:\n- Name: SK_HEAD\n"},
		{guid: "sk2", pathname: "Assets/S/Characters/Warrior_02.sk", asset: "Name: Warrior_02\nParts:\n- Name: SK_ABSENT\n"},
		{guid: "hd1", pathname: "Assets/S/Resources/Meshes/SK_HEAD.fbx", asset: "HEADFBX"},
		{guid: "p1", pathname: "Assets/S/Characters/Warrior_01.prefab", asset: "PREFAB1"},
		{guid: "p2", pathname: "Assets/S/Characters/Warrior_02.prefab", asset: "PREFAB2"},
	})

	assets, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	kept := map[string]bool{}
	for _, a := range assets {
		kept[a.Name] = true
	}
	if !kept["Warrior_01"] {
		t.Error("Warrior_01 did not assemble")
	}
	if kept["Warrior_01.prefab"] {
		t.Error("the assembled character's own prefab survived; it is superseded")
	}
	if !kept["Warrior_02.prefab"] {
		t.Error("Warrior_02 failed to assemble, so its prefab is the only representation it has left — and it was dropped")
	}
}

// A .sk exported to the top of a package makes its directory the whole package. Scoping
// suppression by directory then claimed every prefab, material and .asset in it.
func TestSidekickAtThePackageRootDoesNotClaimEverything(t *testing.T) {
	root, mk := libRoot(t)
	writeUnityPackage(t, mk("synty", "SIDEKICK_Y", "SIDEKICK_Y_Unity_2021_3_v1_0_0.unitypackage"), []unityGUID{
		{guid: "sk1", pathname: "Assets/Hero.sk", asset: "Name: Hero\nParts:\n- Name: SK_HEAD\n"},
		{guid: "hd1", pathname: "Assets/Resources/Meshes/SK_HEAD.fbx", asset: "HEADFBX"},
		{guid: "u1", pathname: "Assets/Totally/Unrelated/Rock.prefab", asset: "ROCK"},
		{guid: "u2", pathname: "Assets/Totally/Unrelated/Rock.mat", asset: "ROCKMAT"},
		{guid: "u3", pathname: "Assets/Other/Tree.asset", asset: "TREE"},
	})

	assets, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	kept := map[string]bool{}
	for _, a := range assets {
		kept[a.Name] = true
	}
	if !kept["Hero"] {
		t.Error("Hero did not assemble")
	}
	for _, n := range []string{"Rock.prefab", "Rock.mat", "Tree.asset"} {
		if !kept[n] {
			t.Errorf("%s was dropped as a Sidekick byproduct; it has nothing to do with the character", n)
		}
	}
}

// Two characters whose names share a prefix. Synty joins a byproduct's suffix to the
// character name with the same "_" the names themselves contain, so only the longer
// claim tells "Hero_Alt.prefab" (Hero_Alt's) apart from "Hero_CombinedMesh.asset"
// (Hero's). Hero_Alt failed to assemble here, so its byproducts are all it has left.
func TestSidekickDoesNotClaimALongerNamedNeighbour(t *testing.T) {
	root, mk := libRoot(t)
	writeUnityPackage(t, mk("synty", "SIDEKICK_Z", "SIDEKICK_Z_Unity_2021_3_v1_0_0.unitypackage"), []unityGUID{
		{guid: "sk1", pathname: "Assets/S/Characters/Hero.sk", asset: "Name: Hero\nParts:\n- Name: SK_HEAD\n"},
		{guid: "hd1", pathname: "Assets/S/Resources/Meshes/SK_HEAD.fbx", asset: "HEADFBX"},
		{guid: "sk2", pathname: "Assets/S/Characters/Hero_Alt.sk", asset: "Name: Hero_Alt\nParts:\n- Name: SK_ABSENT\n"},
		{guid: "p2", pathname: "Assets/S/Characters/Hero_Alt.prefab", asset: "PREFAB2"},
		{guid: "m2", pathname: "Assets/S/Characters/Hero_Alt.mat", asset: "MAT2"},
		// Hero's own byproducts, which it does supersede.
		{guid: "p1", pathname: "Assets/S/Characters/Hero.prefab", asset: "PREFAB1"},
		{guid: "c1", pathname: "Assets/S/Characters/Hero_CombinedMesh.asset", asset: "MESH1"},
		// A neighbour that merely starts with the character name is not a byproduct.
		{guid: "u1", pathname: "Assets/S/Characters/HeroicBanner.prefab", asset: "BANNER"},
	})

	assets, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	kept := map[string]bool{}
	for _, a := range assets {
		kept[a.Name] = true
	}
	if !kept["Hero"] {
		t.Fatal("Hero did not assemble")
	}
	for _, n := range []string{"Hero_Alt.prefab", "Hero_Alt.mat"} {
		if !kept[n] {
			t.Errorf("%s was dropped, but it belongs to Hero_Alt, which failed to assemble and has no other representation", n)
		}
	}
	if !kept["HeroicBanner.prefab"] {
		t.Error("HeroicBanner.prefab was dropped; it only shares a prefix with the character name")
	}
	for _, n := range []string{"Hero.prefab", "Hero_CombinedMesh.asset"} {
		if kept[n] {
			t.Errorf("%s survived; the assembled character supersedes its own byproducts", n)
		}
	}
}

// A Sidekick pass that cannot read the package's .sk data keeps every other asset,
// reports the failure, and must not let the archive's stat print cache the degraded
// enumeration — otherwise the missing characters persist until --reindex.
func TestFailedSidekickAssemblyIsReportedAndNotCached(t *testing.T) {
	root, mk := libRoot(t)
	cacheDir := t.TempDir()
	pkg := mk("synty", "SIDEKICK_W", "SIDEKICK_W_Unity_2021_3_v1_0_0.unitypackage")
	writeUnityPackage(t, pkg, []unityGUID{
		{guid: "sk1", pathname: "Assets/S/Characters/Hero.sk", asset: "Name: Hero\nParts:\n- Name: SK_HEAD\n"},
		{guid: "hd1", pathname: "Assets/S/Resources/Meshes/SK_HEAD.fbx", asset: "HEADFBX"},
	})

	ix, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ix.ArchivePrint[pkg]; !ok {
		t.Fatal("a package that assembled cleanly should have its print cached")
	}

	// Truncate the archive past the enumeration head so the second pass fails while
	// the first still succeeds.
	full, err := os.ReadFile(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pkg, full[:len(full)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	broken, err := LoadOrBuild(Options{Root: root, CacheDir: cacheDir}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := broken.ArchivePrint[pkg]; ok {
		t.Error("a degraded enumeration was cached against the archive's print; it would be reused until --reindex")
	}
	if len(broken.Skipped) == 0 {
		t.Error("nothing was reported; the failure would be invisible to the user")
	}
}
