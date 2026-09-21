package browse

import (
	"os"
	"path"
	"slices"
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
// hijacks a card.
//
// Which RM an in-place asset gets is settled in two stages, and a card can come out of
// them with none. Directory is a filter rather than a rank: groupPairsByDirectory asks,
// per container format inside one archive, whether any card in the group has an RM in
// its own directory, and if so a card whose directory ships none gets nothing rather
// than a neighbour's. Where that does not engage, pickRM ranks directory affinity —
// shared trailing segments — above same-archive, and bestClaim holds each candidate to
// the best affinity any card in the group reaches with it, since ranking alone still
// hands the last remaining candidate to a card it does not belong to.
//
// Which clip inside the chosen file plays is not settled here — an RM file is never
// split, so it arrives whole and the frontend matches the clip.
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
		// Only animations pair (see below), so a group holding none has nothing to decide.
		// Hoisted to a guard because everything past here is built per group rather than
		// per card, and would otherwise be built for a group that never reaches pickRM.
		if !slices.ContainsFunc(g.nonRM, func(ni int) bool {
			return assets[ni].Category == assetindex.CategoryAnimation
		}) {
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
		// Asked per container format and per archive, because that is the granularity
		// pickRM chooses at: one pack can ship its FBX copies beside their RM and split
		// its GLB copies across folders, and a group-wide answer lets the FBX pair —
		// which the GLB clips can never select — decide that the GLB ones have no
		// sibling. The archive is the same argument one step further in: one pack
		// commonly ships as both a SourceFiles zip and a unitypackage whose internal
		// layouts differ, and an answer read across both lets the one that keeps its RM
		// beside the clip decide that the one that does not has no sibling at all.
		sameDirFor := map[probeKey]bool{}
		// What a candidate is claimed at is a property of the group and the candidate, not
		// of the card being paired, so it is answered once per candidate here. Asked inside
		// pickRM it was re-walked for every (card, candidate) pair, and each walk compares
		// directory affinity, which splits two paths into fresh slices per comparison —
		// cubic in the group over exactly the mirrored-tree layout bestClaim exists for.
		claims := make(map[int]int, len(g.rm))
		for _, ri := range g.rm {
			claims[ri] = bestClaim(assets, g.nonRM, assets[ri])
		}
		for _, ni := range g.nonRM {
			a := assets[ni]
			if a.Category != assetindex.CategoryAnimation {
				continue
			}
			k := probeKey{ext: a.Ext, archive: a.Source.ArchivePath}
			sameDir, asked := sameDirFor[k]
			if !asked {
				sameDir = groupPairsByDirectory(assets, g.nonRM, g.rm, k)
				sameDirFor[k] = sameDir
			}
			if rmID := pickRM(assets, g.rm, claims, a, sameDir); rmID != "" {
				sibling[a.ID] = rmID
				suppressed[rmID] = true
			}
		}
	}
	return sibling, suppressed
}

// probeKey is the scope the same-directory question is asked over: one container format
// inside one archive. The format half is also a hard filter in pickRM; the archive half
// is not — there it is only the low bit of the score, so a better-placed RM in another
// archive still wins (see TestPickRMRanksDirectoryAboveArchive). The question is asked
// per archive anyway because the layout it asks about is a property of one archive's
// internal tree: one pack commonly ships as both a SourceFiles zip and a unitypackage
// whose trees differ, and an answer read across both lets the one that keeps its RM
// beside the clip decide that the one that does not has no sibling at all.
type probeKey struct{ ext, archive string }

// holds reports whether a is inside the scope k.
func (k probeKey) holds(a assetindex.Asset) bool {
	return a.Ext == k.ext && a.Source.ArchivePath == k.archive
}

// groupPairsByDirectory reports whether any in-place asset in the group has an RM in
// its own directory, over the pairs that could actually be made inside scope k. See
// buildRootMotionPairs for why that is decided per group, per format and per archive.
//
// Both sides are narrowed to the scope whose layout is being asked about, which is also
// what pickRM would consider: an RM of another extension is never selected at all, a
// non-animation is never paired, and an RM in another archive answers a different
// archive's layout question. Counting any of them would let a pair nobody can make, or
// one made under another tree's rules, decide that a pair somebody can make is
// cross-directory.
func groupPairsByDirectory(assets []assetindex.Asset, nonRM, rm []int, k probeKey) bool {
	dirs := make(map[string]bool, len(rm))
	for _, ri := range rm {
		if !k.holds(assets[ri]) {
			continue
		}
		d, _ := entryParts(assets[ri].Source)
		dirs[d] = true
	}
	for _, ni := range nonRM {
		a := assets[ni]
		if !k.holds(a) || a.Category != assetindex.CategoryAnimation {
			continue
		}
		if d, _ := entryParts(a.Source); dirs[d] {
			return true
		}
	}
	return false
}

// dirAffinity scores how closely two directories are related, for the candidates the
// same-directory filter cannot separate: a pack that mirrors its per-character folders
// under one root-motion tree has no in-place asset in an RM's own directory, so
// sameDirOnly is false and without this every candidate in the archive scores alike —
// leaving scan order to decide which character's travel each card plays.
//
// Shared trailing segments first: Anims/Goblin shares one with RootMotion/Goblin and
// none with RootMotion/Orc. Shared leading segments break the ties that leaves, which
// is what reads a character's own RM subfolder — Anims/Goblin against Anims/Goblin/RM
// shares no trailing segment at all, because the last segments are "Goblin" and "RM",
// and scores the same zero against Anims/Orc/RM. Ranked on the tail alone both cards in
// that pack took whichever RM came first and the other RM stayed unsuppressed, showing
// as a stray card beside the pair it belongs to.
//
// Packed so the tail dominates and the head only breaks its ties, rather than the two
// being summed into a tie again. Leading segments cannot be the primary term: a loose
// library carries absolute paths, so every candidate in a group shares the whole
// /lib/vendor/pack prefix and the mirrored-tree case collapses back.
func dirAffinity(a, b assetindex.Source) int {
	ad, _ := entryParts(a)
	bd, _ := entryParts(b)
	as, bs := splitDir(a, ad), splitDir(b, bd)
	tail := 0
	for tail < len(as) && tail < len(bs) && as[len(as)-1-tail] == bs[len(bs)-1-tail] {
		tail++
	}
	head := 0
	for head < len(as) && head < len(bs) && as[head] == bs[head] {
		head++
	}
	return tail*affinityTailWeight + head
}

// affinityTailWeight separates the two terms dirAffinity packs into one score. Larger
// than any path depth a real library reaches, so a shared head can never outweigh a
// shared tail — which is the ordering the whole metric rests on.
const affinityTailWeight = 1 << 16

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
//
// Ranking is not enough on its own, though, which is what bestClaim covers: ranking
// only orders the candidates this card can see, and with one candidate left it takes it
// however distant. So a card is also held to the best any card in the group reaches
// with that RM — the same "it belongs to someone else" rule the directory filter
// applies, one rung down and reachable where that filter is not.
func pickRM(assets []assetindex.Asset, rm []int, claims map[int]int, nonRM assetindex.Asset, sameDirOnly bool) string {
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
		aff := dirAffinity(nonRM.Source, r.Source)
		if aff < claims[ri] {
			continue
		}
		score := aff << 1
		if r.Source.ArchivePath == nonRM.Source.ArchivePath {
			score++
		}
		if score > bestScore {
			best, bestScore = r.ID, score
		}
	}
	return best
}

// bestClaim is the highest directory affinity any in-place asset in the group reaches
// with r. A card below it is not the card r belongs to, so it takes nothing rather
// than the better-matched card's sibling.
//
// This is the same rule the same-directory filter applies, one rung down, and it is
// what covers the layout that filter cannot see. A pack mirroring per-character folders
// under one root-motion tree puts no RM in any card's own directory, so sameDirOnly is
// false and affinity alone separates the characters — but only while every character
// ships an RM. Root-motion variants usually exist for locomotion and not much else, so
// a group where one character's RM is missing is ordinary, and there every remaining
// candidate is equally distant from it. Without this, that card scores 1 on the archive
// term alone and takes a *different character's* travel animation: a file that loads,
// clips that play, and nothing anywhere to say the body is wrong.
func bestClaim(assets []assetindex.Asset, nonRM []int, r assetindex.Asset) int {
	best := 0
	for _, ni := range nonRM {
		a := assets[ni]
		if a.Ext != r.Ext || a.Category != assetindex.CategoryAnimation {
			continue
		}
		if n := dirAffinity(a.Source, r.Source); n > best {
			best = n
		}
	}
	return best
}
