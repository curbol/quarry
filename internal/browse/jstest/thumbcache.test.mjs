import test from 'node:test';
import assert from 'node:assert/strict';
import { ThumbCache, THUMB_CACHE_MAX, NO_RENDER_MAX } from '../assets/thumbcache.js';

// Holders are opaque to the cache; thumbs.js passes DOM nodes.
const holder = (name) => ({ name });

test('a fresh asset dispatches, a second holder joins it', () => {
  const c = new ThumbCache();
  const a = holder('a'), b = holder('b');
  const first = c.route('x', a);
  assert.equal(first.kind, 'dispatch');
  assert.ok(first.seq > 0, 'a dispatch carries the sequence number the worker echoes back');
  // The same asset on screen twice — a grid card and the lightbox's related strip —
  // must be one render answering both, not two.
  assert.deepEqual(c.route('x', b), { kind: 'joined' });
});

// Insertion order is eviction order, so a thumbnail the user is looking at has to be
// re-inserted when it is served: without that it ages out on its position in a scroll
// that has since come back to it, and the visible card loses its image to a render the
// cache already had.
test('a rendered thumbnail is served from the cache and moves to newest', () => {
  const c = new ThumbCache({ cacheMax: 2 });
  const h = holder('h');
  const { seq } = c.route('x', h);
  assert.deepEqual(c.remember('x', 'blob:x'), [], 'nothing displaced');
  assert.deepEqual(c.claim('x', seq), [h]);
  c.remember('y', 'blob:y');

  // x is the older of the two, until it is served.
  assert.deepEqual(c.route('x', holder('h2')), { kind: 'cached', url: 'blob:x' });
  assert.deepEqual(c.remember('z', 'blob:z'), ['blob:y'], 'the untouched entry is the one evicted');
  assert.deepEqual(c.route('x', holder('h3')), { kind: 'cached', url: 'blob:x' });
});

// Every URL the cache stops pointing at is a PNG blob the browser holds until it is
// revoked. Dropping one silently is a leak outside the bound entirely, and there is
// nothing to notice it by.
test('every displaced URL is handed back to be revoked', () => {
  const c = new ThumbCache({ cacheMax: 3 });
  assert.deepEqual(c.remember('x', 'blob:1'), []);
  assert.deepEqual(c.remember('x', 'blob:2'), ['blob:1'], 'overwriting a key releases what it held');
  assert.deepEqual(c.remember('x', 'blob:2'), [], 'storing the same URL again releases nothing');

  const released = [];
  for (const id of ['a', 'b', 'c', 'd']) released.push(...c.remember(id, 'blob:' + id));
  assert.deepEqual(released, ['blob:2', 'blob:a'], 'the oldest go, in order');
  assert.equal(c.cache.size, 3);
});

// Insertion order is eviction order, and Map.set on a key already present leaves that
// order alone — so a re-rendered thumbnail would keep the slot of the render it
// replaced and age out on a position the scroll has since come back past, losing the
// visible card its image to a render the cache had just made. route() currently answers
// `cached` before it can dispatch for an id the cache holds, so nothing in the browser
// reaches this today; it is one "re-render this card" feature away from doing so.
test('re-remembering an id moves it to newest rather than keeping its old slot', () => {
  const c = new ThumbCache({ cacheMax: 2 });
  c.remember('x', 'blob:x1');
  c.remember('y', 'blob:y');
  assert.deepEqual(c.remember('x', 'blob:x2'), ['blob:x1'], 'the displaced URL still comes back');
  assert.deepEqual(c.remember('z', 'blob:z'), ['blob:y'], 'y is the oldest now, not the re-rendered x');
  assert.deepEqual(c.route('x', holder('h')), { kind: 'cached', url: 'blob:x2' });
});

test('the cache holds no more than its bound over a long scroll', () => {
  const c = new ThumbCache();
  let released = 0;
  for (let i = 0; i < THUMB_CACHE_MAX * 3; i++) released += c.remember('id' + i, 'blob:' + i).length;
  assert.equal(c.cache.size, THUMB_CACHE_MAX);
  assert.equal(released, THUMB_CACHE_MAX * 2, 'everything evicted was handed back');
});

// The sequence number is what makes "this exact request" distinct from "the same asset,
// asked for again". Matching on the id alone let a superseded job's result land on the
// current card, and — worse — mint an object URL for a render nothing was waiting on.
test('a superseded result is refused, and the current one is not', () => {
  const c = new ThumbCache();
  const h = holder('h');
  const first = c.route('x', h);
  assert.equal(c.release('x', h), true, 'the last holder leaving cancels the render');
  const second = c.route('x', h);
  assert.notEqual(second.seq, first.seq);

  assert.equal(c.claim('x', first.seq), null, 'the cancelled request is stale');
  assert.deepEqual(c.claim('x', second.seq), [h]);
  assert.equal(c.claim('x', second.seq), null, 'and a result is claimed only once');
});

test('a render is cancelled only when the last holder lets go', () => {
  const c = new ThumbCache();
  const a = holder('a'), b = holder('b');
  const { seq } = c.route('x', a);
  c.route('x', b);
  assert.equal(c.release('x', a), false, 'another card on screen still wants it');
  assert.deepEqual(c.claim('x', seq), [b], 'and it is still claimable for that one');
});

test('releasing something that was never pending is not a cancel', () => {
  const c = new ThumbCache();
  assert.equal(c.release('nothing', holder('h')), false);
  const h = holder('h');
  c.route('x', h);
  assert.equal(c.release('x', holder('other')), false, 'a holder that never joined does not cancel it');
});

// The memo outlives the card that learned it — the grid rebuilds its live window every
// few dozen rows — so re-asking would cost the worker its whole rig discovery again.
test('nothing to draw is remembered, bounded, and forgettable', () => {
  const c = new ThumbCache({ noRenderMax: 3 });
  c.markNoRender('x');
  assert.deepEqual(c.route('x', holder('h')), { kind: 'settled' });

  for (const id of ['a', 'b', 'c']) c.markNoRender(id);
  assert.equal(c.noRender.size, 3);
  assert.deepEqual(c.route('x', holder('h')), { kind: 'dispatch', seq: 1 }, 'x aged out of the memo');

  // Picking a body in the lightbox changes the answer for every clip that could pose
  // on it, so the memo has to be droppable wholesale.
  c.clearNoRender();
  assert.deepEqual(c.route('c', holder('h')), { kind: 'dispatch', seq: 2 });
});

test('the no-render memo default is far larger than the thumbnail cache', () => {
  // Entries are ids rather than blobs, so it can be — but it still must not grow to one
  // per asset in a 150k-item library.
  assert.ok(NO_RENDER_MAX > THUMB_CACHE_MAX, `NO_RENDER_MAX=${NO_RENDER_MAX} THUMB_CACHE_MAX=${THUMB_CACHE_MAX}`);
  assert.ok(NO_RENDER_MAX < 150000);
});

// A worker that has stopped answering leaves every waiting card spinning, so the
// holders have to come back to be un-marked and released from the observer.
test('draining hands back every waiting holder and leaves nothing pending', () => {
  const c = new ThumbCache();
  const a = holder('a'), b = holder('b'), d = holder('d');
  c.route('x', a);
  c.route('x', b);
  c.route('y', d);
  const drained = c.drainPending();
  assert.equal(drained.length, 3);
  assert.deepEqual(new Set(drained), new Set([a, b, d]));
  assert.equal(c.pending.size, 0);
  assert.equal(c.claim('x', 1), null, 'a result arriving afterwards finds nothing waiting');
});

// Draining is about requests in flight, so it must leave both memos alone. A drain
// that also cleared the cache would drop every resident URL without handing one back
// to be revoked, which is a leak the bound cannot see; one that cleared noRender
// would re-ask the worker for every mesh-less clip on the next scroll. Neither shows
// up in the DOM, and the current implementation touches neither only by omission.
test('draining leaves the rendered and no-render memos untouched', () => {
  const c = new ThumbCache();
  c.remember('rendered', 'blob:rendered');
  c.markNoRender('empty');
  c.route('inflight', holder('h'));

  assert.equal(c.drainPending().length, 1);

  assert.deepEqual(c.route('rendered', holder('h2')), { kind: 'cached', url: 'blob:rendered' },
    'a resident URL survives the drain');
  assert.deepEqual(c.route('empty', holder('h3')), { kind: 'settled' },
    'a settled answer survives the drain');
});
