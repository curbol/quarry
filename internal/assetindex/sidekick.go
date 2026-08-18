package assetindex

import (
	"archive/tar"
	"io"
	"path"
	"strings"
)

// parseSidekick reads a Synty Sidekick character definition (.sk): a top-level
// "Name:" naming the character and a top-level "Parts:" block of "- Name:" entries
// naming the body-part meshes it assembles. Only the Parts block is collected — a
// later top-level key (ColorSet, BlendShapes) ends it, so a "Name" nested under one
// of those never leaks in. The format is YAML-shaped but shallow, so a line scanner
// suffices and avoids a YAML dependency.
func parseSidekick(data []byte) (name string, parts []string) {
	inParts := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, "\r")
		if line == "" {
			continue
		}
		if c := line[0]; c == ' ' || c == '\t' || c == '-' {
			if inParts {
				if t := strings.TrimSpace(line); strings.HasPrefix(t, "- Name:") {
					parts = append(parts, strings.TrimSpace(t[len("- Name:"):]))
				}
			}
			continue
		}
		// A top-level key ends any open Parts block, then may open a new one.
		inParts = false
		switch {
		case name == "" && strings.HasPrefix(line, "Name:"):
			name = strings.TrimSpace(line[len("Name:"):])
		case strings.TrimSpace(line) == "Parts:":
			inParts = true
		}
	}
	return name, parts
}

// applySidekick upgrades the .sk entries of a Synty Sidekick modular-character package
// into assembled-character assets. Each .sk lists the body parts of one character; those
// parts are separate FBX meshes in the same package that share one skeleton. The upgraded
// asset takes the character's name, renders as ThumbSidekick, and carries the parts' ids
// so the frontend can load and merge them. Its own id and fingerprint stay tied to the
// .sk guid, so tags survive. Packages with no .sk entry are returned untouched (the
// common case, so the extra decompress pass only runs for Sidekick packs).
// maxSidekickBytes bounds a .sk read. A character definition is a name and a short
// list of part names; anything approaching this is not one, and the .sk here was
// picked out by extension alone from an archive this tool did not produce.
const maxSidekickBytes = 256 << 10

func applySidekick(archivePath string, assets []Asset) []Asset {
	// Driven off the surviving assets, not the raw enumeration: a part resolved from a
	// guid the enumeration already dropped (no payload, unsafe path, a sidecar) would
	// name an id the index cannot serve, and the frontend would silently render the
	// character short a limb.
	skGuids := map[string]bool{}
	fbxByBase := map[string]string{}
	byGuid := make(map[string]int, len(assets))
	for i := range assets {
		a := &assets[i]
		if a.Source.Kind != SourceUnityPackage {
			continue
		}
		byGuid[a.Source.Guid] = i
		switch a.Ext {
		case "sk":
			skGuids[a.Source.Guid] = true
		case "fbx":
			fbxByBase[strings.TrimSuffix(a.Name, path.Ext(a.Name))] = a.ID
		}
	}
	if len(skGuids) == 0 {
		return assets
	}
	skBytes, err := readUnityAssetBytes(archivePath, skGuids, maxSidekickBytes)
	if err != nil {
		// Assembly is a vendor-specific second pass over a package the vendor-neutral
		// first pass already enumerated. Losing it costs the character cards, not the
		// several thousand assets the package otherwise contributes.
		return assets
	}
	// Only a character that actually assembled supersedes its byproducts; the
	// characters collected here bound the suppression below.
	var assembled []sidekickChar
	for g := range skGuids {
		data, ok := skBytes[g]
		if !ok {
			continue
		}
		name, partNames := parseSidekick(data)
		var partIDs []string
		for _, pn := range partNames {
			if partID, ok := fbxByBase[pn]; ok {
				partIDs = append(partIDs, partID)
			}
		}
		i, ok := byGuid[g]
		if !ok || len(partIDs) == 0 {
			continue
		}
		p := assets[i].Source.Pathname
		if name != "" {
			assets[i].Name = name
		}
		assets[i].Category = CategoryModel
		assets[i].Thumb = ThumbSidekick
		assets[i].Source.Parts = partIDs
		assembled = append(assembled, sidekickChar{
			tree: path.Dir(p) + "/",
			base: strings.TrimSuffix(path.Base(p), path.Ext(p)),
		})
	}
	kept := assets[:0]
	for _, a := range assets {
		if !sidekickByproduct(a, assembled) {
			kept = append(kept, a)
		}
	}
	return kept
}

// sidekickChar is one assembled character's suppression scope: the directory its
// .sk sits in, and the .sk's own base name, which every byproduct of that character
// is named after.
type sidekickChar struct{ tree, base string }

// sidekickByproduct reports a per-character byproduct that its assembled .sk
// character supersedes: the magenta prefab, its material, and the combined-mesh /
// avatar .asset data, which sit beside the .sk (or under its Materials/ and Meshes/
// subdirs) and carry its name. The reusable part meshes live under Resources/ and
// the character's textures are a kept extension, so both stay browseable. A
// character that failed to assemble contributes nothing here, so its byproducts
// survive — they are the only representation it has left.
//
// Matching the name as well as the directory is what keeps that last promise true:
// two characters commonly share a directory, and a .sk exported to the top of a
// package would otherwise claim every prefab and material in it.
func sidekickByproduct(a Asset, assembled []sidekickChar) bool {
	if a.Thumb == ThumbSidekick {
		return false
	}
	switch a.Ext {
	case "prefab", "mat", "asset":
	default:
		return false
	}
	for _, c := range assembled {
		if strings.HasPrefix(a.Source.Pathname, c.tree) && strings.HasPrefix(a.Name, c.base) {
			return true
		}
	}
	return false
}

// readUnityAssetBytes streams a .unitypackage once and returns the `asset` payload of
// each requested GUID, truncated at limit bytes. Used for the few small .sk files a
// Sidekick package holds, whose full bytes the enumeration pass (which buffers only a
// head) doesn't retain. The limit is the caller's contract with an untrusted archive:
// a member is selected by extension, so an unrelated large file that happens to carry
// one must not be read into memory whole.
func readUnityAssetBytes(archivePath string, want map[string]bool, limit int64) (map[string][]byte, error) {
	out := make(map[string][]byte, len(want))
	err := walkUnityTar(archivePath, func(guid, member string, _ *tar.Header, tr *tar.Reader) error {
		if member != "asset" || !want[guid] {
			return nil
		}
		b, err := io.ReadAll(io.LimitReader(tr, limit))
		if err != nil {
			return err
		}
		out[guid] = b
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
