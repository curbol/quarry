package browse

import (
	"encoding/json"
	"errors"
	"mime"
	"net/http"

	"github.com/curbol/quarry/internal/tagstore"
)

// The tag store is the one thing quarry writes inside a user's tree, and every
// endpoint that touches it goes through here rather than reaching for the store
// directly. What this pipeline guarantees is the same for a tag edit and a link edit:
// the mutation and the save happen under one write lock, a save that does not land is
// undone in memory by reloading from disk, and a reload that fails too is named in the
// response instead of leaving the UI showing an edit that is not real.
//
// Requiring an application/json content-type is what keeps a page the user happens to
// have open from writing here at all: it forces a CORS preflight this server does not
// answer. An endpoint that accepted a form or text content-type would lose that, since
// there is no session and nothing else standing in the way.

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
	if err := s.store.Save(s.tagsPath); err != nil {
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
	// Parsed rather than prefix-matched: a media type is case-insensitive and its
	// parameters are not part of it, so "Application/JSON; charset=utf-8" is the same
	// request an ordinary client sends. This cannot loosen the guard — no spelling of
	// application/json is a content-type a form or a beacon is allowed to send without
	// a preflight.
	ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || ct != "application/json" {
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
