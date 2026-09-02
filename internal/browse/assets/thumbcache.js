// The bookkeeping behind the grid's 3D thumbnails: which renders are resident, which
// are in flight and for whom, which assets have already been answered "nothing to
// draw", and whether a result that just arrived is still the one being waited on.
//
// Split out of thumbs.js because none of it needs a browser, and getting any of it
// wrong is silent: an object URL dropped without being revoked is a PNG blob resident
// for the life of the page and outside the bound entirely, and one revoked too early is
// a broken image on a card the user is looking at. Neither reports anything.
//
// Nothing here calls URL.revokeObjectURL. The methods that displace a URL *return* the
// ones the caller must revoke, so the rule about which URLs are released is checkable
// on its own, and so a test can hold the whole cache as plain values. Holders are
// opaque to this module — thumbs.js passes DOM nodes, a test passes anything.
//
// No imports, for the same reason gridwindow.js and jobtracker.js have none: node --test
// loads this with no browser and nothing installed.

// THUMB_CACHE_MAX is how many rendered thumbnails stay resident. Each is a PNG blob
// the browser keeps alive until its object URL is revoked, so this is a memory bound,
// not a hit-rate tuning knob: a few screenfuls in either scroll direction.
export const THUMB_CACHE_MAX = 400;

// NO_RENDER_MAX bounds the memo of assets with nothing to draw. Entries are ids
// rather than blobs, so this can be far larger than the thumbnail cache, but it still
// must not grow to one per asset in the library.
export const NO_RENDER_MAX = 5000;

export class ThumbCache {
  constructor({ cacheMax = THUMB_CACHE_MAX, noRenderMax = NO_RENDER_MAX } = {}) {
    // Insertion order is eviction order, refreshed on each hit.
    this.cache = new Map();   // asset id -> object URL (a rendered blob)
    this.pending = new Map(); // asset id -> { holders, seq } awaiting one render
    this.noRender = new Set(); // assets the worker answered "nothing to draw" for
    this.seq = 0;
    this.cacheMax = cacheMax;
    this.noRenderMax = noRenderMax;
  }

  // route decides what should happen for a holder that has come into view, and records
  // the consequence. The four answers are the four things thumbs.js can do:
  //
  //   settled  — already known to have nothing to draw; stop watching this holder.
  //   cached   — url is resident; show it.
  //   joined   — a render for this asset is already in flight; this holder waits on it.
  //   dispatch — nothing yet; post a request carrying seq.
  //
  // "joined" is what keeps one asset on screen twice — a grid card and the same asset in
  // the lightbox's related strip — from either rendering twice or leaving the second
  // holder on its category icon until it happens to scroll back into view.
  route(id, holder) {
    if (this.noRender.has(id)) return { kind: 'settled' };
    const url = this.cache.get(id);
    if (url !== undefined) {
      // Re-inserted so it is newest: a thumbnail being looked at must not be the next
      // one evicted.
      this.cache.delete(id);
      this.cache.set(id, url);
      return { kind: 'cached', url };
    }
    const inFlight = this.pending.get(id);
    if (inFlight) {
      inFlight.holders.add(holder);
      return { kind: 'joined' };
    }
    // Each request carries a sequence number the worker echoes back, so a result for a
    // request that was since cancelled and re-made is recognisable as stale. Matching on
    // the id alone let a superseded job's result land on the current card's entry.
    const seq = ++this.seq;
    this.pending.set(id, { holders: new Set([holder]), seq });
    return { kind: 'dispatch', seq };
  }

  // release drops one holder from whatever it was waiting on and reports whether the
  // render is now unwanted, so the caller can cancel it. False while another holder on
  // screen still shows the same asset: cancelling on the first one to scroll away would
  // leave the rest spinning for a render nobody is going to ask for again.
  release(id, holder) {
    const p = this.pending.get(id);
    if (!p || !p.holders.has(holder)) return false;
    p.holders.delete(holder);
    if (p.holders.size) return false;
    this.pending.delete(id);
    return true;
  }

  // claim takes the holders waiting on one result, or null when the result is stale —
  // the request was cancelled, or superseded by a later one whose result is the one to
  // use. Called before any object URL is created, which is what keeps a displaced URL
  // from escaping the cache bound unrevoked.
  claim(id, seq) {
    const p = this.pending.get(id);
    if (!p || p.seq !== seq) return null;
    this.pending.delete(id);
    return [...p.holders];
  }

  // remember stores a rendered URL and returns the URLs that are no longer reachable
  // and must be revoked: the one this displaced under the same id, plus whatever fell
  // off the end of the bound.
  remember(id, url) {
    const stale = [];
    const prev = this.cache.get(id);
    // Overwriting the key alone would leave the old URL alive with nothing tracking it,
    // outside the bound entirely.
    if (prev !== undefined && prev !== url) stale.push(prev);
    this.cache.set(id, url);
    while (this.cache.size > this.cacheMax) {
      const oldest = this.cache.keys().next().value;
      stale.push(this.cache.get(oldest));
      this.cache.delete(oldest);
    }
    return stale;
  }

  // markNoRender memoizes that an asset has nothing to draw. The memo outlives the card
  // that learned it: the grid rebuilds its live window every few dozen rows, and
  // re-asking costs the worker the whole rig discovery again — up to fourteen model
  // downloads to re-establish that no rig in this vendor fits.
  markNoRender(id) {
    this.noRender.add(id);
    while (this.noRender.size > this.noRenderMax) {
      this.noRender.delete(this.noRender.values().next().value);
    }
  }

  // clearNoRender forgets every "nothing to draw" answer, for when the question has
  // changed — the user picked a body a mesh-less clip can now pose on.
  clearNoRender() { this.noRender.clear(); }

  // drainPending abandons every request in flight and returns their holders, for a
  // worker that has stopped answering.
  drainPending() {
    const holders = [];
    for (const { holders: hs } of this.pending.values()) holders.push(...hs);
    this.pending.clear();
    return holders;
  }
}
