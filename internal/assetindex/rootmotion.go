package assetindex

import "strings"

// RootMotionVariant reports whether a base file name (extension already stripped) is
// the root-motion (travel) variant of an in-place animation, returning the canonical
// in-place base with the token removed. It is the one recognizer every layer shares —
// the GLB-split gate (a variant is kept whole) and the browse root-motion pairing — so
// a new naming convention is taught in one place. The conventions across the libraries:
//
//   - "_RM" suffix            Quaternius / explosive GLB   "UAL1_RM"           -> "UAL1"
//   - "_RM_" infix            Synty Sidekick FBX           "..._180L_RM_Masc"  -> "..._180L_Masc"
//   - " [RM]" bracket suffix  kevdev FBX                   "...Right [RM]"     -> "...Right"
//   - "_RootMotion_" infix    Synty Polygon FBX            "A_Dodge_L_RootMotion_Sword" -> "A_Dodge_L_Sword"
//
// Each token is bounded by "_", a space before "[", or the end, so substrings like
// "arm"/"Storm"/"Warm" are left alone. Suffixed spellings like "RootMotionVertical" are
// deliberately not matched: their in-place counterpart is ambiguous.
//
// Teaching it a convention means bumping assetindex.indexVersion, because the two
// consumers do not read it at the same time. The split gate's answer is frozen into
// the cache — a loose file whose stat print has not moved keeps the assets it produced
// — while browse pairs live at startup over whatever the cache handed back. Left
// unbumped, a file the old gate split into clips is read by the new pairing as an RM
// variant of its own base: each in-place clip claims one stale clip asset as its
// sibling and hides exactly that one, leaving the rest visible, one card silently
// missing, and a toggle pointed at a clip the RM file need not contain.
func RootMotionVariant(base string) (string, bool) {
	if i := strings.Index(base, "[RM]"); i >= 0 {
		return strings.TrimRight(base[:i], " ") + base[i+len("[RM]"):], true
	}
	for _, tok := range []string{"_RootMotion", "_RM"} {
		if s, ok := stripToken(base, tok); ok {
			return s, true
		}
	}
	return base, false
}

// stripToken removes tok as a whole "_"-delimited element, leaving a name the in-place
// variant could actually be called: an infix keeps the trailing separator
// ("A_RM_B" -> "A_B") and a suffix drops it ("A_RM" -> "A"). Bounded on both sides, so
// "Warm" and "Storm" are left alone.
func stripToken(base, tok string) (string, bool) {
	if i := strings.Index(base, tok+"_"); i >= 0 {
		return base[:i] + base[i+len(tok):], true
	}
	return strings.CutSuffix(base, tok)
}
