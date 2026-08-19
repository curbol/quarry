package assetindex

import (
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
// a clip inside an animation pack. The pack name is the signal (Synty ships every
// clip as a .fbx, byte-indistinguishable by extension from a static mesh), and the
// reference rig/character mesh each pack bundles is excluded by filename so only the
// clips are reclassified.
func refineModel(pack, name string) Category {
	if !animPackRe.MatchString(strings.ToLower(pack)) {
		return CategoryModel
	}
	if rigRefRe.MatchString(strings.ToLower(name)) {
		return CategoryModel
	}
	return CategoryAnimation
}

// refineImage narrows a file already classified as an image to ui, texture, or plain
// image using its path. UI containers win (a HUD sprite is UI even if the pack also
// ships textures), then texture folders and material-map suffixes, else a plain image.
//
// It matches only the path within the pack — after "::" for an archive entry, after
// the vendor/pack prefix for a loose file — never the pack or archive name: a pack
// called "POLYGON_Icons" must not make its textures read as UI. Matching a loose
// file's whole library-relative path would do exactly that, and would also classify
// it differently from the byte-identical copy inside the pack's own archive.
func refineImage(relPath, vendor, pack string) Category {
	p := relPath
	if i := strings.LastIndex(p, "::"); i >= 0 {
		p = p[i+len("::"):]
	} else {
		p = packSubpath(p, vendor, pack)
	}
	p = strings.ToLower(p)
	switch {
	case uiTokenRe.MatchString(p):
		return CategoryUI
	case texFolderRe.MatchString(p) || texSuffixRe.MatchString(p):
		return CategoryTexture
	default:
		return CategoryImage
	}
}
