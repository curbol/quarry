import test from 'node:test';
import assert from 'node:assert/strict';
import { FontCache, FONT_CACHE_MAX } from '../assets/fontcache.js';

// Holders are opaque to the cache; thumbs.js passes DOM nodes and asks isConnected.
const card = (name) => ({ name, live: true });
const liveHolder = (h) => !!h && h.live;

test('a hit refreshes eviction order', () => {
  const c = new FontCache({ max: 2, live: liveHolder });
  c.remember('a', 'face:a', null);
  c.remember('b', 'face:b', null);
  assert.equal(c.hit('a'), 'face:a');
  // a is the older of the two until it is hit, so b is now what goes first.
  assert.deepEqual(c.remember('c', 'face:c', null), ['face:b']);
  assert.equal(c.hit('b'), undefined);
  assert.equal(c.hit('a'), 'face:a');
});

// The lightbox's specimen asks for a font with no card behind it. Letting that clear
// the slot makes a grid card that is still on screen evictable, which is the whole
// failure this cache is the bound for.
test('a holderless hit does not clear the holder already recorded', () => {
  const c = new FontCache({ max: 1, live: liveHolder });
  const el = card('still-on-screen');
  c.remember('a', 'face:a', el);
  c.hit('a');          // the specimen: no holder given
  c.hit('a', null);    // and the same through an absent el
  assert.deepEqual(c.makeRoom(), [], 'a live card kept its font');
  el.live = false;
  assert.deepEqual(c.makeRoom(), ['face:a'], 'and lost it once the card was gone');
});

// Room is made before the insert. Inserted first, a new entry is last in eviction
// order, so a walk skipping every live holder ahead of it arrives at the entry just
// added — and drops it when it came with no holder of its own. The FontFace was then
// deleted before the caller could use it, and the specimen rendered in the fallback
// serif with nothing said.
test('a new entry is never its own victim', () => {
  const c = new FontCache({ max: 2, live: liveHolder });
  c.remember('a', 'face:a', card('a'));
  c.remember('b', 'face:b', card('b'));
  // Both holders are live and the cache is full: the specimen's own entry is the only
  // thing the walk would be willing to drop.
  const dropped = c.remember('specimen', 'face:s', undefined);
  assert.deepEqual(dropped, [], 'nothing evictable, so nothing evicted');
  assert.equal(c.hit('specimen'), 'face:s', 'the entry just added is still there');
});

test('eviction takes the oldest dead holders and only those', () => {
  const c = new FontCache({ max: 3, live: liveHolder });
  const dead1 = card('dead1'), keep = card('keep'), dead2 = card('dead2');
  dead1.live = false;
  dead2.live = false;
  c.remember('d1', 'face:d1', dead1);
  c.remember('k', 'face:k', keep);
  c.remember('d2', 'face:d2', dead2);
  assert.deepEqual(c.remember('new', 'face:new', null), ['face:d1'], 'the oldest dead one, once');
  assert.equal(c.hit('k'), 'face:k', 'the live card kept its font');
  assert.equal(c.hit('d2'), 'face:d2', 'and the younger dead one was not taken with it');
});

// A load that failed is not an answer. Left in, it is re-thrown for every later request
// for that font, and there is nothing to release because nothing was ever registered.
test('forget drops an id without handing anything back', () => {
  const c = new FontCache({ max: 2, live: liveHolder });
  c.remember('a', 'face:a', null);
  c.forget('a');
  assert.equal(c.hit('a'), undefined);
  assert.equal(c.size, 0);
});

// The cache is allowed to sit over its backstop rather than evict a font a card is
// showing: the bound that matters is how many cards the grid keeps connected, and a
// fixed number smaller than a viewport's worth of font cards is how the visible ones
// got evicted in the first place.
test('a wall of live holders is never evicted to meet the bound', () => {
  const c = new FontCache({ max: 2, live: liveHolder });
  c.remember('a', 'face:a', card('a'));
  c.remember('b', 'face:b', card('b'));
  assert.deepEqual(c.remember('c', 'face:c', card('c')), []);
  assert.equal(c.size, 3, 'over the backstop, with every card still on screen');
});

// Straddled, not restated: the default has to be larger than a viewport's worth of
// font cards or the visible ones are what get dropped.
test('the default bound is far larger than one screenful', () => {
  assert.ok(FONT_CACHE_MAX > 50, `FONT_CACHE_MAX is ${FONT_CACHE_MAX}; a screenful of font cards would evict itself`);
  assert.equal(new FontCache().max, FONT_CACHE_MAX);
});

// Without a liveness predicate this is a plain LRU. Defaulting to "everything is live"
// would make a cache built without one silently unbounded.
test('with no liveness predicate nothing is pinned', () => {
  const c = new FontCache({ max: 1 });
  c.remember('a', 'face:a', card('a'));
  assert.deepEqual(c.remember('b', 'face:b', card('b')), ['face:a']);
});
