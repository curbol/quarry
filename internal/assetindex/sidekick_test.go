package assetindex

import (
	"os"
	"reflect"
	"strings"
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

// A ColorSet nested under a part, rather than beside the Parts block, is the shape the
// top-level guard does not see: every line indented under Parts is inside it, so a
// scanner blind to depth collects the swatch names as parts. Each then resolves to no
// mesh, and the character is marked unassembled — which is not a broken card, but it is
// what tells the walk it can drop the pack's prefabs, materials and combined meshes, so
// every character in the pack keeps its byproducts beside it in the grid.
func TestParseSidekickIgnoresNamesNestedUnderAPart(t *testing.T) {
	sk := "" +
		"Name: Hero_01\n" +
		"Parts:\n" +
		"- Name: SK_HEAD\n" +
		"  PartType: Head\n" +
		"  ColorSet:\n" +
		"  - Name: Skin_01\n" +
		"  - Name: Skin_02\n" +
		"  BlendShapes:\n" +
		"  - Name: Jaw_Wide\n" +
		"- Name: SK_TORS\n" +
		"  PartType: Torso\n"

	name, parts := parseSidekick([]byte(sk))
	if name != "Hero_01" {
		t.Errorf("name = %q, want Hero_01", name)
	}
	want := []string{"SK_HEAD", "SK_TORS"}
	if !reflect.DeepEqual(parts, want) {
		t.Errorf("parts = %v, want %v: only the block's own items are parts", parts, want)
	}
}

// The items' own indent is read from the first one rather than assumed, since nothing
// says a .sk indents its lists at column zero.
func TestParseSidekickReadsPartsIndentedAsABlock(t *testing.T) {
	sk := "" +
		"Name: Hero_02\n" +
		"Parts:\n" +
		"  - Name: SK_HEAD\n" +
		"    ColorSet:\n" +
		"      - Name: Skin_01\n" +
		"  - Name: SK_TORS\n"

	_, parts := parseSidekick([]byte(sk))
	want := []string{"SK_HEAD", "SK_TORS"}
	if !reflect.DeepEqual(parts, want) {
		t.Errorf("parts = %v, want %v", parts, want)
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

	// Built with a cache dir because the part ids are resolved through this same index
	// below, the way the frontend resolves them.
	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	assets := ix.Assets
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
	// Parts resolve to the two present FBX meshes, in .sk order; the missing part is
	// dropped. Resolved through the index rather than recomputed with id(): the
	// frontend fetches /api/content per part, so what matters is that each id reaches
	// an asset the index can serve, which calling id() on both sides cannot show.
	var got []string
	for _, pid := range ch.Source.Parts {
		part, ok := ix.Lookup(pid)
		if !ok {
			t.Errorf("part id %s does not resolve through the index; the frontend would 404 that limb", pid)
			continue
		}
		got = append(got, part.Name)
	}
	if want := []string{"SK_HEAD.fbx", "SK_TORS.fbx"}; !reflect.DeepEqual(got, want) {
		t.Errorf("parts resolved to %v, want %v", got, want)
	}
}

// A part name is matched on the base name alone, and a package's demo or showcase
// meshes can carry the same one. The parts tree wins, so a character is assembled from
// the meshes meant to be worn rather than from whichever copy was enumerated last.
func TestSidekickPrefersThePartsTreeMesh(t *testing.T) {
	root, mk := libRoot(t)
	writeUnityPackage(t, mk("synty", "SIDEKICK_P", "SIDEKICK_P_Unity_2021_3_v1_0_0.unitypackage"), []unityGUID{
		{guid: "sk1", pathname: "Assets/S/Characters/Hero.sk", asset: "Name: Hero\nParts:\n- Name: SK_HEAD\n"},
		{guid: "hd1", pathname: "Assets/S/Resources/Meshes/SK_HEAD.fbx", asset: "REALPART"},
		// Enumerated after the real one, so last-write-wins would pick this.
		{guid: "sh1", pathname: "Assets/S/Demo/Showcase/SK_HEAD.fbx", asset: "SHOWCASE"},
	})
	ix, err := Build(Options{Root: root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// Collected rather than asserted in place: the preference only matters once a
	// character assembles, so a scan that upgraded nothing would otherwise run the body
	// zero times and pass — which is the regression this exists to catch.
	var parts []string
	for i := range ix.Assets {
		if ix.Assets[i].Thumb == ThumbSidekick {
			parts = ix.Assets[i].Source.Parts
		}
	}
	if len(parts) != 1 {
		t.Fatalf("character assembled with %d parts, want 1", len(parts))
	}
	part, ok := ix.Lookup(parts[0])
	if !ok {
		t.Fatalf("part id %s does not resolve", parts[0])
	}
	if !strings.Contains(part.Source.Pathname, "/Resources/") {
		t.Errorf("part resolved to %s, want the mesh under Resources/", part.Source.Pathname)
	}
}

// scanPackage writes one unitypackage into a fresh library and reports the display
// names it indexed. Every byproduct case is the same shape — a package in, a set of
// surviving rows out — so they read as one table rather than six near-identical tests.
func scanPackage(t *testing.T, entries []unityGUID) map[string]bool {
	t.Helper()
	root, mk := libRoot(t)
	writeUnityPackage(t, mk("synty", "SIDEKICK", "SIDEKICK_Unity_2021_3_v1_0_0.unitypackage"), entries)
	ix, err := Build(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	assets := ix.Assets
	kept := map[string]bool{}
	for _, a := range assets {
		kept[a.Name] = true
	}
	return kept
}

// Which per-character byproducts an assembled .sk supersedes, and which it must leave
// alone. The rule is that the longest character name matching the file wins the claim,
// and only then does that character's assembly decide — so everything here turns on
// scoping the claim correctly and on what counts as assembled.
func TestSidekickByproductSuppression(t *testing.T) {
	head := unityGUID{guid: "hd1", pathname: "Assets/S/Resources/Meshes/SK_HEAD.fbx", asset: "HEADFBX"}
	for _, tc := range []struct {
		name    string
		entries []unityGUID
		kept    []string
		gone    []string
	}{
		{
			// The magenta prefab, its material and the combined-mesh / avatar data are
			// what the assembled character replaces. The reusable part under Resources/
			// and the character's own textures are not byproducts.
			name: "an assembled character supersedes its own byproducts",
			entries: []unityGUID{
				{guid: "sk1", pathname: "Assets/S/Characters/W_01/W_01.sk", asset: "Name: W_01\nParts:\n- Name: SK_HEAD\n"},
				head,
				{guid: "pf1", pathname: "Assets/S/Characters/W_01/W_01.prefab", asset: "PREFAB", preview: true},
				{guid: "mt1", pathname: "Assets/S/Characters/W_01/Materials/W_01.mat", asset: "MAT", preview: true},
				{guid: "av1", pathname: "Assets/S/Characters/W_01/Meshes/W_01-avatar.asset", asset: "AVATAR"},
				{guid: "tx1", pathname: "Assets/S/Characters/W_01/Textures/T_W_01ColorMap.png", asset: "PNG"},
			},
			kept: []string{"W_01", "SK_HEAD.fbx", "T_W_01ColorMap.png"},
			gone: []string{"W_01.prefab", "W_01.mat", "W_01-avatar.asset"},
		},
		{
			// A character whose parts are not in this archive has its prefab and material
			// as its only representation, and it must not be upgraded either.
			name: "a character that could not assemble keeps everything",
			entries: []unityGUID{
				{guid: "sk1", pathname: "Assets/S/Characters/Warrior_01.sk", asset: "Name: Warrior_01\nParts:\n- Name: SK_ABSENT\n"},
				{guid: "pf1", pathname: "Assets/S/Characters/Warrior_01.prefab", asset: "PREFAB", preview: true},
				{guid: "mt1", pathname: "Assets/S/Characters/Warrior_01.mat", asset: "MAT"},
			},
			kept: []string{"Warrior_01.sk", "Warrior_01.prefab", "Warrior_01.mat"},
			gone: []string{"Warrior_01"},
		},
		{
			// Two characters in one directory: scoping the claim by directory alone let
			// the one that assembled take the other's byproducts.
			name: "an assembled character does not claim its neighbour's byproducts",
			entries: []unityGUID{
				{guid: "sk1", pathname: "Assets/S/Characters/Warrior_01.sk", asset: "Name: Warrior_01\nParts:\n- Name: SK_HEAD\n"},
				{guid: "sk2", pathname: "Assets/S/Characters/Warrior_02.sk", asset: "Name: Warrior_02\nParts:\n- Name: SK_ABSENT\n"},
				head,
				{guid: "p1", pathname: "Assets/S/Characters/Warrior_01.prefab", asset: "PREFAB1"},
				{guid: "p2", pathname: "Assets/S/Characters/Warrior_02.prefab", asset: "PREFAB2"},
			},
			kept: []string{"Warrior_01", "Warrior_02.prefab"},
			gone: []string{"Warrior_01.prefab"},
		},
		{
			// One part of two resolved. The character is still worth showing, but the
			// combined mesh and prefab are the rows that show it whole — the same
			// outcome readUnityAssetBytes refuses a truncated .sk read to avoid.
			name: "a partly assembled character keeps its byproducts",
			entries: []unityGUID{
				{guid: "sk1", pathname: "Assets/S/Characters/Hero.sk", asset: "Name: Hero\nParts:\n- Name: SK_HEAD\n- Name: SK_ABSENT\n"},
				head,
				{guid: "p1", pathname: "Assets/S/Characters/Hero.prefab", asset: "PREFAB"},
				{guid: "c1", pathname: "Assets/S/Characters/Hero_CombinedMesh.asset", asset: "MESH"},
			},
			kept: []string{"Hero", "Hero.prefab", "Hero_CombinedMesh.asset"},
		},
		{
			// A .sk exported to the top of a package makes its directory the whole
			// package, so the name has to carry the claim on its own.
			name: "a character at the package root claims nothing by directory",
			entries: []unityGUID{
				{guid: "sk1", pathname: "Assets/Hero.sk", asset: "Name: Hero\nParts:\n- Name: SK_HEAD\n"},
				{guid: "hd1", pathname: "Assets/Resources/Meshes/SK_HEAD.fbx", asset: "HEADFBX"},
				{guid: "u1", pathname: "Assets/Totally/Unrelated/Rock.prefab", asset: "ROCK"},
				{guid: "u2", pathname: "Assets/Totally/Unrelated/Rock.mat", asset: "ROCKMAT"},
				{guid: "u3", pathname: "Assets/Other/Tree.asset", asset: "TREE"},
			},
			kept: []string{"Hero", "Rock.prefab", "Rock.mat", "Tree.asset"},
		},
		{
			// Synty joins a byproduct's suffix to the character name with the same "_"
			// the names themselves contain, so "Hero_Alt.prefab" is Hero_Alt's only
			// because Hero_Alt is the longer claim on it. "HeroicBanner" merely shares a
			// prefix and is nobody's byproduct.
			name: "the longer character name wins the claim",
			entries: []unityGUID{
				{guid: "sk1", pathname: "Assets/S/Characters/Hero.sk", asset: "Name: Hero\nParts:\n- Name: SK_HEAD\n"},
				head,
				{guid: "sk2", pathname: "Assets/S/Characters/Hero_Alt.sk", asset: "Name: Hero_Alt\nParts:\n- Name: SK_ABSENT\n"},
				{guid: "p2", pathname: "Assets/S/Characters/Hero_Alt.prefab", asset: "PREFAB2"},
				{guid: "m2", pathname: "Assets/S/Characters/Hero_Alt.mat", asset: "MAT2"},
				{guid: "p1", pathname: "Assets/S/Characters/Hero.prefab", asset: "PREFAB1"},
				{guid: "c1", pathname: "Assets/S/Characters/Hero_CombinedMesh.asset", asset: "MESH1"},
				{guid: "u1", pathname: "Assets/S/Characters/HeroicBanner.prefab", asset: "BANNER"},
			},
			kept: []string{"Hero", "Hero_Alt.prefab", "Hero_Alt.mat", "HeroicBanner.prefab"},
			gone: []string{"Hero.prefab", "Hero_CombinedMesh.asset"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kept := scanPackage(t, tc.entries)
			for _, n := range tc.kept {
				if !kept[n] {
					t.Errorf("%s was dropped; want it kept", n)
				}
			}
			for _, n := range tc.gone {
				if kept[n] {
					t.Errorf("%s survived; want it superseded", n)
				}
			}
		})
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

// scanPackagePaths is scanPackage keyed on the .unitypackage pathname rather than the
// asset name, because the depth tie-break below turns on two characters of the same
// name in different directories, which a name-keyed set cannot tell apart.
func scanPackagePaths(t *testing.T, entries []unityGUID) map[string]bool {
	t.Helper()
	root, mk := libRoot(t)
	writeUnityPackage(t, mk("synty", "SIDEKICK", "SIDEKICK_Unity_2021_3_v1_0_0.unitypackage"), entries)
	ix, err := Build(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	assets := ix.Assets
	kept := map[string]bool{}
	for _, a := range assets {
		kept[a.Source.Pathname] = true
	}
	return kept
}

// Two characters of the same base name, one nested under the other's directory. Name
// length cannot separate them, so the deeper tree wins the claim — and it has to, or the
// outer character (which assembled) takes the inner one's prefab and drops it, deleting
// the only row that shows a character its own .sk could not assemble.
func TestTheDeeperSidekickClaimsItsOwnByproducts(t *testing.T) {
	head := unityGUID{guid: "hd1", pathname: "Assets/S/Resources/Meshes/SK_HEAD.fbx", asset: "HEADFBX"}
	torso := unityGUID{guid: "tr1", pathname: "Assets/S/Resources/Meshes/SK_TORSO.fbx", asset: "TORSOFBX"}
	outer := unityGUID{guid: "ou1", pathname: "Assets/S/Hero.sk", asset: "Name: Hero\nParts:\n  - Name: SK_HEAD\n  - Name: SK_TORSO\n"}
	// The inner character names a part the package does not hold, so it never assembles.
	inner := unityGUID{guid: "in1", pathname: "Assets/S/Sub/Hero.sk", asset: "Name: Hero\nParts:\n  - Name: SK_HEAD\n  - Name: SK_MISSING\n"}
	innerPrefab := unityGUID{guid: "ip1", pathname: "Assets/S/Sub/Hero.prefab", asset: "PREFAB"}
	outerPrefab := unityGUID{guid: "op1", pathname: "Assets/S/Hero.prefab", asset: "PREFAB"}

	kept := scanPackagePaths(t, []unityGUID{head, torso, outer, inner, innerPrefab, outerPrefab})

	if kept["Assets/S/Hero.prefab"] {
		t.Error("the outer character assembled, so its own prefab is superseded")
	}
	if !kept["Assets/S/Sub/Hero.prefab"] {
		t.Error("the inner character did not assemble, so its prefab is the only row that shows it and must survive")
	}
}

// readUnityAssetBytes reads one byte past the limit and discards what exceeds it,
// rather than truncating. A truncated .sk parses into a short part list, every name of
// which resolves — so the character reports itself fully assembled while missing the
// limbs the cut tail named, and the prefab that would have shown it whole is dropped.
func TestAnOversizeSidekickDefinitionIsRefusedNotTruncated(t *testing.T) {
	head := unityGUID{guid: "hd1", pathname: "Assets/S/Resources/Meshes/SK_HEAD.fbx", asset: "HEADFBX"}
	torso := unityGUID{guid: "tr1", pathname: "Assets/S/Resources/Meshes/SK_TORSO.fbx", asset: "TORSOFBX"}
	// A valid definition, then padding past the limit. Truncated at the limit this
	// would parse as a complete two-part character.
	body := "Name: Hero\nParts:\n  - Name: SK_HEAD\n  - Name: SK_TORSO\n" +
		"Notes: " + strings.Repeat("x", maxSidekickBytes) + "\n"
	sk := unityGUID{guid: "sk1", pathname: "Assets/S/Hero.sk", asset: body}
	prefab := unityGUID{guid: "pf1", pathname: "Assets/S/Hero.prefab", asset: "PREFAB"}

	root, mk := libRoot(t)
	writeUnityPackage(t, mk("synty", "SIDEKICK", "SIDEKICK_Unity_2021_3_v1_0_0.unitypackage"),
		[]unityGUID{head, torso, sk, prefab})
	ix, err := Build(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	assets := ix.Assets
	var character *Asset
	kept := map[string]bool{}
	for i := range assets {
		kept[assets[i].Source.Pathname] = true
		if assets[i].Ext == "sk" {
			character = &assets[i]
		}
	}
	if character == nil {
		t.Fatal("the .sk row itself must survive")
	}
	if len(character.Source.Parts) != 0 {
		t.Errorf("the .sk was read past its limit and assembled %d parts", len(character.Source.Parts))
	}
	if !kept["Assets/S/Hero.prefab"] {
		t.Error("nothing assembled, so the prefab is the only row showing this character and must survive")
	}
}
