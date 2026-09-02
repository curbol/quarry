// The caches a scrolling grid has to keep bounded, and the observer that decides when
// to fill them. They live together because their eviction rules only read against each
// other: every one is holding something the browser will not release on its own — an
// object URL, a FontFace, an observed node — over a library the user can keep scrolling
// for as long as they like. Split across the page's other concerns, the bounds drifted.
//
// Nothing here knows about cards, queries or the lightbox. It takes an element and an
// asset descriptor and fills the element in when the element is worth filling.

import { contentURL, CharRegistry } from '/static/charstore.js';
import { ThumbCache } from '/static/thumbcache.js';

// ---- fonts ----

// loadedFonts caps how many font files stay registered on the document. Every card
// that scrolls past downloads a whole typeface and document.fonts holds it forever, so
// scrolling a font pack would keep every file it ever showed resident. Insertion order
// is eviction order, matching the thumbnail cache.
//
// Each entry carries the in-flight promise, not just a marker, so two cards for the
// same font share one download instead of each registering its own FontFace — and the
// element that asked for it, because a card still on screen must not be evicted.
// lazyWork fires once per element, so ensureFont never runs again for a card already
// drawn: dropping its FontFace leaves it showing the sample in the fallback serif with
// nothing left to put it back, and no way to notice.
const loadedFonts = new Map(); // asset.id -> { load: Promise<{ fam, face }>, el }

// FONT_CACHE_MAX is the backstop, not the working bound. The bound that matters is how
// many cards the grid keeps connected, which is what the skip in evictFonts follows —
// a fixed number smaller than one viewport's worth of font cards is how the visible
// ones got evicted in the first place.
const FONT_CACHE_MAX = 200;

// ensureFont registers a font's bytes as a FontFace under a per-asset family and
// resolves that family name, so sample text can be rendered in the real typeface. el is
// the node the family will be applied to, held only to tell a live card from a dropped
// one at eviction time.
export function ensureFont(a, el) {
  const fam = 'f' + a.id;
  const cached = loadedFonts.get(a.id);
  if (cached) {
    loadedFonts.delete(a.id);
    // Only when one is given: the lightbox's specimen asks for the same font without a
    // card behind it, and letting that clear the slot would make a grid card that is
    // still on screen evictable.
    if (el) cached.el = el;
    loadedFonts.set(a.id, cached); // refresh its place in eviction order
    return cached.load.then((e) => e.fam);
  }
  const load = new FontFace(fam, `url(${contentURL(a.id)})`).load()
    .then((face) => { document.fonts.add(face); return { fam, face }; })
    .catch((err) => { loadedFonts.delete(a.id); throw err; });
  loadedFonts.set(a.id, { load, el });
  evictFonts();
  return load.then((e) => e.fam);
}

// evictFonts drops the oldest entries whose cards are gone, and only those.
function evictFonts() {
  for (const [id, e] of loadedFonts) {
    if (loadedFonts.size <= FONT_CACHE_MAX) return;
    if (e.el && e.el.isConnected) continue;
    loadedFonts.delete(id);
    e.load.then((v) => document.fonts.delete(v.face)).catch(() => {});
  }
}

// ---- lazy 3D thumbnails: one shared renderer, sequential queue, cached ----

// lazyWork defers per-card work (a thumbnail render, a font download) until the card
// is near the viewport. An IntersectionObserver holds its targets and a card that
// never scrolled into view is never unobserved, so every caller dropping a node must
// forget() it or that node, and the asset it references, outlives the DOM.
//
// It watches more than the grid — the lightbox's related strip defers the same work —
// so there is no wholesale teardown here. Releasing a batch means forgetting its
// nodes; anything coarser takes the other caller's nodes with it.
export const lazyWork = {
  observer: null,
  init() {
    this.observer = new IntersectionObserver((entries) => {
      for (const e of entries) {
        if (!e.isIntersecting) continue;
        this.observer.unobserve(e.target);
        const run = e.target._onVisible;
        e.target._onVisible = null;
        if (run) run();
      }
    }, { rootMargin: '200px' });
  },
  when(el, run) { el._onVisible = run; this.observer.observe(el); },
  forget(el) {
    if (this.observer) this.observer.unobserve(el);
    el._onVisible = null;
  },
};
lazyWork.init();

// ModelThumbnails is a thin client over the thumbnail worker: it observes cards, posts
// each asset's descriptor, and swaps in the PNG blob the worker renders off the main
// thread. No parsing or WebGL happens here, so a large model never blocks the UI.
class ModelThumbnails {
  constructor() {
    // What is resident, what is in flight, and what has already been answered "nothing
    // to draw" — all of it in thumbcache.js, which is checked by the Node tests. This
    // class is what a browser is needed for: the observer, the object URLs, the decode,
    // and the worker.
    this.jobs = new ThumbCache();
    // Holders showing the category icon because nothing was drawn for them. They are
    // no longer observed — that is the point of settling — so clearing the memo alone
    // would never reach them again. See reseed.
    this.settled = new Set();
    this.watch();
    this.dead = false;
    this.worker = new Worker('/static/thumbworker.js', { type: 'module' });
    this.worker.onmessage = (e) => this.onResult(e.data);
    // A worker that fails at module load never installs its own message handler, so no
    // reply ever comes back and every 3D card keeps its spinner for the life of the
    // page with nothing tying the failure to the UI. Give up loudly instead: settle the
    // cards waiting on it and stop asking.
    this.worker.onerror = (e) => this.fail(e && e.message);
    this.worker.onmessageerror = () => this.fail('a thumbnail result could not be delivered');
    // Seed the worker's (localStorage-less) rig registry with the bodies the user has
    // opened or pinned, so mesh-less clips whose rig can't be auto-discovered still pose
    // on the character the user chose in the lightbox.
    this.worker.postMessage({ type: 'seed', list: CharRegistry.list() });
  }
  // reseed hands the worker the rig registry again. The worker has no localStorage, so
  // a body the user picks or pins in the lightbox reaches it only by being sent — and
  // the noRender memo has to go with it, or every clip that could now pose on that body
  // keeps the category icon it was given before the user answered the question.
  reseed() {
    if (this.dead) return;
    this.jobs.clearNoRender();
    // Forgetting the answer is not enough to ask the question again. Both places that
    // settle "nothing to draw" stop observing the holder, so the cards this exists for
    // — the ones on screen when the user picked the body — would keep their category
    // icon with nothing left watching to notice. Re-observing an already-visible node
    // queues a fresh initial observation, so those re-request immediately.
    for (const holder of this.settled) {
      if (holder.isConnected) this.vis.observe(holder);
    }
    this.settled.clear();
    this.worker.postMessage({ type: 'seed', list: CharRegistry.list() });
  }
  // settle stops watching a holder there is nothing to draw for, remembering it so
  // reseed can put it back when the answer might have changed.
  settle(holder) {
    this.settled.add(holder);
    this.vis.unobserve(holder);
  }
  // Unlike the one-shot lazyWork observer, this one keeps watching: a card that
  // scrolls away before its turn in the worker's serial queue has its render cancelled,
  // and asks again if it comes back. Without the cancel, a fast scroll leaves the queue
  // grinding through cards nobody is looking at before it reaches the visible ones;
  // without the re-request, a cancelled card would keep its icon forever.
  watch() {
    this.vis = new IntersectionObserver((entries) => {
      for (const e of entries) {
        if (e.isIntersecting) this.request(e.target);
        else this.cancel(e.target);
      }
    }, { rootMargin: '200px' });
  }
  observe(holder, asset) {
    if (this.dead) return; // the category icon already in the holder is the final answer
    holder._asset = asset;
    this.vis.observe(holder);
  }
  fail(message) {
    if (this.dead) return;
    this.dead = true;
    console.error('thumbnail worker stopped; 3D previews in the grid are unavailable', message || '');
    for (const holder of this.jobs.drainPending()) {
      if (holder.isConnected) holder.classList.remove('loading');
      this.vis.unobserve(holder);
    }
    this.vis.disconnect();
  }
  // forget lets go of one card the grid is dropping: the render queued for it is
  // cancelled (unless another card on screen still wants the same asset) and the
  // observer releases the node, which it would otherwise hold indefinitely.
  forget(holder) {
    this.cancel(holder);
    this.settled.delete(holder);
    if (this.vis) this.vis.unobserve(holder);
  }
  cancel(holder) {
    const asset = holder._asset;
    if (!asset) return;
    const wanted = this.jobs.release(asset.id, holder);
    holder.classList.remove('loading');
    if (wanted) this.worker.postMessage({ type: 'cancel', id: asset.id });
  }
  request(holder) {
    const asset = holder._asset;
    if (this.dead) return;
    const next = this.jobs.route(asset.id, holder);
    if (next.kind === 'settled') {
      holder.classList.remove('loading');
      this.settle(holder);
      return;
    }
    if (next.kind === 'cached') { this.swap(holder, next.url); return; }
    holder.classList.add('loading');
    if (next.kind === 'joined') return;
    // Only the fields the worker's build needs — DTOs aren't structured-clone-friendly wholesale.
    this.worker.postMessage({
      id: asset.id,
      seq: next.seq,
      asset: {
        id: asset.id, name: asset.name, ext: asset.ext, vendor: asset.vendor, pack: asset.pack, thumb: asset.thumb,
        source: {
          clip: asset.source && asset.source.clip,
          clipIndex: asset.source && asset.source.clipIndex,
          filePath: asset.source && asset.source.filePath,
          parts: asset.source && asset.source.parts,
        },
      },
    });
  }
  onResult({ id, seq, blob }) {
    // Claimed before any object URL exists, which is what keeps a displaced URL from
    // escaping the cache bound unrevoked when the result turns out to be stale.
    const holders = this.jobs.claim(id, seq);
    if (!holders) return;
    if (blob) {
      const url = URL.createObjectURL(blob);
      // The cache decides which URLs are no longer reachable; releasing them is this
      // side's job, since it is the side that minted them.
      for (const stale of this.jobs.remember(id, url)) URL.revokeObjectURL(stale);
      for (const holder of holders) {
        if (holder.isConnected) this.swap(holder, url);
      }
    } else {
      this.jobs.markNoRender(id);
      for (const holder of holders) {
        if (!holder.isConnected) continue;
        holder.classList.remove('loading'); // no render (failed / mesh-less with no rig)
        // Settled: there is nothing to draw for this asset, so stop watching rather than
        // re-asking the worker every time the card scrolls back into view.
        this.settle(holder);
      }
    }
  }
  async swap(holder, url) {
    const img = new Image();
    img.src = url;
    // Decode on a background thread before inserting, so painting the thumbnail never
    // blocks the main thread — the jank felt while many thumbnails streamed in during a
    // lightbox animation was the per-image decode on paint.
    try { await img.decode(); } catch { /* fall through and insert anyway */ }
    this.vis.unobserve(holder);
    if (holder.isConnected) holder.replaceWith(img);
  }
}
export const modelThumbs = new ModelThumbnails();

// forgetThumbs releases the observers watching every deferred thumbnail in a subtree.
// Both hold their targets, so a node dropped from the DOM without this keeps itself,
// and the asset it points at, alive until the observer is next rebuilt wholesale.
export function forgetThumbs(root) {
  for (const h of root.querySelectorAll('.thumb-3d')) modelThumbs.forget(h);
  for (const f of root.querySelectorAll('.font-thumb')) lazyWork.forget(f);
}
