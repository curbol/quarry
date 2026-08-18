package browse

import (
	"testing"

	"github.com/curbol/quarry/internal/assetindex"
)

func TestBuildRootMotionPairs(t *testing.T) {
	animZip := func(id, entry, clip, ext string) assetindex.Asset {
		return assetindex.Asset{ID: id, Ext: ext, Vendor: "synty", Pack: "Loco", Category: assetindex.CategoryAnimation,
			Source: assetindex.Source{Kind: assetindex.SourceZip, ArchivePath: "a.zip", Entry: entry, Clip: clip}}
	}
	loose := func(id, fp, clip, ext string, cat assetindex.Category) assetindex.Asset {
		return assetindex.Asset{ID: id, Ext: ext, Vendor: "quaternius", Pack: "UAL", Category: cat,
			Source: assetindex.Source{Kind: assetindex.SourceLoose, FilePath: fp, Clip: clip}}
	}
	kevdev := func(id, fp string) assetindex.Asset {
		return assetindex.Asset{ID: id, Ext: "fbx", Vendor: "kevdev", Pack: "HBM", Category: assetindex.CategoryAnimation,
			Source: assetindex.Source{Kind: assetindex.SourceLoose, FilePath: fp}}
	}
	assets := []assetindex.Asset{
		animZip("n1", "SF/Turn_Masc.fbx", "", "fbx"),                              // Synty non-RM
		animZip("r1", "SF/Turn_RM_Masc.fbx", "", "fbx"),                           // its RM sibling
		loose("g1", "/lib/UAL1.glb", "Walk", "glb", assetindex.CategoryAnimation), // GLB non-RM clips
		loose("g2", "/lib/UAL1.glb", "Run", "glb", assetindex.CategoryAnimation),
		loose("grf", "/lib/UAL1_RM.fbx", "", "fbx", assetindex.CategoryModel),      // wrong-ext RM, listed first (as Unity/ sorts before Unreal-Godot/)
		loose("gr", "/lib/UAL1_RM.glb", "", "glb", assetindex.CategoryModel),       // the glb RM sibling
		loose("solo", "/lib/Idle.glb", "Sit", "glb", assetindex.CategoryAnimation), // no RM sibling
		kevdev("kv", "/lib/Walk/HumanF@Walk_Fwd.fbx"),                              // kevdev in-place
		kevdev("kvrm", "/lib/Walk/RootMotion/HumanF@Walk_Fwd [RM].fbx"),            // kevdev bracket RM, in a RootMotion/ subfolder
		animZip("sp", "SF/A_Dodge_L_Sword.fbx", "", "fbx"),                         // Synty Polygon in-place
		animZip("sprm", "SF/A_Dodge_L_RootMotion_Sword.fbx", "", "fbx"),            // its RootMotion sibling
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

// The RM sibling has to be the same container. A pack that ships the in-place clip
// as one format and the root-motion file as another offers no sibling: pairing them
// would point the toggle at a file the viewer cannot load, and suppressing the RM
// would drop a card for a file that is right there on disk.
func TestRootMotionPairingRequiresTheSameContainer(t *testing.T) {
	clip := assetindex.Asset{ID: "g1", Ext: "glb", Vendor: "quaternius", Pack: "UAL", Category: assetindex.CategoryAnimation,
		Source: assetindex.Source{Kind: assetindex.SourceLoose, FilePath: "/lib/UAL1.glb", Clip: "Walk"}}
	otherFormatRM := assetindex.Asset{ID: "rmf", Ext: "fbx", Vendor: "quaternius", Pack: "UAL", Category: assetindex.CategoryModel,
		Source: assetindex.Source{Kind: assetindex.SourceLoose, FilePath: "/lib/UAL1_RM.fbx"}}

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
	base := assetindex.Asset{ID: "m1", Ext: "png", Vendor: "synty", Pack: "Kit", Category: assetindex.CategoryTexture,
		Source: assetindex.Source{Kind: assetindex.SourceLoose, FilePath: "/lib/T_Sword.png"}}
	rm := assetindex.Asset{ID: "m2", Ext: "png", Vendor: "synty", Pack: "Kit", Category: assetindex.CategoryTexture,
		Source: assetindex.Source{Kind: assetindex.SourceLoose, FilePath: "/lib/T_Sword_RM.png"}}

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
	zipAsset := func(id, entry string) assetindex.Asset {
		return assetindex.Asset{ID: id, Ext: "fbx", Vendor: "synty", Pack: "POLYGON_X", Category: assetindex.CategoryAnimation,
			Source: assetindex.Source{Kind: assetindex.SourceZip, ArchivePath: zipPath, Entry: entry}}
	}
	uniAsset := func(id, pathname string) assetindex.Asset {
		return assetindex.Asset{ID: id, Ext: "fbx", Vendor: "synty", Pack: "POLYGON_X", Category: assetindex.CategoryAnimation,
			Source: assetindex.Source{Kind: assetindex.SourceUnityPackage, ArchivePath: uniPath, Guid: id, Pathname: pathname}}
	}
	sibling, suppressed := buildRootMotionPairs([]assetindex.Asset{
		zipAsset("zip-walk", "SF/A_Walk_Masc.fbx"),
		zipAsset("zip-walk-rm", "SF/A_Walk_RM_Masc.fbx"),
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
	mk := func(id, entry, ext string, cat assetindex.Category) assetindex.Asset {
		return assetindex.Asset{ID: id, Ext: ext, Vendor: "v", Pack: "P", Category: cat,
			Source: assetindex.Source{Kind: assetindex.SourceZip, ArchivePath: "a.zip", Entry: entry}}
	}
	sibling, suppressed := buildRootMotionPairs([]assetindex.Asset{
		mk("anim", "SF/Sword.fbx", "fbx", assetindex.CategoryAnimation),
		mk("anim-rm", "SF/Sword_RM.fbx", "fbx", assetindex.CategoryAnimation),
		mk("tex", "SF/Sword.png", "png", assetindex.CategoryTexture),
		mk("tex-rm", "SF/Sword_RM.png", "png", assetindex.CategoryTexture),
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
