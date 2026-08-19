package browse

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

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

// resolveTags fills each card's Tags with the union of tag ids over its
// fingerprints, so a grouped card shows every tag any of its copies carries.
func (s *server) resolveTags(dtos []assetDTO) {
	s.tagsMu.RLock()
	defer s.tagsMu.RUnlock()
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

// resolveRelated fills each card's Related with the union of link-related
// fingerprints over its own fingerprints, minus its own set.
func (s *server) resolveRelated(dtos []assetDTO) {
	s.tagsMu.RLock()
	defer s.tagsMu.RUnlock()
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
// have survived them to be in preTag). Matches keep their order; companions follow.
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
	if req.On && len(req.Fingerprints) < 2 {
		writeErr(w, http.StatusBadRequest, "need at least two fingerprints to link")
		return
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
	fps := r.URL.Query()["fingerprint"]
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
	grouped := groupItems(s.ix.Assets, sel)
	s.decorate(grouped)
	sortItems(grouped, "")
	writeJSON(w, map[string]any{"items": grouped})
}

func (s *server) requireEnabled(w http.ResponseWriter) bool {
	if !s.tagsEnabled {
		writeErr(w, http.StatusConflict, "tagging is disabled: this server was started without a tag store")
		return false
	}
	return true
}

// writeUnderLock applies one edit to the store, persists it, and answers. Every write
// endpoint goes through here so that mutating without persisting, or answering an
// error while memory keeps the change, is not a shape a handler can express.
func (s *server) writeUnderLock(w http.ResponseWriter, mutate func() (any, error)) {
	body, status := s.applyEdit(mutate)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}

// applyEdit is the whole locked sequence: mutate, persist, recover on failure, and
// encode the answer. It returns the encoded body rather than writing it, because
// writing goes to a socket: net/http's body buffer is 2KB, so a palette any larger
// would flush to the client while the exclusive lock is still held, and one stalled
// reader would block every other tag read and write until it drained.
//
// mutate builds the response body while still under the lock, since the palette and
// a card's union tags are read from the store.
func (s *server) applyEdit(mutate func() (any, error)) ([]byte, int) {
	s.tagsMu.Lock()
	defer s.tagsMu.Unlock()
	resp, err := mutate()
	// Bumped whether or not the edit stuck: a rejected one still reloads the store, and
	// a memoized result set carries the tags it was built with either way.
	s.tagsGeneration++
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, tagstore.ErrStale) {
			status = http.StatusConflict
		}
		return errBody(s.recoverLocked(err.Error())), status
	}
	if err := tagstore.Save(s.tagsPath, s.store); err != nil {
		// Handlers mutate the store and then persist, so a failed save would otherwise
		// leave memory claiming more than disk holds: the UI keeps reporting the tag
		// until a restart silently takes it away again.
		status := http.StatusInternalServerError
		if errors.Is(err, tagstore.ErrStale) {
			status = http.StatusConflict
		}
		return errBody(s.recoverLocked("could not save tags: " + err.Error())), status
	}
	b, encErr := json.Marshal(resp)
	if encErr != nil {
		return errBody("could not encode the response: " + encErr.Error()), http.StatusInternalServerError
	}
	return b, http.StatusOK
}

// recoverLocked drops the in-memory store for what is on disk, so a write the file
// never received cannot survive in memory, and returns the message to answer with.
// The caller must hold the write lock.
//
// A reload that itself fails is the one case where memory and disk genuinely diverge
// and nothing can be done about it, so it is named in the response rather than
// swallowed: the client has just been told the write was rejected, and without this
// the UI would keep showing the edit with no way to know it is not real.
func (s *server) recoverLocked(msg string) string {
	if err := s.store.Reload(s.tagsPath); err != nil {
		return msg + " (and reloading the store from disk failed: " + err.Error() +
			"; what is shown may be ahead of the file until quarry is restarted)"
	}
	return msg
}

func errBody(msg string) []byte {
	b, err := json.Marshal(map[string]string{"error": msg})
	if err != nil {
		return []byte(`{"error":"could not encode the error"}`)
	}
	return b
}

// maxTagBodyBytes bounds a write request. The payloads are a handful of
// fingerprints and a tag name; anything larger is a mistake or an attack.
const maxTagBodyBytes = 1 << 20

// decodeJSON reads a write request's body. browse has no session by design, so any
// page the user has open can reach it on localhost; requiring a JSON content-type
// forces a CORS preflight that this server does not answer, which is what keeps a
// drive-by fetch from writing to the committed tag store.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeErr(w, http.StatusUnsupportedMediaType, "expected application/json")
		return false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTagBodyBytes)).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
