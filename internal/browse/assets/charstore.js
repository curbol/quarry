// The half of the preview-character registry that is only bookkeeping: what is stored,
// what a change to it means, and which stored body a clip should be posed on. No THREE,
// so the page and the thumbnail worker can both reach a card's URLs and the registry
// without pulling the 3D stack in behind them — app.js and thumbs.js need nothing else
// from scene.js, and statically importing it put three, both loaders and OrbitControls
// on the path of the grid's very first request.
//
// scene.js owns the other half — loading a model, deciding whether it is a rig,
// searching for one — and composes it onto the same CharRegistry object, so the two
// stay one thing at every call site. Anything here has to keep working before that has
// happened, since thumbs.js reads the registry without importing scene.js at all.
//
// rigmatch.js is imported relatively rather than by absolute /static/ path: it is the
// same file either way in the browser (this module is served from /static/), and it is
// what lets node --test load this one with no server and nothing installed.
import { hasNamedBody, matchRig } from './rigmatch.js';

export const contentURL = (id) => '/api/content?id=' + encodeURIComponent(id);
export const thumbURL = (id) => '/api/thumb?id=' + encodeURIComponent(id);

// CharRegistry persists to localStorage on the main thread; a worker has none, so it
// falls back to an in-memory store (its rig cache then lasts the worker's lifetime).
const memStore = new Map();
const store = {
  get(k) { try { return typeof localStorage !== 'undefined' ? localStorage.getItem(k) : (memStore.has(k) ? memStore.get(k) : null); } catch { return memStore.has(k) ? memStore.get(k) : null; } },
  set(k, v) { try { if (typeof localStorage !== 'undefined') localStorage.setItem(k, v); else memStore.set(k, v); } catch { memStore.set(k, v); } },
};

// A skinned character mesh whose bone names cover a clip's tracks can play that clip
// directly (proven for the Synty rig: the native body and the clips share a rest
// pose). Different rigs (e.g. the goblin A-pose rig) match a different body, or none
// — in-browser retargeting of these mesh-less clips is unreliable, so a non-matching
// rig falls back to the manual picker rather than a distorted pose.
export const CharRegistry = {
  key: 'browsePreviewChars',
  // A value that will not parse reports as an empty registry, and the next save
  // overwrites it — so the user's pinned characters go without a word, and every
  // preview afterwards falls back to the manual picker for no visible reason.
  list() {
    try {
      return JSON.parse(store.get(this.key)) || [];
    } catch (e) {
      console.warn('the stored preview-character registry could not be read and is being replaced', e);
      return [];
    }
  },
  save(l) { try { store.set(this.key, JSON.stringify(l.slice(0, 40))); } catch { /* quota */ } },
  // add records a character's rig, most recent first, and reports whether what the
  // matcher can pick actually changed — the order is refreshed on every lightbox open
  // and nothing downstream reads it.
  add(entry) {
    if (!entry.bones || entry.bones.length < 10) return false;
    const l = this.list();
    const prev = l.find((e) => e.id === entry.id);
    // rigEntry rebuilds an entry from the model and knows nothing about pinning, so the
    // flag is carried across here. Without it, opening a pinned character un-pins it.
    if (prev && prev.pinned) entry = { ...entry, pinned: true };
    const rest = l.filter((e) => e.id !== entry.id);
    rest.unshift(entry);
    this.save(rest);
    return !prev || JSON.stringify(prev) !== JSON.stringify(entry);
  },
  remove(id) { this.save(this.list().filter((e) => e.id !== id)); },
  // match picks the registered character whose skeleton best covers a clip's bones.
  // A pinned character that covers the clip wins over a higher-coverage unpinned one,
  // so pinning a body for a rig makes it the default for every clip on that rig.
  // clipName settles the bodies coverage cannot separate — a pack's variants share one
  // skeleton, so the one named for the clip's own series is the one it belongs on.
  // Auto-match is scoped to the clip's own vendor: cross-vendor skeletons share enough
  // bone names to pass the coverage bar but differ in rest pose, posing a clip into a
  // shredded/T-posed garbage still. A legacy entry with no recorded vendor is a wildcard
  // until it is re-registered (see register), so old caches keep working. The ranking
  // itself is matchRig, in rigmatch.js, where it is checked without a GL context.
  match(bones, vendor, clipName) { return matchRig(this.list(), bones, vendor, clipName); },
  // pin reports whether the flag moved, so a caller can tell a real change from a
  // click that re-asserted what was already true.
  pin(id, on) {
    const l = this.list();
    const e = l.find((x) => x.id === id);
    if (!e || !!e.pinned === !!on) return false;
    e.pinned = on;
    this.save(l);
    return true;
  },
  isPinned(id) { return !!(this.list().find((x) => x.id === id) || {}).pinned; },
  // hasNamed reports whether the body this clip is named after is already registered, so
  // registerNamed is only paid for when it is not. The ranking in match() does the rest.
  hasNamed(asset) { return hasNamedBody(this.list(), asset && asset.vendor, asset && asset.name); },
};
