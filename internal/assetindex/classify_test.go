package assetindex

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		ext   string
		cat   Category
		thumb ThumbKind
	}{
		{"fbx", CategoryModel, ThumbFBX},
		{"glb", CategoryModel, ThumbGLB},
		{"gltf", CategoryModel, ThumbGLB},
		{"obj", CategoryModel, ThumbNone},
		{"png", CategoryImage, ThumbImage},
		{"tga", CategoryImage, ThumbNone},
		{"mat", CategoryMaterial, ThumbNone},
		{"tres", CategoryMaterial, ThumbNone},
		{"tscn", CategoryScene, ThumbNone},
		{"prefab", CategoryScene, ThumbNone},
		{"controller", CategoryAnimation, ThumbNone},
		{"wav", CategoryAudio, ThumbNone},
		{"cs", CategoryScript, ThumbNone},
		{"pdf", CategoryDoc, ThumbNone},
		{"json", CategoryDoc, ThumbNone},
		{"asset", CategoryData, ThumbNone},
		{"meta", CategoryData, ThumbNone},
		{"res", CategoryData, ThumbNone},
		{"playable", CategoryData, ThumbNone},
		{"terrainlayer", CategoryData, ThumbNone},
		{"preset", CategoryData, ThumbNone},
		{"lighting", CategoryData, ThumbNone},
		{"mesh", CategoryData, ThumbNone},
		{"sk", CategoryData, ThumbNone},
		{"ttf", CategoryFont, ThumbFont},
		{"otf", CategoryFont, ThumbFont},
		{"xyz", CategoryOther, ThumbNone},
	}
	for _, c := range cases {
		gotCat, gotThumb := classify(c.ext)
		if gotCat != c.cat || gotThumb != c.thumb {
			t.Errorf("classify(%q) = (%s,%s), want (%s,%s)", c.ext, gotCat, gotThumb, c.cat, c.thumb)
		}
	}
}

func TestRefineImage(t *testing.T) {
	cases := []struct {
		relPath      string
		vendor, pack string
		want         Category
	}{
		// UI: INTERFACE-pack sprite/icon/branding paths, and generic UI folders.
		{"synty/INTERFACE_Dark_Fantasy_HUD/x.zip::Source_Sprites/Core/Icons_Input/ICON_Input_Stick.png", "", "", CategoryUI},
		{"synty/INTERFACE_Fantasy_Menus/x.zip::Source_Sprites/Core/Branding/SPR_Logo.png", "", "", CategoryUI},
		{"pack/UI/button_01.png", "", "", CategoryUI},
		{"pack/HUD/minimap.png", "", "", CategoryUI},
		// UI wins over a texture folder when both are present in the path.
		{"pack/UI/Textures/icon.png", "", "", CategoryUI},
		// texture: /textures/ tree, sibling folders, and map suffixes.
		{"synty/POLYGON_Nature/x.zip::Textures/PolygonNature_Texture_01.png", "", "", CategoryTexture},
		{"pack/Textures/Wall_Normal.png", "", "", CategoryTexture},
		{"pack/Decals/blood_01.png", "", "", CategoryTexture},
		{"pack/Materials/rock_emissive.png", "", "", CategoryTexture},
		// image: the remainder (no UI token, no texture folder/suffix).
		{"pack/Misc/fx_circle_01.png", "", "", CategoryImage},
		{"pack/color_palette.png", "", "", CategoryImage},
		// "build" contains the substring "ui" but is not a UI token boundary.
		{"pack/Buildings/wall.png", "", "", CategoryImage},
		// The pack/archive NAME must not drive classification: POLYGON_Icons ships 3D
		// props, and a file under its Textures/ folder is a texture even though "Icons"
		// is in the pack name. Only the path inside the archive (after "::") counts.
		{"synty/POLYGON_Icons/POLYGON_Icons_Unity_2022_3_v1_2_1.unitypackage::Assets/Synty/PolygonGeneric/Textures/Alts/Generic_01_A.png", "", "", CategoryTexture},
		// A genuine UI sprite inside an archive still reads as UI from its entry path,
		// even when the pack name carries no UI token.
		{"synty/POLYGON_Kit/POLYGON_Kit.unitypackage::Assets/UI/HUD/health_bar.png", "", "", CategoryUI},
		// The same rule for a loose file, where the pack name is a prefix of the
		// library-relative path rather than something before a "::". An extracted copy
		// must classify as whatever the archive entry beside it classifies as.
		{"synty/POLYGON_Icons/Textures/Generic_01_A.png", "synty", "POLYGON_Icons", CategoryTexture},
		{"synty/INTERFACE_Fantasy_Menus/Textures/Wall_Normal.png", "synty", "INTERFACE_Fantasy_Menus", CategoryTexture},
		// A real UI path under a UI-named pack still reads as UI on its own evidence.
		{"synty/INTERFACE_Fantasy_Menus/Source_Sprites/Branding/SPR_Logo.png", "synty", "INTERFACE_Fantasy_Menus", CategoryUI},
		// A file directly under a vendor dir has no pack; the vendor name must not
		// drive classification either.
		{"icons/Textures/rock_albedo.png", "icons", "", CategoryTexture},
	}
	for _, c := range cases {
		if got := refineImage(c.relPath, c.vendor, c.pack); got != c.want {
			t.Errorf("refineImage(%q, %q, %q) = %s, want %s", c.relPath, c.vendor, c.pack, got, c.want)
		}
	}
}

func TestRefineModel(t *testing.T) {
	cases := []struct {
		relPath      string
		vendor, pack string
		name         string
		want         Category
	}{
		// The pack name carries the signal across every vendor convention: Synty
		// ANIMATION_*, the explosive "RPG Animations" vendor, and kevdev
		// "*_Animations"/"*_Motions". The path inside the pack need not repeat it.
		{"synty/ANIMATION_Sword_Combat/x.zip::SourceFiles/Polygon/A_Block_Loop_Sword.fbx", "synty", "ANIMATION_Sword_Combat", "A_Block_Loop_Sword.fbx", CategoryAnimation},
		{"explosive/RPG Animations GLB-0.1.0/2Hand Sword.glb", "explosive", "RPG Animations GLB-0.1.0", "2Hand Sword.glb", CategoryAnimation},
		{"kevdev/Human_Melee_Animations/x.zip::src/HumanF@Attack1H01_L.fbx", "kevdev", "Human_Melee_Animations", "HumanF@Attack1H01_L.fbx", CategoryAnimation},
		{"kevdev/Human_Basic_Motions/x.zip::src/HumanM@Idle.fbx", "kevdev", "Human_Basic_Motions", "HumanM@Idle.fbx", CategoryAnimation},
		// A vendor that ships a whole engine project puts a container like "Assets" in
		// the pack slot, leaving the signal to a directory deeper inside the pack.
		{"doublel/Assets/DoubleL/FBX_Animations/Base Move/Run/Run_F.fbx", "doublel", "Assets", "Run_F.fbx", CategoryAnimation},
		{"doublel/Assets/DoubleL/FBX_Unreal_Animations/Bow/Bow_Aim.fbx", "doublel", "Assets", "Bow_Aim.fbx", CategoryAnimation},
		// A sibling directory in that same pack holds meshes, not clips.
		{"doublel/Assets/DoubleL/Model/SM_Wep_Sword_03.fbx", "doublel", "Assets", "SM_Wep_Sword_03.fbx", CategoryModel},
		// Reference rig/skeleton/character meshes bundled in an animation pack are
		// not clips: they stay model.
		{"explosive/RPG Animations GLB-0.1.0/RPG-Character-Bones.FBX", "explosive", "RPG Animations GLB-0.1.0", "RPG-Character-Bones.FBX", CategoryModel},
		{"kevdev/Human_Melee_Animations/x.zip::Models/HumanF_Model.fbx", "kevdev", "Human_Melee_Animations", "HumanF_Model.fbx", CategoryModel},
		{"synty/ANIMATION_Base_Locomotion/x.zip::SourceFiles/Animations/TPose/A_TPose_Neut.fbx", "synty", "ANIMATION_Base_Locomotion", "A_TPose_Neut.fbx", CategoryModel},
		// A non-animation pack: models stay models.
		{"synty/POLYGON_Nature/x.zip::SourceFiles/SM_Env_Tree_01.fbx", "synty", "POLYGON_Nature", "SM_Env_Tree_01.fbx", CategoryModel},
		// "Animation"/"Motion" must be a whole token, not an incidental substring, in
		// the pack name and in the path inside it alike.
		{"synty/POLYGON_Automation_Kit/x.zip::SourceFiles/SM_Prop.fbx", "synty", "POLYGON_Automation_Kit", "SM_Prop.fbx", CategoryModel},
		{"synty/POLYGON_Kit/x.zip::SourceFiles/Automation/SM_Prop.fbx", "synty", "POLYGON_Kit", "SM_Prop.fbx", CategoryModel},
		// The filename is not a promotion signal: a prop named for motion is a prop.
		{"synty/POLYGON_Kit/x.zip::SourceFiles/Props/SM_Prop_Motion_Sensor.fbx", "synty", "POLYGON_Kit", "SM_Prop_Motion_Sensor.fbx", CategoryModel},
	}
	for _, c := range cases {
		if got := refineModel(c.relPath, c.vendor, c.pack, c.name); got != c.want {
			t.Errorf("refineModel(%q, %q, %q, %q) = %s, want %s", c.relPath, c.vendor, c.pack, c.name, got, c.want)
		}
	}
}

func TestNewAssetRefinesAnimationCategory(t *testing.T) {
	anim := newAsset(Source{Kind: SourceZip, ArchivePath: "/x.zip", Entry: "SourceFiles/Animations/A_Block_Loop_Sword.fbx"},
		"A_Block_Loop_Sword.fbx", "synty/ANIMATION_Sword_Combat/x.zip::SourceFiles/Animations/A_Block_Loop_Sword.fbx", "synty", "ANIMATION_Sword_Combat", "SourceFiles", 10, "")
	if anim.Category != CategoryAnimation {
		t.Errorf("animation clip category = %s, want animation", anim.Category)
	}
	if anim.Thumb != ThumbFBX {
		t.Errorf("animation fbx thumb = %s, want fbx (still three.js-renderable)", anim.Thumb)
	}

	deep := newAsset(Source{Kind: SourceLoose, FilePath: "/lib/doublel/Assets/DoubleL/FBX_Animations/Base Move/Run/Run_F.fbx"},
		"Run_F.fbx", "doublel/Assets/DoubleL/FBX_Animations/Base Move/Run/Run_F.fbx", "doublel", "Assets", "", 10, "")
	if deep.Category != CategoryAnimation {
		t.Errorf("clip under an animation dir inside a project-shaped pack = %s, want animation", deep.Category)
	}

	skel := newAsset(Source{Kind: SourceLoose, FilePath: "/lib/explosive/RPG Animations GLB-0.1.0/RPG-Character-Bones.FBX"},
		"RPG-Character-Bones.FBX", "explosive/RPG Animations GLB-0.1.0/RPG-Character-Bones.FBX", "explosive", "RPG Animations GLB-0.1.0", "", 10, "")
	if skel.Category != CategoryModel {
		t.Errorf("reference skeleton category = %s, want model", skel.Category)
	}
}

func TestNewAssetRefinesImageCategory(t *testing.T) {
	ui := newAsset(Source{Kind: SourceZip, ArchivePath: "/x.zip", Entry: "Source_Sprites/Icons/ICON_x.png"},
		"ICON_x.png", "synty/INTERFACE_Pack/x.zip::Source_Sprites/Icons/ICON_x.png", "synty", "INTERFACE_Pack", "SourceSprites", 10, "")
	if ui.Category != CategoryUI {
		t.Errorf("ui image category = %s, want ui", ui.Category)
	}
	if ui.Thumb != ThumbImage {
		t.Errorf("ui png thumb = %s, want image (still renderable)", ui.Thumb)
	}

	tex := newAsset(Source{Kind: SourceZip, ArchivePath: "/x.zip", Entry: "Textures/Wall_Normal.png"},
		"Wall_Normal.png", "synty/POLYGON_Pack/x.zip::Textures/Wall_Normal.png", "synty", "POLYGON_Pack", "SourceFiles", 10, "")
	if tex.Category != CategoryTexture {
		t.Errorf("texture image category = %s, want texture", tex.Category)
	}
}

func TestNewAssetDerivesFields(t *testing.T) {
	// A loose fbx: model category, fbx thumb, copyPath is the absolute file path.
	a := newAsset(Source{Kind: SourceLoose, FilePath: "/lib/synty/Pack/Foo.FBX"},
		"Foo.FBX", "synty/Pack/Foo.FBX", "synty", "Pack", "SourceFiles", 123, "")
	if a.Category != CategoryModel || a.Thumb != ThumbFBX {
		t.Errorf("category/thumb = %s/%s", a.Category, a.Thumb)
	}
	if a.Ext != "fbx" {
		t.Errorf("ext = %q, want fbx (lowercased)", a.Ext)
	}
	if a.CopyPath != "/lib/synty/Pack/Foo.FBX" {
		t.Errorf("copyPath = %q", a.CopyPath)
	}
	if a.ID == "" {
		t.Error("empty id")
	}

	// A unitypackage fbx WITH a preview overrides the thumbnail to preview.
	b := newAsset(Source{Kind: SourceUnityPackage, ArchivePath: "/lib/p.unitypackage", Guid: "g1", Pathname: "Assets/Foo.fbx", HasPreview: true},
		"Foo.fbx", "synty/Pack/p.unitypackage::Assets/Foo.fbx", "synty", "Pack", "Unity_2022_3", 9, "")
	if b.Thumb != ThumbPreview {
		t.Errorf("unity+preview thumb = %s, want preview", b.Thumb)
	}
	if b.CopyPath != "/lib/p.unitypackage::Assets/Foo.fbx" {
		t.Errorf("unity copyPath = %q", b.CopyPath)
	}
}

func TestIDStableAndDistinct(t *testing.T) {
	s1 := Source{Kind: SourceZip, ArchivePath: "/a.zip", Entry: "x/Foo.fbx"}
	s2 := Source{Kind: SourceZip, ArchivePath: "/a.zip", Entry: "y/Foo.fbx"}
	if id(s1) != id(s1) {
		t.Error("id not stable")
	}
	if id(s1) == id(s2) {
		t.Error("distinct entries share an id")
	}
}
