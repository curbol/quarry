// FontCache is the bound on how many font files stay registered on the document. Every
// font card that scrolls past downloads a whole typeface, and document.fonts holds it
// forever, so scrolling a font pack keeps every file it ever showed resident.
//
// Its own module, THREE-free and importing nothing, for the same reason ThumbCache is:
// the rule it implements fails silently in both directions. Evict an entry whose card
// is still on screen and that card shows its sample in the fallback serif with nothing
// left to put it back; fail to evict and the bound does nothing. Neither shows up as an
// error, and neither is visible in a DOM the tests cannot build.
//
// Two things are deliberately left to the caller. A holder is opaque here and liveness
// arrives as an injected predicate, rather than this module reaching for isConnected —
// that is what lets it be checked without a document. And nothing here releases an
// evicted value: what a FontFace has to be handed to is document.fonts, which is the
// caller's business, exactly as revoking an object URL is ThumbCache's caller's.

// FONT_CACHE_MAX is the backstop, not the working bound. The bound that matters is how
// many cards the grid keeps connected, which is what the liveness skip follows — a
// fixed number smaller than one viewport's worth of font cards is how the visible ones
// got evicted in the first place.
export const FONT_CACHE_MAX = 200;

export class FontCache {
  // live decides whether an entry's holder still needs it. It defaults to "nothing is
  // live", so a cache built without one is a plain LRU rather than one that silently
  // pins everything.
  constructor({ max = FONT_CACHE_MAX, live = () => false } = {}) {
    this.max = max;
    this.live = live;
    this.entries = new Map(); // id -> { value, holder }; insertion order is eviction order
  }

  get size() {
    return this.entries.size;
  }

  // hit returns the stored value and refreshes its place in eviction order, or
  // undefined for an id the cache does not hold.
  //
  // The holder is replaced only when one is given. The lightbox's specimen asks for a
  // font with no holder of its own, and letting that clear the slot would make a grid
  // card still on screen evictable — which is the failure this whole module is the
  // bound for.
  hit(id, holder) {
    const e = this.entries.get(id);
    if (!e) return undefined;
    this.entries.delete(id);
    if (holder !== undefined && holder !== null) e.holder = holder;
    this.entries.set(id, e);
    return e.value;
  }

  // remember records a value, making room for it first, and returns the values it
  // stopped pointing at so the caller can release them — the ones it evicted, and the
  // one already held under this id, which is displaced just as finally. Handing that
  // one back rather than dropping it is what keeps "every value this cache lets go of
  // comes back" true of the whole method, so a caller cannot register a FontFace the
  // document then holds forever with nothing left naming it.
  //
  // Room is made before the insert, never after: an entry inserted first is last in
  // eviction order, so a walk that skips every live holder ahead of it arrives at the
  // entry that was just added — and evicts it if it came with no holder.
  remember(id, value, holder) {
    const dropped = this.makeRoom();
    const prev = this.entries.get(id);
    if (prev && prev.value !== value) dropped.push(prev.value);
    this.entries.delete(id);
    this.entries.set(id, { value, holder });
    return dropped;
  }

  // makeRoom drops the oldest entries whose holder is no longer live, and only those,
  // until there is space for one more. One pass: where every holder is live there is
  // nothing to drop, and the cache is allowed to sit over its backstop rather than
  // evict something a card is showing.
  makeRoom() {
    const dropped = [];
    for (const [id, e] of this.entries) {
      if (this.entries.size < this.max) break;
      if (this.live(e.holder)) continue;
      this.entries.delete(id);
      dropped.push(e.value);
    }
    return dropped;
  }

  // forget drops an id without returning its value, for a load that failed: there is
  // nothing to release, and leaving it in would serve the failure as the answer to
  // every later request for that font.
  forget(id) {
    this.entries.delete(id);
  }
}
