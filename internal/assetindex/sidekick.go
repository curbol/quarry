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

// maxSidekickBytes bounds a .sk read. A character definition is a name and a short
// list of part names; anything approaching this is not one, and the .sk here was
// picked out by extension alone from an archive this tool did not produce.
const maxSidekickBytes = 256 << 10

// partMesh is one candidate FBX for a .sk part name. reusable marks the ones sitting
// where a package keeps its shared body parts, which is what settles a base name two
// meshes answer to.
type partMesh struct {
	id       string
	reusable bool
}

// isPartTree reports a mesh sitting where a Sidekick package keeps its reusable body
// parts, rather than in a demo or showcase folder that happens to name one the same.
func isPartTree(pathname string) bool {
	return strings.Contains(pathname, "/Resources/")
}

// applySidekick upgrades the .sk entries of a Synty Sidekick modular-character package
// into assembled-character assets. Each .sk lists the body parts of one character; those
// parts are separate FBX meshes in the same package that share one skeleton. The upgraded
// asset takes the character's name, renders as ThumbSidekick, and carries the parts' ids
// so the frontend can load and merge them. Its own id and fingerprint stay tied to the
// .sk guid, so tags survive. Packages with no .sk entry are returned untouched (the
// common case, so the extra decompress pass only runs for Sidekick packs).
func applySidekick(archivePath string, assets []Asset) ([]Asset, *SkippedFile) {
	// Driven off the surviving assets, not the raw enumeration: a part resolved from a
	// guid the enumeration already dropped (no payload, unsafe path, a sidecar) would
	// name an id the index cannot serve, and the frontend would silently render the
	// character short a limb.
	skGuids := map[string]bool{}
	fbxByBase := map[string]partMesh{}
	for i := range assets {
		a := &assets[i]
		if a.Source.Kind != SourceUnityPackage {
			continue
		}
		switch a.Ext {
		case "sk":
			skGuids[a.Source.Guid] = true
		case "fbx":
			// A part name is matched on the base name alone, which a demo or showcase
			// mesh elsewhere in the package can also carry. The parts tree wins, so the
			// character is assembled from the meshes meant to be worn rather than from
			// whichever copy the enumeration reached last.
			base := strings.TrimSuffix(a.Name, path.Ext(a.Name))
			cand := partMesh{id: a.ID, reusable: isPartTree(a.Source.Pathname)}
			if prev, taken := fbxByBase[base]; !taken || (cand.reusable && !prev.reusable) {
				fbxByBase[base] = cand
			}
		}
	}
	if len(skGuids) == 0 {
		return assets, nil
	}
	skBytes, err := readUnityAssetBytes(archivePath, skGuids, maxSidekickBytes)
	if err != nil {
		// Assembly is a vendor-specific second pass over a package the vendor-neutral
		// first pass already enumerated. Losing it costs the character cards, not the
		// several thousand assets the package otherwise contributes — so the assets
		// are kept and the failure is reported, never absorbed. Reporting it is what
		// keeps the caller from caching this enumeration against the archive's stat
		// print: the print describes the file, not whether the second pass worked.
		return assets, &SkippedFile{Reason: "sidekick characters could not be assembled: " + err.Error()}
	}
	// Every .sk in the package, assembled or not. Built by walking the assets rather
	// than the guid set, so the order the claims are compared in is the scan's, not a
	// map's.
	chars := make([]sidekickChar, 0, len(skGuids))
	for i := range assets {
		a := &assets[i]
		if a.Source.Kind != SourceUnityPackage || a.Ext != "sk" {
			continue
		}
		p := a.Source.Pathname
		chars = append(chars, sidekickChar{
			tree: path.Dir(p) + "/",
			base: strings.TrimSuffix(path.Base(p), path.Ext(p)),
		})
		// The claim is appended before the upgrade is attempted, and stands whether or
		// not it succeeds: an unassembled character still has to hold its own name, or
		// the assembled character whose name prefixes it takes the byproducts that are
		// the unassembled one's only representation. sidekickByproduct runs after this
		// loop, over the finished slice, so nothing here reads another character's claim.
		claim := len(chars) - 1
		data, ok := skBytes[a.Source.Guid]
		if !ok {
			continue
		}
		name, partNames := parseSidekick(data)
		var partIDs []string
		for _, pn := range partNames {
			if m, ok := fbxByBase[pn]; ok {
				partIDs = append(partIDs, m.id)
			}
		}
		if len(partIDs) == 0 {
			continue
		}
		if name != "" {
			a.Name = name
		}
		a.Category = CategoryModel
		a.Thumb = ThumbSidekick
		a.Source.Parts = partIDs
		// Only a character every one of whose parts resolved supersedes its byproducts.
		// A .sk naming a part this package does not hold assembles into a torso and one
		// hand, and the prefab and combined mesh it would otherwise drop are the rows
		// that still show the whole character — the same outcome readUnityAssetBytes
		// refuses a truncated read to avoid.
		if len(partIDs) == len(partNames) {
			chars[claim].assembled = true
		}
	}
	kept := assets[:0]
	for _, a := range assets {
		if !sidekickByproduct(a, chars) {
			kept = append(kept, a)
		}
	}
	return kept, nil
}

// sidekickChar is one character's suppression scope: the directory its .sk sits in,
// the .sk's own base name, which every byproduct of that character is named after,
// and whether the character actually assembled. Unassembled characters are carried
// too, because a name only claims its own byproducts by being the longest one that
// matches them.
type sidekickChar struct {
	tree, base string
	assembled  bool
}

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
func sidekickByproduct(a Asset, chars []sidekickChar) bool {
	if a.Thumb == ThumbSidekick {
		return false
	}
	switch a.Ext {
	case "prefab", "mat", "asset":
	default:
		return false
	}
	// The longest matching name wins, and only then does assembly decide. Synty joins
	// a byproduct's suffix to the character name with the same "_" the names
	// themselves contain, so "Hero" matching "Hero_Alt.prefab" is indistinguishable
	// from "Hero" matching "Hero_CombinedMesh.asset" on separators alone. What tells
	// them apart is that "Hero_Alt" is itself a character in the package: it is the
	// longer claim on that file, so the file is its byproduct, not Hero's.
	stem := strings.TrimSuffix(a.Name, path.Ext(a.Name))
	best := -1
	for i, c := range chars {
		if !strings.HasPrefix(a.Source.Pathname, c.tree) || !namedFor(stem, c.base) {
			continue
		}
		if best < 0 || c.claimsOver(chars[best]) {
			best = i
		}
	}
	return best >= 0 && chars[best].assembled
}

// claimsOver reports whether c is the closer claim on a byproduct both characters
// match. The longer name wins; between two of the same length the one whose .sk sits
// deeper does, so "S/Sub/Hero.sk" keeps the files beside it from "S/Hero.sk". Two
// characters cannot tie on both, since equal-length names that are each a prefix of
// one stem are the same name, and equal-length trees that both prefix one path are
// the same tree.
func (c sidekickChar) claimsOver(best sidekickChar) bool {
	if len(c.base) != len(best.base) {
		return len(c.base) > len(best.base)
	}
	return len(c.tree) > len(best.tree)
}

// namedFor reports whether stem names a byproduct of base: base itself, or base
// followed by a separator. Requiring the separator is what stops "Base" from
// claiming "BaseSkeleton".
func namedFor(stem, base string) bool {
	if stem == base {
		return true
	}
	if !strings.HasPrefix(stem, base) {
		return false
	}
	c := stem[len(base)]
	return !isAlnum(c)
}

func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
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
		// limit+1 so an over-long member is recognised rather than silently truncated:
		// a short read would assemble a character missing the limbs the truncated tail
		// named, and still suppress the byproducts that would have shown it.
		b, err := io.ReadAll(io.LimitReader(tr, limit+1))
		if err != nil {
			return err
		}
		if int64(len(b)) > limit {
			return nil
		}
		out[guid] = b
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
