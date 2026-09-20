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
//
// How many entries a save keeps belongs to the store rather than to CharRegistry,
// because the two realms are bounded by different things. STORED_MAX is the page's:
// one localStorage value, which grows until a save throws and every later one is
// silently dropped. MEMORY_MAX is the worker's, where there is no quota and the only
// cost is the linear scan match() makes.
//
// The worker's has to be the looser of the two. Its entries are what discoverForVendor
// found by loading models, and they are work the page cannot redo: the memos recording
// a scope as already searched live on CharRegistry and survive any eviction. A reseed
// carries the page's whole list, so at a shared bound one merge pushed every discovered
// body out permanently and the pack's clips drew the category icon for the session.
export const STORED_MAX = 40;
export const MEMORY_MAX = 400;

const memStore = new Map();
const hasLocalStorage = () => { try { return typeof localStorage !== 'undefined'; } catch { return false; } };
// Once a write has fallen back to memory, reads have to follow it there. Otherwise the
// two halves disagree: a setItem that throws — a quota exhausted, a browser with site
// data blocked, Safari private browsing — sent the write to memStore while get kept
// reading localStorage and handing back the value from before it, so every save
// vanished silently and nothing downstream could tell a stored registry from a stale one.
let degraded = false;
const fromMemory = (k) => (memStore.has(k) ? memStore.get(k) : null);
const store = {
  limit: hasLocalStorage() ? STORED_MAX : MEMORY_MAX,
  get(k) {
    if (degraded) return fromMemory(k);
    try { return typeof localStorage !== 'undefined' ? localStorage.getItem(k) : fromMemory(k); } catch { return fromMemory(k); }
  },
  set(k, v) {
    if (!degraded) {
      try {
        if (typeof localStorage !== 'undefined') { localStorage.setItem(k, v); return; }
      } catch { degraded = true; }
    }
    memStore.set(k, v);
  },
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
  // Pinned entries are held above the recency trim. add() unshifts on every lightbox
  // open of a rigged character, so browsing a character library pushes a pinned body
  // past the bound within one session — and dropping it reverts every clip on that rig
  // to coverage ranking, which is the one thing pinning exists to override. Order is
  // read for nothing but tie-breaking, and pinned already wins every tie, so hoisting
  // them costs nothing.
  save(l) {
    const keep = [...l.filter((e) => e.pinned), ...l.filter((e) => !e.pinned)].slice(0, store.limit);
    try { store.set(this.key, JSON.stringify(keep)); } catch { /* quota */ }
  },
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
    // The vendor is carried the same way, and for a sharper reason: an entry saved
    // without one is a wildcard the scope check in match() no longer skips, so a body
    // re-registered from an item that did not carry its vendor starts auto-matching
    // every other vendor's clips — in the grid's thumbnails as well as the lightbox.
    // Re-registration must not be able to widen what an entry already knows.
    if (prev && prev.vendor && !entry.vendor) entry = { ...entry, vendor: prev.vendor };
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

// resolveRig finds a rig a clip can play on: the best registry match, the next one if
// that fails to load, then vendor discovery, then whatever that turns up. A cached
// entry goes stale when a re-index changes its id, so a failed load evicts the entry
// and the search continues rather than ending there.
//
// tryLoad is handed a registry entry and returns whatever the caller wants to keep —
// a loaded rig, or true for a caller that only cares that it played — or a falsy value
// when that entry could not be loaded. cancelled says the caller has been torn down;
// it is required because its two callers disagree about what a falsy tryLoad means and
// a default would pick one of them silently.
//
// The eviction is the part to be careful with: a falsy result only proves the entry is
// stale while the search is still wanted. A caller that gives up mid-await returns
// falsy for every remaining candidate, and evicting on that empties the registry of
// every body covering the skeleton — including ones seeded from the user's own pinned
// characters — while leaving the memos that record a scope as already searched intact,
// so nothing re-finds them. Checking cancelled after the await is what separates "this
// entry does not load" from "nobody is waiting for it any more".
//
// It lives here rather than in scene.js because it is control flow over the registry
// and two callbacks, with no THREE in it: seed, registerNamed and discoverForVendor are
// composed onto CharRegistry by scene.js, and a test supplies its own.
//
// The lightbox and the thumbnail worker both search this way, and the order matters to
// what each of them shows: written out twice, one of them fell through to discovery
// once every known entry had failed and the other gave up there.
export async function resolveRig(bones, asset, tryLoad, cancelled) {
  const attempt = async () => {
    // tried is what bounds the walk, rather than the eviction below. The eviction is
    // the registry's business and can fail to take — a store whose write does not
    // land hands match() the same entry forever — and the loop then never advanced:
    // in the lightbox an unbounded stream of 404s for as long as it stayed open, in
    // the worker a spin until the card scrolled away, neither stopped by the job
    // deadline, which only stops a job being waited on. Termination should not depend
    // on a write.
    const tried = new Set();
    for (let m = CharRegistry.match(bones, asset.vendor, asset.name); m && !tried.has(m.id) && !cancelled(); m = CharRegistry.match(bones, asset.vendor, asset.name)) {
      tried.add(m.id);
      const got = await tryLoad(m);
      if (got) return got;
      if (cancelled()) return null;
      CharRegistry.remove(m.id);
    }
    return null;
  };
  await CharRegistry.seed();
  if (cancelled()) return null;
  // A registry holding any body that fits settles the clip here, and the pack search that
  // would turn up the body it is named after only runs when nothing fits at all — so a
  // registry written before this session preferred the named one would go on answering
  // with the other body for as long as it survives. Fetching the named body first is one
  // search and at most one load, once per vendor and series, and leaves the ranking to it.
  if (!CharRegistry.hasNamed(asset)) await CharRegistry.registerNamed(asset);
  if (cancelled()) return null;
  const known = await attempt();
  if (known || cancelled()) return known;
  await CharRegistry.discoverForVendor(asset, bones);
  if (cancelled()) return null;
  return attempt();
}
