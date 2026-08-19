package browse

import (
	"path"
	"strings"
	"testing"

	"github.com/curbol/quarry/internal/assetindex"
)

// zipAnim is one animation inside a zip, the shape most pairing cases are built from.
func zipAnim(id, vendor, pack, archive, entry string) assetindex.Asset {
	return assetindex.Asset{
		ID: id, Ext: strings.TrimPrefix(path.Ext(entry), "."), Vendor: vendor, Pack: pack,
		Category: assetindex.CategoryAnimation,
		Source:   assetindex.Source{Kind: assetindex.SourceZip, ArchivePath: archive, Entry: entry},
	}
}

// looseAnim is one animation in a loose file, where a clip name disambiguates the
// several a single file can hold. Ext follows the path in both helpers, as the scan
// sets it, so a case cannot pair on an extension its own filename contradicts.
func looseAnim(id, vendor, pack, file, clip string) assetindex.Asset {
	return assetindex.Asset{
		ID: id, Ext: strings.TrimPrefix(path.Ext(file), "."), Vendor: vendor, Pack: pack,
		Category: assetindex.CategoryAnimation,
		Source:   assetindex.Source{Kind: assetindex.SourceLoose, FilePath: file, Clip: clip},
	}
}

// as returns the asset with another category, for the cases that turn on a group
// holding more than one kind.
func as(a assetindex.Asset, cat assetindex.Category) assetindex.Asset {
	a.Category = cat
	return a
}

func TestBuildRootMotionPairs(t *testing.T) {
	assets := []assetindex.Asset{
		zipAnim("n1", "synty", "Loco", "a.zip", "SF/Turn_Masc.fbx"),    // Synty non-RM
		zipAnim("r1", "synty", "Loco", "a.zip", "SF/Turn_RM_Masc.fbx"), // its RM sibling
		looseAnim("g1", "quaternius", "UAL", "/lib/UAL1.glb", "Walk"),  // GLB non-RM clips
		looseAnim("g2", "quaternius", "UAL", "/lib/UAL1.glb", "Run"),
		// wrong-ext RM, listed first (as Unity/ sorts before Unreal-Godot/)
		as(looseAnim("grf", "quaternius", "UAL", "/lib/UAL1_RM.fbx", ""), assetindex.CategoryModel),
		// the glb RM sibling
		as(looseAnim("gr", "quaternius", "UAL", "/lib/UAL1_RM.glb", ""), assetindex.CategoryModel),
		looseAnim("solo", "quaternius", "UAL", "/lib/Idle.glb", "Sit"),                  // no RM sibling
		looseAnim("kv", "kevdev", "HBM", "/lib/Walk/HumanF@Walk_Fwd.fbx", ""),           // kevdev in-place
		looseAnim("kvrm", "kevdev", "HBM", "/lib/Walk/RM/HumanF@Walk_Fwd [RM].fbx", ""), // kevdev bracket RM, in its own subfolder
		zipAnim("sp", "synty", "Loco", "a.zip", "SF/A_Dodge_L_Sword.fbx"),               // Synty Polygon in-place
		zipAnim("sprm", "synty", "Loco", "a.zip", "SF/A_Dodge_L_RootMotion_Sword.fbx"),  // its RootMotion sibling
	}

	sibling, suppressed := buildRootMotionPairs(assets)

	if sibling["n1"] != "r1" {
		t.Errorf("Synty pair: n1 -> %q, want r1", sibling["n1"])
	}
	if sibling["kv"] != "kvrm" {
		t.Errorf("kevdev bracket pair: kv -> %q, want kvrm (the [RM] file, folder-agnostic)", sibling["kv"])
	}
	if sibling["sp"] != "sprm" {
		t.Errorf("Synty RootMotion pair: sp -> %q, want sprm", sibling["sp"])
	}
	if !suppressed["kvrm"] || !suppressed["sprm"] {
		t.Errorf("RootMotion/bracket siblings must be suppressed: kvrm=%v sprm=%v", suppressed["kvrm"], suppressed["sprm"])
	}
	if sibling["g1"] != "gr" || sibling["g2"] != "gr" {
		t.Errorf("GLB clips must pair the glb RM (same ext), not the fbx RM: g1=%q g2=%q, want gr", sibling["g1"], sibling["g2"])
	}
	if _, ok := sibling["solo"]; ok {
		t.Error("an animation with no RM sibling must not be paired")
	}
	if !suppressed["r1"] || !suppressed["gr"] {
		t.Errorf("RM siblings that a card actually plays must be suppressed: r1=%v gr=%v", suppressed["r1"], suppressed["gr"])
	}
	// grf is an RM file no in-place card selected (the glb clips prefer the glb RM).
	// Hiding it too would make it unreachable in browse though the file exists.
	if suppressed["grf"] {
		t.Error("an RM file that no card plays must stay visible, not vanish from the grid")
	}
	if suppressed["n1"] || suppressed["g1"] {
		t.Error("non-RM cards must never be suppressed")
	}
}

// splitEntry is where the platform enters pairing, so it is checked over both
// separator sets directly: a Unix host leaves a backslash alone, which would leave
// the Windows half of this untested everywhere it can actually be run.
func TestSplitEntry(t *testing.T) {
	tests := []struct {
		name, path, seps  string
		wantDir, wantBase string
	}{
		{"slash path, slash seps", "Anims/Goblin/Walk.fbx", "/", "Anims/Goblin", "Walk.fbx"},
		{"no separator", "Walk.fbx", "/", "", "Walk.fbx"},
		{"backslash path read as Windows", `lib\Walk\RM\Fwd [RM].fbx`, `/\`, `lib\Walk\RM`, "Fwd [RM].fbx"},
		{"backslash path read as slash-only", `lib\Walk\Fwd.fbx`, "/", "", `lib\Walk\Fwd.fbx`},
		{"mixed separators read as Windows", `lib/Walk\Fwd.fbx`, `/\`, `lib/Walk`, "Fwd.fbx"},
		{"trailing separator", "Anims/", "/", "Anims", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir, base := splitEntry(tc.path, tc.seps)
			if dir != tc.wantDir || base != tc.wantBase {
				t.Errorf("splitEntry(%q, %q) = (%q, %q), want (%q, %q)", tc.path, tc.seps, dir, base, tc.wantDir, tc.wantBase)
			}
		})
	}
}

func TestSeparatorsForFollowsThePlatform(t *testing.T) {
	if got := separatorsFor('\\'); got != `/\` {
		t.Errorf("windows separators = %q, want %q", got, `/\`)
	}
	if got := separatorsFor('/'); got != "/" {
		t.Errorf("unix separators = %q, want %q", got, "/")
	}
}

// windowsPaths installs the Windows separator set for one test. A loose file's path
// is whatever the filesystem handed the scan, so on Windows it holds backslashes
// while a zip entry and a unity pathname never do; a Unix host cannot produce that
// input on its own, and this is the behaviour that has to be pinned.
func windowsPaths(t *testing.T) {
	t.Helper()
	prev := osSeparators
	osSeparators = separatorsFor('\\')
	t.Cleanup(func() { osSeparators = prev })
}

// Splitting a loose path with "/" alone left the directory inside the base name, so a
// pack keeping its root-motion files in their own folder stopped pairing entirely: no
// toggle on the card, and the RM file showing as a card of its own beside it.
func TestPairingPairsAcrossDirectoriesOnWindows(t *testing.T) {
	windowsPaths(t)
	at := func(id string, parts ...string) assetindex.Asset {
		return looseAnim(id, "kevdev", "HBM", strings.Join(parts, `\`), "")
	}
	sibling, suppressed := buildRootMotionPairs([]assetindex.Asset{
		at("kv", "lib", "Walk", "HumanF@Walk_Fwd.fbx"),
		at("kvrm", "lib", "Walk", "RootMotion", "HumanF@Walk_Fwd [RM].fbx"),
	})
	if sibling["kv"] != "kvrm" {
		t.Errorf("kv -> %q, want kvrm", sibling["kv"])
	}
	if !suppressed["kvrm"] {
		t.Error("the RM file was not suppressed, so it shows as a card beside the pair")
	}
}

// The same-directory preference is a tie-break between candidates, so it has to read
// a real directory. Taking every loose asset's directory as the same empty string made
// it fire for every candidate at once, leaving the pick to whichever RM came first.
func TestPickRMPrefersTheSameDirectoryOnWindows(t *testing.T) {
	windowsPaths(t)
	at := func(id string, parts ...string) assetindex.Asset {
		return looseAnim(id, "synty", "P", strings.Join(parts, `\`), "")
	}
	// Orc's RM is listed first, so a directory term that does not work leaves the
	// goblin card holding it.
	sibling, _ := buildRootMotionPairs([]assetindex.Asset{
		at("orcRM", "Anims", "Orc", "Walk_RM.fbx"),
		at("goblin", "Anims", "Goblin", "Walk.fbx"),
		at("goblinRM", "Anims", "Goblin", "Walk_RM.fbx"),
	})
	if got := sibling["goblin"]; got != "goblinRM" {
		t.Errorf("goblin paired with %q, want goblinRM (its own directory)", got)
	}
}

// The RM sibling has to be the same container. A pack that ships the in-place clip
// as one format and the root-motion file as another offers no sibling: pairing them
// would point the toggle at a file the viewer cannot load, and suppressing the RM
// would drop a card for a file that is right there on disk.
func TestRootMotionPairingRequiresTheSameContainer(t *testing.T) {
	clip := looseAnim("g1", "quaternius", "UAL", "/lib/UAL1.glb", "Walk")
	otherFormatRM := as(looseAnim("rmf", "quaternius", "UAL", "/lib/UAL1_RM.fbx", ""), assetindex.CategoryModel)

	sibling, suppressed := buildRootMotionPairs([]assetindex.Asset{clip, otherFormatRM})
	if got, ok := sibling["g1"]; ok {
		t.Errorf("a .glb clip was paired to %q, an RM in another container", got)
	}
	if suppressed["rmf"] {
		t.Error("the unmatched RM was hidden from the grid")
	}
}

// The visible side of a group has to include an animation. Two unrelated files that
// merely share a Foo / Foo_RM name — "_RM" is also how a roughness-metallic texture
// is labelled — must not collapse into one card with the second hidden behind it.
func TestRootMotionPairingIgnoresGroupsWithNoAnimation(t *testing.T) {
	base := as(looseAnim("m1", "synty", "Kit", "/lib/T_Sword.png", ""), assetindex.CategoryTexture)
	rm := as(looseAnim("m2", "synty", "Kit", "/lib/T_Sword_RM.png", ""), assetindex.CategoryTexture)

	sibling, suppressed := buildRootMotionPairs([]assetindex.Asset{base, rm})
	if len(sibling) != 0 || len(suppressed) != 0 {
		t.Errorf("non-animation files paired on their names alone: sibling=%v suppressed=%v", sibling, suppressed)
	}
}

// One pack commonly ships as both a SourceFiles zip and a unitypackage holding the same
// animations, and Pack is just a directory name, so both land in one pairing group.
// Scoring only on extension made every in-place card pick the same first RM: the other
// archive's RM was never suppressed and showed up beside the card it belongs to, while
// that card's toggle fetched a different archive than the one it was displaying.
func TestPairingKeepsEachArchiveToItsOwnRootMotionSibling(t *testing.T) {
	const zipPath, uniPath = "/lib/synty/POLYGON_X/POLYGON_X_SourceFiles_v3.zip", "/lib/synty/POLYGON_X/POLYGON_X_Unity_2022_3_v1.unitypackage"
	uniAsset := func(id, pathname string) assetindex.Asset {
		return assetindex.Asset{ID: id, Ext: "fbx", Vendor: "synty", Pack: "POLYGON_X", Category: assetindex.CategoryAnimation,
			Source: assetindex.Source{Kind: assetindex.SourceUnityPackage, ArchivePath: uniPath, Guid: id, Pathname: pathname}}
	}
	sibling, suppressed := buildRootMotionPairs([]assetindex.Asset{
		zipAnim("zip-walk", "synty", "POLYGON_X", zipPath, "SF/A_Walk_Masc.fbx"),
		zipAnim("zip-walk-rm", "synty", "POLYGON_X", zipPath, "SF/A_Walk_RM_Masc.fbx"),
		uniAsset("uni-walk", "Assets/Anim/A_Walk_Masc.fbx"),
		uniAsset("uni-walk-rm", "Assets/Anim/A_Walk_RM_Masc.fbx"),
	})

	if sibling["zip-walk"] != "zip-walk-rm" {
		t.Errorf("zip card -> %q, want its own archive's RM", sibling["zip-walk"])
	}
	if sibling["uni-walk"] != "uni-walk-rm" {
		t.Errorf("unitypackage card -> %q, want its own archive's RM", sibling["uni-walk"])
	}
	for _, id := range []string{"zip-walk-rm", "uni-walk-rm"} {
		if !suppressed[id] {
			t.Errorf("%s is not suppressed, so it renders as its own card beside the one it belongs to", id)
		}
	}
}

// A group holds whatever shares a base name in one pack, which need not be one kind:
// "_RM" is also the conventional suffix for a roughness-metallic map. Testing the group
// as a whole let one animation's presence hide a texture nothing will ever play.
func TestPairingLeavesANonAnimationRMAlone(t *testing.T) {
	sibling, suppressed := buildRootMotionPairs([]assetindex.Asset{
		zipAnim("anim", "v", "P", "a.zip", "SF/Sword.fbx"),
		zipAnim("anim-rm", "v", "P", "a.zip", "SF/Sword_RM.fbx"),
		as(zipAnim("tex", "v", "P", "a.zip", "SF/Sword.png"), assetindex.CategoryTexture),
		as(zipAnim("tex-rm", "v", "P", "a.zip", "SF/Sword_RM.png"), assetindex.CategoryTexture),
	})

	if sibling["anim"] != "anim-rm" || !suppressed["anim-rm"] {
		t.Errorf("the animation pair was not made: sibling=%v suppressed=%v", sibling, suppressed)
	}
	if suppressed["tex-rm"] {
		t.Error("a roughness-metallic map was hidden as if it were a root-motion animation")
	}
	if _, ok := sibling["tex"]; ok {
		t.Error("a texture was given a root-motion toggle")
	}
}

// The group key is only (vendor, pack, base), so a pack laid out per character puts
// Goblin/Walk.fbx, Orc/Walk.fbx and both their RM siblings in one group. Without a
// directory term both cards took the same first RM: one toggle played the wrong
// character's travel, and the other RM was never suppressed.
func TestPickRMPrefersASiblingInTheSameDirectory(t *testing.T) {
	assets := []assetindex.Asset{
		zipAnim("goblin", "synty", "P", "/p.zip", "Anims/Goblin/Walk.fbx"),
		zipAnim("goblinRM", "synty", "P", "/p.zip", "Anims/Goblin/Walk_RM.fbx"),
		zipAnim("orc", "synty", "P", "/p.zip", "Anims/Orc/Walk.fbx"),
		zipAnim("orcRM", "synty", "P", "/p.zip", "Anims/Orc/Walk_RM.fbx"),
	}

	sibling, suppressed := buildRootMotionPairs(assets)
	if got := sibling["goblin"]; got != "goblinRM" {
		t.Errorf("goblin paired with %q, want goblinRM", got)
	}
	if got := sibling["orc"]; got != "orcRM" {
		t.Errorf("orc paired with %q, want orcRM", got)
	}
	for _, id := range []string{"goblinRM", "orcRM"} {
		if !suppressed[id] {
			t.Errorf("%s was not suppressed; it would show as a card beside the pair it belongs to", id)
		}
	}
}

// A vendor shipping its root-motion variants in their own subfolder still pairs, so
// the directory has to stay a preference rather than part of the group key.
func TestPickRMStillPairsAcrossDirectoriesWhenThatIsAllThereIs(t *testing.T) {
	assets := []assetindex.Asset{
		zipAnim("walk", "kevdev", "P", "/p.zip", "Animations/Walk.fbx"),
		zipAnim("walkRM", "kevdev", "P", "/p.zip", "Animations/RootMotion/Walk_RM.fbx"),
	}
	sibling, suppressed := buildRootMotionPairs(assets)
	if got := sibling["walk"]; got != "walkRM" {
		t.Errorf("walk paired with %q, want walkRM (the only candidate, in another folder)", got)
	}
	if !suppressed["walkRM"] {
		t.Error("walkRM was not suppressed")
	}
}
