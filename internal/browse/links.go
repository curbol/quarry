package browse

import (
	"net/http"
	"slices"
)

// Links group fingerprints that belong together — a UI frame and its background fill,
// say — so they travel as a set. They are orthogonal to tags: a link never changes what
// tags a fingerprint carries, and only ever widens a result set.
//
// Two shapes read a group. includeRelated folds each tag match's companions back into
// the results, relaxing the tag filter alone so the other facets and the text search
// still apply; /api/related resolves a fingerprint set's companions to whole cards for
// the lightbox strip.

// resolveRelatedLocked fills each card's Related with the union of link-related
// fingerprints over its own fingerprints, minus its own set. The caller must hold the
// read lock (see decorate).
func (s *server) resolveRelatedLocked(dtos []assetDTO) {
	// With no links at all there is nothing any card could relate to, and the loop
	// below would allocate two maps per card across the whole result set to prove it.
	if !s.store.HasGroups() {
		return
	}
	for i := range dtos {
		own := dtos[i].Fingerprints
		var rel map[string]bool
		for _, fp := range own {
			for _, r := range s.store.Related(fp) {
				if slices.Contains(own, r) {
					continue
				}
				if rel == nil {
					rel = map[string]bool{}
				}
				rel[r] = true
			}
		}
		if len(rel) > 0 {
			dtos[i].Related = sortedSet(rel)
		}
	}
}

// expandRelated pulls companion cards into a tag-filtered result: any card from the
// pre-tag-filter set (preTag) that shares a link group with a match. It relaxes only
// the tag filter, so other facets and the text search still apply (a companion must
// have survived them to be in preTag).
func expandRelated(filtered, preTag []assetDTO) []assetDTO {
	if len(filtered) == 0 {
		return filtered
	}
	want := map[string]bool{}
	for _, d := range filtered {
		for _, fp := range d.Related {
			want[fp] = true
		}
	}
	if len(want) == 0 {
		return filtered
	}
	present := make(map[string]bool, len(filtered))
	for _, d := range filtered {
		present[d.ID] = true
	}
	out := filtered
	for _, d := range preTag {
		if present[d.ID] {
			continue
		}
		for _, fp := range d.Fingerprints {
			if want[fp] {
				out = append(out, d)
				break
			}
		}
	}
	return out
}

// handleLink links or unlinks a set of fingerprints as one travel-together group,
// mirroring handleAssign's shape. Linking needs at least two fingerprints.
func (s *server) handleLink(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Fingerprints []string `json:"fingerprints"`
		On           bool     `json:"on"`
	}
	if !s.requireEnabled(w) || !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Fingerprints) == 0 {
		writeErr(w, http.StatusBadRequest, "missing fingerprints")
		return
	}
	// Counted the way Link counts, which drops empties and folds repeats: a request
	// naming one fingerprint twice, or one alongside an unreadable asset's empty
	// print, passes a bare length check and then makes no group at all — answered
	// {"ok":true} for a link that does not exist, with the store rewritten unchanged.
	if req.On {
		distinct := map[string]bool{}
		for _, fp := range req.Fingerprints {
			if fp != "" {
				distinct[fp] = true
			}
		}
		if len(distinct) < 2 {
			writeErr(w, http.StatusBadRequest, "need at least two distinct fingerprints to link")
			return
		}
	}
	s.writeUnderLock(w, func() (any, error) {
		if req.On {
			s.store.Link(req.Fingerprints)
		} else {
			s.store.Unlink(req.Fingerprints)
		}
		return map[string]any{"ok": true}, nil
	})
}

// handleRelated returns the cards linked to the given fingerprints (a card's whole
// fingerprint set, passed as repeated ?fingerprint= params). It searches the whole
// library, not the current page or facet filter, so companions surface regardless of
// how the grid is filtered.
func (s *server) handleRelated(w http.ResponseWriter, r *http.Request) {
	query, ok := requestQuery(w, r)
	if !ok {
		return
	}
	fps := query["fingerprint"]
	own := make(map[string]bool, len(fps))
	for _, fp := range fps {
		own[fp] = true
	}
	s.tagsMu.RLock()
	related := map[string]bool{}
	for _, fp := range fps {
		for _, rfp := range s.store.Related(fp) {
			if !own[rfp] {
				related[rfp] = true
			}
		}
	}
	s.tagsMu.RUnlock()

	var sel []int32
	seen := map[int32]bool{}
	for rfp := range related {
		for _, ai := range s.byFP[rfp] {
			// Suppressed root-motion siblings are folded into their in-place card
			// everywhere else; surfacing one here would show the strip a card the grid
			// never has.
			if seen[ai] || s.rmSuppressed[s.ix.Assets[ai].ID] {
				continue
			}
			seen[ai] = true
			sel = append(sel, ai)
		}
	}
	// Back into index order before grouping. related is a map, so sel arrives in a
	// different order each call, and groupItems keeps the first position it saw for a
	// card whose copies tie on thumb rank — which is every card shipped twice in one
	// format. The representative decides the id, path and source the strip serves, so
	// without this one card previews a different file from one open to the next.
	slices.Sort(sel)
	grouped := groupItems(s.ix.Assets, sel)
	s.decorate(grouped)
	sortItems(grouped, "")
	writeJSON(w, map[string]any{"items": grouped})
}
