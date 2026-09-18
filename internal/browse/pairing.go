package browse

import (
	"os"
	"path"
	"strings"

	"github.com/curbol/quarry/internal/assetindex"
)

// Root-motion pairing collapses each animation that ships in two variants — one with
// world travel baked into the root (a root-motion file) and one that animates in place —
// into a single card. The in-place variant is the visible card; the browse lightbox's
// root-motion toggle loads the RM sibling to show the travel. Which file base names are
// root-motion variants is decided by assetindex.RootMotionVariant, the shared recognizer.

// osSeparators is what divides one path element from the next on this platform. A
// loose file's path is the filesystem's own, so it is split by these; an archive
// entry is split by "/" whatever the host is, because that is what the format stores.
// Naming the set rather than reaching for filepath is what lets splitEntry's Windows
// behaviour be reached from a test on a Unix host, where a backslash is an ordinary
// filename character and filepath would leave it alone.
var osSeparators = separatorsFor(os.PathSeparator)

func separatorsFor(sep rune) string {
	if sep == '\\' {
		return `/\`
	}
	return "/"
}

// splitEntry divides a path into the directory holding it and its base name, exactly
// as path.Split does but over a chosen separator set. dir is "" for a path with no
// separator in it, so two such paths compare equal — they are in the same place.
func splitEntry(p, seps string) (dir, base string) {
	if i := strings.LastIndexAny(p, seps); i >= 0 {
		return p[:i], p[i+1:]
	}
	return "", p
}

// entryParts splits where an asset lives into its directory and its base name. Which
// characters separate the two follows from where the path came from rather than from
// the host: a zip entry and a unity pathname are slash-delimited by their formats,
// while a loose file's path arrives with backslashes on Windows. Reading a backslash
// path with "/" alone left the whole path in the base name and every directory equal
// to "", so cross-directory siblings never paired and the directory preference fired
// for every candidate at once.
func entryParts(s assetindex.Source) (dir, base string) {
	seps := "/"
	if s.Kind == assetindex.SourceLoose {
		seps = osSeparators
	}
	return splitEntry(s.EntryPath(), seps)
}

// assetFileBase is the extension-less base name of the file an asset lives in (the
// archive entry, unity pathname, or loose path), where the root-motion token appears.
func assetFileBase(s assetindex.Source) string {
	_, name := entryParts(s)
	return strings.TrimSuffix(name, path.Ext(name))
}

// buildRootMotionPairs maps each in-place animation asset to its root-motion sibling
// (sibling: assetID -> RM assetID) and marks the RM assets that a non-RM sibling
// covers for suppression from the grid (suppressed: RM assetID -> true). Assets are
// grouped by (vendor, pack, canonical file base); a group with both variants pairs
// only when its visible side includes an animation, so an unrelated "_RM" file never
// hijacks a card. Which RM an in-place asset gets is pickRM's decision: same directory
// first, then same archive. Which clip inside that file plays is not settled here —
// an RM file is never split, so it arrives whole and the frontend matches the clip.
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
		// Whether the directory is decisive is a property of the group, not of the card
		// being paired. A pack laid out per character keeps each clip beside its own RM,
		// and there the directory is the only thing telling one character's "Walk" from
		// another's — so a card whose own directory ships no RM has no sibling, rather
		// than the neighbouring character's. A pack that puts every RM in one folder has
		// no such pair anywhere in the group, and there the unrestricted weighting is
		// what makes the layout pair at all.
		//
		// Asked per container format, because that is the granularity pickRM chooses at:
		// one pack can ship its FBX copies beside their RM and split its GLB copies
		// across folders, and a group-wide answer lets the FBX pair — which the GLB
		// clips can never select — decide that the GLB ones have no sibling.
		sameDirFor := map[string]bool{}
		for _, ni := range g.nonRM {
			a := assets[ni]
			if a.Category != assetindex.CategoryAnimation {
				continue
			}
			sameDir, asked := sameDirFor[a.Ext]
			if !asked {
				sameDir = groupPairsByDirectory(assets, g.nonRM, g.rm, a.Ext)
				sameDirFor[a.Ext] = sameDir
			}
			if rmID := pickRM(assets, g.rm, a, sameDir); rmID != "" {
				sibling[a.ID] = rmID
				suppressed[rmID] = true
			}
		}
	}
	return sibling, suppressed
}

// groupPairsByDirectory reports whether any in-place asset in the group has an RM in
// its own directory, over the pairs that could actually be made in container format
// ext. See buildRootMotionPairs for why that is decided per group and per format.
//
// Both sides are narrowed to what pickRM would consider: an RM of another extension is
// never selected, and a non-animation is never paired at all, so counting either would
// let a pair nobody can make decide that a pair somebody can make is cross-directory.
func groupPairsByDirectory(assets []assetindex.Asset, nonRM, rm []int, ext string) bool {
	dirs := make(map[string]bool, len(rm))
	for _, ri := range rm {
		if assets[ri].Ext != ext {
			continue
		}
		d, _ := entryParts(assets[ri].Source)
		dirs[d] = true
	}
	for _, ni := range nonRM {
		a := assets[ni]
		if a.Ext != ext || a.Category != assetindex.CategoryAnimation {
			continue
		}
		if d, _ := entryParts(a.Source); dirs[d] {
			return true
		}
	}
	return false
}

// dirAffinity counts the trailing path segments two directories share. It is what
// separates candidates the same-directory filter cannot: a pack that mirrors its
// per-character folders under one root-motion tree has no in-place asset in an RM's
// own directory, so sameDirOnly is false and every candidate in the archive scores
// alike — leaving scan order to decide which character's travel each card plays.
// Anims/Goblin shares one segment with RootMotion/Goblin and none with RootMotion/Orc.
func dirAffinity(a, b assetindex.Source) int {
	ad, _ := entryParts(a)
	bd, _ := entryParts(b)
	as, bs := splitDir(a, ad), splitDir(b, bd)
	n := 0
	for n < len(as) && n < len(bs) && as[len(as)-1-n] == bs[len(bs)-1-n] {
		n++
	}
	return n
}

// splitDir breaks a directory into its segments, by the separators that source's own
// path uses. Empty segments are dropped so a leading or doubled separator cannot
// register as a shared one.
func splitDir(s assetindex.Source, dir string) []string {
	seps := "/"
	if s.Kind == assetindex.SourceLoose {
		seps = osSeparators
	}
	var out []string
	for _, p := range strings.FieldsFunc(dir, func(r rune) bool { return strings.ContainsRune(seps, r) }) {
		out = append(out, p)
	}
	return out
}

// pickRM chooses the RM sibling for an in-place asset. The sibling has to be the same
// container format: a glb clip's travel is the glb RM, not the fbx RM of the same
// library shipped in the same pack, and loading the wrong one fails. That is a
// requirement rather than a preference — a pack that ships only the other format has
// no sibling to offer, and pairing it anyway would both break the toggle and hide a
// file the grid should still show.
//
// The directory outranks the archive, and does so as a filter rather than a weight.
// sameDirOnly says the group has some in-place asset with an RM beside it, and in a
// pack laid out per character the directory is the only thing telling one character's
// "Walk" from another's — so a same-directory RM in another archive beats a
// different-directory RM in this one, and a character whose own folder ships no RM
// gets none rather than a neighbour's. Ranked instead of filtered, the neighbour's
// would win by default and the frontend would play it on this character's body: a
// plausible clip, out of a file that loads, with nothing to signal it. Where the layout
// puts every RM in one folder no candidate is in the card's own directory anyway,
// sameDirOnly is false, and the archive alone decides.
//
// The archive matters because Pack is a directory name: one pack commonly ships as both
// a SourceFiles zip and a unitypackage holding the same animations, which lands both
// copies in one group. Without it every in-place card in that group picks the same first
// RM — so the other archive's RM is never suppressed and shows up beside the card it
// belongs to, while that card's toggle fetches a different archive than the one it is
// displaying.
//
// Where the filter does not apply, how much of the path the two share outranks the
// archive: a mirrored root-motion tree puts no RM in any card's own directory, and with
// the archive alone every candidate ties and the first one found wins for every card in
// the group. The score packs the two terms so affinity dominates and the archive breaks
// its ties, rather than the two being summed into a tie again.
func pickRM(assets []assetindex.Asset, rm []int, nonRM assetindex.Asset, sameDirOnly bool) string {
	best, bestScore := "", -1
	nonDir, _ := entryParts(nonRM.Source)
	for _, ri := range rm {
		r := assets[ri]
		if r.Ext != nonRM.Ext {
			continue
		}
		if rDir, _ := entryParts(r.Source); sameDirOnly && rDir != nonDir {
			continue
		}
		score := dirAffinity(nonRM.Source, r.Source) << 1
		if r.Source.ArchivePath == nonRM.Source.ArchivePath {
			score++
		}
		if score > bestScore {
			best, bestScore = r.ID, score
		}
	}
	return best
}
