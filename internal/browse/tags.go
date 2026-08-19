package browse

import (
	"net/http"
	"slices"

	"github.com/curbol/quarry/internal/tagstore"
)

// tagView is one palette entry for the client: a tag's id, color, and how many
// assets (fingerprints) carry it.
type tagView struct {
	ID    string `json:"id"`
	Color string `json:"color"`
	Count int    `json:"count"`
}

// paletteView is the tag palette the client renders slivers and filters from, plus
// whether tagging is enabled at all.
type paletteView struct {
	Enabled bool      `json:"enabled"`
	Tags    []tagView `json:"tags"`
}

func (s *server) paletteLocked() paletteView {
	counts := s.store.Counts()
	defs := s.store.Tags()
	tv := make([]tagView, len(defs))
	for i, d := range defs {
		tv[i] = tagView{ID: d.ID, Color: d.Color, Count: counts[d.ID]}
	}
	return paletteView{Enabled: s.tagsEnabled, Tags: tv}
}

// resolveTagsLocked fills each card's Tags with the union of tag ids over its
// fingerprints, so a grouped card shows every tag any of its copies carries. The
// caller must hold the read lock (see decorate).
func (s *server) resolveTagsLocked(dtos []assetDTO) {
	for i := range dtos {
		dtos[i].Tags = s.unionTagsLocked(dtos[i].Fingerprints)
	}
}

func (s *server) unionTagsLocked(fps []string) []string {
	// The overwhelmingly common card has one fingerprint, and TagsFor already returns
	// a fresh sorted slice. Taking the general path there would allocate a map and a
	// second slice per card, across the whole library, to reproduce it.
	if len(fps) == 1 {
		return s.store.TagsFor(fps[0])
	}
	set := map[string]bool{}
	for _, fp := range fps {
		for _, id := range s.store.TagsFor(fp) {
			set[id] = true
		}
	}
	// sortedSet even when empty, so an untagged card serializes "tags": [] whatever its
	// group size. Returning nil here made the field null for a grouped card and [] for
	// a single one, which is a difference in the public response shape that says
	// nothing about the card.
	return sortedSet(set)
}

// filterByTags keeps cards matching the requested tags against the card's union tag
// set: AND requires all, OR (the default) requires any.
//
// It compacts dtos in place, so the caller must not keep using the slice it passed.
func filterByTags(dtos []assetDTO, tags []string, mode string) []assetDTO {
	if len(tags) == 0 {
		return dtos
	}
	and := mode == "and"
	out := dtos[:0]
	for _, d := range dtos {
		if matchTags(d.Tags, tags, and) {
			out = append(out, d)
		}
	}
	return out
}

// matchTags tests a card's tag set against the requested ones. Both sides are a
// handful of entries at most, so a linear scan beats building a map per card.
func matchTags(have, want []string, and bool) bool {
	if and {
		for _, t := range want {
			if !slices.Contains(have, t) {
				return false
			}
		}
		return true
	}
	for _, t := range want {
		if slices.Contains(have, t) {
			return true
		}
	}
	return false
}

func (s *server) handleTags(w http.ResponseWriter, r *http.Request) {
	s.tagsMu.RLock()
	palette := s.paletteLocked()
	s.tagsMu.RUnlock()
	// Encoded after the unlock: Go's RWMutex queues new readers behind a waiting
	// writer, so holding this across the response would let one slow client stall
	// every tag write too.
	writeJSON(w, palette)
}

func (s *server) handleTagCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID    string `json:"id"`
		Color string `json:"color"`
	}
	if !s.requireEnabled(w) || !decodeJSON(w, r, &req) {
		return
	}
	if req.ID == "" {
		writeErr(w, http.StatusBadRequest, "missing tag id")
		return
	}
	color := req.Color
	if color == "" {
		color = tagstore.DefaultColor(req.ID)
	}
	s.writeUnderLock(w, func() (any, error) {
		if err := s.store.Define(req.ID, color); err != nil {
			return nil, err
		}
		return s.paletteLocked(), nil
	})
}

func (s *server) handleTagPatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID    string `json:"id"`
		NewID string `json:"newId"`
		Color string `json:"color"`
	}
	if !s.requireEnabled(w) || !decodeJSON(w, r, &req) {
		return
	}
	if req.ID == "" {
		writeErr(w, http.StatusBadRequest, "missing tag id")
		return
	}
	// The color is validated up front because a patch is two edits: a rename that
	// lands followed by a color that fails would answer "rejected" while the palette
	// keeps the new name.
	color := ""
	if req.Color != "" {
		c, err := tagstore.NormalizeColor(req.Color)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		color = c
	}
	s.writeUnderLock(w, func() (any, error) {
		target := req.ID
		if req.NewID != "" && req.NewID != req.ID {
			if err := s.store.Rename(req.ID, req.NewID); err != nil {
				return nil, err
			}
			target = req.NewID
		}
		if color != "" {
			if err := s.store.Define(target, color); err != nil {
				return nil, err
			}
		}
		return s.paletteLocked(), nil
	})
}

func (s *server) handleTagDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if !s.requireEnabled(w) || !decodeJSON(w, r, &req) {
		return
	}
	if req.ID == "" {
		writeErr(w, http.StatusBadRequest, "missing tag id")
		return
	}
	s.writeUnderLock(w, func() (any, error) {
		s.store.Delete(req.ID)
		return s.paletteLocked(), nil
	})
}

// handleAssign toggles a tag across a set of fingerprints (a card's whole group),
// so the card's union display matches what was written. It returns the set's
// resulting union tags plus the full palette (so a just-created tag's color is
// known to the client without a second request).
func (s *server) handleAssign(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Fingerprints []string `json:"fingerprints"`
		Tag          string   `json:"tag"`
		On           bool     `json:"on"`
	}
	if !s.requireEnabled(w) || !decodeJSON(w, r, &req) {
		return
	}
	if req.Tag == "" {
		writeErr(w, http.StatusBadRequest, "missing tag")
		return
	}
	if len(req.Fingerprints) == 0 {
		writeErr(w, http.StatusBadRequest, "missing fingerprints")
		return
	}
	s.writeUnderLock(w, func() (any, error) {
		for _, fp := range req.Fingerprints {
			if req.On {
				s.store.Assign(fp, req.Tag)
			} else {
				s.store.Unassign(fp, req.Tag)
			}
		}
		return map[string]any{
			"tags":    s.unionTagsLocked(req.Fingerprints),
			"palette": s.paletteLocked(),
		}, nil
	})
}
