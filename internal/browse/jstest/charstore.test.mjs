import test from 'node:test';
import assert from 'node:assert/strict';
import { CharRegistry, MEMORY_MAX, STORED_MAX, contentURL, thumbURL } from '../assets/charstore.js';

// Node has no localStorage, which is the same branch the thumbnail worker takes, so
// these run against the in-memory fallback the shim provides for exactly that case.
function reset() {
  CharRegistry.save([]);
}

function rig(id, extra = {}) {
  return { id, bones: Array.from({ length: 12 }, (_, i) => 'Bone' + i), ...extra };
}

test('the URL builders escape the id', () => {
  assert.equal(contentURL('a b&c'), '/api/content?id=a%20b%26c');
  assert.equal(thumbURL('a/b'), '/api/thumb?id=a%2Fb');
});

test('add reports whether what the matcher can pick actually changed', () => {
  reset();
  assert.equal(CharRegistry.add(rig('a')), true, 'a new entry is a change');
  assert.equal(CharRegistry.add(rig('a')), false, 'the identical entry again is not');
  assert.equal(CharRegistry.add(rig('a', { vendor: 'synty' })), true, 'a changed field is');
});

test('too few bones is not a rig', () => {
  reset();
  assert.equal(CharRegistry.add({ id: 'thin', bones: ['Root'] }), false);
  assert.equal(CharRegistry.list().length, 0);
});

// rigEntry rebuilds an entry from the model and knows nothing about pinning, so
// re-registering a character the user pinned must not quietly un-pin it.
test('re-registering carries the pinned flag across', () => {
  reset();
  CharRegistry.add(rig('a'));
  assert.equal(CharRegistry.pin('a', true), true, 'pin reports the flag moved');
  assert.equal(CharRegistry.pin('a', true), false, 'and reports when it did not');
  CharRegistry.add(rig('a', { vendor: 'synty' }));
  assert.equal(CharRegistry.isPinned('a'), true);
});

test('the most recent entry leads, and remove takes only its own', () => {
  reset();
  CharRegistry.add(rig('a'));
  CharRegistry.add(rig('b'));
  assert.deepEqual(CharRegistry.list().map((e) => e.id), ['b', 'a']);
  CharRegistry.add(rig('a', { vendor: 'v' }));
  assert.deepEqual(CharRegistry.list().map((e) => e.id), ['a', 'b'], 're-adding moves it to the front, not a duplicate');
  CharRegistry.remove('a');
  assert.deepEqual(CharRegistry.list().map((e) => e.id), ['b']);
});

// The store is what a scroll and a session accumulate into, and unbounded it grows
// until a save throws — on the page, where it is a single localStorage value — and
// every later one is silently dropped. Node has no localStorage, so this runs on the
// worker's branch and straddles that realm's bound rather than restating a number.
test('the registry is bounded', () => {
  reset();
  for (let i = 0; i < MEMORY_MAX + 10; i++) CharRegistry.add(rig('c' + i));
  assert.equal(CharRegistry.list().length, MEMORY_MAX);
  assert.equal(CharRegistry.list()[0].id, 'c' + (MEMORY_MAX + 9), 'the newest survives the trim, not the oldest');
});

// Pinning a body makes it the default for every clip on that rig, and add() unshifts on
// every lightbox open of a rigged character — so browsing a character library pushes a
// pinned entry past the bound within one session. Trimmed by recency alone it is simply
// dropped: the bookmark comes back empty and every clip reverts to coverage ranking,
// with nothing said. The bound is still the bound; pinned entries are held above it.
test('the recency trim does not drop a pinned entry', () => {
  reset();
  CharRegistry.add(rig('kept'));
  assert.equal(CharRegistry.pin('kept', true), true);
  for (let i = 0; i < MEMORY_MAX + 10; i++) CharRegistry.add(rig('c' + i));
  assert.equal(CharRegistry.isPinned('kept'), true, 'the pinned body was trimmed away by newer ones');
  assert.equal(CharRegistry.list().length, MEMORY_MAX, 'and the bound still holds');
});

// An entry saved without a vendor is a wildcard the scope check in match() no longer
// skips. The picker's suggestion rows once rebuilt an item from a stored entry without
// carrying the field, so choosing one re-registered that body as cross-vendor — in the
// grid's thumbnails as well as the lightbox, and persisted. Re-registration must never
// widen what an entry already knows.
test('re-registering cannot widen a body past its vendor', () => {
  reset();
  const clipBones = Array.from({ length: 12 }, (_, i) => 'Bone' + i);
  CharRegistry.add({ id: 'body', vendor: 'kevdev', name: 'HumanM_Model', bones: clipBones });
  assert.equal(CharRegistry.match(clipBones, 'synty', 'A_Walk'), null, 'scoped to its own vendor to begin with');

  // What the picker hands back when the row it built carried no vendor.
  CharRegistry.add({ id: 'body', name: 'HumanM_Model', bones: clipBones });
  assert.equal(CharRegistry.list()[0].vendor, 'kevdev');
  assert.equal(CharRegistry.match(clipBones, 'synty', 'A_Walk'), null, 'still not another vendor\'s to match');
});

// A value that will not parse has to read as an empty registry rather than throwing out
// of every caller — list() is on the path of every card that needs a body — and it has
// to say so, because the next save overwrites it and the user's pinned characters go
// with it. The shim reads localStorage when there is one, which is also the branch the
// page takes, so this is where a truncated or hand-edited value actually arrives.
test('a corrupt stored value reads as empty rather than throwing, and says so', () => {
  const backing = new Map();
  globalThis.localStorage = {
    getItem: (k) => (backing.has(k) ? backing.get(k) : null),
    setItem: (k, v) => backing.set(k, String(v)),
  };
  const warned = [];
  const prevWarn = console.warn;
  console.warn = (...args) => warned.push(args);
  try {
    CharRegistry.add(rig('a'));
    assert.equal(CharRegistry.list().length, 1, 'the fake storage is the one being read');
    backing.set(CharRegistry.key, '[{"id":"a",');
    assert.deepEqual(CharRegistry.list(), []);
  } finally {
    console.warn = prevWarn;
    delete globalThis.localStorage;
  }
  assert.equal(warned.length, 1, 'silently it is indistinguishable from a fresh profile');
});

// The ranking itself is rigmatch.js's, checked there. What is pinned here is that the
// registry hands it the stored list and its own vendor scoping, so a stored body is
// reachable at all — and that a body from another vendor is not, since cross-vendor
// skeletons share enough bone names to pass the coverage bar with a different rest pose.
test('match picks a stored body that covers the clip, within its vendor', () => {
  reset();
  const clipBones = Array.from({ length: 12 }, (_, i) => 'Bone' + i);
  CharRegistry.add({ id: 'body', vendor: 'synty', name: 'Character_Model', bones: clipBones.concat(['Prop', 'Cape']) });
  const got = CharRegistry.match(clipBones, 'synty', 'Character@Walk');
  assert.ok(got && got.id === 'body', `match = ${JSON.stringify(got)}`);
  assert.equal(CharRegistry.match(clipBones, 'kevdev', 'Character@Walk'), null, 'another vendor\'s body is not a match');
  assert.equal(CharRegistry.match([], 'synty', 'Character@Walk'), null, 'a clip with no bones asks for nothing');
});

test('hasNamed asks about the body a clip is named after', () => {
  reset();
  assert.equal(CharRegistry.hasNamed({ vendor: 'synty', name: 'HumanM@CombatIdle' }), false);
  CharRegistry.add(rig('m', { vendor: 'synty', name: 'HumanM_Model' }));
  assert.equal(CharRegistry.hasNamed({ vendor: 'synty', name: 'HumanM@CombatIdle' }), true);
  assert.equal(CharRegistry.hasNamed({ vendor: 'kevdev', name: 'HumanM@CombatIdle' }), false);
});

// The worker's registry is bounded by nothing but memory, and its entries are the ones
// the page cannot rebuild: discoverForVendor found them by loading models, and the
// memos that record a scope as already searched live on CharRegistry rather than in the
// store, so they survive an eviction and short-circuit every re-search. Bounding both
// realms at the main thread's localStorage quota meant a reseed — which viewer.js fires
// on every newly registered lightbox model, carrying the whole main-thread list — pushed
// those discoveries out for the rest of the session, and the pack's clips fell back to
// the category icon with nothing said.
//
// Node has no localStorage, so these run on exactly the branch the worker takes.
test('a full reseed does not evict the bodies the worker discovered itself', () => {
  reset();
  const discovered = ['disc0', 'disc1', 'disc2'];
  for (const id of discovered) CharRegistry.add(rig(id, { vendor: 'synty' }));

  // What thumbworker's seed handler does with a main thread whose list is at its cap.
  const seed = Array.from({ length: 40 }, (_, i) => rig('seed' + i, { vendor: 'synty' }));
  for (let i = seed.length - 1; i >= 0; i--) CharRegistry.add(seed[i]);

  const ids = new Set(CharRegistry.list().map((e) => e.id));
  for (const id of discovered) {
    assert.ok(ids.has(id), `${id} was evicted by the reseed and cannot be found again`);
  }
  for (const e of seed) {
    assert.ok(ids.has(e.id), `${e.id} did not survive the merge`);
  }
});

// The bound the worker keeps has to be the larger of the two, or the test above passes
// while a reseed still evicts: a merge carries the page's whole list, so anything at or
// below the page's own bound leaves no room for what the worker found.
test('the worker is bounded more loosely than the page', () => {
  assert.ok(MEMORY_MAX > STORED_MAX, `worker bound ${MEMORY_MAX} does not clear the page's ${STORED_MAX}`);
});

// The other side of the pinned hoist. Pinned entries win the contest for the bound's
// slots, but the bound is still the bound: pin more bodies than it holds and the oldest
// pinned ones go, because save() hoists and then slices. That is the deliberate trade —
// an unbounded store is a save that throws and a fallback nobody asked for — and it was
// pinned from neither side, so a change making pinned entries exempt, or dropping the
// hoist, both passed.
test('the bound still applies once everything in it is pinned', () => {
  reset();
  const all = [];
  for (let i = 0; i < MEMORY_MAX + 10; i++) all.push(rig('p' + i, { pinned: true }));
  CharRegistry.save(all);

  const kept = CharRegistry.list();
  assert.equal(kept.length, MEMORY_MAX, 'pinning must not let the store grow past its bound');
  assert.ok(kept.every((e) => e.pinned), 'pinned entries are held above the trim');
  assert.equal(kept[0].id, 'p0', 'save keeps the head of the list it was handed');
  // And a pinned entry still outranks an unpinned one at the boundary.
  reset();
  const mixed = [rig('unpinned-first')];
  for (let i = 0; i < MEMORY_MAX; i++) mixed.push(rig('q' + i, { pinned: true }));
  CharRegistry.save(mixed);
  assert.ok(!CharRegistry.list().some((e) => e.id === 'unpinned-first'),
    'an unpinned entry took a slot from a pinned one');
});
