package assetindex

import (
	"path"
	"regexp"
	"strings"
)

// classify maps a lowercased, dotless file extension to its browse category and
// the kind of thumbnail the frontend can render for it. A Unity preview.png, when
// present, overrides the thumbnail to ThumbPreview at scan time (see newAsset).
//
// Unexported because the lowercased-and-dotless part is a precondition with no way to
// report a breach: ".PNG" comes back as CategoryOther with nothing said. newAsset is
// the one caller and derives the extension itself.
func classify(ext string) (Category, ThumbKind) {
	switch ext {
	case "glb", "gltf":
		return CategoryModel, ThumbGLB
	case "fbx":
		return CategoryModel, ThumbFBX
	case "obj", "blend", "dae", "stl", "ply":
		return CategoryModel, ThumbNone

	case "png", "jpg", "jpeg", "gif", "webp", "bmp":
		return CategoryImage, ThumbImage
	case "tga", "psd", "exr", "tif", "tiff", "hdr", "svg":
		return CategoryImage, ThumbNone

	case "mat", "material", "tres", "physicmaterial":
		return CategoryMaterial, ThumbNone

	case "tscn", "scn", "prefab", "unity", "scene":
		return CategoryScene, ThumbNone

	case "anim", "controller", "fbxanim":
		return CategoryAnimation, ThumbNone

	case "wav", "mp3", "ogg", "aiff", "aif", "flac":
		return CategoryAudio, ThumbNone

	case "cs", "gd", "js", "hlsl", "glsl", "shader", "shadergraph", "shadersubgraph", "gdshader", "cginc":
		return CategoryScript, ThumbNone

	case "ttf", "otf", "woff", "woff2":
		return CategoryFont, ThumbFont

	case "asset", "meta", "res", "playable", "terrainlayer", "preset", "lighting", "mesh", "sk":
		return CategoryData, ThumbNone

	case "pdf", "txt", "md", "rtf", "url", "json", "xml", "yaml", "yml", "csv":
		return CategoryDoc, ThumbNone
	}
	return CategoryOther, ThumbNone
}

// UI containers, texture folders, and material-map filename suffixes. Anchored to
// path boundaries (/, _, :) so the archive "::" separator and folder joins both act
// as boundaries, and a substring like the "ui" inside "building" never matches.
var (
	uiTokenRe   = regexp.MustCompile(`(^|[/_:])(ui|hud|gui|interface|menus?|icons?|sprites?|branding|widgets?|cursor|minimap)([/_.:]|$)`)
	texFolderRe = regexp.MustCompile(`(^|[/_:])(textures?|decals?|emissive|normals?)([/:]|$)`)
	texSuffixRe = regexp.MustCompile(`_(albedo|basecolor|diffuse|normals?|metallic(smoothness)?|roughness|specular|emissive|emission|occlusion|ao|height|orm|gloss|opacity|mask|texture)([._]|$|[0-9])`)
)

// An animation pack (Synty ANIMATION_*, explosive "RPG Animations", kevdev
// "*_Animations"/"*_Motions"), and the reference rig/character meshes such packs
// bundle alongside their clips. Both anchored to path/word boundaries so a substring
// like "animation" inside "Automation" never matches.
var (
	animPackRe = regexp.MustCompile(`(^|[/_ -])(animations?|motions?)([/_ .-]|$)`)
	rigRefRe   = regexp.MustCompile(`(^|[/_ -])(bones|skeleton|skel|rig|armature|tpose|reference|model)([/_ .-]|$)`)
)

// refineModel promotes a file already classified as a model to animation when it is
// a clip inside an animation pack. Some signal beyond the extension is needed because
// Synty ships every clip as a .fbx, byte-indistinguishable from a static mesh. The
// pack name carries it for a pack released as itself; a vendor that ships a whole
// engine project instead puts a container like "Assets" in the pack slot, leaving a
// directory further inside the pack to carry it. The reference rig/character mesh
// each pack bundles is excluded by filename so only the clips are reclassified.
//
// The filename is deliberately not a promotion signal, only an exclusion one: a prop
// named for motion is a prop, and no vendor here names a clip for the pack it is in.
func refineModel(relPath, vendor, pack, name string) Category {
	dir := path.Dir(withinPack(relPath, vendor, pack))
	if !animPackRe.MatchString(strings.ToLower(pack)) && !animPackRe.MatchString(strings.ToLower(dir)) {
		return CategoryModel
	}
	if rigRefRe.MatchString(strings.ToLower(name)) {
		return CategoryModel
	}
	return CategoryAnimation
}

// withinPack returns the path inside the pack: the part after "::" for an archive
// entry, the library-relative path minus the vendor/pack prefix for a loose file.
// The two forms have to agree, since an extracted file and the archive entry it came
// from are the same asset and must classify the same way.
func withinPack(relPath, vendor, pack string) string {
	if i := strings.LastIndex(relPath, "::"); i >= 0 {
		return relPath[i+len("::"):]
	}
	return packSubpath(relPath, vendor, pack)
}

// refineImage narrows a file already classified as an image to ui, texture, or plain
// image using its path. UI containers win (a HUD sprite is UI even if the pack also
// ships textures), then texture folders and material-map suffixes, else a plain image.
//
// It matches only the path within the pack, never the pack or archive name: a pack
// called "POLYGON_Icons" must not make its textures read as UI.
func refineImage(relPath, vendor, pack string) Category {
	p := strings.ToLower(withinPack(relPath, vendor, pack))
	switch {
	case uiTokenRe.MatchString(p):
		return CategoryUI
	case texFolderRe.MatchString(p) || texSuffixRe.MatchString(p):
		return CategoryTexture
	default:
		return CategoryImage
	}
}
