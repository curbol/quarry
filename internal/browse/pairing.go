package browse

import (
	"path"
	"strings"

	"github.com/curbol/quarry/internal/assetindex"
)

// Root-motion pairing collapses each animation that ships in two variants — one with
// world travel baked into the root (a root-motion file) and one that animates in place —
// into a single card. The in-place variant is the visible card; the browse lightbox's
// root-motion toggle loads the RM sibling to show the travel. Which file base names are
// root-motion variants is decided by assetindex.RootMotionVariant, the shared recognizer.

// assetFileBase is the extension-less base name of the file an asset lives in (the
// archive entry, unity pathname, or loose path), where the root-motion token appears.
func assetFileBase(s assetindex.Source) string {
	name := path.Base(s.EntryPath())
	return strings.TrimSuffix(name, path.Ext(name))
}

// buildRootMotionPairs maps each in-place animation asset to its root-motion sibling
// (sibling: assetID -> RM assetID) and marks the RM assets that a non-RM sibling
// covers for suppression from the grid (suppressed: RM assetID -> true). Assets are
// grouped by (vendor, pack, canonical file base); a group with both variants pairs
// only when its visible side includes an animation, so an unrelated "_RM" file never
// hijacks a card. For each in-place asset the RM with the same clip is preferred, then
// a whole-file RM (the lightbox plays the in-place asset's clip name from it).
func buildRootMotionPairs(assets []assetindex.Asset) (sibling map[string]string, suppressed map[string]bool) {
	type group struct{ nonRM, rm []int }
	groups := map[string]*group{}
	for i := range assets {
		canon, isRM := assetindex.RootMotionVariant(assetFileBase(assets[i].Source))
		key := assets[i].Vendor + "\x00" + assets[i].Pack + "\x00" + canon
		g := groups[key]
		if g == nil {
			g = &group{}
			groups[key] = g
		}
		if isRM {
			g.rm = append(g.rm, i)
		} else {
			g.nonRM = append(g.nonRM, i)
		}
	}

	sibling = map[string]string{}
	suppressed = map[string]bool{}
	for _, g := range groups {
		if len(g.rm) == 0 || len(g.nonRM) == 0 {
			continue
		}
		// Suppress only the RM files some in-place card actually plays. pickRM picks one
		// per card (preferring the same container), so hiding the whole group would make
		// an RM with no in-place counterpart in its own format unreachable in browse
		// even though the file is right there on disk.
		//
		// Only animations pair. A group can hold more than one kind — a pack shipping
		// Sword.fbx beside Sword.png, whose roughness-metallic map is Sword_RM.png — and
		// testing the group as a whole would let the animation's presence hide a texture
		// nothing will ever play.
		for _, ni := range g.nonRM {
			if assets[ni].Category != assetindex.CategoryAnimation {
				continue
			}
			if rmID := pickRM(assets, g.rm, assets[ni]); rmID != "" {
				sibling[assets[ni].ID] = rmID
				suppressed[rmID] = true
			}
		}
	}
	return sibling, suppressed
}

// pickRM chooses the RM sibling for an in-place asset. The sibling has to be the same
// container format: a glb clip's travel is the glb RM, not the fbx RM of the same
// library shipped in the same pack, and loading the wrong one fails. That is a
// requirement rather than a preference — a pack that ships only the other format has
// no sibling to offer, and pairing it anyway would both break the toggle and hide a
// file the grid should still show.
//
// Among same-format candidates, the same archive wins over another, then the same
// clip over a whole-file RM. The archive term matters because Pack is a directory
// name: one pack commonly ships as both a SourceFiles zip and a unitypackage holding
// the same animations, which lands both copies in one group. Without it every in-place
// card in that group picks the same first RM — so the other archive's RM is never
// suppressed and shows up beside the card it belongs to, while that card's toggle
// fetches a different archive than the one it is displaying.
func pickRM(assets []assetindex.Asset, rm []int, nonRM assetindex.Asset) string {
	best, bestScore := "", -1
	for _, ri := range rm {
		r := assets[ri]
		if r.Ext != nonRM.Ext {
			continue
		}
		score := 0
		if r.Source.ArchivePath == nonRM.Source.ArchivePath {
			score += 2
		}
		if r.Source.Clip == nonRM.Source.Clip {
			score++
		}
		if score > bestScore {
			best, bestScore = r.ID, score
		}
	}
	return best
}
